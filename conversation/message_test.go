package conversation

import (
	"testing"
)

func TestNewID_Unique(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		id := NewID()
		if id == "" {
			t.Fatal("NewID() returned empty string")
		}
		if seen[id] {
			t.Fatalf("NewID() returned duplicate: %s", id)
		}
		seen[id] = true
	}
}

func TestNewID_Format(t *testing.T) {
	id := NewID()
	if len(id) != 36 {
		t.Fatalf("NewID() length = %d, want 36", len(id))
	}
	if id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		t.Fatalf("NewID() invalid format: %s", id)
	}
}

func TestCharRatioEstimator(t *testing.T) {
	tests := []struct {
		name  string
		ratio float64
		input string
		want  int
	}{
		{"empty string", 4.0, "", 0},
		{"exact division", 4.0, "abcd", 1},
		{"rounds up", 4.0, "abcde", 2},
		{"short ratio", 3.5, "1234567", 2},
		{"single char", 4.0, "a", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			est := CharRatioEstimator(tt.ratio)
			got := est(tt.input)
			if got != tt.want {
				t.Errorf("CharRatioEstimator(%v)(%q) = %d, want %d", tt.ratio, tt.input, got, tt.want)
			}
		})
	}
}
