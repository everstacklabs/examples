package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// Probe is a single-request client used by the correctness scenarios, where we
// care about one response in detail rather than a distribution.
type Probe struct {
	Target Target
	HTTP   *http.Client
}

func NewProbe(t Target, timeout time.Duration) *Probe {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Probe{Target: t, HTTP: &http.Client{Timeout: timeout}}
}

// ProbeResult is one observed response.
type ProbeResult struct {
	Status    int               `json:"status"`
	Latency   time.Duration     `json:"latency"`
	TTFT      time.Duration     `json:"ttft"`
	Err       string            `json:"error,omitempty"`
	Body      string            `json:"body,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Chunks    int               `json:"chunks"`
	Truncated bool              `json:"truncated"`
	Usage     Usage             `json:"usage"`
	CacheHint string            `json:"cache_hint,omitempty"`
}

// Usage is the token accounting the gateway reported back to the client.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Do sends one request. apiKey overrides the target's default credential, which
// is how the tenant-isolation scenario acts as a second tenant.
func (p *Probe) Do(ctx context.Context, body []byte, stream bool, apiKey string, extra map[string]string) ProbeResult {
	res := ProbeResult{Headers: map[string]string{}}
	start := time.Now()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Target.ChatURL, bytes.NewReader(body))
	if err != nil {
		res.Err = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey == "" {
		apiKey = p.Target.APIKey()
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range p.Target.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := p.HTTP.Do(req)
	if err != nil {
		res.Err = err.Error()
		res.Latency = time.Since(start)
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode

	// Capture the headers gateways use to advertise cache and retry behaviour.
	for _, h := range []string{
		"Retry-After", "X-Cache", "X-Cache-Status", "Cf-Aig-Cache-Status",
		"X-Portkey-Cache-Status", "X-Everstack-Cache", "X-Litellm-Cache-Hit",
		"X-Helicone-Cache", "X-Ratelimit-Remaining-Requests", "X-Request-Id",
	} {
		if v := resp.Header.Get(h); v != "" {
			res.Headers[h] = v
			if strings.Contains(strings.ToLower(h), "cache") {
				res.CacheHint = h + "=" + v
			}
		}
	}

	if !stream {
		b, _ := io.ReadAll(resp.Body)
		res.Latency = time.Since(start)
		res.TTFT = res.Latency
		res.Body = truncate(string(b), 2000)
		var parsed struct {
			Usage Usage `json:"usage"`
		}
		if json.Unmarshal(b, &parsed) == nil {
			res.Usage = parsed.Usage
		}
		return res
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	sawDone := false
	var sb strings.Builder
	for sc.Scan() {
		line := sc.Text()
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
			Usage *Usage `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		if frame.Usage != nil {
			res.Usage = *frame.Usage
		}
		if len(frame.Choices) > 0 && frame.Choices[0].Delta.Content != "" {
			if res.Chunks == 0 {
				res.TTFT = time.Since(start)
			}
			res.Chunks++
			if sb.Len() < 500 {
				sb.WriteString(frame.Choices[0].Delta.Content)
			}
		}
	}
	if err := sc.Err(); err != nil {
		res.Err = err.Error()
	}
	res.Truncated = !sawDone
	res.Latency = time.Since(start)
	res.Body = truncate(sb.String(), 500)
	return res
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
