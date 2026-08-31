package matrix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "m.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const validMatrix = `
version: 1
verified_on: "2026-08-31"
subject: everstack
vendors:
  - {id: everstack, name: Everstack, benchmarked: true}
  - {id: rival, name: Rival}
groups:
  - id: g1
    name: Group One
    dimensions:
      - id: d1
        name: Dim One
        why: because it matters
        cells:
          everstack: {score: yes, source: repo, evidence: "internal/x"}
          rival: {score: no, source: docs, evidence: "https://example.com"}
`

func TestValidMatrixLoads(t *testing.T) {
	m, err := Load(write(t, validMatrix))
	if err != nil {
		t.Fatalf("valid matrix rejected: %v", err)
	}
	if len(m.Vendors) != 2 || len(m.Groups) != 1 {
		t.Errorf("unexpected shape: %d vendors, %d groups", len(m.Vendors), len(m.Groups))
	}
}

func TestScoredCellNeedsEvidence(t *testing.T) {
	// A claim with no citation is marketing. The loader must refuse it rather
	// than let it reach a published report.
	body := strings.Replace(validMatrix, `everstack: {score: yes, source: repo, evidence: "internal/x"}`,
		`everstack: {score: yes, source: repo}`, 1)
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("a scored cell with no evidence link was accepted")
	}
	if !strings.Contains(err.Error(), "no evidence link") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMissingVendorCellIsRejected(t *testing.T) {
	// A vendor silently absent from a dimension reads as "not applicable" in a
	// rendered table, which is not a thing the reader can verify.
	body := strings.Replace(validMatrix, `          rival: {score: no, source: docs, evidence: "https://example.com"}`, "", 1)
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("a dimension missing a vendor column was accepted")
	}
	if !strings.Contains(err.Error(), "missing cell") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDimensionNeedsBuyerConsequence(t *testing.T) {
	body := strings.Replace(validMatrix, "        why: because it matters\n", "", 1)
	_, err := Load(write(t, body))
	if err == nil || !strings.Contains(err.Error(), "buyer consequence") {
		t.Fatalf("a dimension with no `why` should be rejected, got %v", err)
	}
}

func TestUnknownExcludedFromScoringInBothDirections(t *testing.T) {
	// A vendor we researched less must not score worse for it. Otherwise the
	// matrix rewards whoever the author studied least.
	body := `
version: 1
verified_on: "2026-08-31"
vendors:
  - {id: a, name: A}
  - {id: b, name: B}
groups:
  - id: g
    name: G
    dimensions:
      - id: d1
        name: D1
        why: w
        cells:
          a: {score: yes, source: docs, evidence: "https://e"}
          b: {score: yes, source: docs, evidence: "https://e"}
      - id: d2
        name: D2
        why: w
        cells:
          a: {score: no, source: docs, evidence: "https://e"}
          b: {score: unknown, source: unknown}
`
	m, err := Load(write(t, body))
	if err != nil {
		t.Fatal(err)
	}
	a := m.Score("a")[0]
	b := m.Score("b")[0]

	if a.Pct != 50 {
		t.Errorf("A scored %v%%, want 50 (one yes, one no)", a.Pct)
	}
	if b.Pct != 100 {
		t.Errorf("B scored %v%%, want 100: the unknown cell must not count against it", b.Pct)
	}
	if b.Unknowns != 1 {
		t.Errorf("B unknowns = %d, want 1 reported separately", b.Unknowns)
	}

	// Coverage is what exposes the thin research, not the score.
	cov := m.Coverage()
	if cov["a"] != 100 || cov["b"] != 50 {
		t.Errorf("coverage a=%v b=%v, want 100 and 50", cov["a"], cov["b"])
	}
}

func TestPaidScoresBelowYes(t *testing.T) {
	// "Available above a commercial tier" is the distinction that decides deals
	// against an OSS proxy, so it must not collapse into a plain yes.
	if Paid.Weight() >= Yes.Weight() {
		t.Error("paid should weigh less than yes")
	}
	if Paid.Weight() <= No.Weight() {
		t.Error("paid should weigh more than no")
	}
	if Unknown.Counts() {
		t.Error("unknown must not participate in scoring")
	}
}

func TestShippedMatrixIsValid(t *testing.T) {
	// The matrix that actually ships must satisfy its own rules.
	path := filepath.Join("..", "..", "matrix", "capabilities.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("shipped matrix not present: %v", err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatalf("the shipped capability matrix does not validate: %v", err)
	}
	// Everstack's own column must not be the best-researched by a wide margin,
	// or the comparison is unbalanced by construction.
	cov := m.Coverage()
	if cov["everstack"] < 50 {
		t.Errorf("everstack coverage is only %.0f%%", cov["everstack"])
	}
}
