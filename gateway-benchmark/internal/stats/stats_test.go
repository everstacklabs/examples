package stats

import (
	"math"
	"testing"
)

func TestQuantileUsesNearestRank(t *testing.T) {
	// Nearest rank must return a value that actually occurred. Interpolating
	// would report a p99 nobody observed, which is the thing a reader checks
	// first when they suspect a benchmark of smoothing its tail.
	samples := []Sample{1, 2, 3, 4, 5, 6, 7, 8, 9, 100}
	got := Summarize(samples)

	if got.P50 != 5 {
		t.Errorf("p50 = %v, want 5", got.P50)
	}
	if got.P99 != 100 {
		t.Errorf("p99 = %v, want 100 (the outlier must survive)", got.P99)
	}
	if got.Max != 100 {
		t.Errorf("max = %v, want 100", got.Max)
	}
	if got.Min != 1 {
		t.Errorf("min = %v, want 1", got.Min)
	}
}

func TestSummarizeDoesNotReorderCallerSlice(t *testing.T) {
	// Raw samples are exported in arrival order as part of the credibility
	// contract, so Summarize must not sort them in place.
	samples := []Sample{9, 3, 7, 1}
	Summarize(samples)
	want := []Sample{9, 3, 7, 1}
	for i := range want {
		if samples[i] != want[i] {
			t.Fatalf("Summarize reordered the caller's slice: %v", samples)
		}
	}
}

func TestSummarizeEmpty(t *testing.T) {
	got := Summarize(nil)
	if got.Count != 0 || got.P99 != 0 {
		t.Errorf("empty summary should be zero-valued, got %+v", got)
	}
}

func TestMeanAndStdDev(t *testing.T) {
	got := Summarize([]Sample{2, 4, 4, 4, 5, 5, 7, 9})
	if math.Abs(got.Mean-5) > 1e-9 {
		t.Errorf("mean = %v, want 5", got.Mean)
	}
	if math.Abs(got.StdDev-2) > 1e-9 {
		t.Errorf("stddev = %v, want 2", got.StdDev)
	}
}

func TestSubProducesAddedLatency(t *testing.T) {
	gateway := Summarize([]Sample{110, 120, 130})
	control := Summarize([]Sample{100, 110, 120})
	added := Sub(gateway, control)
	if math.Abs(added.P50-10) > 1e-9 {
		t.Errorf("added p50 = %v, want 10", added.P50)
	}
}

func TestMeanAcrossReportsSpread(t *testing.T) {
	// The report shows run-to-run variance rather than the best run, so the
	// spread has to survive aggregation.
	got := MeanAcross([]float64{10, 12, 14})
	if math.Abs(got.Mean-12) > 1e-9 {
		t.Errorf("mean = %v, want 12", got.Mean)
	}
	if got.Min != 10 || got.Max != 14 {
		t.Errorf("min/max = %v/%v, want 10/14", got.Min, got.Max)
	}
	if got.Runs != 3 {
		t.Errorf("runs = %d, want 3", got.Runs)
	}
	if got.StdDev <= 0 {
		t.Errorf("stddev should be positive for spread input, got %v", got.StdDev)
	}
}
