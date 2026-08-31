// Command gwbench runs the AI gateway benchmark: performance phases,
// correctness scenarios, and the capability matrix, into one report.
//
//	gwbench run      -suite targets.yaml -out results/
//	gwbench perf     -suite targets.yaml           # performance only
//	gwbench checks   -suite targets.yaml           # correctness only
//	gwbench matrix   -matrix matrix/capabilities.yaml
//	gwbench report   -in results/run.json -out results/report.md
//
// See METHODOLOGY.md for the methodology this implements.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/everstacklabs/examples/gateway-benchmark/internal/harness"
	"github.com/everstacklabs/examples/gateway-benchmark/internal/matrix"
	"github.com/everstacklabs/examples/gateway-benchmark/internal/report"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(ctx, os.Args[2:], true, true)
	case "perf":
		err = cmdRun(ctx, os.Args[2:], true, false)
	case "checks":
		err = cmdRun(ctx, os.Args[2:], false, true)
	case "validate":
		err = cmdValidate(ctx, os.Args[2:])
	case "matrix":
		err = cmdMatrix(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `gwbench - AI gateway benchmark

  run      performance + correctness + report
  perf     performance phases only
  checks   correctness scenarios only
  validate check every target answers correctly before a real run
  matrix   validate and render the capability matrix
  report   regenerate the report from a saved run

Run "gwbench <command> -h" for flags.
`)
}

func cmdRun(ctx context.Context, args []string, doPerf, doChecks bool) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	suitePath := fs.String("suite", "targets.yaml", "suite definition")
	matrixPath := fs.String("matrix", "matrix/capabilities.yaml", "capability matrix (empty to skip)")
	outDir := fs.String("out", "results", "output directory")
	only := fs.String("only", "", "comma-separated target names to run (default: all)")
	_ = fs.Parse(args)

	suite, err := harness.LoadSuite(*suitePath)
	if err != nil {
		return err
	}

	ctrl := harness.NewControl(suite.Upstream.ControlURL)
	logf := func(format string, a ...any) {
		fmt.Printf(format+"\n", a...)
	}

	logf("waiting for the mock upstream at %s", suite.Upstream.ControlURL)
	if err := ctrl.WaitReady(ctx, 20*time.Second); err != nil {
		return fmt.Errorf("%w\n\nStart it with:\n  go run ./cmd/mockupstream -addr :9800", err)
	}

	targets := filterTargets(suite.Active(), *only)
	if len(targets) == 0 {
		return fmt.Errorf("no targets selected")
	}

	bundle := &report.Bundle{
		GeneratedAt: time.Now(),
		Subject:     subjectOf(suite),
		Hardware:    suite.Hardware,
		Notes:       suite.Notes,
		Upstream:    suite.Upstream,
		Load:        suite.Load,
		ControlName: controlName(suite),
	}

	for _, t := range targets {
		logf("\n=== %s (%s) ===", t.Name, t.ChatURL)
		if err := preflight(ctx, t); err != nil {
			if t.Kind == "control" {
				// Without the control there is nothing to express added latency
				// against, so this one is fatal.
				return fmt.Errorf("the control target %q is not reachable: %w", t.Name, err)
			}
			// Any other target that cannot answer a single request is recorded
			// and skipped. Aborting the whole run because one competitor's
			// container is misconfigured would make the suite useless in
			// practice, and dropping it silently would be dishonest.
			logf("  SKIPPED: %v", err)
			bundle.Unmeasured = append(bundle.Unmeasured, report.Unmeasured{
				Target: t.Name, Reason: "preflight failed: " + err.Error(),
			})
			continue
		}

		if doPerf {
			p, err := harness.RunPerf(ctx, suite, t, ctrl, logf)
			if err != nil {
				return fmt.Errorf("perf %q: %w", t.Name, err)
			}
			bundle.Perf = append(bundle.Perf, p)
		}
		if doChecks && t.Kind != "control" {
			// The control is the harness talking to the mock directly. Asking
			// whether it has failover or a cache is meaningless.
			bundle.Checks = append(bundle.Checks, harness.RunCorrectness(ctx, suite, t, ctrl, logf)...)
		}
	}

	if *matrixPath != "" {
		m, err := matrix.Load(*matrixPath)
		if err != nil {
			return fmt.Errorf("matrix: %w", err)
		}
		bundle.Matrix = m
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	stamp := time.Now().Format("20060102-150405")

	jsonPath := filepath.Join(*outDir, "run-"+stamp+".json")
	raw, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(jsonPath, raw, 0o644); err != nil {
		return err
	}

	mdPath := filepath.Join(*outDir, "report-"+stamp+".md")
	if err := os.WriteFile(mdPath, []byte(report.Markdown(bundle)), 0o644); err != nil {
		return err
	}
	// A stable path so tooling and docs can link to "the latest report".
	_ = os.WriteFile(filepath.Join(*outDir, "latest.json"), raw, 0o644)
	_ = os.WriteFile(filepath.Join(*outDir, "latest.md"), []byte(report.Markdown(bundle)), 0o644)

	logf("\nwrote %s", jsonPath)
	logf("wrote %s", mdPath)
	return nil
}

