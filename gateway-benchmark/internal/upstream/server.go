// Package upstream implements a deterministic, OpenAI-compatible model
// endpoint used as the single upstream for every gateway under test.
//
// Benchmarking gateways against a live provider measures the provider's
// variance, which is two orders of magnitude larger than gateway overhead. This
// server removes that variance: fixed time-to-first-token, fixed inter-chunk
// pacing, a deterministic token stream, and programmable faults. See
// METHODOLOGY.md section 4.
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Server is the mock upstream.
type Server struct {
	Registry *Registry
	Journal  *Journal
}

func New() *Server {
	return &Server{Registry: NewRegistry(), Journal: NewJournal()}
}

// Handler returns the full mux: the model surface plus the control API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Control plane. Namespaced under /__control so it can never collide with a
	// provider path a gateway might construct.
	mux.HandleFunc("POST /__control/profile/{name}", s.handleSetProfile)
	mux.HandleFunc("GET /__control/profile/{name}", s.handleGetProfile)
	mux.HandleFunc("POST /__control/reset", s.handleReset)
	mux.HandleFunc("GET /__control/journal", s.handleJournal)
	mux.HandleFunc("GET /__control/stats", s.handleStats)
	mux.HandleFunc("GET /__control/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})

	// Model surface, both at the root (default profile) and under /p/{profile}
	// so a single upstream can serve a healthy secondary and a failing primary
	// at the same time.
	for _, prefix := range []string{"", "/p/{profile}"} {
		mux.HandleFunc("POST "+prefix+"/v1/chat/completions", s.handleChat)
		mux.HandleFunc("POST "+prefix+"/v1/completions", s.handleChat)
		mux.HandleFunc("POST "+prefix+"/v1/embeddings", s.handleEmbeddings)
		mux.HandleFunc("POST "+prefix+"/v1/messages", s.handleAnthropicMessages)
		mux.HandleFunc("GET "+prefix+"/v1/models", s.handleModels)
	}

	// Catch-all. A gateway that builds the wrong upstream path otherwise gets
	// Go's bare "404 page not found" and never appears in the journal at all,
	// so the failure looks like a bad request rather than a path mismatch.
	// Recording it turns "why is this gateway 404ing" into a one-line answer.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/__control") {
			http.NotFound(w, r)
			return
		}
		s.Journal.Append(Entry{
			At:        time.Now(),
			Profile:   "-",
			Path:      r.URL.Path,
			Outcome:   "unmatched_path",
			Status:    404,
			ClientTag: r.Header.Get("X-Bench-Client"),
		})
		writeJSON(w, 404, oaiError("invalid_request_error",
			"no handler for "+r.Method+" "+r.URL.Path+
				" (the mock upstream serves /v1/... and /p/{profile}/v1/...); "+
				"this usually means the gateway's configured base URL double-prefixes or omits /v1"))
	})

	return mux
}

// ---------- chat completions ----------

type chatRequest struct {
	Model    string           `json:"model"`
	Stream   bool             `json:"stream"`
	Messages []map[string]any `json:"messages"`
	// Both spellings are accepted. OpenAI deprecated max_tokens in favour of
	// max_completion_tokens, and a gateway that modernises the field is doing
	// the right thing. Parsing only the old name would make the parameter
	// fidelity scenario accuse it of dropping a parameter it faithfully
	// translated, which is worse than not testing for it at all.
	MaxTokens           *int     `json:"max_tokens"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`
	Temperature         *float64 `json:"temperature"`
	TopP                *float64 `json:"top_p"`
	N                   *int     `json:"n"`
	Stop                any      `json:"stop"`
	Seed                *int     `json:"seed"`
	Tools               []any    `json:"tools"`
	ToolChoice          any      `json:"tool_choice"`
	ResponseFormat      any      `json:"response_format"`
	Logprobs            *bool    `json:"logprobs"`
	User                string   `json:"user"`
	StreamOptions       any      `json:"stream_options"`
}

