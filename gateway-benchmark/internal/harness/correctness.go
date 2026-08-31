package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/everstacklabs/examples/gateway-benchmark/internal/upstream"
)

// CheckStatus is the outcome of one correctness scenario.
type CheckStatus string

const (
	// Pass means the gateway did the right thing, proven from the upstream journal.
	Pass CheckStatus = "pass"
	// Fail means it did the wrong thing. This is a real defect, not a missing feature.
	Fail CheckStatus = "fail"
	// NotSupported means the capability is absent. Distinct from Fail on purpose:
	// "does not have retries" and "retries incorrectly" are different products.
	NotSupported CheckStatus = "not_supported"
	// NotConfigured means the capability exists but this deployment did not enable
	// it, so the run proves nothing either way.
	NotConfigured CheckStatus = "not_configured"
	// Inconclusive means the probe could not establish the answer.
	Inconclusive CheckStatus = "inconclusive"
	// MatrixOnly means the dimension is scored from documented evidence rather
	// than probed here, and the matrix carries the citation.
	MatrixOnly CheckStatus = "matrix_only"
)

// Check is one scenario outcome with the evidence that produced it.
type Check struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Target   string      `json:"target"`
	Status   CheckStatus `json:"status"`
	Detail   string      `json:"detail"`
	Evidence any         `json:"evidence,omitempty"`
	// Metric is the headline number for scenarios that produce one, such as
	// failover recovery time in milliseconds.
	Metric *float64 `json:"metric,omitempty"`
	Unit   string   `json:"unit,omitempty"`
}

func f64(v float64) *float64 { return &v }

// timeoutPatience is how long the timeout scenarios wait before concluding the
// gateway will never give up. It must comfortably exceed the request timeouts
// the gateways under test are configured with (30s in this suite's compose
// files), or the probe's own deadline becomes the thing being measured.
func timeoutPatience(suite *Suite) time.Duration {
	if suite.Load.TimeoutPatienceSeconds > 0 {
		return time.Duration(suite.Load.TimeoutPatienceSeconds) * time.Second
	}
	return 75 * time.Second
}

// RunCorrectness executes the behavioural suite (C1 to C12) for one target.
func RunCorrectness(ctx context.Context, suite *Suite, t Target, ctrl *Control, log func(string, ...any)) []Check {
	probe := NewProbe(t, suite.Load.Timeout())
	// The two scenarios that measure who gives up first need to out-wait the
	// gateway, not race it. With equal deadlines the client always appears to
	// lose and every gateway looks broken, which says more about the probe than
	// the product.
	patient := NewProbe(t, timeoutPatience(suite))
	var checks []Check

	type step struct {
		id string
		fn func() Check
	}
	steps := []step{
		{"C1", func() Check { return checkFailover(ctx, t, ctrl, probe) }},
		{"C2", func() Check { return checkRetryAfter(ctx, t, ctrl, probe) }},
		{"C3", func() Check { return checkRetryAmplification(ctx, t, ctrl, probe) }},
		{"C4", func() Check { return checkStreamingFailure(ctx, t, ctrl, patient) }},
		{"C5", func() Check { return checkRateLimit(ctx, t, ctrl, probe) }},
		{"C6", func() Check { return checkBudget(ctx, t, ctrl, probe) }},
		{"C7", func() Check { return checkCache(ctx, t, ctrl, probe) }},
		{"C8", func() Check { return checkParameterFidelity(ctx, t, ctrl, probe) }},
		{"C9", func() Check { return checkTokenAccounting(ctx, t, ctrl, probe) }},
		{"C10", func() Check { return checkTenantIsolation(ctx, t, ctrl, probe) }},
		{"C11", func() Check { return checkTimeout(ctx, t, ctrl, patient) }},
		{"C12", func() Check { return checkObservability(t) }},
	}

	for _, s := range steps {
		if !t.SupportsScenario(s.id) {
			checks = append(checks, Check{
				ID: s.id, Target: t.Name, Name: scenarioNames[s.id],
				Status: NotSupported,
				Detail: "target declares no support for this scenario in targets.yaml",
			})
			continue
		}
		log("  [%s] %s %s", t.Name, s.id, scenarioNames[s.id])
		c := s.fn()
		c.ID, c.Target, c.Name = s.id, t.Name, scenarioNames[s.id]
		checks = append(checks, c)
		// Always leave the upstream healthy for the next scenario, whatever the
		// previous one did to it.
		_ = ctrl.Healthy(ctx, "primary")
		_ = ctrl.Healthy(ctx, "secondary")
	}
	return checks
}

