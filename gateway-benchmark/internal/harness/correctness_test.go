package harness

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/everstacklabs/examples/gateway-benchmark/internal/upstream"
)

// A bare passthrough is the useful control for the scenario suite: it is the
// mock upstream itself, with no gateway in the path. Every verdict below is
// therefore the verdict a gateway that does nothing should receive. If the
// suite cannot tell "does nothing" apart from "does the right thing", none of
// its findings about a real gateway mean anything.
func runAgainstBarePassthrough(t *testing.T) map[string]Check {
	t.Helper()

	up := upstream.New()
	ts := httptest.NewServer(up.Handler())
	t.Cleanup(ts.Close)

	suite := &Suite{
		Upstream: UpstreamRef{BaseURL: ts.URL, ControlURL: ts.URL},
		Load:     LoadDefaults{TimeoutSeconds: 3},
	}
	target := Target{
		Name:    "passthrough",
		ChatURL: ts.URL + "/p/primary/v1/chat/completions",
		Model:   "bench-model",
	}
	ctrl := NewControl(ts.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	out := map[string]Check{}
	for _, c := range RunCorrectness(ctx, suite, target, ctrl, func(string, ...any) {}) {
		out[c.ID] = c
	}
	return out
}

func TestScenariosDetectABarePassthrough(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test drives real HTTP timing")
	}
	checks := runAgainstBarePassthrough(t)

	cases := []struct {
		id     string
		want   CheckStatus
		reason string
	}{
		{"C1", NotConfigured, "no fallback_model is set, so failover cannot be claimed either way"},
		{"C3", Pass, "a passthrough makes exactly one upstream call per client call"},
		{"C7", NotConfigured, "a passthrough has no cache, so the second identical request must reach the upstream"},
		{"C8", Pass, "a passthrough forwards the body byte for byte"},
		{"C9", Pass, "a passthrough returns the upstream's own usage block"},
		{"C10", NotConfigured, "no second credential is configured, so a second tenant cannot be simulated"},
		{"C11", Fail, "a passthrough has no timeout of its own, so the client hits its own deadline"},
		{"C12", MatrixOnly, "observability depth is matrix-scored, not probed over HTTP"},
	}

	for _, tc := range cases {
		got, ok := checks[tc.id]
		if !ok {
			t.Errorf("%s: no result produced", tc.id)
			continue
		}
		if got.Status != tc.want {
			t.Errorf("%s (%s): got %q, want %q\n  reason: %s\n  detail: %s",
				tc.id, got.Name, got.Status, tc.want, tc.reason, got.Detail)
		}
	}
}

func TestRetryAmplificationCountsUpstreamCalls(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	checks := runAgainstBarePassthrough(t)
	c := checks["C3"]
	if c.Metric == nil {
		t.Fatal("C3 produced no amplification metric")
	}
	// A passthrough is exactly 1.0. Anything else means the journal is not
	// counting what the scenario claims it counts.
	if *c.Metric != 1.0 {
		t.Errorf("amplification = %v, want exactly 1.0 for a passthrough", *c.Metric)
	}
}

func TestTimeoutScenarioDistinguishesWhoGaveUp(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	// The distinction the scenario exists to make: a passthrough holds the
	// connection, so the client's own deadline fires and the verdict is Fail.
	// A gateway with its own timeout returns a 5xx first and passes.
	checks := runAgainstBarePassthrough(t)
	c := checks["C11"]
	if c.Status != Fail {
		t.Fatalf("C11 = %q, want fail: the passthrough has no timeout of its own", c.Status)
	}
	if c.Metric == nil || *c.Metric < 2500 {
		t.Errorf("C11 should record that the client waited out its full 3s deadline, got %v", c.Metric)
	}
}

func TestFailoverIsDetectedWhenConfigured(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	// With the primary failing and no gateway in the path, a target that
	// *claims* a fallback must be caught not delivering one. This proves C1
	// reports Fail rather than defaulting to a pass.
	up := upstream.New()
	ts := httptest.NewServer(up.Handler())
	defer ts.Close()

	ctrl := NewControl(ts.URL)
	target := Target{
		Name:          "claims-failover",
		ChatURL:       ts.URL + "/p/primary/v1/chat/completions",
		Model:         "bench-model",
		FallbackModel: "bench-model-fallback", // claimed, but nothing implements it
	}
	probe := NewProbe(target, 5*time.Second)

	got := checkFailover(context.Background(), target, ctrl, probe)
	if got.Status != Fail {
		t.Errorf("C1 = %q, want fail: the primary returned 503 and nothing failed over", got.Status)
	}
}

func TestCacheScenarioDetectsAHitWhenOneExists(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	// Prove the cache check is not vacuous: put a trivial caching proxy in
	// front of the upstream and confirm C7 flips from not_configured to pass.
	up := upstream.New()
	ts := httptest.NewServer(up.Handler())
	defer ts.Close()

	cache := httptest.NewServer(newCachingProxy(ts.URL + "/p/primary/v1/chat/completions"))
	defer cache.Close()

	ctrl := NewControl(ts.URL)
	target := Target{Name: "cached", ChatURL: cache.URL, Model: "bench-model"}
	probe := NewProbe(target, 10*time.Second)

	got := checkCache(context.Background(), target, ctrl, probe)
	if got.Status != Pass {
		t.Fatalf("C7 = %q (%s), want pass against a proxy that genuinely caches", got.Status, got.Detail)
	}
	if got.Metric == nil {
		t.Error("a cache hit should report its latency")
	}
}