// maxTokens returns whichever spelling the caller used.
func (c chatRequest) maxTokens() *int {
	if c.MaxTokens != nil {
		return c.MaxTokens
	}
	return c.MaxCompletionTokens
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	profile := s.Registry.Get(r.PathValue("profile"))

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, oaiError("invalid_request_error", "unreadable body"))
		return
	}
	var req chatRequest
	// A malformed body is itself a finding (some gateways rewrite bodies badly),
	// so record it rather than silently 400ing without a journal entry.
	decodeErr := json.Unmarshal(body, &req)

	entry := s.baseEntry(r, profile.Name, body, req)
	fault, status := profile.nextFault()

	if decodeErr != nil {
		entry.Outcome = "bad_json"
		entry.Status = 400
		s.Journal.Append(entry)
		writeJSON(w, 400, oaiError("invalid_request_error", "body was not valid JSON: "+decodeErr.Error()))
		return
	}

	switch fault {
	case FaultRateLimit:
		entry.Outcome, entry.Status = "rate_limited", 429
		s.Journal.Append(entry)
		retry := profile.RetryAfterSec
		if retry <= 0 {
			retry = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		w.Header().Set("X-RateLimit-Remaining-Requests", "0")
		writeJSON(w, 429, oaiError("rate_limit_error", "rate limit exceeded"))
		return
	case FaultStatus:
		entry.Outcome, entry.Status = "injected_error", status
		s.Journal.Append(entry)
		writeJSON(w, status, oaiError("server_error", fmt.Sprintf("injected fault status %d", status)))
		return
	case FaultHang:
		entry.Outcome, entry.Status = "hang", 0
		s.Journal.Append(entry)
		// Hold until the client or gateway gives up. Whoever gives up first is
		// exactly what scenario C11 measures.
		<-r.Context().Done()
		return
	case FaultSlow:
		time.Sleep(profile.ExtraLatency)
	}

	promptToks := countPromptTokens(req.Messages)
	entry.PromptToks = promptToks

	if req.Stream {
		s.streamChat(w, r, profile, req, entry, fault == FaultStreamAbort)
		return
	}

	outputToks := profile.Chunks
	if mt := req.maxTokens(); mt != nil && *mt > 0 && *mt < outputToks {
		outputToks = *mt
	}

	// Scale with the tokens actually produced, matching what the streaming path
	// costs for the same output. A gateway that requests the upstream in
	// streaming mode must not be charged more than one that does not.
	sleepCtx(r, profile.EquivalentUnaryDelay(outputToks))

	entry.Outcome, entry.Status, entry.OutputToks = "ok", 200, outputToks
	seq := s.Journal.Append(entry)

	writeJSON(w, 200, map[string]any{
		"id":      "chatcmpl-bench-" + strconv.Itoa(seq),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": deterministicText(outputToks)},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens":     promptToks,
			"completion_tokens": outputToks,
			"total_tokens":      promptToks + outputToks,
		},
	})
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, p *Profile, req chatRequest, entry Entry, abort bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, oaiError("server_error", "streaming unsupported"))
		return
	}

	total := p.Chunks
	if mt := req.maxTokens(); mt != nil && *mt > 0 && *mt < total {
		total = *mt
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Some proxies buffer SSE unless told not to. Announcing this removes an
	// unfair variable: we are measuring gateway design, not nginx defaults.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	id := "chatcmpl-bench-stream"
	created := time.Now().Unix()

	if !sleepCtx(r, p.TTFT) {
		entry.Outcome, entry.Status = "client_gone", 0
		s.Journal.Append(entry)
		return
	}

	// Role delta first, matching OpenAI's actual wire shape so gateways that
	// parse the stream see what they expect.
	writeSSE(w, flusher, chunkFrame(id, created, req.Model, map[string]any{"role": "assistant"}, nil))

	sent := 0
	for i := 0; i < total; i++ {
		if abort && p.AbortAfterChunks > 0 && i >= p.AbortAfterChunks {
			// Abort by returning without the terminator. The connection closes
			// with an incomplete stream, which is the realistic failure and the
			// one gateways handle least well.
			entry.Outcome, entry.Status, entry.OutputToks = "stream_aborted", 200, sent
			s.Journal.Append(entry)
			return
		}
		if !sleepCtx(r, p.ChunkDelay) {
			entry.Outcome, entry.Status, entry.OutputToks = "client_gone", 0, sent
			s.Journal.Append(entry)
			return
		}
		writeSSE(w, flusher, chunkFrame(id, created, req.Model, map[string]any{"content": token(i)}, nil))
		sent++
	}

	writeSSE(w, flusher, chunkFrame(id, created, req.Model, map[string]any{}, strPtr("stop")))

	// usage frame, as OpenAI emits when stream_options.include_usage is set.
	if req.StreamOptions != nil {
		writeSSE(w, flusher, mustJSON(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created,
			"model": req.Model, "choices": []any{},
			"usage": map[string]any{
				"prompt_tokens": entry.PromptToks, "completion_tokens": sent,
				"total_tokens": entry.PromptToks + sent,
			},
		}))
	}

	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()

	entry.Outcome, entry.Status, entry.OutputToks = "ok", 200, sent
	s.Journal.Append(entry)
}

