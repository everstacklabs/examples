// Package stats computes exact quantiles over raw latency samples.
//
// We deliberately keep every sample rather than feeding an HDR histogram. Run
// sizes here are in the tens of thousands, which is small enough that exact
// quantiles are cheap, and publishing raw samples is part of the credibility
// contract in METHODOLOGY.md.
package stats

import (
	"math"
	"sort"
	"time"
)

// Sample is one observation, in milliseconds.
type Sample = float64

// Summary is the distribution of a single metric across one run.
type Summary struct {
	Count  int     `json:"count"`
	Min    float64 `json:"min_ms"`
	Mean   float64 `json:"mean_ms"`
	P50    float64 `json:"p50_ms"`
	P90    float64 `json:"p90_ms"`
	P95    float64 `json:"p95_ms"`
	P99    float64 `json:"p99_ms"`
	P999   float64 `json:"p999_ms"`
	Max    float64 `json:"max_ms"`
	StdDev float64 `json:"stddev_ms"`
}

// Summarize computes the distribution of samples. It sorts a copy, so the
// caller's slice is left in arrival order for raw-sample export.
func Summarize(samples []Sample) Summary {
	if len(samples) == 0 {
		return Summary{}
	}
	sorted := make([]Sample, len(samples))
	copy(sorted, samples)
	sort.Float64s(sorted)

	var sum float64
	for _, v := range sorted {
		sum += v
	}
	mean := sum / float64(len(sorted))

	var sq float64
	for _, v := range sorted {
		d := v - mean
		sq += d * d
	}

	return Summary{
		Count:  len(sorted),
		Min:    sorted[0],
		Mean:   mean,
		P50:    quantile(sorted, 0.50),
		P90:    quantile(sorted, 0.90),
		P95:    quantile(sorted, 0.95),
		P99:    quantile(sorted, 0.99),
		P999:   quantile(sorted, 0.999),
		Max:    sorted[len(sorted)-1],
		StdDev: math.Sqrt(sq / float64(len(sorted))),
	}
}

// quantile uses the nearest-rank method on an already-sorted slice. Nearest
// rank always returns an observed value, which keeps the reported p99 a number
// that actually happened rather than an interpolation between two that did.
func quantile(sorted []Sample, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// Sub returns the element-wise difference of two summaries, which is how the
// report expresses added latency over the direct-to-upstream control.
func Sub(a, b Summary) Summary {
	return Summary{
		Count:  a.Count,
		Min:    a.Min - b.Min,
		Mean:   a.Mean - b.Mean,
		P50:    a.P50 - b.P50,
		P90:    a.P90 - b.P90,
		P95:    a.P95 - b.P95,
		P99:    a.P99 - b.P99,
		P999:   a.P999 - b.P999,
		Max:    a.Max - b.Max,
		StdDev: a.StdDev,
	}
}

// MeanAcross averages a metric across repeated runs and reports the spread, so
// the report can show run-to-run variance instead of only the best run.
type Across struct {
	Mean   float64 `json:"mean"`
	StdDev float64 `json:"stddev"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Runs   int     `json:"runs"`
}

// MeanAcross aggregates one scalar metric over N runs.
func MeanAcross(values []float64) Across {
	if len(values) == 0 {
		return Across{}
	}
	var sum, min, max float64
	min = math.Inf(1)
	max = math.Inf(-1)
	for _, v := range values {
		sum += v
		min = math.Min(min, v)
		max = math.Max(max, v)
	}
	mean := sum / float64(len(values))
	var sq float64
	for _, v := range values {
		d := v - mean
		sq += d * d
	}
	return Across{
		Mean:   mean,
		StdDev: math.Sqrt(sq / float64(len(values))),
		Min:    min,
		Max:    max,
		Runs:   len(values),
	}
}

// Ms converts a duration to float milliseconds.
func Ms(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }
