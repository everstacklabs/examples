package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := New()
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func chatReq(t *testing.T, ts *httptest.Server, path string, body map[string]any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	return resp
}

func TestFaultRateIsDeterministicNotRandom(t *testing.T) {
	// The whole point of a deterministic stride is that a scenario asserts an
	// exact count. A PRNG here would force statistical tolerances, and a
	// tolerance is exactly where an off-by-N retry bug hides.
	p := DefaultProfile("p")
	p.Fault = FaultStatus
	p.FaultRate = 0.2
	p.Status = 503

	failures := 0
	for i := 0; i < 100; i++ {
		if mode, _ := p.nextFault(); mode == FaultStatus {
			failures++
		}
	}
	if failures != 20 {
		t.Errorf("fault rate 0.2 over 100 requests produced %d failures, want exactly 20", failures)
	}
}

func TestFailFirstNIsExact(t *testing.T) {
	// Failover recovery time is only measurable if the upstream recovers on a
	// known request, not a probable one.
	p := DefaultProfile("p")
	p.Fault = FaultFailFirstN
	p.FailFirstN = 3

	var modes []FaultMode
	for i := 0; i < 6; i++ {
		m, _ := p.nextFault()
		modes = append(modes, m)
	}
	want := []FaultMode{FaultStatus, FaultStatus, FaultStatus, FaultNone, FaultNone, FaultNone}
	for i := range want {
		if modes[i] != want[i] {
			t.Fatalf("request %d: got %q, want %q (sequence %v)", i+1, modes[i], want[i], modes)
		}
	}
}

