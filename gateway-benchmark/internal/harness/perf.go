package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/everstacklabs/examples/gateway-benchmark/internal/loadgen"
	"github.com/everstacklabs/examples/gateway-benchmark/internal/stats"
)

// PhaseResult is one measured load phase for one target.
type PhaseResult struct {
	Phase  string `json:"phase"`
	Target string `json:"target"`

	Latency    stats.Summary `json:"latency"`
	TTFT       stats.Summary `json:"ttft"`
	InterChunk stats.Summary `json:"inter_chunk"`

	Requests  int `json:"requests"`
	Offered   int `json:"offered"`
	Dropped   int `json:"dropped"`
	Errors    int `json:"errors"`
	NonOK     int `json:"non_2xx"`
	Truncated int `json:"truncated"`
	// NoSentinel counts complete streams that omitted the [DONE] terminator.
	NoSentinel int `json:"no_sentinel"`
	// UpstreamStreamedPct is the share of upstream calls this phase made in
	// streaming mode. Gateways differ: some upgrade a non-streaming client
	// request to a streaming upstream call, which costs more on the wire for
	// the same output. Comparing such a gateway against a control that did not
	// stream charges it for an internal choice, so the report prints this
	// beside the latency rather than leaving it to be taken at face value.
	UpstreamStreamedPct float64 `json:"upstream_streamed_pct"`
	UpstreamCalls       int     `json:"upstream_calls"`
	ErrorRate           float64 `json:"error_rate"`
	Throughput          float64 `json:"achieved_rps"`
	PeakInUse           int     `json:"peak_in_flight"`

	OfferedRPS  float64 `json:"offered_rps"`
	Concurrency int     `json:"concurrency,omitempty"`

	Resources ResourceStats `json:"resources"`

	// TopErrors keeps the distinct failure strings so a target that quietly
	// errors under load cannot be mistaken for a fast one.
	TopErrors map[string]int `json:"top_errors,omitempty"`
}

// PerfReport is every phase measured for one target.
type PerfReport struct {
	Target  string `json:"target"`
	Image   string `json:"image"`
	Version string `json:"version"`
	// Emulated marks a target that ran under CPU emulation, whose latency and
	// resource figures are therefore not comparable to the others.
	Emulated    bool          `json:"emulated"`
	Unary       []PhaseResult `json:"unary_runs"`
	Streaming   []PhaseResult `json:"streaming_runs"`
	Concurrency []PhaseResult `json:"concurrency_sweep"`
	Saturation  []PhaseResult `json:"saturation_sweep"`
	Degraded    []PhaseResult `json:"degraded_runs"`
}

