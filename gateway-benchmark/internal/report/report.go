// Package report turns a benchmark run into the published artefacts: a JSON
// bundle with every number, and a Markdown report a human reads.
//
// The generator enforces the reporting rules from
// METHODOLOGY.md section 6, including the one that matters
// most: it computes the axes where Everstack loses and writes them into the
// report automatically, so shipping a flattering report requires deleting code
// rather than just omitting a table.
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/everstacklabs/examples/gateway-benchmark/internal/harness"
	"github.com/everstacklabs/examples/gateway-benchmark/internal/matrix"
	"github.com/everstacklabs/examples/gateway-benchmark/internal/stats"
)

// Bundle is the complete machine-readable result of a run.
type Bundle struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Subject     string                `json:"subject"`
	Hardware    string                `json:"hardware"`
	Notes       string                `json:"notes"`
	Upstream    harness.UpstreamRef   `json:"upstream"`
	Load        harness.LoadDefaults  `json:"load"`
	Perf        []*harness.PerfReport `json:"performance"`
	Checks      []harness.Check       `json:"correctness"`
	Matrix      *matrix.Matrix        `json:"matrix,omitempty"`
	ControlName string                `json:"control_target"`
	// Unmeasured records targets that were defined but could not be measured,
	// with the reason. A target that silently vanishes from a comparison is
	// indistinguishable from one that was never in it.
	Unmeasured []Unmeasured `json:"unmeasured,omitempty"`
}

// Unmeasured is a target that was configured but produced no data.
type Unmeasured struct {
	Target string `json:"target"`
	Reason string `json:"reason"`
}

// aggregate is one target's headline numbers, already expressed as deltas.
type aggregate struct {
	target string

	unaryP50, unaryP95, unaryP99 stats.Across
	streamTTFT                   stats.Across
	interChunkP99                stats.Across

	addedP50, addedP95, addedP99 float64
	addedTTFT                    float64

	maxSustainedRPS  float64
	errorRate        float64
	degradedAddedP99 float64
	cpuPer10k        float64
	peakMemMB        float64
	resourcesKnown   bool
	hasStreaming     bool
}

// Markdown renders the full report.
func Markdown(b *Bundle) string {
	var s strings.Builder

	aggs := aggregates(b)
	control, ok := aggs[b.ControlName]

	s.WriteString("# AI Gateway Benchmark\n\n")
	fmt.Fprintf(&s, "Generated %s. Subject: %s.\n\n", b.GeneratedAt.Format("2006-01-02 15:04 MST"), orDash(b.Subject))

	writeMethodology(&s, b)
	writeEnvironment(&s, b)

	if !ok {
		s.WriteString("> No control target was measured, so added-latency figures cannot be computed. " +
			"Absolute latency through the mock upstream is an artefact of the mock's configuration and is not reported.\n\n")
	} else {
		writeAddedLatency(&s, b, aggs, control)
		writeStreaming(&s, b, aggs, control)
		writeThroughput(&s, b, aggs)
		writeConcurrency(&s, b)
		writeResources(&s, b, aggs)
		writeDegraded(&s, b, aggs, control)
	}

	writeUnmeasured(&s, b)
	writeCorrectness(&s, b)
	writeMatrix(&s, b)
	writeLosses(&s, b, aggs)
	writeLimits(&s, b)

	return s.String()
}

func writeMethodology(s *strings.Builder, b *Bundle) {
	s.WriteString("## Methodology in one screen\n\n")
	s.WriteString("Every gateway in this report proxies to the **same deterministic mock upstream**, not to a live model provider. " +
		"A live provider varies by hundreds of milliseconds between calls; gateway overhead is sub-millisecond to low-milliseconds. " +
		"Benchmarking through a real provider measures the provider's variance and labels it a gateway difference.\n\n")
	fmt.Fprintf(s, "- **Control**: `%s` is the harness talking straight to the mock with no gateway in the path.\n", b.ControlName)
	s.WriteString("- **Every latency number below is added latency over that control.** Absolute numbers would just describe the mock's configuration.\n")
	s.WriteString("- **Load is open-loop.** Arrivals are scheduled against a wall clock and latency is measured from the scheduled arrival, not from when a worker picked the request up. Closed-loop generators stop offering load exactly when the system slows down, which flatters every tail number.\n")
	s.WriteString("- **Only successful responses enter the latency distribution.** A fast 500 is not a fast response.\n")
	fmt.Fprintf(s, "- **Runs**: %d per phase, %ds warmup discarded, %ds measured.\n",
		b.Load.Runs, b.Load.WarmupSeconds, b.Load.DurationSeconds)
	s.WriteString("- **Cloud-only gateways are not in the latency tables.** Measuring one from a laptop measures the internet. They appear in the feature matrix only.\n\n")
}

