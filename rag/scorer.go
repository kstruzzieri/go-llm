package rag

import (
	"context"
	"time"
)

// SignalScorer computes retrieval signals for a batch of chunks.
// Batch operation is required because FTS5 and behavioral scorers
// need a single query to score all candidates efficiently.
type SignalScorer interface {
	Name() string
	ScoreBatch(ctx context.Context, chunks []Chunk, query string,
		queryEmbedding []float64, qCtx QueryContext) ([]float64, error)
}

// BehavioralWeighter supplies optional per-chunk behavioral weights keyed by
// stable chunk key. It is the only behavioral surface the ranking path holds,
// so ranking physically cannot open attribution windows or record signals.
// *feedback.WeightReader satisfies it structurally; rag does not import feedback.
type BehavioralWeighter interface {
	WeightsBatch(ctx context.Context, keys []string) (map[string]float64, error)
}

// QueryContext carries metadata about the retrieval request.
// It provides contextual signals (current file, workspace root, open files)
// that individual scorers can use to improve relevance.
type QueryContext struct {
	// CurrentFile is the file the user is currently editing.
	CurrentFile string
	// WorkspaceRoot is the root directory of the user's workspace.
	WorkspaceRoot string
	// OpenFiles lists all files currently open in the editor.
	OpenFiles []string
	// Timestamp records when the query was issued.
	Timestamp time.Time
	// Metadata holds arbitrary key-value pairs for scorer-specific context.
	Metadata map[string]string
}

// MultiSignalSearcher is an optional interface for stores that support
// multi-signal retrieval. Stores that implement this interface can combine
// vector similarity with keyword, temporal, structural, and behavioral signals.
type MultiSignalSearcher interface {
	SearchMulti(ctx context.Context, queryEmbedding []float64, query string,
		k int, qCtx QueryContext) ([]ScoredResult, error)
}

// ScoredResult extends SearchResult with per-signal score breakdowns.
// Used by explain mode and prompt formatting to show why each result ranked.
type ScoredResult struct {
	SearchResult                    // embeds existing type (Chunk + Score + Distance)
	Signals      map[string]float64 // per-signal scores: "semantic" -> 0.85, "keyword" -> 0.3, etc.
}
