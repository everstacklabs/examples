// Package matrix models the capability comparison that sits alongside the
// performance numbers.
//
// The matrix is deliberately separate from the latency tables. Blending a
// feature win into a performance chart is the fastest way to lose a technical
// reader, so the report keeps them apart and labels the provenance of every
// cell (see METHODOLOGY.md section 6).
package matrix

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// Score is a cell value.
type Score string

const (
	Yes     Score = "yes"
	Partial Score = "partial"
	No      Score = "no"
	// Paid means the capability exists but only above a commercial tier. For a
	// buyer comparing an OSS proxy against a platform this is the distinction
	// that actually decides the deal, so it is not collapsed into "yes".
	Paid    Score = "paid"
	Unknown Score = "unknown"
)

// Weight is how much a score contributes when a group is rolled up. Unknown
// scores zero rather than counting against a vendor: absence of evidence is not
// evidence of absence, and pretending otherwise is how a matrix gets rigged.
func (s Score) Weight() float64 {
	switch s {
	case Yes:
		return 1
	case Paid:
		return 0.6
	case Partial:
		return 0.5
	default:
		return 0
	}
}

// Counts reports whether a score participates in the denominator.
func (s Score) Counts() bool { return s != Unknown }

// Source records where a cell's claim came from. A report that cannot say how
// it knows something is marketing, not a benchmark.
type Source string

const (
	// FromProbe means this harness measured it in a run.
	FromProbe Source = "probe"
	// FromRepo means it was verified in Everstack's own source.
	FromRepo Source = "repo"
	// FromDocs means it is stated in the vendor's public documentation.
	FromDocs Source = "docs"
	// FromUnknown means nobody has checked yet.
	FromUnknown Source = "unknown"
)

// Cell is one vendor's standing on one dimension.
type Cell struct {
	Score    Score  `yaml:"score" json:"score"`
	Note     string `yaml:"note" json:"note,omitempty"`
	Evidence string `yaml:"evidence" json:"evidence,omitempty"`
	Source   Source `yaml:"source" json:"source,omitempty"`
	Verified string `yaml:"verified" json:"verified,omitempty"`
}

// Dimension is one comparable capability.
type Dimension struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
	// Why states the buyer consequence. A dimension nobody can explain the
	// consequence of does not belong in the matrix.
	Why    string          `yaml:"why" json:"why"`
	Weight float64         `yaml:"weight" json:"weight"`
	Cells  map[string]Cell `yaml:"cells" json:"cells"`
}

// Group bundles related dimensions.
type Group struct {
	ID         string      `yaml:"id" json:"id"`
	Name       string      `yaml:"name" json:"name"`
	Summary    string      `yaml:"summary" json:"summary"`
	Dimensions []Dimension `yaml:"dimensions" json:"dimensions"`
}

// Vendor is one column.
type Vendor struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name" json:"name"`
	// Tier is "self-hosted", "cloud", or "both". Cloud-only vendors appear in
	// the matrix but never in the latency tables.
	Tier    string `yaml:"tier" json:"tier"`
	License string `yaml:"license" json:"license"`
	Docs    string `yaml:"docs" json:"docs"`
	// Benchmarked marks vendors that also appear in the performance results.
	Benchmarked bool `yaml:"benchmarked" json:"benchmarked"`
}

// Matrix is the whole comparison.
type Matrix struct {
	Version    int      `yaml:"version" json:"version"`
	VerifiedOn string   `yaml:"verified_on" json:"verified_on"`
	Subject    string   `yaml:"subject" json:"subject"`
	Vendors    []Vendor `yaml:"vendors" json:"vendors"`
	Groups     []Group  `yaml:"groups" json:"groups"`
}

// Load reads and validates a matrix file.
func Load(path string) (*Matrix, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read matrix: %w", err)
	}
	var m Matrix
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse matrix: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}

