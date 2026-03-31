package feedback

import "testing"

func TestDefaultStrength(t *testing.T) {
	tests := []struct {
		name     string
		kind     SignalKind
		expected float64
	}{
		{"completion accepted", SignalCompletionAccepted, 0.8},
		{"completion rejected", SignalCompletionRejected, -0.8},
		{"code kept", SignalCodeKept, 0.6},
		{"code undone", SignalCodeUndone, -0.7},
		{"file opened", SignalFileOpened, 0.3},
		{"query repeated", SignalQueryRepeated, -0.5},
		{"insight acted on", SignalInsightActedOn, 0.5},
		{"insight dismissed", SignalInsightDismissed, -0.5},
		{"unknown kind", SignalKind("unknown"), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DefaultStrength(tt.kind)
			if got != tt.expected {
				t.Errorf("DefaultStrength(%q) = %v, want %v", tt.kind, got, tt.expected)
			}
		})
	}
}

func TestSignalEffectiveStrength(t *testing.T) {
	tests := []struct {
		name     string
		signal   Signal
		expected float64
	}{
		{
			name:     "explicit strength used",
			signal:   Signal{Kind: SignalCompletionAccepted, Strength: 0.5},
			expected: 0.5,
		},
		{
			name:     "zero strength uses default",
			signal:   Signal{Kind: SignalCompletionAccepted},
			expected: 0.8,
		},
		{
			name:     "negative explicit strength used",
			signal:   Signal{Kind: SignalFileOpened, Strength: -0.2},
			expected: -0.2,
		},
		{
			name:     "unknown kind zero strength returns zero",
			signal:   Signal{Kind: SignalKind("bogus")},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.signal.effectiveStrength()
			if got != tt.expected {
				t.Errorf("effectiveStrength() = %v, want %v", got, tt.expected)
			}
		})
	}
}
