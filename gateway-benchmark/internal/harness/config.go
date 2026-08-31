// Package harness defines the gateways under test and the scenarios run
// against them. See METHODOLOGY.md.
package harness

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Suite is the whole benchmark definition, loaded from targets.yaml.
type Suite struct {
	// Upstream is the mock model endpoint every target proxies to.
	Upstream UpstreamRef  `yaml:"upstream"`
	Targets  []Target     `yaml:"targets"`
	Load     LoadDefaults `yaml:"load"`
	// Hardware is recorded verbatim in the report. A benchmark without the
	// machine it ran on is not reproducible.
	Hardware string `yaml:"hardware"`
	Notes    string `yaml:"notes"`
}

// UpstreamRef locates the mock upstream and its control API.
type UpstreamRef struct {
	BaseURL    string `yaml:"base_url"`
	ControlURL string `yaml:"control_url"`
}

// Target is one gateway (or the direct control) under test.
type Target struct {
	Name string `yaml:"name"`
	// Kind "control" marks the direct-to-upstream baseline. Every latency
	// number in the report is a delta against it.
	Kind string `yaml:"kind"`

	ChatURL      string `yaml:"chat_url"`
	ModelsURL    string `yaml:"models_url"`
	AnthropicURL string `yaml:"anthropic_url"`
	Model        string `yaml:"model"`
	// FallbackModel is the alias the gateway is configured to fail over to. Empty
	// means the target has no failover configured, and the failover scenario
	// records "not configured" rather than a failure.
	FallbackModel string `yaml:"fallback_model"`
	// SecondaryKeyEnv holds a second credential used by the tenant-isolation
	// scenario. Empty means that scenario is skipped for this target.
	SecondaryKeyEnv string            `yaml:"secondary_key_env"`
	APIKeyEnv       string            `yaml:"api_key_env"`
	Headers         map[string]string `yaml:"headers"`

	// Container is the docker container name, used for CPU and RSS sampling.
	Container  string `yaml:"container"`
	Version    string `yaml:"version"`
	Image      string `yaml:"image"`
	ConfigRef  string `yaml:"config_ref"`
	Skip       bool   `yaml:"skip"`
	SkipReason string `yaml:"skip_reason"`

	// Supports lets a target opt out of scenarios it genuinely cannot serve, so
	// the report distinguishes "does not have the feature" from "failed the test".
	Supports map[string]bool `yaml:"supports"`
}

// APIKey resolves the credential from the environment. Keys are never written
// into targets.yaml, which gets forked and pasted into issues.
func (t Target) APIKey() string {
	if t.APIKeyEnv == "" {
		return "bench-key"
	}
	if v := os.Getenv(t.APIKeyEnv); v != "" {
		return v
	}
	return "bench-key"
}

// SecondaryKey resolves the second tenant credential, if configured.
func (t Target) SecondaryKey() string {
	if t.SecondaryKeyEnv == "" {
		return ""
	}
	return os.Getenv(t.SecondaryKeyEnv)
}

// SupportsScenario reports whether a scenario should run for this target.
// Unlisted scenarios default to enabled.
func (t Target) SupportsScenario(id string) bool {
	if t.Supports == nil {
		return true
	}
	v, ok := t.Supports[id]
	if !ok {
		return true
	}
	return v
}

// LoadDefaults are the knobs the perf scenarios share.
type LoadDefaults struct {
	Runs             int       `yaml:"runs"`
	WarmupSeconds    int       `yaml:"warmup_seconds"`
	DurationSeconds  int       `yaml:"duration_seconds"`
	RPS              float64   `yaml:"rps"`
	Poisson          bool      `yaml:"poisson"`
	ConcurrencySteps []int     `yaml:"concurrency_steps"`
	SaturationSteps  []float64 `yaml:"saturation_rps_steps"`
	TimeoutSeconds   int       `yaml:"timeout_seconds"`
	PromptChars      int       `yaml:"prompt_chars"`
}

func (l LoadDefaults) Warmup() time.Duration   { return time.Duration(l.WarmupSeconds) * time.Second }
func (l LoadDefaults) Duration() time.Duration { return time.Duration(l.DurationSeconds) * time.Second }
func (l LoadDefaults) Timeout() time.Duration  { return time.Duration(l.TimeoutSeconds) * time.Second }

// LoadSuite reads and validates targets.yaml.
func LoadSuite(path string) (*Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read suite: %w", err)
	}
	var s Suite
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse suite: %w", err)
	}
	s.applyDefaults()
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Suite) applyDefaults() {
	if s.Load.Runs == 0 {
		s.Load.Runs = 3
	}
	if s.Load.WarmupSeconds == 0 {
		s.Load.WarmupSeconds = 5
	}
	if s.Load.DurationSeconds == 0 {
		s.Load.DurationSeconds = 20
	}
	if s.Load.RPS == 0 {
		s.Load.RPS = 50
	}
	if s.Load.TimeoutSeconds == 0 {
		s.Load.TimeoutSeconds = 30
	}
	if s.Load.PromptChars == 0 {
		s.Load.PromptChars = 400
	}
	if len(s.Load.ConcurrencySteps) == 0 {
		s.Load.ConcurrencySteps = []int{1, 10, 50, 200}
	}
	if len(s.Load.SaturationSteps) == 0 {
		s.Load.SaturationSteps = []float64{50, 100, 200, 400, 800}
	}
	if s.Upstream.ControlURL == "" {
		s.Upstream.ControlURL = s.Upstream.BaseURL
	}
}

func (s *Suite) validate() error {
	if s.Upstream.BaseURL == "" {
		return fmt.Errorf("upstream.base_url is required")
	}
	if len(s.Targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	controls := 0
	seen := map[string]bool{}
	for _, t := range s.Targets {
		if t.Name == "" {
			return fmt.Errorf("every target needs a name")
		}
		if seen[t.Name] {
			return fmt.Errorf("duplicate target name %q", t.Name)
		}
		seen[t.Name] = true
		if t.ChatURL == "" && !t.Skip {
			return fmt.Errorf("target %q needs chat_url", t.Name)
		}
		if t.Kind == "control" {
			controls++
		}
	}
	if controls != 1 {
		// Without exactly one control there is nothing to express added latency
		// against, and absolute latency through a mock upstream is meaningless.
		return fmt.Errorf("exactly one target must have kind: control, found %d", controls)
	}
	return nil
}

// Control returns the baseline target.
func (s *Suite) Control() *Target {
	for i := range s.Targets {
		if s.Targets[i].Kind == "control" {
			return &s.Targets[i]
		}
	}
	return nil
}

// Active returns the targets that are not skipped.
func (s *Suite) Active() []Target {
	var out []Target
	for _, t := range s.Targets {
		if !t.Skip {
			out = append(out, t)
		}
	}
	return out
}
