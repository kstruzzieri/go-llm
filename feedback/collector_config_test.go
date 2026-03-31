package feedback

import "testing"

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MinRetrievals != 5 {
		t.Errorf("MinRetrievals = %d, want 5", cfg.MinRetrievals)
	}
	if cfg.WarmupSignals != 100 {
		t.Errorf("WarmupSignals = %d, want 100", cfg.WarmupSignals)
	}
	if cfg.DecayLambda != 0.1 {
		t.Errorf("DecayLambda = %f, want 0.1", cfg.DecayLambda)
	}
}

func TestWithDefaults(t *testing.T) {
	tests := []struct {
		name   string
		input  CollectorConfig
		expect CollectorConfig
	}{
		{
			name:   "all zeros preserved",
			input:  CollectorConfig{},
			expect: CollectorConfig{},
		},
		{
			name:   "negative values replaced with defaults",
			input:  CollectorConfig{MinRetrievals: -1, WarmupSignals: -1, DecayLambda: -1},
			expect: DefaultConfig(),
		},
		{
			name:   "partial negative replaced",
			input:  CollectorConfig{MinRetrievals: -1, WarmupSignals: 50},
			expect: CollectorConfig{MinRetrievals: 5, WarmupSignals: 50, DecayLambda: 0},
		},
		{
			name:   "positive values preserved",
			input:  CollectorConfig{MinRetrievals: 3, WarmupSignals: 50, DecayLambda: 0.05},
			expect: CollectorConfig{MinRetrievals: 3, WarmupSignals: 50, DecayLambda: 0.05},
		},
		{
			name:   "zero values are intentional",
			input:  CollectorConfig{MinRetrievals: 0, WarmupSignals: 0, DecayLambda: 0},
			expect: CollectorConfig{MinRetrievals: 0, WarmupSignals: 0, DecayLambda: 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.input.withDefaults()
			if got != tt.expect {
				t.Errorf("withDefaults() = %+v, want %+v", got, tt.expect)
			}
		})
	}
}
