package loadgen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenLoopKeepsOfferingLoadWhileTheServerIsSlow(t *testing.T) {
	// This is the property the whole generator exists for. A closed-loop
	// generator would send roughly one request per worker per response, so a
	// server this slow would receive a handful. Open-loop keeps to the schedule,
	// which is what makes the recorded tail latency honest.
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		time.Sleep(300 * time.Millisecond)
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	run, err := Execute(context.Background(), Config{
		URL:         srv.URL,
		Body:        []byte(`{}`),
		RPS:         50,
		Duration:    time.Second,
		MaxInFlight: 512,
		Timeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 50 rps for 1s against a 300ms server: a closed-loop generator with a
	// handful of workers would offer far fewer than 40.
	if run.Offered < 40 {
		t.Errorf("offered %d requests, want at least 40: the generator stopped offering load when the server slowed", run.Offered)
	}
	if run.PeakInUse < 10 {
		t.Errorf("peak in-flight %d, want at least 10 concurrent: arrivals were serialised", run.PeakInUse)
	}
}

func TestLatencyIsMeasuredFromScheduledArrival(t *testing.T) {
	// If latency were measured from when a worker picked the request up, a
	// saturated generator would report the server as fast while clients queued.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	run, err := Execute(context.Background(), Config{
		URL: srv.URL, Body: []byte(`{}`),
		RPS: 20, Duration: 600 * time.Millisecond, MaxInFlight: 64, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Results) == 0 {
		t.Fatal("no results recorded")
	}
	for _, r := range run.Results {
		if r.Latency < 45*time.Millisecond {
			t.Fatalf("latency %v is below the server's own 50ms floor; it was not measured from the scheduled arrival", r.Latency)
		}
	}
}

func TestWarmupResultsAreDiscarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	run, err := Execute(context.Background(), Config{
		URL: srv.URL, Body: []byte(`{}`),
		RPS: 100, Warmup: 400 * time.Millisecond, Duration: 400 * time.Millisecond,
		MaxInFlight: 64, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Roughly 80 requests are offered across both windows; only the measured
	// half may be recorded.
	if len(run.Results) >= run.Offered {
		t.Errorf("recorded %d of %d offered: warmup was not discarded", len(run.Results), run.Offered)
	}
	for _, r := range run.Results {
		if r.ScheduledAt.Before(run.WindowStart) {
			t.Fatal("a warmup result leaked into the measured set")
		}
	}
}

func TestStreamingTimingIsCapturedPerChunk(t *testing.T) {
	const chunks = 5
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		time.Sleep(40 * time.Millisecond) // TTFT
		// A role-only frame first, exactly as OpenAI sends. It must NOT count
		// as the first token, or a gateway could post a flattering TTFT by
		// emitting an empty opening frame.
		writeFrame(w, f, map[string]any{"role": "assistant"})
		for i := 0; i < chunks; i++ {
			time.Sleep(20 * time.Millisecond)
			writeFrame(w, f, map[string]any{"content": fmt.Sprintf("t%d ", i)})
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer srv.Close()

	run, err := Execute(context.Background(), Config{
		URL: srv.URL, Body: []byte(`{}`), Stream: true,
		RPS: 5, Duration: 500 * time.Millisecond, MaxInFlight: 8, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Results) == 0 {
		t.Fatal("no streaming results recorded")
	}
	r := run.Results[0]
	if r.Chunks != chunks {
		t.Errorf("chunks = %d, want %d", r.Chunks, chunks)
	}
	if r.TTFT < 55*time.Millisecond {
		t.Errorf("TTFT %v is too low: the role-only frame was counted as the first token", r.TTFT)
	}
	if len(r.InterChunk) != chunks-1 {
		t.Errorf("inter-chunk gaps = %d, want %d", len(r.InterChunk), chunks-1)
	}
	if r.Truncated {
		t.Error("a stream that ended with [DONE] was marked truncated")
	}
}

func TestTruncatedStreamIsFlagged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		writeFrame(w, f, map[string]any{"content": "only "})
		// No [DONE]: the realistic mid-stream failure.
	}))
	defer srv.Close()

	run, err := Execute(context.Background(), Config{
		URL: srv.URL, Body: []byte(`{}`), Stream: true,
		RPS: 5, Duration: 300 * time.Millisecond, MaxInFlight: 4, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Results) == 0 {
		t.Fatal("no results")
	}
	if !run.Results[0].Truncated {
		t.Error("a stream with no terminator was not flagged as truncated")
	}
}

func TestSaturationIsReportedAsDroppedNotHidden(t *testing.T) {
	// Blocking on a full in-flight semaphore would silently turn this into a
	// closed-loop generator. Dropping and counting keeps it honest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	run, err := Execute(context.Background(), Config{
		URL: srv.URL, Body: []byte(`{}`),
		RPS: 200, Duration: time.Second, MaxInFlight: 5, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Dropped == 0 {
		t.Error("expected drops when the in-flight cap is far below the offered rate")
	}
	if run.Offered <= run.Dropped {
		t.Error("offered count should include both sent and dropped arrivals")
	}
}

func TestBodyFnVariesPerRequest(t *testing.T) {
	// Every perf request must be unique so an incidentally-enabled cache on a
	// target cannot turn a proxy benchmark into a cache benchmark.
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen[string(b)] = true
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	_, err := Execute(context.Background(), Config{
		URL: srv.URL, BodyFn: func(i int) []byte { return []byte(fmt.Sprintf(`{"i":%d}`, i)) },
		RPS: 30, Duration: 400 * time.Millisecond, MaxInFlight: 16, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) < 5 {
		t.Errorf("only %d distinct bodies reached the server", len(seen))
	}
}

func writeFrame(w http.ResponseWriter, f http.Flusher, delta map[string]any) {
	payload, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	})
	_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
	f.Flush()
}