var scenarioNames = map[string]string{
	"C1":  "Failover to a healthy provider",
	"C2":  "Honours Retry-After on 429",
	"C3":  "Retry amplification is bounded",
	"C4":  "Mid-stream upstream failure is handled",
	"C5":  "Rate limit is enforced and shaped correctly",
	"C6":  "Spend budget is enforced",
	"C7":  "Response cache hits and is correct",
	"C8":  "Request parameters survive the round trip",
	"C9":  "Token accounting matches upstream truth",
	"C10": "Tenants cannot read each other's cache",
	"C11": "Gateway times out a hung upstream",
	"C12": "Observability completeness",
}

// C1: with the primary always 503ing, a gateway with failover configured should
// still return a 200 to the client, and we measure what that costs.
func checkFailover(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	if t.FallbackModel == "" {
		return Check{Status: NotConfigured, Detail: "no fallback_model configured for this target"}
	}
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{Fault: "status", FaultRate: 1, Status: 503}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	if err := ctrl.Healthy(ctx, "secondary"); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	const attempts = 5
	var recovered int
	var worst time.Duration
	statuses := map[int]int{}
	for i := 0; i < attempts; i++ {
		r := p.Do(ctx, ChatBody(t.Model, fmt.Sprintf("failover probe %d", i), false), false, "", nil)
		statuses[r.Status]++
		if r.Status == 200 {
			recovered++
			if r.Latency > worst {
				worst = r.Latency
			}
		}
	}

	journal, _ := ctrl.Journal(ctx)
	ev := map[string]any{
		"statuses":         statuses,
		"upstream_hits":    len(journal),
		"upstream_by_prof": byProfile(journal),
	}

	switch {
	case recovered == attempts:
		return Check{
			Status: Pass, Metric: f64(float64(worst.Milliseconds())), Unit: "ms",
			Detail:   fmt.Sprintf("all %d requests served despite the primary returning 503; worst-case recovery %dms", attempts, worst.Milliseconds()),
			Evidence: ev,
		}
	case recovered > 0:
		return Check{
			Status: Fail, Metric: f64(float64(recovered) / attempts),
			Detail:   fmt.Sprintf("failover is partial: %d/%d requests recovered, the rest surfaced the upstream error", recovered, attempts),
			Evidence: ev,
		}
	default:
		return Check{Status: Fail, Detail: "no request survived the primary failing; the client saw every upstream error", Evidence: ev}
	}
}

