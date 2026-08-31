package upstream

import (
	"sync"
	"sync/atomic"
	"time"
)

// FaultMode describes how a profile misbehaves. Everything the failover, retry,
// timeout, and rate-limit scenarios need is expressible as one of these.
type FaultMode string

const (
	FaultNone FaultMode = "none"
	// FaultStatus returns Status for a fraction of requests.
	FaultStatus FaultMode = "status"
	// FaultFailFirstN returns Status for the first N requests then recovers.
	// This is what makes time-to-recovery measurable rather than inferred.
	FaultFailFirstN FaultMode = "fail_first_n"
	// FaultHang accepts the request and never responds, so we can prove whether
	// a gateway enforces its own timeout or holds the client connection open.
	FaultHang FaultMode = "hang"
	// FaultSlow adds ExtraLatency to every response.
	FaultSlow FaultMode = "slow"
	// FaultStreamAbort closes the connection mid-stream after AbortAfterChunks.
	FaultStreamAbort FaultMode = "stream_abort"
	// FaultRateLimit returns 429 with a Retry-After header.
	FaultRateLimit FaultMode = "rate_limit"
)

// Profile is one addressable upstream. A benchmark run typically has a
// "primary" and a "secondary" so failover has somewhere to fail over to.
type Profile struct {
	Name string `json:"name"`

	// Timing knobs. These are what make the upstream deterministic, and every
	// latency number in the report is an added-latency delta against a control
	// run through this same configuration.
	TTFT       time.Duration `json:"ttft"`
	ChunkDelay time.Duration `json:"chunk_delay"`
	Chunks     int           `json:"chunks"`
	UnaryDelay time.Duration `json:"unary_delay"`

	// Fault injection.
	Fault            FaultMode     `json:"fault"`
	FaultRate        float64       `json:"fault_rate"`
	Status           int           `json:"status"`
	FailFirstN       int64         `json:"fail_first_n"`
	ExtraLatency     time.Duration `json:"extra_latency"`
	AbortAfterChunks int           `json:"abort_after_chunks"`
	RetryAfterSec    int           `json:"retry_after_seconds"`

	// counters are runtime state, not config.
	seen   atomic.Int64
	failed atomic.Int64
}

// DefaultProfile is the healthy baseline: a 120 ms first token then a token
// every 12 ms, which is roughly a fast hosted model and slow enough that a
// gateway that buffers the stream is visible in the cadence metric.
func DefaultProfile(name string) *Profile {
	return &Profile{
		Name:       name,
		TTFT:       120 * time.Millisecond,
		ChunkDelay: 12 * time.Millisecond,
		Chunks:     64,
		UnaryDelay: 300 * time.Millisecond,
		Fault:      FaultNone,
		Status:     503,
	}
}

// Seen returns how many requests this profile has received.
func (p *Profile) Seen() int64 { return p.seen.Load() }

// Failed returns how many requests this profile deliberately failed.
func (p *Profile) Failed() int64 { return p.failed.Load() }

// nextFault decides what to do with an incoming request and advances counters.
// It is called exactly once per request so that fail_first_n is exact rather
// than probabilistic.
func (p *Profile) nextFault() (FaultMode, int) {
	n := p.seen.Add(1)

	switch p.Fault {
	case FaultNone:
		return FaultNone, 0
	case FaultFailFirstN:
		if n <= p.FailFirstN {
			p.failed.Add(1)
			return FaultStatus, p.statusOr(503)
		}
		return FaultNone, 0
	case FaultStatus, FaultRateLimit, FaultHang, FaultStreamAbort, FaultSlow:
		if p.FaultRate <= 0 || p.shouldFaultAt(n) {
			if p.Fault != FaultSlow {
				p.failed.Add(1)
			}
			return p.Fault, p.statusOr(503)
		}
		return FaultNone, 0
	}
	return FaultNone, 0
}

func (p *Profile) statusOr(def int) int {
	if p.Status > 0 {
		return p.Status
	}
	return def
}

// shouldFaultAt is a deterministic stride rather than a PRNG: at rate 0.2
// exactly one request in five faults, and over 100 requests exactly 20 do.
// Determinism means a scenario asserts an exact count instead of a statistical
// tolerance, and a tolerance is exactly where an off-by-N retry bug hides.
//
// The arithmetic is integer parts-per-million on purpose. Accumulating the rate
// in floating point drifts: 0.2 is not representable, so a fixed-point
// accumulator yields 19 faults per 100 rather than 20, and the scenario's exact
// assertion fails for a reason that has nothing to do with the gateway.
func (p *Profile) shouldFaultAt(n int64) bool {
	if p.FaultRate >= 1 {
		return true
	}
	if p.FaultRate <= 0 || n < 1 {
		return false
	}
	const million = 1_000_000
	ppm := int64(p.FaultRate*million + 0.5)
	// Fire on the request that carries the running total past a whole unit.
	return (n*ppm)/million > ((n-1)*ppm)/million
}

// Registry holds the live profiles and lets the control API mutate them
// between scenario phases without restarting the upstream.
type Registry struct {
	mu       sync.RWMutex
	profiles map[string]*Profile
}

func NewRegistry() *Registry {
	r := &Registry{profiles: map[string]*Profile{}}
	r.profiles["primary"] = DefaultProfile("primary")
	r.profiles["secondary"] = DefaultProfile("secondary")
	return r
}

// Get returns a profile, creating a healthy one on first reference so a gateway
// config can point at any name without pre-registration.
func (r *Registry) Get(name string) *Profile {
	if name == "" {
		name = "primary"
	}
	r.mu.RLock()
	p, ok := r.profiles[name]
	r.mu.RUnlock()
	if ok {
		return p
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.profiles[name]; ok {
		return p
	}
	p = DefaultProfile(name)
	r.profiles[name] = p
	return p
}

// Set replaces a profile's configuration, preserving nothing: counters reset so
// fail_first_n restarts cleanly for the next scenario phase.
func (r *Registry) Set(name string, p *Profile) {
	p.Name = name
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profiles[name] = p
}

// Names lists registered profiles.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.profiles))
	for k := range r.profiles {
		out = append(out, k)
	}
	return out
}

// ResetCounters zeroes every profile's counters without changing config.
func (r *Registry) ResetCounters() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.profiles {
		p.seen.Store(0)
		p.failed.Store(0)
	}
}