// ---------- other surfaces ----------

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	profile := s.Registry.Get(r.PathValue("profile"))
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
		Input any    `json:"input"`
	}
	_ = json.Unmarshal(body, &req)

	entry := s.baseEntry(r, profile.Name, body, chatRequest{Model: req.Model})
	if fault, status := profile.nextFault(); fault == FaultStatus {
		entry.Outcome, entry.Status = "injected_error", status
		s.Journal.Append(entry)
		writeJSON(w, status, oaiError("server_error", "injected fault"))
		return
	}
	sleepCtx(r, profile.TTFT)

	const dims = 8
	vec := make([]float64, dims)
	for i := range vec {
		vec[i] = float64((i*7)%13) / 13.0
	}
	entry.Outcome, entry.Status = "ok", 200
	s.Journal.Append(entry)
	writeJSON(w, 200, map[string]any{
		"object": "list",
		"data":   []any{map[string]any{"object": "embedding", "index": 0, "embedding": vec}},
		"model":  req.Model,
		"usage":  map[string]any{"prompt_tokens": 8, "total_tokens": 8},
	})
}

// handleAnthropicMessages exists so gateways can be exercised on their Anthropic
// surface too. Protocol breadth is a matrix dimension, and a gateway that only
// speaks OpenAI should be visibly unable to serve this.
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	profile := s.Registry.Get(r.PathValue("profile"))
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model    string           `json:"model"`
		Stream   bool             `json:"stream"`
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)

	entry := s.baseEntry(r, profile.Name, body, chatRequest{Model: req.Model, Stream: req.Stream})
	if fault, status := profile.nextFault(); fault == FaultStatus {
		entry.Outcome, entry.Status = "injected_error", status
		s.Journal.Append(entry)
		writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "injected fault"}})
		return
	}
	sleepCtx(r, profile.UnaryDelay)

	toks := profile.Chunks
	entry.Outcome, entry.Status, entry.OutputToks = "ok", 200, toks
	s.Journal.Append(entry)
	writeJSON(w, 200, map[string]any{
		"id": "msg_bench", "type": "message", "role": "assistant", "model": req.Model,
		"content":     []any{map[string]any{"type": "text", "text": deterministicText(toks)}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": countPromptTokens(req.Messages), "output_tokens": toks},
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := []string{"bench-model", "bench-model-fast", "bench-model-large"}
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{"id": m, "object": "model", "created": 1700000000, "owned_by": "everstack-bench"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// ---------- control API ----------

// profileWire is the JSON shape the control API accepts. Durations are
// milliseconds so scenario definitions stay readable.
type profileWire struct {
	TTFTMs           int     `json:"ttft_ms"`
	ChunkDelayMs     int     `json:"chunk_delay_ms"`
	Chunks           int     `json:"chunks"`
	UnaryDelayMs     int     `json:"unary_delay_ms"`
	Fault            string  `json:"fault"`
	FaultRate        float64 `json:"fault_rate"`
	Status           int     `json:"status"`
	FailFirstN       int64   `json:"fail_first_n"`
	ExtraLatencyMs   int     `json:"extra_latency_ms"`
	AbortAfterChunks int     `json:"abort_after_chunks"`
	RetryAfterSec    int     `json:"retry_after_seconds"`
}

func (s *Server) handleSetProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var wire profileWire
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	p := DefaultProfile(name)
	if wire.TTFTMs > 0 {
		p.TTFT = time.Duration(wire.TTFTMs) * time.Millisecond
	}
	if wire.ChunkDelayMs > 0 {
		p.ChunkDelay = time.Duration(wire.ChunkDelayMs) * time.Millisecond
	}
	if wire.Chunks > 0 {
		p.Chunks = wire.Chunks
	}
	if wire.UnaryDelayMs > 0 {
		p.UnaryDelay = time.Duration(wire.UnaryDelayMs) * time.Millisecond
		p.UnaryDelayExplicit = true
	}
	if wire.Fault != "" {
		p.Fault = FaultMode(wire.Fault)
	}
	p.FaultRate = wire.FaultRate
	if wire.Status > 0 {
		p.Status = wire.Status
	}
	p.FailFirstN = wire.FailFirstN
	p.ExtraLatency = time.Duration(wire.ExtraLatencyMs) * time.Millisecond
	p.AbortAfterChunks = wire.AbortAfterChunks
	p.RetryAfterSec = wire.RetryAfterSec

	s.Registry.Set(name, p)
	writeJSON(w, 200, map[string]string{"status": "set", "profile": name})
}

func (s *Server) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	p := s.Registry.Get(r.PathValue("name"))
	writeJSON(w, 200, map[string]any{
		"name": p.Name, "ttft_ms": p.TTFT.Milliseconds(), "chunk_delay_ms": p.ChunkDelay.Milliseconds(),
		"chunks": p.Chunks, "fault": p.Fault, "seen": p.Seen(), "failed": p.Failed(),
	})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.Journal.Reset()
	s.Registry.ResetCounters()
	writeJSON(w, 200, map[string]string{"status": "reset"})
}

func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"entries": s.Journal.Snapshot()})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	for _, name := range s.Registry.Names() {
		p := s.Registry.Get(name)
		out[name] = map[string]any{"seen": p.Seen(), "failed": p.Failed(), "fault": p.Fault}
	}
	writeJSON(w, 200, map[string]any{"profiles": out, "journal_entries": s.Journal.Count()})
}