// C2: a gateway that ignores Retry-After turns one rate limit into a hot loop
// that makes the rate limit worse. The journal timestamps prove which happened.
func checkRetryAfter(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	const retryAfter = 2
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{Fault: "rate_limit", FaultRate: 1, RetryAfterSec: retryAfter}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	r := p.Do(ctx, ChatBody(t.Model, "retry-after probe", false), false, "", nil)
	journal, _ := ctrl.Journal(ctx)

	ev := map[string]any{
		"client_status":  r.Status,
		"client_headers": r.Headers,
		"upstream_hits":  len(journal),
		"gaps_ms":        gapsMs(journal),
	}

	if len(journal) <= 1 {
		return Check{
			Status: Pass, Metric: f64(1), Unit: "upstream attempts",
			Detail:   "did not retry a 429, which is the safe behaviour when Retry-After exceeds the request deadline",
			Evidence: ev,
		}
	}

	gaps := gapsMs(journal)
	minGap := gaps[0]
	for _, g := range gaps {
		if g < minGap {
			minGap = g
		}
	}
	// Allow a 10% clock tolerance rather than demanding the gap be exactly the
	// advertised seconds.
	if minGap >= float64(retryAfter)*1000*0.9 {
		return Check{
			Status: Pass, Metric: f64(minGap), Unit: "ms between retries",
			Detail:   fmt.Sprintf("retried %d times, honouring the %ds Retry-After (smallest gap %.0fms)", len(journal)-1, retryAfter, minGap),
			Evidence: ev,
		}
	}
	return Check{
		Status: Fail, Metric: f64(minGap), Unit: "ms between retries",
		Detail:   fmt.Sprintf("retried %d times with a smallest gap of %.0fms against an advertised %ds Retry-After; this amplifies the rate limit it is reacting to", len(journal)-1, minGap, retryAfter),
		Evidence: ev,
	}
}

// C3: a gateway that fans one client request into many upstream calls under
// fault is a cost bug that only shows up on the provider invoice.
func checkRetryAmplification(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{Fault: "status", FaultRate: 1, Status: 500}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	const clientRequests = 5
	for i := 0; i < clientRequests; i++ {
		p.Do(ctx, ChatBody(t.Model, fmt.Sprintf("amplification probe %d", i), false), false, "", nil)
	}
	journal, _ := ctrl.Journal(ctx)
	amp := float64(len(journal)) / clientRequests

	ev := map[string]any{"client_requests": clientRequests, "upstream_hits": len(journal), "amplification": amp}
	// 4x is the point where a provider-side incident gets amplified into an
	// outage rather than absorbed.
	if amp <= 4 {
		return Check{
			Status: Pass, Metric: f64(amp), Unit: "upstream calls per client call",
			Detail:   fmt.Sprintf("%.1f upstream calls per client call under total upstream failure", amp),
			Evidence: ev,
		}
	}
	return Check{
		Status: Fail, Metric: f64(amp), Unit: "upstream calls per client call",
		Detail:   fmt.Sprintf("%.1f upstream calls per client call: a provider incident is amplified, not absorbed", amp),
		Evidence: ev,
	}
}

// C4: the upstream dies after N chunks. Recovering is best; a clean truncation
// the client can detect is acceptable; hanging is a failure.
func checkStreamingFailure(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	if !t.SupportsScenario("streaming") {
		return Check{Status: NotSupported, Detail: "target does not serve streaming"}
	}
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{
		Fault: "stream_abort", FaultRate: 1, AbortAfterChunks: 5, Chunks: 40,
	}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	start := time.Now()
	r := p.Do(ctx, ChatBody(t.Model, "mid-stream failure probe", true), true, "", nil)
	elapsed := time.Since(start)
	journal, _ := ctrl.Journal(ctx)

	ev := map[string]any{
		"chunks_received": r.Chunks, "truncated": r.Truncated, "error": r.Err,
		"elapsed_ms": elapsed.Milliseconds(), "upstream_hits": len(journal),
	}

	switch {
	case r.Err != "" && elapsed >= time.Duration(float64(p.HTTP.Timeout)*0.95):
		return Check{Status: Fail, Metric: f64(float64(r.Chunks)), Unit: "chunks",
			Detail: fmt.Sprintf("delivered %d chunks then held the connection open for the full %s the probe waited; "+
				"the client cannot tell a truncated stream from a slow one", r.Chunks, p.HTTP.Timeout),
			Evidence: ev}
	case r.Chunks >= 40:
		return Check{Status: Pass, Metric: f64(float64(r.Chunks)), Unit: "chunks",
			Detail: "recovered the full stream after the upstream aborted mid-response", Evidence: ev}
	case r.Chunks > 0:
		return Check{Status: Pass, Metric: f64(float64(r.Chunks)), Unit: "chunks",
			Detail: fmt.Sprintf("delivered %d chunks then closed cleanly; the client can detect the truncation", r.Chunks), Evidence: ev}
	default:
		return Check{Status: Fail, Detail: "no chunks reached the client at all", Evidence: ev}
	}
}

