package main

import (
	"math"
	"sort"
)

// percentiles returns nearest-rank percentiles for the given fractions
// (each in [0,1]). NaN is returned for every fraction when values is
// empty so callers can decide whether to render "n/a" or hide the row.
// Nearest-rank: for ordered sample of size N, p_q is the value at
// 1-indexed rank ceil(q*N), or the last element when q==0 or N==1.
func percentiles(values []float64, fractions ...float64) []float64 {
	out := make([]float64, len(fractions))
	if len(values) == 0 {
		for i := range out {
			out[i] = math.NaN()
		}
		return out
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	for i, q := range fractions {
		out[i] = sorted[percentileIndex(len(sorted), q)]
	}
	return out
}

// int64Percentiles mirrors percentiles for integer latency samples.
// Empty input returns zeros — callers should length-check before
// interpreting (latency in ms is always non-negative so 0 is a valid
// observation).
func int64Percentiles(values []int64, fractions ...float64) []int64 {
	out := make([]int64, len(fractions))
	if len(values) == 0 {
		return out
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	for i, q := range fractions {
		out[i] = sorted[percentileIndex(len(sorted), q)]
	}
	return out
}

func percentileIndex(n int, q float64) int {
	if q <= 0 {
		return 0
	}
	if q >= 1 {
		return n - 1
	}
	rank := int(math.Ceil(q * float64(n)))
	if rank <= 0 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return rank - 1
}
