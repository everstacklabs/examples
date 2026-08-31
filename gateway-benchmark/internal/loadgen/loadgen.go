// Package loadgen is an open-loop HTTP load generator for OpenAI-compatible
// endpoints.
//
// Open-loop matters. A closed-loop generator (N workers, each sending the next
// request only after the previous one returns) stops offering load exactly when
// the system slows down, so the recorded tail latency is far better than what a
// real client population would see. That is coordinated omission, and it is the
// single most common reason a published gateway benchmark is wrong. Here,
// arrivals are scheduled against a wall clock and latency is measured from the
// scheduled arrival time, not from when a worker happened to pick the request up.
package loadgen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Result is one request's observation.
type Result struct {
	// ScheduledAt is when the request was supposed to leave. Latency is measured
	// from here so queueing inside the generator counts against the system,
	// which is what an open-loop measurement means.
	ScheduledAt time.Time     `json:"scheduled_at"`
	Latency     time.Duration `json:"latency_ns"`
	TTFT        time.Duration `json:"ttft_ns"`
	Status      int           `json:"status"`
	Err         string        `json:"error,omitempty"`
	Chunks      int           `json:"chunks"`
	// InterChunk holds the gap between consecutive content chunks. A gateway
	// that buffers the upstream stream and releases it in bursts shows up here
	// even when its TTFT looks fine.
	InterChunk []time.Duration `json:"-"`
	Bytes      int             `json:"bytes"`
	// PromptTokens/CompletionTokens are what the gateway reported, which the
	// token-accounting scenario compares against the upstream's ground truth.
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// Truncated marks a stream that ended without a terminator.
	Truncated bool `json:"truncated"`
}

// Config describes one load phase.
type Config struct {
	URL     string
	APIKey  string
	Headers map[string]string

	// Body is the request template. BodyFn, if set, wins and is called per
	// request with the request index, which is how cache scenarios vary or
	// repeat the prompt deliberately.
	Body   []byte
	BodyFn func(i int) []byte

	Stream bool

	// RPS is the open-loop arrival rate. Zero means "as fast as MaxInFlight
	// allows", used only for saturation discovery.
	RPS float64
	// Poisson uses exponential inter-arrival gaps instead of a fixed interval.
	// Real traffic is bursty; fixed-rate arrivals understate queueing.
	Poisson bool

	Duration    time.Duration
	Warmup      time.Duration
	MaxInFlight int
	Timeout     time.Duration

	Client *http.Client
	Seed   int64
}

// Run drives one phase and returns the results recorded after warmup.
type Run struct {
	Results []Result
	// Offered is how many requests the schedule wanted to send.
	Offered int
	// Dropped counts arrivals skipped because MaxInFlight was saturated. A
	// nonzero value means the generator itself was the constraint, and the
	// report must say so rather than presenting the numbers as a clean result.
	Dropped int
	// WindowStart and WindowEnd bound the measurement window: the schedule
	// after warmup, not the wall-clock end of the run. Dividing by the actual
	// end would include the drain of in-flight requests after the last arrival,
	// which understates throughput on short phases.
	WindowStart time.Time
	WindowEnd   time.Time
	// Ended is when the last in-flight request actually completed.
	Ended     time.Time
	PeakInUse int
}

// Window is the measured duration used for throughput.
func (r *Run) Window() time.Duration { return r.WindowEnd.Sub(r.WindowStart) }

// Execute runs the phase described by cfg.
func Execute(ctx context.Context, cfg Config) (*Run, error) {
	if cfg.Client == nil {
		cfg.Client = DefaultClient(cfg.Timeout, cfg.MaxInFlight)
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 512
	}
	if cfg.Duration <= 0 {
		cfg.Duration = 10 * time.Second
	}

	rng := rand.New(rand.NewSource(cfg.Seed))
	sem := make(chan struct{}, cfg.MaxInFlight)

	var (
		mu      sync.Mutex
		results []Result
		wg      sync.WaitGroup
		offered atomic.Int64
		dropped atomic.Int64
		inUse   atomic.Int64
		peak    atomic.Int64
	)

	start := time.Now()
	warmupUntil := start.Add(cfg.Warmup)
	deadline := start.Add(cfg.Warmup + cfg.Duration)

	runCtx, cancel := context.WithDeadline(ctx, deadline.Add(cfg.Timeout))
	defer cancel()

	interval := time.Duration(0)
	if cfg.RPS > 0 {
		interval = time.Duration(float64(time.Second) / cfg.RPS)
	}

	next := start
	i := 0
	for {
		now := time.Now()
		if now.After(deadline) {
			break
		}
		if runCtx.Err() != nil {
			break
		}

		// Wait until this arrival is due. Sleeping to an absolute schedule (not
		// "sleep interval after the last send") stops generator drift from
		// silently lowering the offered rate.
		if interval > 0 && next.After(now) {
			timer := time.NewTimer(next.Sub(now))
			select {
			case <-timer.C:
			case <-runCtx.Done():
				timer.Stop()
			}
			timer.Stop()
		}

		scheduled := next
		if interval > 0 {
			if cfg.Poisson {
				gap := time.Duration(rng.ExpFloat64() * float64(interval))
				next = next.Add(gap)
			} else {
				next = next.Add(interval)
			}
		} else {
			scheduled = time.Now()
			next = scheduled
		}

		offered.Add(1)

		select {
		case sem <- struct{}{}:
		default:
			// Saturated. Record the drop instead of blocking, because blocking
			// here would convert this into a closed-loop generator.
			dropped.Add(1)
			i++
			continue
		}

		cur := inUse.Add(1)
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}

		body := cfg.Body
		if cfg.BodyFn != nil {
			body = cfg.BodyFn(i)
		}
		idx := i
		i++

		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem; inUse.Add(-1) }()

			res := doRequest(runCtx, cfg, body, scheduled, idx)
			// Warmup results are measured but thrown away: the first requests
			// pay for TLS handshakes, connection pool fill, and JIT-ish
			// warmup in the gateways written on runtimes that have it.
			if res.ScheduledAt.Before(warmupUntil) {
				return
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}

	wg.Wait()
	end := time.Now()

	return &Run{
		Results:     results,
		Offered:     int(offered.Load()),
		Dropped:     int(dropped.Load()),
		WindowStart: warmupUntil,
		WindowEnd:   deadline,
		Ended:       end,
		PeakInUse:   int(peak.Load()),
	}, nil
}

func doRequest(ctx context.Context, cfg Config, body []byte, scheduled time.Time, idx int) Result {
	res := Result{ScheduledAt: scheduled}

	reqCtx := ctx
	if cfg.Timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		res.Err = err.Error()
		res.Latency = time.Since(scheduled)
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("X-Bench-Seq", fmt.Sprint(idx))
	if cfg.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	sent := time.Now()
	resp, err := cfg.Client.Do(req)
	if err != nil {
		res.Err = err.Error()
		res.Latency = time.Since(scheduled)
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode

	if !cfg.Stream {
		b, err := io.ReadAll(resp.Body)
		res.Bytes = len(b)
		res.Latency = time.Since(scheduled)
		res.TTFT = res.Latency
		if err != nil {
			res.Err = err.Error()
			return res
		}
		res.PromptTokens, res.CompletionTokens = parseUsage(b)
		return res
	}

	readStream(resp.Body, sent, scheduled, &res)
	return res
}

// readStream parses SSE and records first-token and inter-chunk timing.
//
// TTFT is deliberately the first frame carrying *content*, not the first byte
// or the role-delta frame. A gateway that emits its own opening frame early
// would otherwise post a flattering TTFT while the user still sees nothing.
func readStream(body io.Reader, sent, scheduled time.Time, res *Result) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var lastChunk time.Time
	sawDone := false

	for scanner.Scan() {
		line := scanner.Text()
		res.Bytes += len(line) + 1
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawDone = true
			break
		}

		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}
		if frame.Usage != nil {
			res.PromptTokens = frame.Usage.PromptTokens
			res.CompletionTokens = frame.Usage.CompletionTokens
		}
		if len(frame.Choices) == 0 || frame.Choices[0].Delta.Content == "" {
			continue
		}

		now := time.Now()
		if res.Chunks == 0 {
			res.TTFT = now.Sub(scheduled)
		} else {
			res.InterChunk = append(res.InterChunk, now.Sub(lastChunk))
		}
		lastChunk = now
		res.Chunks++
	}

	if err := scanner.Err(); err != nil && res.Err == "" {
		res.Err = err.Error()
	}
	res.Truncated = !sawDone && res.Err == ""
	res.Latency = time.Since(scheduled)
	_ = sent
}

func parseUsage(b []byte) (prompt, completion int) {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return 0, 0
	}
	return resp.Usage.PromptTokens, resp.Usage.CompletionTokens
}

// DefaultClient builds a client sized so the generator is not the bottleneck.
// Connection reuse is on: measuring TLS/TCP setup on every request would
// measure the kernel, not the gateway.
func DefaultClient(timeout time.Duration, maxInFlight int) *http.Client {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if maxInFlight <= 0 {
		maxInFlight = 512
	}
	conns := int(math.Max(float64(maxInFlight)*2, 256))
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConns:          conns,
			MaxIdleConnsPerHost:   conns,
			MaxConnsPerHost:       0,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 0,
			DisableCompression:    true,
			ForceAttemptHTTP2:     false,
		},
	}
}