// C5: drive well past the configured limit and check both that requests are
// refused and that they are refused as 429s rather than 500s.
func checkRateLimit(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	if err := ctrl.Healthy(ctx, "primary"); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{UnaryDelayMs: 5, Chunks: 4}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	const burst = 120
	statuses := map[int]int{}
	var limited, allowed int
	var sawRetryAfter bool
	for i := 0; i < burst; i++ {
		r := p.Do(ctx, ChatBody(t.Model, fmt.Sprintf("rate limit probe %d", i), false), false, "", nil)
		statuses[r.Status]++
		switch {
		case r.Status == 429:
			limited++
			if r.Headers["Retry-After"] != "" {
				sawRetryAfter = true
			}
		case r.Status == 200:
			allowed++
		}
	}

	ev := map[string]any{"burst": burst, "statuses": statuses, "allowed": allowed, "limited": limited, "retry_after_present": sawRetryAfter}
	switch {
	case limited == 0:
		return Check{Status: NotConfigured, Metric: f64(float64(allowed)), Unit: "allowed of 120",
			Detail: "no request was refused; either no limit is configured on this deployment or the limit is above the burst", Evidence: ev}
	case statuses[500] > 0 || statuses[503] > 0:
		return Check{Status: Fail, Metric: f64(float64(limited)), Unit: "refused",
			Detail: "refused requests, but some came back as 5xx; clients cannot distinguish a limit from an outage", Evidence: ev}
	case !sawRetryAfter:
		return Check{Status: Fail, Metric: f64(float64(limited)), Unit: "refused",
			Detail: fmt.Sprintf("refused %d/%d with 429 but sent no Retry-After, so a client cannot back off correctly", limited, burst), Evidence: ev}
	default:
		return Check{Status: Pass, Metric: f64(float64(limited)), Unit: "refused",
			Detail: fmt.Sprintf("refused %d/%d as 429 with Retry-After", limited, burst), Evidence: ev}
	}
}

// C6: budgets are what stop a runaway agent loop from becoming a five-figure
// invoice. Probed by spending against whatever cap the deployment configured.
func checkBudget(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{UnaryDelayMs: 5, Chunks: 64}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	const attempts = 60
	statuses := map[int]int{}
	refusedAt := -1
	for i := 0; i < attempts; i++ {
		r := p.Do(ctx, ChatBody(t.Model, fmt.Sprintf("budget probe %d", i), false), false, "", nil)
		statuses[r.Status]++
		// 402 is the honest code for "you are out of money"; several gateways
		// use 429 instead, which is accepted here as long as it is not a 5xx.
		if refusedAt < 0 && (r.Status == 402 || (r.Status == 403 && containsBudgetWord(r.Body))) {
			refusedAt = i
		}
	}
	ev := map[string]any{"attempts": attempts, "statuses": statuses, "refused_at": refusedAt}
	if refusedAt >= 0 {
		return Check{Status: Pass, Metric: f64(float64(refusedAt)), Unit: "requests before refusal",
			Detail: fmt.Sprintf("enforced the spend cap after %d requests", refusedAt), Evidence: ev}
	}
	return Check{Status: NotConfigured,
		Detail:   "no budget refusal observed; either this deployment has no cap set or the cap exceeds the probe spend",
		Evidence: ev}
}

