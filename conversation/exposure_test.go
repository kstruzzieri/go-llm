package conversation

import (
	"strings"
	"testing"
)

func TestExposureExposed(t *testing.T) {
	tests := []struct {
		name string
		e    Exposure
		want bool
	}{
		{"no record reads as unknown", Exposure{}, true},
		{"recorded and clean", Exposure{Recorded: true}, false},
		{"recorded and foreign", Exposure{Recorded: true, Foreign: true}, true},
		{"recorded and unknown", Exposure{Recorded: true, Unknown: true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.e.Exposed(); got != tt.want {
				t.Fatalf("Exposure%+v.Exposed() = %v, want %v", tt.e, got, tt.want)
			}
		})
	}
}

func TestBoundExposureFirst(t *testing.T) {
	long := strings.Repeat("a", MaxExposureFirstBytes+10)
	// 127 ASCII bytes then a 3-byte rune straddling the bound.
	straddle := strings.Repeat("a", MaxExposureFirstBytes-1) + "€"
	tests := []struct {
		name, in, want string
	}{
		{"short unchanged", "tool:x", "tool:x"},
		{"exact bound unchanged", strings.Repeat("b", MaxExposureFirstBytes), strings.Repeat("b", MaxExposureFirstBytes)},
		{"long ASCII cut to bound", long, long[:MaxExposureFirstBytes]},
		{"multi-byte cut at rune boundary", straddle, strings.Repeat("a", MaxExposureFirstBytes-1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BoundExposureFirst(tt.in); got != tt.want {
				t.Fatalf("BoundExposureFirst() = %q, want %q", got, tt.want)
			}
		})
	}
}
