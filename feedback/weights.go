package feedback

import "context"

const weightLookupBatchSize = 900

// weightsBatch applies cold-start (warmup) and MinRetrievals gating over a
// store's aggregates. It is the single source of truth shared by Collector and
// WeightReader so the two cannot diverge. Keys below MinRetrievals, unknown
// keys, and every key during warmup return 0.
func weightsBatch(ctx context.Context, store SignalStore, cfg CollectorConfig, chunkKeys []string) (map[string]float64, error) {
	result := make(map[string]float64, len(chunkKeys))
	for _, k := range chunkKeys {
		result[k] = 0
	}

	totalSignals, err := store.SignalCount(ctx)
	if err != nil {
		return nil, err
	}
	if totalSignals < cfg.WarmupSignals {
		return result, nil
	}

	for start := 0; start < len(chunkKeys); start += weightLookupBatchSize {
		end := start + weightLookupBatchSize
		if end > len(chunkKeys) {
			end = len(chunkKeys)
		}
		aggs, err := store.GetAggregatesBatch(ctx, chunkKeys[start:end])
		if err != nil {
			return nil, err
		}
		for k, agg := range aggs {
			if agg.RetrievalCount >= cfg.MinRetrievals {
				result[k] = agg.WeightedScore
			}
		}
	}
	return result, nil
}

// WeightReader exposes read-only behavioral weights without the Collector's
// attribution windows or background sweep goroutine. Use it where ranking only
// consumes weights and must never open attribution windows or emit signals.
type WeightReader struct {
	store  SignalStore
	config CollectorConfig
}

// NewWeightReader builds a read-only reader over store. Negative config fields
// resolve to their defaults (matching Collector); zero values keep their
// meaning (no warmup / no minimum / no decay).
func NewWeightReader(store SignalStore, config CollectorConfig) *WeightReader {
	return &WeightReader{store: store, config: config.withDefaults()}
}

// WeightsBatch returns gated behavioral weights for chunkKeys. Unknown or
// below-threshold keys return 0. During warmup all keys return 0.
func (r *WeightReader) WeightsBatch(ctx context.Context, chunkKeys []string) (map[string]float64, error) {
	return weightsBatch(ctx, r.store, r.config, chunkKeys)
}