// C7: the same prompt twice. A cache hit means the second request never reaches
// the upstream, which the journal proves far more reliably than a header does.
func checkCache(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{UnaryDelayMs: 400, Chunks: 16}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	body := ChatBody(t.Model, "cache probe: what is the capital of France?", false)
	first := p.Do(ctx, body, false, "", nil)
	if first.Status != 200 {
		return Check{Status: Inconclusive, Detail: fmt.Sprintf("priming request returned %d", first.Status)}
	}
	afterFirst, _ := ctrl.Journal(ctx)
	second := p.Do(ctx, body, false, "", nil)
	afterSecond, _ := ctrl.Journal(ctx)

	hit := len(afterSecond) == len(afterFirst)
	ev := map[string]any{
		"first_latency_ms":      first.Latency.Milliseconds(),
		"second_latency_ms":     second.Latency.Milliseconds(),
		"upstream_after_first":  len(afterFirst),
		"upstream_after_second": len(afterSecond),
		"cache_header":          second.CacheHint,
	}

	if !hit {
		return Check{Status: NotConfigured,
			Detail:   "the second identical request reached the upstream, so no cache is active on this deployment",
			Evidence: ev}
	}
	if second.Status != 200 {
		return Check{Status: Fail, Detail: fmt.Sprintf("served a cache hit but returned %d", second.Status), Evidence: ev}
	}
	return Check{Status: Pass, Metric: f64(float64(second.Latency.Milliseconds())), Unit: "ms cache-hit latency",
		Detail:   fmt.Sprintf("cache hit served in %dms against %dms uncached", second.Latency.Milliseconds(), first.Latency.Milliseconds()),
		Evidence: ev}
}

// C8: gateways that rebuild the request body sometimes drop the fields agent
// frameworks depend on. Structured output and tool calling break silently.
func checkParameterFidelity(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	if err := ctrl.Healthy(ctx, "primary"); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	body, _ := json.Marshal(map[string]any{
		"model": t.Model,
		"messages": []any{
			map[string]any{"role": "system", "content": "you are a fidelity probe"},
			map[string]any{"role": "user", "content": "call the tool"},
		},
		"temperature": 0.7,
		"top_p":       0.9,
		"max_tokens":  32,
		"seed":        4242,
		"stop":        []string{"END"},
		"user":        "bench-user-1",
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "lookup_weather",
				"description": "look up the weather",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"city": map[string]any{"type": "string"}},
					"required":   []string{"city"},
				},
			},
		}},
		"tool_choice":     "auto",
		"response_format": map[string]any{"type": "json_object"},
	})

	r := p.Do(ctx, body, false, "", nil)
	journal, _ := ctrl.Journal(ctx)
	if len(journal) == 0 {
		return Check{Status: Inconclusive, Detail: fmt.Sprintf("no upstream request recorded (client saw %d)", r.Status)}
	}
	got := journal[len(journal)-1].Params

	want := map[string]any{
		"temperature": 0.7, "top_p": 0.9, "max_tokens": float64(32),
		"seed": float64(4242), "user": "bench-user-1",
		"tools_count": float64(1), "tool_choice": "auto",
	}
	// A gateway may legitimately rename a field rather than drop it. OpenAI
	// deprecated max_tokens in favour of max_completion_tokens, and Bifrost
	// translates it; treating that as a dropped parameter would be a false
	// accusation, which is worse than not running the check at all.
	aliases := map[string][]string{
		"max_tokens": {"max_completion_tokens"},
	}

	var missing, wrong []string
	for k, expect := range want {
		actual, ok := got[k]
		if !ok {
			for _, alt := range aliases[k] {
				if v, found := got[alt]; found {
					actual, ok = v, true
					k = k + " (as " + alt + ")"
					break
				}
			}
		}
		if !ok {
			missing = append(missing, k)
			continue
		}
		if fmt.Sprint(actual) != fmt.Sprint(expect) {
			wrong = append(wrong, fmt.Sprintf("%s: sent %v, forwarded %v", k, expect, actual))
		}
	}
	if _, ok := got["response_format"]; !ok {
		missing = append(missing, "response_format")
	}
	if _, ok := got["stop"]; !ok {
		missing = append(missing, "stop")
	}
	sort.Strings(missing)
	sort.Strings(wrong)

	ev := map[string]any{"forwarded": got, "missing": missing, "altered": wrong, "client_status": r.Status}
	if len(missing) == 0 && len(wrong) == 0 {
		return Check{Status: Pass, Detail: "every parameter reached the upstream unchanged", Evidence: ev}
	}
	return Check{Status: Fail, Metric: f64(float64(len(missing) + len(wrong))), Unit: "fields lost or altered",
		Detail:   fmt.Sprintf("%d dropped (%v), %d altered (%v)", len(missing), missing, len(wrong), wrong),
		Evidence: ev}
}