// RunPerf executes the performance suite (P1 to P7) against a single target.
func RunPerf(ctx context.Context, suite *Suite, t Target, ctrl *Control, log func(string, ...any)) (*PerfReport, error) {
	rep := &PerfReport{
		Target:   t.Name,
		Version:  t.Version,
		Image:    t.Image,
		Emulated: t.EmulatedOnARM64 && runtime.GOARCH == "arm64",
	}
	if img := ContainerImage(ctx, t.Container); img != "" {
		rep.Image = img
	}

	// P1: non-streaming added latency, repeated so the report can show
	// run-to-run spread rather than a single flattering run.
	for run := 0; run < suite.Load.Runs; run++ {
		log("  [%s] unary run %d/%d", t.Name, run+1, suite.Load.Runs)
		if err := ctrl.Healthy(ctx, "primary"); err != nil {
			return nil, err
		}
		res, err := runPhase(ctx, phaseSpec{
			name:        fmt.Sprintf("unary/run%d", run+1),
			suite:       suite,
			ctrl:        ctrl,
			target:      t,
			stream:      false,
			rps:         suite.Load.RPS,
			maxInFlight: 256,
		})
		if err != nil {
			return nil, err
		}
		rep.Unary = append(rep.Unary, *res)
	}

	// P2 and P3: streaming TTFT and inter-chunk cadence.
	if t.SupportsScenario("streaming") {
		for run := 0; run < suite.Load.Runs; run++ {
			log("  [%s] streaming run %d/%d", t.Name, run+1, suite.Load.Runs)
			if err := ctrl.Healthy(ctx, "primary"); err != nil {
				return nil, err
			}
			res, err := runPhase(ctx, phaseSpec{
				name:        fmt.Sprintf("streaming/run%d", run+1),
				suite:       suite,
				ctrl:        ctrl,
				target:      t,
				stream:      true,
				rps:         suite.Load.RPS,
				maxInFlight: 256,
			})
			if err != nil {
				return nil, err
			}
			rep.Streaming = append(rep.Streaming, *res)
		}
	}

	// P5: concurrency sweep. Driven by in-flight cap at a rate high enough to
	// keep the cap saturated, which is how a connection-pool ceiling surfaces.
	for _, c := range suite.Load.ConcurrencySteps {
		log("  [%s] concurrency %d", t.Name, c)
		if err := ctrl.Healthy(ctx, "primary"); err != nil {
			return nil, err
		}
		res, err := runPhase(ctx, phaseSpec{
			name:        fmt.Sprintf("concurrency/%d", c),
			suite:       suite,
			ctrl:        ctrl,
			target:      t,
			stream:      false,
			rps:         0, // unthrottled; the in-flight cap is the control
			maxInFlight: c,
			duration:    12 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		res.Concurrency = c
		rep.Concurrency = append(rep.Concurrency, *res)
	}

	// P4: saturation sweep. The reported capacity is the highest offered rate
	// the target sustained without its added p99 blowing past the threshold.
	for _, rps := range suite.Load.SaturationSteps {
		log("  [%s] saturation %.0f rps", t.Name, rps)
		if err := ctrl.Healthy(ctx, "primary"); err != nil {
			return nil, err
		}
		res, err := runPhase(ctx, phaseSpec{
			name:        fmt.Sprintf("saturation/%.0frps", rps),
			suite:       suite,
			ctrl:        ctrl,
			target:      t,
			stream:      false,
			rps:         rps,
			maxInFlight: 2048,
			duration:    12 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		rep.Saturation = append(rep.Saturation, *res)
		// Stop climbing once the target is clearly past its knee: more steps
		// past this point measure queue depth, not capacity.
		if res.ErrorRate > 0.05 || res.Dropped > res.Requests/10 {
			log("  [%s] stopping sweep at %.0f rps (error rate %.1f%%, dropped %d)",
				t.Name, rps, res.ErrorRate*100, res.Dropped)
			break
		}
	}

	// P7: overhead while the upstream is unhealthy. Most published gateway
	// benchmarks only measure the happy path, which is the path that matters
	// least in production.
	log("  [%s] degraded upstream (20%% 503)", t.Name)
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{Fault: "status", FaultRate: 0.2, Status: 503}); err != nil {
		return nil, err
	}
	res, err := runPhase(ctx, phaseSpec{
		name:        "degraded/20pct-503",
		suite:       suite,
		ctrl:        ctrl,
		target:      t,
		stream:      false,
		rps:         suite.Load.RPS,
		maxInFlight: 256,
		duration:    12 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	rep.Degraded = append(rep.Degraded, *res)
	if err := ctrl.Healthy(ctx, "primary"); err != nil {
		return nil, err
	}

	return rep, nil
}

type phaseSpec struct {
	name        string
	ctrl        *Control
	suite       *Suite
	target      Target
	stream      bool
	rps         float64
	maxInFlight int
	duration    time.Duration
}

func runPhase(ctx context.Context, spec phaseSpec) (*PhaseResult, error) {
	t := spec.target
	load := spec.suite.Load

	duration := spec.duration
	if duration == 0 {
		duration = load.Duration()
	}

	sampler := NewResourceSampler(t.Container)
	sampler.Start(ctx)

	cfg := loadgen.Config{
		URL:         t.ChatURL,
		APIKey:      t.APIKey(),
		Headers:     mergeHeaders(t.Headers, map[string]string{"X-Bench-Client": t.Name, "X-Bench-Case": spec.name}),
		Stream:      spec.stream,
		RPS:         spec.rps,
		Poisson:     load.Poisson,
		Duration:    duration,
		Warmup:      load.Warmup(),
		MaxInFlight: spec.maxInFlight,
		Timeout:     load.Timeout(),
		Seed:        1,
		BodyFn: func(i int) []byte {
			// Every request is unique, so an incidentally-enabled response cache
			// on any target cannot turn a proxy benchmark into a cache benchmark.
			return ChatBody(t.Model, uniquePrompt(load.PromptChars, i), spec.stream)
		},
	}

	// Bracket the phase so the journal reflects only this phase's traffic.
	// Other gateways left running can be chatty in the background (a
	// latency-based load balancer probes continuously), so counting from a
	// reset is the only way this share means anything.
	_ = spec.ctrl.Reset(ctx)

	run, err := loadgen.Execute(ctx, cfg)
	if err != nil {
		sampler.Stop(0)
		return nil, err
	}

	out := summarizePhase(spec.name, t.Name, run)
	if entries, jerr := spec.ctrl.Journal(ctx); jerr == nil && len(entries) > 0 {
		streamed := 0
		for _, e := range entries {
			if e.Stream {
				streamed++
			}
		}
		out.UpstreamCalls = len(entries)
		out.UpstreamStreamedPct = float64(streamed) / float64(len(entries)) * 100
	}
	out.OfferedRPS = spec.rps
	out.Resources = sampler.Stop(out.Requests)
	return out, nil
}

func summarizePhase(phase, target string, run *loadgen.Run) *PhaseResult {
	res := &PhaseResult{
		Phase:     phase,
		Target:    target,
		Offered:   run.Offered,
		Dropped:   run.Dropped,
		PeakInUse: run.PeakInUse,
		TopErrors: map[string]int{},
	}

	var latencies, ttfts, gaps []float64
	for _, r := range run.Results {
		res.Requests++
		if r.Err != "" {
			res.Errors++
			res.TopErrors[normalizeErr(r.Err)]++
			continue
		}
		if r.Status < 200 || r.Status >= 300 {
			res.NonOK++
			res.TopErrors[fmt.Sprintf("http_%d", r.Status)]++
			continue
		}
		if r.Truncated {
			res.Truncated++
		}
		if r.NoSentinel {
			res.NoSentinel++
		}
		// Only successful requests contribute to the latency distribution. A
		// fast 500 is not a fast response, and letting errors into the
		// histogram is how a failing gateway posts the best p99 in the table.
		latencies = append(latencies, stats.Ms(r.Latency))
		if r.TTFT > 0 {
			ttfts = append(ttfts, stats.Ms(r.TTFT))
		}
		for _, g := range r.InterChunk {
			gaps = append(gaps, stats.Ms(g))
		}
	}

	res.Latency = stats.Summarize(latencies)
	res.TTFT = stats.Summarize(ttfts)
	res.InterChunk = stats.Summarize(gaps)

	if res.Requests > 0 {
		res.ErrorRate = float64(res.Errors+res.NonOK) / float64(res.Requests)
	}
	// Throughput is counted over the scheduled measurement window, not to the
	// wall-clock end of the run: including the drain of in-flight requests
	// after the last arrival would understate every target equally but
	// misleadingly, and worse on short phases than long ones.
	if w := run.Window().Seconds(); w > 0 {
		res.Throughput = float64(len(latencies)) / w
	}
	if len(res.TopErrors) == 0 {
		res.TopErrors = nil
	}
	return res
}

// normalizeErr collapses per-request noise (ports, addresses) so error counts
// group meaningfully.
func normalizeErr(s string) string {
	switch {
	case strings.Contains(s, "context deadline exceeded"):
		return "timeout"
	case strings.Contains(s, "connection refused"):
		return "connection_refused"
	case strings.Contains(s, "connection reset"):
		return "connection_reset"
	case strings.Contains(s, "EOF"):
		return "eof"
	case strings.Contains(s, "no such host"):
		return "dns"
	default:
		if len(s) > 80 {
			return s[:80]
		}
		return s
	}
}

func mergeHeaders(base, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// ChatBody builds an OpenAI chat completion request.
func ChatBody(model, prompt string, stream bool) []byte {
	req := map[string]any{
		"model":       model,
		"messages":    []any{map[string]any{"role": "user", "content": prompt}},
		"temperature": 0,
		"max_tokens":  64,
	}
	if stream {
		req["stream"] = true
	}
	b, _ := json.Marshal(req)
	return b
}

// uniquePrompt returns a prompt of roughly n characters that differs per index.
func uniquePrompt(n, i int) string {
	head := fmt.Sprintf("[req-%d] ", i)
	if n <= len(head) {
		return head
	}
	var b strings.Builder
	b.WriteString(head)
	const filler = "benchmark payload token stream measurement control plane routing "
	for b.Len() < n {
		b.WriteString(filler)
	}
	return b.String()[:n]
}