func writeEnvironment(s *strings.Builder, b *Bundle) {
	s.WriteString("## What was measured\n\n")
	if b.Hardware != "" {
		fmt.Fprintf(s, "**Hardware**: %s\n\n", b.Hardware)
	}
	s.WriteString("| Target | Image | Version |\n|---|---|---|\n")
	var emulated []string
	for _, p := range b.Perf {
		name := p.Target
		if p.Emulated {
			name += " (emulated)"
			emulated = append(emulated, p.Target)
		}
		fmt.Fprintf(s, "| %s | `%s` | %s |\n", name, orDash(p.Image), orDash(p.Version))
	}
	s.WriteString("\n")
	if len(emulated) > 0 {
		fmt.Fprintf(s, "> **Not comparable: %s.** These images ship for a different CPU architecture "+
			"than the host and ran under emulation, which commonly costs several times the native latency and CPU. "+
			"Their rows are reported for completeness, not for ranking against the natively-running targets. "+
			"Re-run on a matching architecture before drawing any conclusion about them.\n\n",
			strings.Join(emulated, ", "))
	}
	if b.Notes != "" {
		fmt.Fprintf(s, "%s\n\n", b.Notes)
	}
}

func writeAddedLatency(s *strings.Builder, b *Bundle, aggs map[string]aggregate, control aggregate) {
	s.WriteString("## P1. Added latency, non-streaming\n\n")
	fmt.Fprintf(s, "Offered load %.0f rps. Figures are milliseconds added over the direct control, averaged across %d runs with the run-to-run standard deviation.\n\n",
		b.Load.RPS, b.Load.Runs)
	s.WriteString("| Gateway | added p50 | added p95 | added p99 | run-to-run sd (p99) | errors |\n|---|---|---|---|---|---|\n")

	for _, a := range sortedBy(aggs, b, func(x aggregate) float64 { return x.addedP99 }) {
		if a.target == b.ControlName {
			continue
		}
		fmt.Fprintf(s, "| %s | %s | %s | %s | %.2f | %.2f%% |\n",
			a.target, ms(a.addedP50), ms(a.addedP95), ms(a.addedP99), a.unaryP99.StdDev, a.errorRate*100)
	}
	fmt.Fprintf(s, "\nControl absolute p50/p95/p99: %.1f / %.1f / %.1f ms.\n\n",
		control.unaryP50.Mean, control.unaryP95.Mean, control.unaryP99.Mean)
}

func writeStreaming(s *strings.Builder, b *Bundle, aggs map[string]aggregate, control aggregate) {
	any := false
	for _, a := range aggs {
		if a.hasStreaming && a.target != b.ControlName {
			any = true
		}
	}
	if !any {
		return
	}

	s.WriteString("## P2 and P3. Streaming: first token and cadence\n\n")
	s.WriteString("Time-to-first-token is measured at the first frame carrying **content**, not the first byte. " +
		"A gateway that emits its own opening frame early would otherwise post a flattering TTFT while the user still sees nothing.\n\n")
	s.WriteString("Inter-chunk p99 is the tell for buffering: the upstream paces tokens at a fixed interval, so a gateway that " +
		"collects and re-emits them in bursts shows a stretched tail here even when its TTFT looks fine.\n\n")
	s.WriteString("| Gateway | added TTFT | inter-chunk p99 | truncated streams |\n|---|---|---|---|\n")

	for _, a := range sortedBy(aggs, b, func(x aggregate) float64 { return x.addedTTFT }) {
		if a.target == b.ControlName || !a.hasStreaming {
			continue
		}
		fmt.Fprintf(s, "| %s | %s | %.2f ms | %d |\n",
			a.target, ms(a.addedTTFT), a.interChunkP99.Mean, truncatedCount(b, a.target))
	}
	fmt.Fprintf(s, "\nControl TTFT %.1f ms, control inter-chunk p99 %.2f ms.\n\n",
		control.streamTTFT.Mean, control.interChunkP99.Mean)
}

