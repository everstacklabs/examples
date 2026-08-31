package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSuite(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimal = `
upstream:
  base_url: http://localhost:9800
targets:
  - name: direct
    kind: control
    chat_url: http://localhost:9800/v1/chat/completions
    model: bench-model
  - name: gw
    chat_url: http://localhost:8080/openai/v1/chat/completions
    model: bench-model
`

func TestSuiteRequiresExactlyOneControl(t *testing.T) {
	// Without a control there is nothing to express added latency against, and
	// absolute latency through a mock upstream is meaningless.
	body := strings.Replace(minimal, "    kind: control\n", "", 1)
	if _, err := LoadSuite(writeSuite(t, body)); err == nil {
		t.Fatal("a suite with no control target was accepted")
	}

	two := minimal + `
  - name: direct2
    kind: control
    chat_url: http://localhost:9800/v1/chat/completions
    model: bench-model
`
	if _, err := LoadSuite(writeSuite(t, two)); err == nil {
		t.Fatal("a suite with two control targets was accepted")
	}
}

func TestDuplicateTargetNamesRejected(t *testing.T) {
	body := minimal + `
  - name: gw
    chat_url: http://localhost:9090/v1/chat/completions
    model: bench-model
`
	if _, err := LoadSuite(writeSuite(t, body)); err == nil {
		t.Fatal("duplicate target names were accepted; the report would silently merge them")
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	s, err := LoadSuite(writeSuite(t, minimal))
	if err != nil {
		t.Fatal(err)
	}
	if s.Load.Runs < 1 {
		t.Error("runs must default above zero, or no phase is measured")
	}
	if len(s.Load.ConcurrencySteps) == 0 || len(s.Load.SaturationSteps) == 0 {
		t.Error("sweeps must have default steps")
	}
	if s.Upstream.ControlURL == "" {
		t.Error("control_url should fall back to base_url")
	}
}

func TestAPIKeyComesFromEnvNotTheSuiteFile(t *testing.T) {
	// Suite files get forked and pasted into issues, so they must never be
	// able to hold a credential in the first place.
	t.Setenv("BENCH_TEST_KEY", "sk-from-env")
	tgt := Target{APIKeyEnv: "BENCH_TEST_KEY"}
	if got := tgt.APIKey(); got != "sk-from-env" {
		t.Errorf("APIKey() = %q, want the env value", got)
	}

	unset := Target{APIKeyEnv: "BENCH_TEST_KEY_DEFINITELY_UNSET"}
	if got := unset.APIKey(); got == "" {
		t.Error("an unset env var should fall back to a placeholder, not an empty credential")
	}
}

func TestSupportsScenarioDefaultsToEnabled(t *testing.T) {
	tgt := Target{Supports: map[string]bool{"C6": false}}
	if tgt.SupportsScenario("C6") {
		t.Error("an explicitly disabled scenario should be reported as unsupported")
	}
	if !tgt.SupportsScenario("C1") {
		t.Error("an unlisted scenario should default to enabled")
	}
	if !(Target{}).SupportsScenario("C1") {
		t.Error("a target with no supports map should run every scenario")
	}
}

func TestActiveExcludesSkippedTargets(t *testing.T) {
	body := minimal + `
  - name: broken
    chat_url: http://localhost:9999/v1/chat/completions
    model: bench-model
    skip: true
    skip_reason: "config not validated"
`
	s, err := LoadSuite(writeSuite(t, body))
	if err != nil {
		t.Fatal(err)
	}
	for _, tgt := range s.Active() {
		if tgt.Name == "broken" {
			t.Fatal("a skipped target was returned as active")
		}
	}
	if len(s.Targets) != 3 {
		t.Errorf("skipped targets must stay in the suite so the report can name them, got %d", len(s.Targets))
	}
}

func TestShippedSuiteIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "targets.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("shipped suite not present: %v", err)
	}
	s, err := LoadSuite(path)
	if err != nil {
		t.Fatalf("the shipped targets.yaml does not validate: %v", err)
	}
	if s.Control() == nil {
		t.Fatal("shipped suite has no control target")
	}
	// Everstack must be benchmarked on the OpenAI-compatible surface. The /v1
	// path accepts OpenAI-format requests but returns proto-JSON that an SSE
	// client cannot read (internal/api/api.go:571).
	for _, tgt := range s.Targets {
		if tgt.Name == "everstack" && !strings.Contains(tgt.ChatURL, "/openai/v1") {
			t.Errorf("everstack target points at %q, which is not the OpenAI-compatible surface", tgt.ChatURL)
		}
	}
}