// preflight sends one request and fails loudly rather than letting a
// misconfigured target quietly produce a column of zeroes.
func preflight(ctx context.Context, t harness.Target) error {
	probe := harness.NewProbe(t, 30*time.Second)
	r := probe.Do(ctx, harness.ChatBody(t.Model, "preflight", false), false, "", nil)
	if r.Err != "" {
		return fmt.Errorf("%s", r.Err)
	}
	if r.Status != 200 {
		return fmt.Errorf("status %d, body: %s", r.Status, r.Body)
	}
	return nil
}

func cmdMatrix(args []string) error {
	fs := flag.NewFlagSet("matrix", flag.ExitOnError)
	path := fs.String("matrix", "matrix/capabilities.yaml", "capability matrix")
	_ = fs.Parse(args)

	m, err := matrix.Load(*path)
	if err != nil {
		return err
	}
	fmt.Printf("matrix ok: %d vendors, %d groups\n\n", len(m.Vendors), len(m.Groups))

	cov := m.Coverage()
	fmt.Println("coverage (how much of the matrix is actually researched):")
	for _, v := range m.Vendors {
		fmt.Printf("  %-14s %5.0f%%\n", v.ID, cov[v.ID])
	}
	fmt.Println("\ngroup scores (unknown cells excluded from both numerator and denominator):")
	for _, v := range m.Vendors {
		fmt.Printf("  %s\n", v.Name)
		for _, gs := range m.Score(v.ID) {
			fmt.Printf("    %-24s %5.0f%%  (%d scored, %d unknown)\n", gs.Group, gs.Pct, gs.Evaluated, gs.Unknowns)
		}
	}
	return nil
}

func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", "results/latest.json", "saved run bundle")
	out := fs.String("out", "results/latest.md", "markdown output")
	_ = fs.Parse(args)

	raw, err := os.ReadFile(*in)
	if err != nil {
		return err
	}
	var b report.Bundle
	if err := json.Unmarshal(raw, &b); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(report.Markdown(&b)), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *out)
	return nil
}

func filterTargets(all []harness.Target, only string) []harness.Target {
	if only == "" {
		return all
	}
	want := map[string]bool{}
	for _, n := range splitComma(only) {
		want[n] = true
	}
	var out []harness.Target
	for _, t := range all {
		// The control is always kept: without it there is nothing to express
		// added latency against.
		if want[t.Name] || t.Kind == "control" {
			out = append(out, t)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		if r != ' ' {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func controlName(s *harness.Suite) string {
	if c := s.Control(); c != nil {
		return c.Name
	}
	return ""
}

// subjectOf finds the target the "where we lose" section is written about.
func subjectOf(s *harness.Suite) string {
	for _, t := range s.Targets {
		if t.Kind == "subject" {
			return t.Name
		}
	}
	return "everstack"
}

// cmdValidate preflights every target, skipped ones included, and reports which
// are actually usable. Competitor configs drift with each of their releases, so
// this is the first thing to run after pulling new images.
func cmdValidate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	suitePath := fs.String("suite", "targets.yaml", "suite definition")
	_ = fs.Parse(args)

	suite, err := harness.LoadSuite(*suitePath)
	if err != nil {
		return err
	}

	ctrl := harness.NewControl(suite.Upstream.ControlURL)
	if err := ctrl.WaitReady(ctx, 10*time.Second); err != nil {
		return fmt.Errorf("mock upstream unreachable: %w", err)
	}
	if err := ctrl.Healthy(ctx, "primary"); err != nil {
		return err
	}
	if err := ctrl.Healthy(ctx, "secondary"); err != nil {
		return err
	}

	fmt.Printf("%-14s %-8s %s\n", "TARGET", "STATE", "DETAIL")
	ok, bad := 0, 0
	for _, t := range suite.Targets {
		state, detail := "ok", ""
		if t.Skip {
			state, detail = "skipped", t.SkipReason
		} else if err := preflight(ctx, t); err != nil {
			state, detail = "FAIL", err.Error()
			bad++
		} else {
			ok++
			detail = t.ChatURL
		}
		if len(detail) > 110 {
			detail = detail[:110] + "..."
		}
		fmt.Printf("%-14s %-8s %s\n", t.Name, state, detail)
	}
	fmt.Printf("\n%d usable, %d failing\n", ok, bad)
	if bad > 0 {
		fmt.Println("\nFailing targets are skipped by a real run and listed in the report as unmeasured,")
		fmt.Println("so a broken competitor container never silently disappears from the comparison.")
	}
	return nil
}