func writeThroughput(s *strings.Builder, b *Bundle, aggs map[string]aggregate) {
	s.WriteString("## P4. Sustained throughput\n\n")
	s.WriteString("The offered rate is stepped until the target's error rate passes 5% or the generator's in-flight cap saturates. " +
		"The reported figure is the highest offered rate the target actually served.\n\n")
	s.WriteString("| Gateway | max sustained rps | notes |\n|---|---|---|\n")

	list := sortedBy(aggs, b, func(x aggregate) float64 { return -x.maxSustainedRPS })
	for _, a := range list {
		note := ""
		if p := findPerf(b, a.target); p != nil && len(p.Saturation) > 0 {
			last := p.Saturation[len(p.Saturation)-1]
			if last.Dropped > 0 {
				note = fmt.Sprintf("generator dropped %d arrivals at %.0f rps, so this is a floor not a ceiling", last.Dropped, last.OfferedRPS)
			} else if last.ErrorRate > 0.05 {
				note = fmt.Sprintf("error rate %.1f%% at %.0f rps", last.ErrorRate*100, last.OfferedRPS)
			}
		}
		fmt.Fprintf(s, "| %s | %.0f | %s |\n", a.target, a.maxSustainedRPS, orDash(note))
	}
	s.WriteString("\n")
}

func writeConcurrency(s *strings.Builder, b *Bundle) {
	steps := map[int]bool{}
	for _, p := range b.Perf {
		for _, c := range p.Concurrency {
			steps[c.Concurrency] = true
		}
	}
	if len(steps) == 0 {
		return
	}
	var ordered []int
	for k := range steps {
		ordered = append(ordered, k)
	}
	sort.Ints(ordered)

	s.WriteString("## P5. Behaviour under concurrency\n\n")
	s.WriteString("p99 latency in milliseconds at a fixed in-flight ceiling. Growth that outpaces the control is connection-pool or event-loop pressure inside the gateway.\n\n")

	s.WriteString("| Gateway |")
	for _, c := range ordered {
		fmt.Fprintf(s, " %d |", c)
	}
	s.WriteString("\n|---|")
	for range ordered {
		s.WriteString("---|")
	}
	s.WriteString("\n")

	for _, p := range b.Perf {
		fmt.Fprintf(s, "| %s |", p.Target)
		for _, c := range ordered {
			v := ""
			for _, ph := range p.Concurrency {
				if ph.Concurrency == c {
					v = fmt.Sprintf("%.1f", ph.Latency.P99)
				}
			}
			fmt.Fprintf(s, " %s |", orDash(v))
		}
		s.WriteString("\n")
	}
	s.WriteString("\n")
}

func writeResources(s *strings.Builder, b *Bundle, aggs map[string]aggregate) {
	any := false
	for _, a := range aggs {
		if a.resourcesKnown {
			any = true
		}
	}
	if !any {
		s.WriteString("## P6. Resource cost\n\nNot collected: container stats were unavailable for this run.\n\n")
		return
	}

	s.WriteString("## P6. What the hop costs to run\n\n")
	s.WriteString("This is the dimension that turns \"a few milliseconds\" into a line on a cloud bill. " +
		"CPU-seconds are normalised per 10,000 requests so targets that served different volumes stay comparable.\n\n")
	s.WriteString("| Gateway | CPU-seconds / 10k requests | peak RSS |\n|---|---|---|\n")

	for _, a := range sortedBy(aggs, b, func(x aggregate) float64 { return x.cpuPer10k }) {
		if !a.resourcesKnown {
			continue
		}
		fmt.Fprintf(s, "| %s | %.1f | %.0f MB |\n", a.target, a.cpuPer10k, a.peakMemMB)
	}
	s.WriteString("\n")
}

