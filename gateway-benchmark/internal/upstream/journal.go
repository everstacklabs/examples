package upstream

import (
	"sync"
	"time"
)

// Entry is one request as the upstream actually received it. The journal is the
// ground truth for every correctness scenario: retry amplification is counted
// here, parameter fidelity is checked here, and token accounting is compared
// against the deterministic usage the upstream reported.
type Entry struct {
	Seq         int               `json:"seq"`
	At          time.Time         `json:"at"`
	Profile     string            `json:"profile"`
	Path        string            `json:"path"`
	Model       string            `json:"model"`
	Stream      bool              `json:"stream"`
	ClientTag   string            `json:"client_tag"`
	AuthPresent bool              `json:"auth_present"`
	AuthFP      string            `json:"auth_fingerprint"`
	BodySHA     string            `json:"body_sha256"`
	Params      map[string]any    `json:"params"`
	Headers     map[string]string `json:"headers"`
	Outcome     string            `json:"outcome"`
	Status      int               `json:"status"`
	PromptToks  int               `json:"prompt_tokens"`
	OutputToks  int               `json:"output_tokens"`
}

// Journal is an append-only, concurrency-safe record of upstream requests.
type Journal struct {
	mu      sync.Mutex
	entries []Entry
	seq     int
}

func NewJournal() *Journal { return &Journal{} }

// Append records an entry and returns its sequence number.
func (j *Journal) Append(e Entry) int {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	e.Seq = j.seq
	j.entries = append(j.entries, e)
	return e.Seq
}

// Snapshot returns a copy of every entry recorded so far.
func (j *Journal) Snapshot() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Entry, len(j.entries))
	copy(out, j.entries)
	return out
}

// Since returns entries recorded at or after t, which is how a scenario scopes
// the journal to its own window without resetting shared state.
func (j *Journal) Since(t time.Time) []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []Entry
	for _, e := range j.entries {
		if !e.At.Before(t) {
			out = append(out, e)
		}
	}
	return out
}

// Reset clears the journal. Scenarios call this between phases.
func (j *Journal) Reset() {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.entries = nil
	j.seq = 0
}

// Count returns the number of recorded entries.
func (j *Journal) Count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.entries)
}
