package main

import (
	"math"
	"reflect"
	"testing"
)

func TestPercentilesEmpty(t *testing.T) {
	got := percentiles(nil, 0.25, 0.5, 0.75)
	if len(got) != 3 {
		t.Fatalf("len=%d; want 3", len(got))
	}
	for i, v := range got {
		if !math.IsNaN(v) {
			t.Errorf("got[%d]=%v; want NaN", i, v)
		}
	}
}

func TestPercentilesSingle(t *testing.T) {
	got := percentiles([]float64{0.42}, 0.25, 0.5, 0.75, 0.9)
	want := []float64{0.42, 0.42, 0.42, 0.42}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v; want %v", got, want)
	}
}

func TestPercentilesEvenSplit(t *testing.T) {
	// Nearest-rank: for N=4 values [1,2,3,4], p25 → rank ceil(0.25*4)=1 → values[0]=1.
	got := percentiles([]float64{1, 2, 3, 4}, 0.25, 0.5, 0.75, 1.0)
	want := []float64{1, 2, 3, 4}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v; want %v", got, want)
	}
}

func TestPercentilesRequiresSortedOutputDespiteShuffledInput(t *testing.T) {
	got := percentiles([]float64{4, 2, 1, 3}, 0.5)
	if got[0] != 2 {
		t.Fatalf("p50 of [4,2,1,3] = %v; want 2 (nearest-rank ceil(0.5*4)=2 → sorted[1]=2)", got[0])
	}
}

func TestInt64PercentilesEqualValues(t *testing.T) {
	got := int64Percentiles([]int64{100, 100, 100}, 0.5, 0.9)
	if got[0] != 100 || got[1] != 100 {
		t.Fatalf("got %v; want [100 100]", got)
	}
}

func TestInt64PercentilesP90KnownFixture(t *testing.T) {
	// N=10 → p90 → ceil(0.9*10)=9 → sorted[8]
	xs := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	got := int64Percentiles(xs, 0.9)
	if got[0] != 90 {
		t.Fatalf("p90 = %v; want 90", got[0])
	}
}