func TestUnaryResponseShapeAndUsage(t *testing.T) {
	_, ts := newTestServer(t)
	resp := chatReq(t, ts, "/v1/chat/completions", map[string]any{
		"model":      "bench-model",
		"max_tokens": 6,
		"messages":   []any{map[string]any{"role": "user", "content": "12345678"}},
	})
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// 8 characters at 4 chars per token is the deterministic ground truth that
	// scenario C9 compares a gateway's reported usage against.
	if out.Usage.PromptTokens != 2 {
		t.Errorf("prompt_tokens = %d, want 2", out.Usage.PromptTokens)
	}
	if out.Usage.CompletionTokens != 6 {
		t.Errorf("completion_tokens = %d, want 6 (max_tokens must cap the stream)", out.Usage.CompletionTokens)
	}
	if out.Usage.TotalTokens != 8 {
		t.Errorf("total_tokens = %d, want 8", out.Usage.TotalTokens)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content == "" {
		t.Errorf("expected one non-empty choice, got %+v", out.Choices)
	}
}

func TestStreamingEmitsRoleThenContentThenDone(t *testing.T) {
	_, ts := newTestServer(t)
	resp := chatReq(t, ts, "/v1/chat/completions", map[string]any{
		"model":      "bench-model",
		"stream":     true,
		"max_tokens": 3,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	var contentFrames, doneSeen int
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			doneSeen++
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil {
			t.Fatalf("unparseable SSE frame: %s", payload)
		}
		if len(frame.Choices) > 0 && frame.Choices[0].Delta.Content != "" {
			contentFrames++
		}
	}
	if contentFrames != 3 {
		t.Errorf("content frames = %d, want 3", contentFrames)
	}
	if doneSeen != 1 {
		t.Errorf("[DONE] terminators = %d, want exactly 1", doneSeen)
	}
}

func TestStreamAbortOmitsTerminator(t *testing.T) {
	// Scenario C4 depends on the stream ending WITHOUT [DONE], because that is
	// the realistic provider failure and the one gateways handle worst.
	s, ts := newTestServer(t)
	s.Registry.Set("primary", &Profile{
		Name: "primary", TTFT: time.Millisecond, ChunkDelay: time.Millisecond,
		Chunks: 20, Fault: FaultStreamAbort, FaultRate: 1, AbortAfterChunks: 4,
	})

	resp := chatReq(t, ts, "/p/primary/v1/chat/completions", map[string]any{
		"model": "bench-model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	defer resp.Body.Close()

	var content, done int
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.Contains(line, "[DONE]") {
			done++
		} else if strings.Contains(line, `"content"`) {
			content++
		}
	}
	if done != 0 {
		t.Errorf("aborted stream emitted a [DONE] terminator; the client could not detect truncation")
	}
	if content != 4 {
		t.Errorf("content frames = %d, want 4 before the abort", content)
	}
}

func TestRateLimitFaultSetsRetryAfter(t *testing.T) {
	s, ts := newTestServer(t)
	s.Registry.Set("primary", &Profile{
		Name: "primary", Fault: FaultRateLimit, FaultRate: 1, RetryAfterSec: 7,
	})
	resp := chatReq(t, ts, "/p/primary/v1/chat/completions", map[string]any{
		"model": "bench-model", "messages": []any{map[string]any{"role": "user", "content": "x"}},
	})
	defer resp.Body.Close()

	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want 7", got)
	}
}

func TestJournalRecordsParametersAndCountsRetries(t *testing.T) {
	// The journal is the ground truth for both parameter fidelity (C8) and
	// retry amplification (C3).
	s, ts := newTestServer(t)
	for i := 0; i < 3; i++ {
		resp := chatReq(t, ts, "/v1/chat/completions", map[string]any{
			"model": "bench-model", "temperature": 0.7, "seed": 4242,
			"messages": []any{map[string]any{"role": "user", "content": fmt.Sprintf("probe %d", i)}},
		})
		resp.Body.Close()
	}

	entries := s.Journal.Snapshot()
	if len(entries) != 3 {
		t.Fatalf("journal has %d entries, want 3", len(entries))
	}
	last := entries[2]
	if fmt.Sprint(last.Params["temperature"]) != "0.7" {
		t.Errorf("temperature not journalled: %v", last.Params)
	}
	if fmt.Sprint(last.Params["seed"]) != "4242" {
		t.Errorf("seed not journalled: %v", last.Params)
	}
	if last.Seq != 3 {
		t.Errorf("seq = %d, want 3", last.Seq)
	}
}

func TestJournalNeverStoresTheCredential(t *testing.T) {
	// The journal is written into results/ and may be attached to a published
	// report, so it must fingerprint the credential rather than keep it.
	s, ts := newTestServer(t)
	raw, _ := json.Marshal(map[string]any{
		"model": "bench-model", "messages": []any{map[string]any{"role": "user", "content": "x"}},
	})
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer sk-super-secret-value")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	dump, _ := json.Marshal(s.Journal.Snapshot())
	if bytes.Contains(dump, []byte("sk-super-secret-value")) {
		t.Fatal("the journal serialised the raw credential")
	}
	entries := s.Journal.Snapshot()
	if !entries[0].AuthPresent || entries[0].AuthFP == "" {
		t.Error("expected an auth fingerprint to be recorded")
	}
}

func TestProfilesAreIndependent(t *testing.T) {
	// Failover needs a failing primary and a healthy secondary at the same time.
	s, ts := newTestServer(t)
	s.Registry.Set("primary", &Profile{Name: "primary", Fault: FaultStatus, FaultRate: 1, Status: 503})

	bad := chatReq(t, ts, "/p/primary/v1/chat/completions", map[string]any{
		"model": "bench-model", "messages": []any{map[string]any{"role": "user", "content": "x"}},
	})
	bad.Body.Close()
	good := chatReq(t, ts, "/p/secondary/v1/chat/completions", map[string]any{
		"model": "bench-model", "messages": []any{map[string]any{"role": "user", "content": "x"}},
	})
	good.Body.Close()

	if bad.StatusCode != 503 {
		t.Errorf("primary status = %d, want 503", bad.StatusCode)
	}
	if good.StatusCode != 200 {
		t.Errorf("secondary status = %d, want 200; failover has nowhere to go", good.StatusCode)
	}
}