func writeDegraded(s *strings.Builder, b *Bundle, aggs map[string]aggregate, control aggregate) {
	s.WriteString("## P7. Overhead while the upstream is failing\n\n")
	s.WriteString("Most published gateway benchmarks only measure the happy path, which is the path that matters least. " +
		"Here the upstream returns 503 for one request in five throughout the phase.\n\n")
	s.WriteString("| Gateway | added p99 while degraded | added p99 healthy | delta |\n|---|---|---|---|\n")

	for _, a := range sortedBy(aggs, b, func(x aggregate) float64 { return x.degradedAddedP99 }) {
		if a.target == b.ControlName {
			continue
		}
		fmt.Fprintf(s, "| %s | %s | %s | %s |\n",
			a.target, ms(a.degradedAddedP99), ms(a.addedP99), ms(a.degradedAddedP99-a.addedP99))
	}
	s.WriteString("\n")
}

// writeUnmeasured names every target that dropped out. Omitting them would let
// a gateway that failed to start read as one that was never considered.
func writeUnmeasured(s *strings.Builder, b *Bundle) {
	if len(b.Unmeasured) == 0 {
		return
	}
	s.WriteString("## Targets that could not be measured\n\n")
	s.WriteString("These were configured for this run but produced no data. They are listed rather than dropped, " +
		"because a comparison that quietly omits the gateways it could not start is not a comparison.\n\n")
	s.WriteString("| Target | Why |\n|---|---|\n")
	for _, u := range b.Unmeasured {
		fmt.Fprintf(s, "| %s | %s |\n", u.Target, u.Reason)
	}
	s.WriteString("\n")
}

