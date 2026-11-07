package conversation

import "testing"

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
