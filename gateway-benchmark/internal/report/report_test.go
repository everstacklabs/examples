package report

import (
	"strings"
	"testing"
	"time"

	"github.com/everstacklabs/examples/gateway-benchmark/internal/harness"
	"github.com/everstacklabs/examples/gateway-benchmark/internal/stats"
)

func phase(name, target string, p50, p95, p99 float64) harness.PhaseResult {
	return harness.PhaseResult{
		Phase:   name,
		Target:  target,
		Latency: stats.Summary{Count: 100, P50: p50, P95: p95, P99: p99},
	}
}

// bundle builds a run where the subject is deliberately SLOWER than a rival, so
// the generated "where we lose" section has something real to find.
func bundle() *Bundle {
	return &Bundle{
		GeneratedAt: time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
		Subject:     "everstack",
		ControlName: "direct",
		Load:        harness.LoadDefaults{Runs: 3, RPS: 50, WarmupSeconds: 5, DurationSeconds: 20},
		Perf: []*harness.PerfReport{
			{Target: "direct", Unary: []harness.PhaseResult{phase("u", "direct", 100, 110, 120)}},
			{Target: "everstack", Unary: []harness.PhaseResult{phase("u", "everstack", 104, 116, 130)},
				Saturation: []harness.PhaseResult{{Phase: "s", Throughput: 400, ErrorRate: 0}}},
			{Target: "rival", Unary: []harness.PhaseResult{phase("u", "rival", 101, 112, 122)},
				Saturation: []harness.PhaseResult{{Phase: "s", Throughput: 900, ErrorRate: 0}}},
		},
	}
}

func TestReportComputesAddedLatencyNotAbsolute(t *testing.T) {
	md := Markdown(bundle())
	// everstack p99 130 - control 120 = +10.00
	if !strings.Contains(md, "+10.00 ms") {
		t.Errorf("expected the added p99 of +10.00 ms in the table:\n%s", md)
	}
	if !strings.Contains(md, "added p99") {
		t.Error("the latency table must be labelled as added latency")
	}
}

func TestWhereWeLoseIsGeneratedNotCurated(t *testing.T) {
	// This is the credibility guarantee: shipping a flattering report has to
	// require deleting code, not just omitting a section.
	md := Markdown(bundle())

	if !strings.Contains(md, "## Where we lose") {
		t.Fatal("the report has no losses section")
	}
	if !strings.Contains(md, "Added p99 latency") {
		t.Error("the subject is slower on p99 than the rival, and the report did not say so")
	}
	if !strings.Contains(md, "Sustained throughput") {
		t.Error("the rival sustains more than twice the throughput, and the report did not say so")
	}
	if !strings.Contains(md, "rival") {
		t.Error("the losses section must name who is better")
	}
}

func TestLossesSectionFlagsAnImplausibleCleanSweep(t *testing.T) {
	// If nothing beats us anywhere, that is far more likely to be a rigged
	// setup than a real result, and the report should say so rather than
	// celebrate.
	b := bundle()
	b.Perf = b.Perf[:2] // subject and control only
	md := Markdown(b)
	if !strings.Contains(md, "suspicion") {
		t.Errorf("a clean sweep should be flagged as suspect, not presented as a win:\n%s", md)
	}
}

func TestUnmeasuredTargetsAreNamedNotDropped(t *testing.T) {
	b := bundle()
	b.Unmeasured = []Unmeasured{{Target: "bifrost", Reason: "preflight failed: connection refused"}}
	md := Markdown(b)
	if !strings.Contains(md, "could not be measured") || !strings.Contains(md, "bifrost") {
		t.Error("a target that failed preflight must appear in the report, not vanish from it")
	}
}

func TestCorrectnessLossesSurfaceToo(t *testing.T) {
	b := bundle()
	b.Checks = []harness.Check{
		{ID: "C7", Name: "Response cache hits", Target: "everstack", Status: harness.Fail, Detail: "no hit"},
		{ID: "C7", Name: "Response cache hits", Target: "rival", Status: harness.Pass},
	}
	md := Markdown(b)
	if !strings.Contains(md, "C7. Response cache hits") {
		t.Error("a scenario a competitor passes and we fail must appear in the losses table")
	}
	if !strings.Contains(md, "Every failure, with its evidence") {
		t.Error("failures must be listed with their reasoning, not just a red cell")
	}
}

func TestMethodologyStatesTheLoadModel(t *testing.T) {
	md := Markdown(bundle())
	for _, want := range []string{"open-loop", "added latency", "deterministic mock upstream", "Cloud-only"} {
		if !strings.Contains(md, want) {
			t.Errorf("methodology section is missing %q", want)
		}
	}
}

func TestNoControlProducesAnHonestRefusal(t *testing.T) {
	b := bundle()
	b.ControlName = "missing"
	md := Markdown(b)
	if !strings.Contains(md, "cannot be computed") {
		t.Error("without a control the report must refuse to publish latency figures, not print absolutes")
	}
}

func TestLossesSectionSaysSoWhenTheSubjectDroppedOut(t *testing.T) {
	// If the subject itself failed to run, the section must say that rather
	// than disappear. A missing "where we lose" reads as "we lost nowhere".
	b := bundle()
	b.Subject = "everstack"
	b.Perf = []*harness.PerfReport{
		{Target: "direct", Unary: []harness.PhaseResult{phase("u", "direct", 100, 110, 120)}},
		{Target: "rival", Unary: []harness.PhaseResult{phase("u", "rival", 101, 112, 122)}},
	}
	b.Unmeasured = []Unmeasured{{Target: "everstack", Reason: "preflight failed"}}

	md := Markdown(b)
	if !strings.Contains(md, "## Where we lose") {
		t.Fatal("the losses section vanished when the subject was unmeasured")
	}
	if !strings.Contains(md, "Not computed") {
		t.Error("the section must explain why it is empty")
	}
}