func writeCorrectness(s *strings.Builder, b *Bundle) {
	if len(b.Checks) == 0 {
		return
	}
	s.WriteString("## Behaviour under failure: the scorecard\n\n")
	s.WriteString("Speed is table stakes. These scenarios are what separate a proxy from a control plane. " +
		"Every verdict is derived from the mock upstream's request journal, so \"the gateway retried four times\" " +
		"is a counted fact rather than an inference from client-side timing.\n\n")
	s.WriteString("Legend: **pass** did the right thing. **fail** did the wrong thing. " +
		"**not supported** means the capability is absent, which is a different product, not a defect. " +
		"**not configured** means the capability exists but this deployment did not enable it, so the run proves nothing.\n\n")

	targets := []string{}
	byTarget := map[string]map[string]harness.Check{}
	ids := []string{}
	seenID := map[string]bool{}
	for _, c := range b.Checks {
		if _, ok := byTarget[c.Target]; !ok {
			byTarget[c.Target] = map[string]harness.Check{}
			targets = append(targets, c.Target)
		}
		byTarget[c.Target][c.ID] = c
		if !seenID[c.ID] {
			seenID[c.ID] = true
			ids = append(ids, c.ID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return checkOrder(ids[i]) < checkOrder(ids[j]) })

	s.WriteString("| Scenario |")
	for _, t := range targets {
		fmt.Fprintf(s, " %s |", t)
	}
	s.WriteString("\n|---|")
	for range targets {
		s.WriteString("---|")
	}
	s.WriteString("\n")

	for _, id := range ids {
		name := id
		for _, t := range targets {
			if c, ok := byTarget[t][id]; ok && c.Name != "" {
				name = fmt.Sprintf("%s. %s", id, c.Name)
				break
			}
		}
		fmt.Fprintf(s, "| %s |", name)
		for _, t := range targets {
			c, ok := byTarget[t][id]
			if !ok {
				s.WriteString(" - |")
				continue
			}
			fmt.Fprintf(s, " %s |", statusCell(c))
		}
		s.WriteString("\n")
	}
	s.WriteString("\n")

	// Failures get their detail printed. A scorecard without the reasoning is
	// an assertion.
	var fails []harness.Check
	for _, c := range b.Checks {
		if c.Status == harness.Fail {
			fails = append(fails, c)
		}
	}
	if len(fails) > 0 {
		s.WriteString("### Every failure, with its evidence\n\n")
		for _, c := range fails {
			fmt.Fprintf(s, "- **%s / %s (%s)**: %s\n", c.Target, c.ID, c.Name, c.Detail)
		}
		s.WriteString("\n")
	}
}

func writeMatrix(s *strings.Builder, b *Bundle) {
	m := b.Matrix
	if m == nil {
		return
	}
	s.WriteString("## Capability matrix\n\n")
	fmt.Fprintf(s, "Verified %s. This table is deliberately separate from the performance numbers: a feature win is not a latency win.\n\n", m.VerifiedOn)
	s.WriteString("Legend: `yes` available. `partial` limited or requires assembly. `paid` exists only above a commercial tier. " +
		"`no` absent. `unknown` not yet verified, and excluded from scoring in both directions.\n\n")

	cov := m.Coverage()
	s.WriteString("| Vendor | Deployment | License | Matrix coverage | In latency tables |\n|---|---|---|---|---|\n")
	for _, v := range m.Vendors {
		bench := "no"
		if v.Benchmarked {
			bench = "yes"
		}
		fmt.Fprintf(s, "| %s | %s | %s | %.0f%% | %s |\n", v.Name, orDash(v.Tier), orDash(v.License), cov[v.ID], bench)
	}
	s.WriteString("\n")

	for _, g := range m.Groups {
		fmt.Fprintf(s, "### %s\n\n", g.Name)
		if g.Summary != "" {
			fmt.Fprintf(s, "%s\n\n", g.Summary)
		}
		s.WriteString("| Capability | Why it matters |")
		for _, v := range m.Vendors {
			fmt.Fprintf(s, " %s |", v.Name)
		}
		s.WriteString("\n|---|---|")
		for range m.Vendors {
			s.WriteString("---|")
		}
		s.WriteString("\n")
		for _, d := range g.Dimensions {
			fmt.Fprintf(s, "| %s | %s |", d.Name, d.Why)
			for _, v := range m.Vendors {
				c := d.Cells[v.ID]
				fmt.Fprintf(s, " %s |", cellText(c))
			}
			s.WriteString("\n")
		}
		s.WriteString("\n")
	}
}

// writeLosses computes, rather than curates, the axes where the subject is
// beaten. This section is why the rest of the report is worth reading.
func writeLosses(s *strings.Builder, b *Bundle, aggs map[string]aggregate) {
	subject := b.Subject
	me, ok := aggs[subject]
	if !ok {
		// The subject dropped out. Silently omitting the section here would be
		// the exact failure it exists to prevent, so say so instead.
		s.WriteString("## Where we lose\n\n")
		fmt.Fprintf(s, "Not computed: the subject (`%s`) produced no performance data in this run, "+
			"so there is nothing to compare against the other targets. See the unmeasured-targets table above.\n\n", subject)
		return
	}

	type loss struct{ axis, winner, detail string }
	var losses []loss

	best := func(pick func(aggregate) float64, lowerWins bool) (string, float64) {
		bestName, bestVal := "", 0.0
		first := true
		for name, a := range aggs {
			if name == b.ControlName {
				continue
			}
			v := pick(a)
			if first {
				bestName, bestVal, first = name, v, false
				continue
			}
			if (lowerWins && v < bestVal) || (!lowerWins && v > bestVal) {
				bestName, bestVal = name, v
			}
		}
		return bestName, bestVal
	}

	if w, v := best(func(a aggregate) float64 { return a.addedP99 }, true); w != subject && w != "" {
		losses = append(losses, loss{"Added p99 latency, non-streaming", w,
			fmt.Sprintf("%s adds %.2f ms against our %.2f ms", w, v, me.addedP99)})
	}
	if w, v := best(func(a aggregate) float64 { return a.addedTTFT }, true); w != subject && w != "" && me.hasStreaming {
		losses = append(losses, loss{"Added time-to-first-token", w,
			fmt.Sprintf("%s adds %.2f ms against our %.2f ms", w, v, me.addedTTFT)})
	}
	if w, v := best(func(a aggregate) float64 { return a.maxSustainedRPS }, false); w != subject && w != "" {
		losses = append(losses, loss{"Sustained throughput", w,
			fmt.Sprintf("%s sustained %.0f rps against our %.0f rps", w, v, me.maxSustainedRPS)})
	}
	if me.resourcesKnown {
		if w, v := best(func(a aggregate) float64 {
			if !a.resourcesKnown {
				return 1e18
			}
			return a.cpuPer10k
		}, true); w != subject && w != "" && v < 1e17 {
			losses = append(losses, loss{"CPU cost per 10k requests", w,
				fmt.Sprintf("%s burns %.1f CPU-seconds against our %.1f", w, v, me.cpuPer10k)})
		}
	}

	// Correctness losses: anywhere a competitor passed and we did not.
	byTargetID := map[string]map[string]harness.Check{}
	for _, c := range b.Checks {
		if byTargetID[c.Target] == nil {
			byTargetID[c.Target] = map[string]harness.Check{}
		}
		byTargetID[c.Target][c.ID] = c
	}
	for id, mine := range byTargetID[subject] {
		if mine.Status == harness.Pass {
			continue
		}
		for t, checks := range byTargetID {
			if t == subject {
				continue
			}
			if other, ok := checks[id]; ok && other.Status == harness.Pass {
				losses = append(losses, loss{fmt.Sprintf("%s. %s", id, mine.Name), t,
					fmt.Sprintf("%s passes; we are %s (%s)", t, mine.Status, mine.Detail)})
				break
			}
		}
	}

	s.WriteString("## Where we lose\n\n")
	if len(losses) == 0 {
		s.WriteString("On this run, no other target beat the subject on any measured axis. " +
			"Treat that with suspicion rather than satisfaction: check that every competitor was configured " +
			"for production rather than defaults, and that the load actually reached each one's saturation point.\n\n")
		return
	}
	s.WriteString("This section is generated from the same data as every table above. " +
		"It exists because a vendor benchmark where the vendor wins everything is not evidence, it is marketing.\n\n")
	s.WriteString("| Axis | Who is better | By how much |\n|---|---|---|\n")
	sort.Slice(losses, func(i, j int) bool { return losses[i].axis < losses[j].axis })
	for _, l := range losses {
		fmt.Fprintf(s, "| %s | %s | %s |\n", l.axis, l.winner, l.detail)
	}
	s.WriteString("\n")
}

func writeLimits(s *strings.Builder, b *Bundle) {
	s.WriteString("## Limits of this benchmark\n\n")
	s.WriteString("- **Single host.** Every gateway and the upstream share one machine. This removes network variance, which is the point, but it also hides how each design behaves across a real network and under horizontal scale.\n")
	s.WriteString("- **A mock upstream is not a model provider.** It has no queueing, no token-level rate limits, and no regional failover. Scenarios that depend on real provider behaviour are out of scope here.\n")
	s.WriteString("- **Configuration is a judgement call.** Every competitor runs the settings committed under `benchmarks/gateway/compose/`, each with a comment justifying it. A gateway tuned differently will produce different numbers, and we would rather be corrected than be wrong quietly.\n")
	s.WriteString("- **Cloud-only gateways have no latency numbers here at all**, by design.\n")
	s.WriteString("- **`not configured` is not `not supported`.** Several cells in the scorecard record that a capability exists but was not enabled in this deployment. Those prove nothing and are not counted either way.\n\n")
}

// ---------- helpers ----------

func aggregates(b *Bundle) map[string]aggregate {
	control := findPerf(b, b.ControlName)
	out := map[string]aggregate{}

	var cP50, cP95, cP99, cTTFT float64
	if control != nil {
		cP50 = meanOf(control.Unary, func(p harness.PhaseResult) float64 { return p.Latency.P50 }).Mean
		cP95 = meanOf(control.Unary, func(p harness.PhaseResult) float64 { return p.Latency.P95 }).Mean
		cP99 = meanOf(control.Unary, func(p harness.PhaseResult) float64 { return p.Latency.P99 }).Mean
		cTTFT = meanOf(control.Streaming, func(p harness.PhaseResult) float64 { return p.TTFT.P50 }).Mean
	}

	for _, p := range b.Perf {
		a := aggregate{target: p.Target}
		a.unaryP50 = meanOf(p.Unary, func(x harness.PhaseResult) float64 { return x.Latency.P50 })
		a.unaryP95 = meanOf(p.Unary, func(x harness.PhaseResult) float64 { return x.Latency.P95 })
		a.unaryP99 = meanOf(p.Unary, func(x harness.PhaseResult) float64 { return x.Latency.P99 })
		a.streamTTFT = meanOf(p.Streaming, func(x harness.PhaseResult) float64 { return x.TTFT.P50 })
		a.interChunkP99 = meanOf(p.Streaming, func(x harness.PhaseResult) float64 { return x.InterChunk.P99 })
		a.hasStreaming = len(p.Streaming) > 0

		a.addedP50 = a.unaryP50.Mean - cP50
		a.addedP95 = a.unaryP95.Mean - cP95
		a.addedP99 = a.unaryP99.Mean - cP99
		a.addedTTFT = a.streamTTFT.Mean - cTTFT

		a.errorRate = meanOf(p.Unary, func(x harness.PhaseResult) float64 { return x.ErrorRate }).Mean

		for _, sat := range p.Saturation {
			if sat.ErrorRate <= 0.05 && sat.Throughput > a.maxSustainedRPS {
				a.maxSustainedRPS = sat.Throughput
			}
		}

		if len(p.Degraded) > 0 {
			a.degradedAddedP99 = p.Degraded[0].Latency.P99 - cP99
		}

		for _, ph := range p.Unary {
			if ph.Resources.Unavailable == "" && ph.Resources.Samples > 0 {
				a.resourcesKnown = true
				a.cpuPer10k = ph.Resources.CPUSecondsPer10k
				if ph.Resources.PeakMemMB > a.peakMemMB {
					a.peakMemMB = ph.Resources.PeakMemMB
				}
			}
		}

		out[p.Target] = a
	}
	return out
}

func meanOf(phases []harness.PhaseResult, pick func(harness.PhaseResult) float64) stats.Across {
	var vals []float64
	for _, p := range phases {
		vals = append(vals, pick(p))
	}
	return stats.MeanAcross(vals)
}

func findPerf(b *Bundle, name string) *harness.PerfReport {
	for _, p := range b.Perf {
		if p.Target == name {
			return p
		}
	}
	return nil
}

func sortedBy(aggs map[string]aggregate, b *Bundle, key func(aggregate) float64) []aggregate {
	var out []aggregate
	for _, a := range aggs {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].target == b.ControlName {
			return true
		}
		if out[j].target == b.ControlName {
			return false
		}
		return key(out[i]) < key(out[j])
	})
	return out
}