// C9: billing and chargeback are built on these numbers, so a gateway that
// reports usage inconsistent with the provider is quietly mis-billing.
func checkTokenAccounting(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{Chunks: 24, UnaryDelayMs: 50}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	r := p.Do(ctx, ChatBody(t.Model, "token accounting probe with a deliberately measurable prompt", false), false, "", nil)
	journal, _ := ctrl.Journal(ctx)
	if len(journal) == 0 {
		return Check{Status: Inconclusive, Detail: "no upstream request recorded"}
	}
	truth := journal[len(journal)-1]

	ev := map[string]any{
		"gateway_reported": r.Usage,
		"upstream_truth":   map[string]int{"prompt_tokens": truth.PromptToks, "completion_tokens": truth.OutputToks},
	}
	if r.Usage.TotalTokens == 0 && r.Usage.PromptTokens == 0 {
		return Check{Status: Fail, Detail: "the gateway returned no usage block, so per-request cost cannot be attributed", Evidence: ev}
	}
	if r.Usage.PromptTokens == truth.PromptToks && r.Usage.CompletionTokens == truth.OutputToks {
		return Check{Status: Pass, Detail: "reported usage matches the upstream exactly", Evidence: ev}
	}
	return Check{Status: Fail,
		Detail:   fmt.Sprintf("reported %d/%d tokens against an upstream truth of %d/%d", r.Usage.PromptTokens, r.Usage.CompletionTokens, truth.PromptToks, truth.OutputToks),
		Evidence: ev}
}

// C10: if tenant A's prompt can be served from cache to tenant B, the gateway
// has a data leak, not a cache. This is the check that matters most and the one
// nobody publishes.
func checkTenantIsolation(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	second := t.SecondaryKey()
	if second == "" {
		return Check{Status: NotConfigured, Detail: "no secondary_key_env configured, so a second tenant could not be simulated"}
	}
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{UnaryDelayMs: 300, Chunks: 16}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	body := ChatBody(t.Model, "tenant isolation probe: this prompt belongs to tenant A only", false)

	tenantA := p.Do(ctx, body, false, "", nil)
	if tenantA.Status != 200 {
		return Check{Status: Inconclusive, Detail: fmt.Sprintf("tenant A priming request returned %d", tenantA.Status)}
	}
	// Confirm the cache is warm for tenant A, otherwise a "miss" for tenant B
	// proves isolation only by accident.
	afterA := len(mustJournal(ctx, ctrl))
	tenantARepeat := p.Do(ctx, body, false, "", nil)
	afterARepeat := len(mustJournal(ctx, ctrl))
	cacheActive := afterARepeat == afterA

	tenantB := p.Do(ctx, body, false, second, nil)
	afterB := len(mustJournal(ctx, ctrl))

	ev := map[string]any{
		"cache_active_for_tenant_a": cacheActive,
		"upstream_after_a":          afterA,
		"upstream_after_a_repeat":   afterARepeat,
		"upstream_after_b":          afterB,
		"tenant_a_repeat_ms":        tenantARepeat.Latency.Milliseconds(),
		"tenant_b_ms":               tenantB.Latency.Milliseconds(),
		"tenant_b_status":           tenantB.Status,
	}

	if !cacheActive {
		return Check{Status: NotConfigured,
			Detail:   "no cache was active for the first tenant, so cross-tenant cache reads could not be tested",
			Evidence: ev}
	}
	if afterB == afterARepeat {
		return Check{Status: Fail,
			Detail:   "tenant B's identical prompt never reached the upstream: it was served from tenant A's cache entry, which is a cross-tenant data leak",
			Evidence: ev}
	}
	return Check{Status: Pass,
		Detail:   "tenant B's identical prompt went to the upstream rather than reading tenant A's cache entry",
		Evidence: ev}
}

