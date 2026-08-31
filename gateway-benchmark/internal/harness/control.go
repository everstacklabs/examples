package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/everstacklabs/examples/gateway-benchmark/internal/upstream"
)

// Control drives the mock upstream's scenario API. Correctness scenarios use it
// to make the upstream misbehave on cue and then read back exactly what each
// gateway forwarded.
type Control struct {
	BaseURL string
	HTTP    *http.Client
}

func NewControl(baseURL string) *Control {
	return &Control{BaseURL: baseURL, HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// ProfileSpec mirrors the upstream's control wire format.
type ProfileSpec struct {
	TTFTMs           int     `json:"ttft_ms,omitempty"`
	ChunkDelayMs     int     `json:"chunk_delay_ms,omitempty"`
	Chunks           int     `json:"chunks,omitempty"`
	UnaryDelayMs     int     `json:"unary_delay_ms,omitempty"`
	Fault            string  `json:"fault,omitempty"`
	FaultRate        float64 `json:"fault_rate,omitempty"`
	Status           int     `json:"status,omitempty"`
	FailFirstN       int64   `json:"fail_first_n,omitempty"`
	ExtraLatencyMs   int     `json:"extra_latency_ms,omitempty"`
	AbortAfterChunks int     `json:"abort_after_chunks,omitempty"`
	RetryAfterSec    int     `json:"retry_after_seconds,omitempty"`
}

// SetProfile reconfigures one upstream profile and resets its counters.
func (c *Control) SetProfile(ctx context.Context, name string, spec ProfileSpec) error {
	body, _ := json.Marshal(spec)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/__control/profile/"+name, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("set profile %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("set profile %s: status %d", name, resp.StatusCode)
	}
	return nil
}

// Healthy restores a profile to the deterministic no-fault baseline.
func (c *Control) Healthy(ctx context.Context, name string) error {
	return c.SetProfile(ctx, name, ProfileSpec{})
}

// Reset clears the journal and every profile counter.
func (c *Control) Reset(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/__control/reset", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("reset upstream: %w", err)
	}
	defer resp.Body.Close()
	return nil
}

// Journal reads every request the upstream received.
func (c *Control) Journal(ctx context.Context) ([]upstream.Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/__control/journal", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Entries []upstream.Entry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

// WaitReady blocks until the upstream answers its health endpoint.
func (c *Control) WaitReady(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/__control/health", nil)
		resp, err := c.HTTP.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
			last = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("upstream not ready after %s: %w", d, last)
}