func truncatedCount(b *Bundle, target string) int {
	p := findPerf(b, target)
	if p == nil {
		return 0
	}
	n := 0
	for _, ph := range p.Streaming {
		n += ph.Truncated
	}
	return n
}

func statusCell(c harness.Check) string {
	label := map[harness.CheckStatus]string{
		harness.Pass:          "pass",
		harness.Fail:          "**fail**",
		harness.NotSupported:  "not supported",
		harness.NotConfigured: "not configured",
		harness.Inconclusive:  "inconclusive",
		harness.MatrixOnly:    "see matrix",
	}[c.Status]
	if label == "" {
		label = string(c.Status)
	}
	if c.Metric != nil && c.Unit != "" {
		return fmt.Sprintf("%s (%.4g %s)", label, *c.Metric, c.Unit)
	}
	return label
}

func cellText(c matrix.Cell) string {
	if c.Score == "" {
		return "unknown"
	}
	txt := string(c.Score)
	if c.Note != "" {
		txt += " - " + c.Note
	}
	if c.Evidence != "" {
		txt += fmt.Sprintf(" ([src](%s))", c.Evidence)
	}
	return txt
}

func checkOrder(id string) int {
	n := 0
	for _, r := range id {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
		}
	}
	return n
}

func ms(v float64) string {
	if v > -0.005 && v < 0.005 {
		return "~0 ms"
	}
	return fmt.Sprintf("%+.2f ms", v)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