// C11: a gateway that holds the client connection open while the upstream hangs
// converts one slow provider into an exhausted connection pool downstream.
func checkTimeout(ctx context.Context, t Target, ctrl *Control, p *Probe) Check {
	if err := ctrl.SetProfile(ctx, "primary", ProfileSpec{Fault: "hang", FaultRate: 1}); err != nil {
		return Check{Status: Inconclusive, Detail: err.Error()}
	}
	_ = ctrl.Reset(ctx)

	clientTimeout := p.HTTP.Timeout
	start := time.Now()
	r := p.Do(ctx, ChatBody(t.Model, "timeout probe", false), false, "", nil)
	elapsed := time.Since(start)

	ev := map[string]any{
		"client_timeout_ms": clientTimeout.Milliseconds(),
		"elapsed_ms":        elapsed.Milliseconds(),
		"status":            r.Status,
		"error":             r.Err,
	}
	// A 5% margin distinguishes "the gateway gave up" from "the client did".
	if r.Err != "" && elapsed >= time.Duration(float64(clientTimeout)*0.95) {
		return Check{Status: Fail, Metric: f64(float64(elapsed.Milliseconds())), Unit: "ms",
			Detail: fmt.Sprintf("held the client connection open for the full %s the probe was willing to wait, "+
				"against an upstream that never responded; either it enforces no request timeout of its own or its "+
				"timeout is longer than %s", clientTimeout, clientTimeout),
			Evidence: ev}
	}
	if r.Status >= 500 && r.Status < 600 {
		return Check{Status: Pass, Metric: f64(float64(elapsed.Milliseconds())), Unit: "ms",
			Detail:   fmt.Sprintf("gave up on the hung upstream after %dms and returned %d", elapsed.Milliseconds(), r.Status),
			Evidence: ev}
	}
	return Check{Status: Inconclusive,
		Detail:   fmt.Sprintf("returned %d after %dms against a hung upstream", r.Status, elapsed.Milliseconds()),
		Evidence: ev}
}

// C12 is scored from the feature matrix rather than probed: what a gateway
// retains about a request is only visible in its own UI and API, which differ
// too much to probe uniformly. The matrix carries a citation per cell.
func checkObservability(t Target) Check {
	return Check{Status: MatrixOnly,
		Detail: "scored in the feature matrix (observability group) with per-cell evidence links rather than probed over HTTP"}
}

func containsBudgetWord(body string) bool {
	for _, w := range []string{"budget", "quota", "spend", "credit", "insufficient"} {
		if len(body) > 0 && containsFold(body, w) {
			return true
		}
	}
	return false
}

func containsFold(haystack, needle string) bool {
	h, n := []rune(haystack), []rune(needle)
	if len(n) > len(h) {
		return false
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(n) <= len(h); i++ {
		ok := true
		for j := range n {
			if lower(h[i+j]) != lower(n[j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func mustJournal(ctx context.Context, ctrl *Control) []upstream.Entry {
	j, _ := ctrl.Journal(ctx)
	return j
}

func byProfile(entries []upstream.Entry) map[string]int {
	out := map[string]int{}
	for _, e := range entries {
		out[e.Profile]++
	}
	return out
}

func gapsMs(entries []upstream.Entry) []float64 {
	if len(entries) < 2 {
		return []float64{0}
	}
	sorted := make([]upstream.Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })
	var out []float64
	for i := 1; i < len(sorted); i++ {
		out = append(out, float64(sorted[i].At.Sub(sorted[i-1].At).Milliseconds()))
	}
	return out
}