// Validate catches the failure modes that would make the matrix dishonest: a
// cell claiming a capability with no evidence, or a vendor column silently
// missing from a dimension.
func (m *Matrix) Validate() error {
	if len(m.Vendors) == 0 {
		return fmt.Errorf("matrix has no vendors")
	}
	known := map[string]bool{}
	for _, v := range m.Vendors {
		known[v.ID] = true
	}
	var problems []string
	for _, g := range m.Groups {
		for _, d := range g.Dimensions {
			if d.Why == "" {
				problems = append(problems, fmt.Sprintf("%s/%s: no `why` (buyer consequence)", g.ID, d.ID))
			}
			for vid, c := range d.Cells {
				if !known[vid] {
					problems = append(problems, fmt.Sprintf("%s/%s: unknown vendor %q", g.ID, d.ID, vid))
				}
				if c.Score != Unknown && c.Source == FromUnknown {
					problems = append(problems, fmt.Sprintf("%s/%s/%s: scored %q with source: unknown", g.ID, d.ID, vid, c.Score))
				}
				if c.Score != Unknown && c.Evidence == "" && c.Source != FromProbe {
					problems = append(problems, fmt.Sprintf("%s/%s/%s: scored %q with no evidence link", g.ID, d.ID, vid, c.Score))
				}
			}
			for vid := range known {
				if _, ok := d.Cells[vid]; !ok {
					problems = append(problems, fmt.Sprintf("%s/%s: missing cell for %q", g.ID, d.ID, vid))
				}
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("matrix validation failed:\n  - %s", joinLines(problems))
	}
	return nil
}

// GroupScore is a vendor's rolled-up standing in one group.
type GroupScore struct {
	Vendor    string  `json:"vendor"`
	Group     string  `json:"group"`
	Earned    float64 `json:"earned"`
	Possible  float64 `json:"possible"`
	Pct       float64 `json:"pct"`
	Unknowns  int     `json:"unknown_cells"`
	Evaluated int     `json:"evaluated_cells"`
}

// Score rolls a group up for one vendor. Unknown cells are excluded from both
// numerator and denominator and reported separately, so a vendor we have
// researched less does not look worse than one we researched thoroughly.
func (m *Matrix) Score(vendorID string) []GroupScore {
	var out []GroupScore
	for _, g := range m.Groups {
		gs := GroupScore{Vendor: vendorID, Group: g.ID}
		for _, d := range g.Dimensions {
			w := d.Weight
			if w == 0 {
				w = 1
			}
			c, ok := d.Cells[vendorID]
			if !ok || c.Score == Unknown {
				gs.Unknowns++
				continue
			}
			gs.Evaluated++
			gs.Earned += c.Score.Weight() * w
			gs.Possible += w
		}
		if gs.Possible > 0 {
			gs.Pct = gs.Earned / gs.Possible * 100
		}
		out = append(out, gs)
	}
	return out
}

// Coverage reports how much of the matrix has been researched per vendor. It is
// printed in the report so a reader can see where our homework is thin.
func (m *Matrix) Coverage() map[string]float64 {
	total := 0
	for _, g := range m.Groups {
		total += len(g.Dimensions)
	}
	out := map[string]float64{}
	if total == 0 {
		return out
	}
	for _, v := range m.Vendors {
		known := 0
		for _, g := range m.Groups {
			for _, d := range g.Dimensions {
				if c, ok := d.Cells[v.ID]; ok && c.Score != Unknown {
					known++
				}
			}
		}
		out[v.ID] = float64(known) / float64(total) * 100
	}
	return out
}

// Benchmarked returns the vendors that also appear in the latency results.
func (m *Matrix) Benchmarked() []Vendor {
	var out []Vendor
	for _, v := range m.Vendors {
		if v.Benchmarked {
			out = append(out, v)
		}
	}
	return out
}

func joinLines(xs []string) string {
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += "\n  - "
		}
		s += x
	}
	return s
}
