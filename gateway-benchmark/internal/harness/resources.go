package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ResourceSample is one observation of a container's cost.
type ResourceSample struct {
	At       time.Time `json:"at"`
	CPUPct   float64   `json:"cpu_pct"`
	MemBytes float64   `json:"mem_bytes"`
}

// ResourceStats summarises what a gateway cost while it served the load. This
// is the dimension that turns "a few milliseconds of overhead" into a number a
// buyer can put in a cloud bill.
type ResourceStats struct {
	Container  string  `json:"container"`
	Samples    int     `json:"samples"`
	MeanCPUPct float64 `json:"mean_cpu_pct"`
	PeakCPUPct float64 `json:"peak_cpu_pct"`
	MeanMemMB  float64 `json:"mean_mem_mb"`
	PeakMemMB  float64 `json:"peak_mem_mb"`
	// CPUSecondsPer10k normalises CPU cost by throughput so gateways that ran
	// at different request counts stay comparable.
	CPUSecondsPer10k float64 `json:"cpu_seconds_per_10k_requests"`
	Unavailable      string  `json:"unavailable,omitempty"`
}

// ResourceSampler polls `docker stats` for one container while a phase runs.
//
// `docker stats --no-stream` is used in a loop rather than the streaming form
// because the streaming form emits an initial zeroed sample and redraws with
// ANSI control codes, which is fragile to parse.
type ResourceSampler struct {
	Container string
	Interval  time.Duration

	mu      sync.Mutex
	samples []ResourceSample
	cancel  context.CancelFunc
	done    chan struct{}
	err     string
}

func NewResourceSampler(container string) *ResourceSampler {
	return &ResourceSampler{Container: container, Interval: 500 * time.Millisecond}
}

// Start begins sampling. A missing container or missing docker is recorded and
// reported as "unavailable" rather than failing the run: the latency numbers
// are still valid without it.
func (s *ResourceSampler) Start(ctx context.Context) {
	if s.Container == "" {
		s.err = "no container configured"
		return
	}
	if _, err := exec.LookPath("docker"); err != nil {
		s.err = "docker not on PATH"
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})

	go func() {
		defer close(s.done)
		ticker := time.NewTicker(s.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sample, err := s.poll(ctx)
				if err != nil {
					s.mu.Lock()
					if s.err == "" {
						s.err = err.Error()
					}
					s.mu.Unlock()
					continue
				}
				s.mu.Lock()
				s.samples = append(s.samples, sample)
				s.mu.Unlock()
			}
		}
	}()
}

func (s *ResourceSampler) poll(ctx context.Context) (ResourceSample, error) {
	cmd := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{json .}}", s.Container)
	out, err := cmd.Output()
	if err != nil {
		return ResourceSample{}, err
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		var row struct {
			CPUPerc  string `json:"CPUPerc"`
			MemUsage string `json:"MemUsage"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			continue
		}
		return ResourceSample{
			At:       time.Now(),
			CPUPct:   parsePercent(row.CPUPerc),
			MemBytes: parseMemUsage(row.MemUsage),
		}, nil
	}
	return ResourceSample{}, nil
}

// Stop halts sampling and returns the summary, normalised by request count.
func (s *ResourceSampler) Stop(requests int) ResourceStats {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := ResourceStats{Container: s.Container, Samples: len(s.samples), Unavailable: s.err}
	if len(s.samples) == 0 {
		if stats.Unavailable == "" {
			stats.Unavailable = "no samples collected"
		}
		return stats
	}

	var cpuSum, memSum float64
	for _, sm := range s.samples {
		cpuSum += sm.CPUPct
		memSum += sm.MemBytes
		if sm.CPUPct > stats.PeakCPUPct {
			stats.PeakCPUPct = sm.CPUPct
		}
		if sm.MemBytes/1e6 > stats.PeakMemMB {
			stats.PeakMemMB = sm.MemBytes / 1e6
		}
	}
	n := float64(len(s.samples))
	stats.MeanCPUPct = cpuSum / n
	stats.MeanMemMB = memSum / n / 1e6

	// docker reports CPU as a percentage of one core, so mean% x elapsed
	// seconds / 100 is CPU-seconds consumed.
	elapsed := s.samples[len(s.samples)-1].At.Sub(s.samples[0].At).Seconds()
	if elapsed > 0 && requests > 0 {
		cpuSeconds := stats.MeanCPUPct / 100 * elapsed
		stats.CPUSecondsPer10k = cpuSeconds / float64(requests) * 10000
	}
	stats.Unavailable = ""
	return stats
}

func parsePercent(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	if err != nil {
		return 0
	}
	return v
}

// parseMemUsage turns docker's "123.4MiB / 2GiB" into bytes.
func parseMemUsage(s string) float64 {
	parts := strings.Split(s, "/")
	if len(parts) == 0 {
		return 0
	}
	return parseSize(strings.TrimSpace(parts[0]))
}

func parseSize(s string) float64 {
	units := []struct {
		suffix string
		mult   float64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 64)
			if err != nil {
				return 0
			}
			return v * u.mult
		}
	}
	return 0
}

// ContainerImage reports the pinned image a container is running, so the report
// records exactly which version of each competitor was measured.
func ContainerImage(ctx context.Context, container string) string {
	if container == "" {
		return ""
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return ""
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Config.Image}}", container).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
