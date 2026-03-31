package feedback

// CollectorConfig tunes the behavioral feedback collector.
//
// A zero-valued config is valid: MinRetrievals=0 means no minimum,
// WarmupSignals=0 means no warmup phase, DecayLambda=0 means no decay.
// Use DefaultConfig() when you want the recommended defaults, or set
// MinRetrievals/WarmupSignals/DecayLambda to -1 to request the default
// for individual fields while leaving others at zero.
type CollectorConfig struct {
	// MinRetrievals is the minimum number of retrievals a chunk must appear
	// in before its weight is considered meaningful. Chunks below this
	// threshold return a weight of 0.0.
	MinRetrievals int

	// WarmupSignals is the total signal count below which the system is
	// considered to be in a cold-start phase. During warmup, all chunk
	// weights return 0.0.
	WarmupSignals int

	// DecayLambda controls the exponential time-decay applied when
	// recomputing aggregate scores. Higher values cause older signals to
	// lose influence faster.
	DecayLambda float64
}

// DefaultConfig returns a CollectorConfig populated with sensible defaults.
func DefaultConfig() CollectorConfig {
	return CollectorConfig{
		MinRetrievals: 5,
		WarmupSignals: 100,
		DecayLambda:   0.1,
	}
}

// withDefaults returns a copy of cfg where any negative field has been
// replaced with the corresponding default. Zero values are left as-is
// since they carry meaning (e.g. no warmup, no minimum).
func (cfg CollectorConfig) withDefaults() CollectorConfig {
	d := DefaultConfig()
	if cfg.MinRetrievals < 0 {
		cfg.MinRetrievals = d.MinRetrievals
	}
	if cfg.WarmupSignals < 0 {
		cfg.WarmupSignals = d.WarmupSignals
	}
	if cfg.DecayLambda < 0 {
		cfg.DecayLambda = d.DecayLambda
	}
	return cfg
}