// ---------- helpers ----------

func (s *Server) baseEntry(r *http.Request, profile string, body []byte, req chatRequest) Entry {
	sum := sha256.Sum256(body)

	// Record the top-level key names as they arrived on the wire.
	var raw map[string]json.RawMessage
	var bodyKeys []string
	if json.Unmarshal(body, &raw) == nil {
		bodyKeys = make([]string, 0, len(raw))
		for k := range raw {
			bodyKeys = append(bodyKeys, k)
		}
		sort.Strings(bodyKeys)
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		auth = r.Header.Get("x-api-key")
	}

	// Fingerprint rather than store the credential: this journal is written to
	// results/ and could be shared with a report.
	fp := ""
	if auth != "" {
		h := sha256.Sum256([]byte(auth))
		fp = hex.EncodeToString(h[:])[:12]
	}

	params := map[string]any{}
	if req.Temperature != nil {
		params["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		params["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		params["max_tokens"] = *req.MaxTokens
	}
	if req.MaxCompletionTokens != nil {
		params["max_completion_tokens"] = *req.MaxCompletionTokens
	}
	if req.Seed != nil {
		params["seed"] = *req.Seed
	}
	if req.N != nil {
		params["n"] = *req.N
	}
	if req.Stop != nil {
		params["stop"] = req.Stop
	}
	if req.Tools != nil {
		params["tools_count"] = len(req.Tools)
	}
	if req.ToolChoice != nil {
		params["tool_choice"] = req.ToolChoice
	}
	if req.ResponseFormat != nil {
		params["response_format"] = req.ResponseFormat
	}
	if req.Logprobs != nil {
		params["logprobs"] = *req.Logprobs
	}
	if req.User != "" {
		params["user"] = req.User
	}

	// Only headers a scenario asserts on. Capturing everything would put
	// credentials in the journal.
	headers := map[string]string{}
	for _, h := range []string{"X-Bench-Client", "X-Bench-Case", "X-Request-Id", "User-Agent", "X-Everstack-Tenant"} {
		if v := r.Header.Get(h); v != "" {
			headers[h] = v
		}
	}

	return Entry{
		At:          time.Now(),
		Profile:     profile,
		Path:        r.URL.Path,
		Model:       req.Model,
		Stream:      req.Stream,
		ClientTag:   r.Header.Get("X-Bench-Client"),
		AuthPresent: auth != "",
		AuthFP:      fp,
		BodySHA:     hex.EncodeToString(sum[:])[:16],
		Params:      params,
		BodyKeys:    bodyKeys,
		Headers:     headers,
	}
}

// countPromptTokens is a deterministic stand-in for a tokenizer: one token per
// four characters of message content. Scenario C9 asserts a gateway's reported
// usage against this exact number, so it only has to be stable, not accurate.
func countPromptTokens(messages []map[string]any) int {
	chars := 0
	for _, m := range messages {
		switch c := m["content"].(type) {
		case string:
			chars += len(c)
		case []any:
			for _, part := range c {
				if pm, ok := part.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						chars += len(t)
					}
				}
			}
		}
	}
	if chars == 0 {
		return 1
	}
	return (chars + 3) / 4
}

func token(i int) string { return "tok" + strconv.Itoa(i) + " " }

func deterministicText(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(token(i))
	}
	return strings.TrimSpace(b.String())
}

func chunkFrame(id string, created int64, model string, delta map[string]any, finish *string) []byte {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if finish != nil {
		choice["finish_reason"] = *finish
	}
	return mustJSON(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created,
		"model": model, "choices": []any{choice},
	})
}

func writeSSE(w http.ResponseWriter, f http.Flusher, payload []byte) {
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(payload)
	_, _ = io.WriteString(w, "\n\n")
	f.Flush()
}

// sleepCtx sleeps unless the request is cancelled first. Returns false if the
// client went away, so the caller can journal it rather than writing into a
// dead connection.
func sleepCtx(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func oaiError(kind, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"message": msg, "type": kind, "code": kind}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func strPtr(s string) *string { return &s }
