package rag

import (
	"context"
	"errors"
)

// BehavioralScorer scores chunks by accumulated behavioral feedback weight,
// keyed by stable chunk key. It is fail-open: any weighter error yields neutral
// (all-zero) scores and a nil error, so retrieval never aborts on feedback
// failure.
type BehavioralScorer struct {
	w BehavioralWeighter
}

// NewBehavioralScorer wraps a BehavioralWeighter. A nil weighter scores neutral.
func NewBehavioralScorer(w BehavioralWeighter) *BehavioralScorer {
	return &BehavioralScorer{w: w}
}

// Name returns "behavioral".
func (s *BehavioralScorer) Name() string { return "behavioral" }

// behavioralKey returns the stable attribution key for a chunk and whether it is
// usable. An empty StableKey is not usable and is never passed to the weighter;
// behavioral identity must survive re-index, and chunk.ID does not.
func behavioralKey(c Chunk) (string, bool) {
	if c.StableKey == "" {
		return "", false
	}
	return c.StableKey, true
}

// ScoreBatch returns per-chunk behavioral weights aligned to the input order.
// Chunks with no usable key, or any key the weighter reports nothing for, score
// 0. On weighter error all chunks score 0 and the error is swallowed.
func (s *BehavioralScorer) ScoreBatch(ctx context.Context, chunks []Chunk, query string,
	queryEmbedding []float64, qCtx QueryContext) ([]float64, error) {
	scores := make([]float64, len(chunks))
	if s.w == nil || len(chunks) == 0 {
		return scores, nil
	}

	keys := make([]string, 0, len(chunks))
	seen := make(map[string]struct{}, len(chunks))
	for _, c := range chunks {
		if k, ok := behavioralKey(c); ok {
			if _, dup := seen[k]; !dup {
				seen[k] = struct{}{}
				keys = append(keys, k)
			}
		}
	}
	if len(keys) == 0 {
		return scores, nil
	}

	weights, err := s.w.WeightsBatch(ctx, keys)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return scores, err
		}
		return scores, nil // fail open: neutral (scores still all-zero), no error
	}
	for i, c := range chunks {
		if k, ok := behavioralKey(c); ok {
			scores[i] = weights[k]
		}
	}
	return scores, nil
}
