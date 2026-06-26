package conversation

import (
	"context"
	"errors"
	"strings"
)

// ErrEmptySummary indicates the summarizer returned successfully but with blank
// output. CompressMessages treats this as a failed compression so raw messages
// are never evicted into an empty durable summary.
var ErrEmptySummary = errors.New("conversation: summarizer returned empty summary")

// errNilSummarizer is returned when CompressMessages is called without a
// Summarizer. It does NOT fall back to FallbackSummarizer: an unbounded fallback
// would reintroduce the pinned-blob growth this feature exists to prevent.
var errNilSummarizer = errors.New("conversation: CompressMessages requires a non-nil Summarizer")

// Summarizer rewrites a rolling durable summary. prior is the existing summary
// content (may be ""); msgs are the newly-evicted messages to fold in. The
// returned string REPLACES prior — implementations must consolidate, not append,
// and are responsible for bounding their own output.
type Summarizer func(ctx context.Context, prior string, msgs []Message) (string, error)

// FallbackSummarizer is a deterministic, model-free Summarizer for tests and
// offline use ONLY. It concatenates prior + rendered msgs and is NOT
// size-bounded; production code must supply a bounded (model-backed) Summarizer.
func FallbackSummarizer(_ context.Context, prior string, msgs []Message) (string, error) {
	var b strings.Builder
	if p := strings.TrimSpace(prior); p != "" {
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	for _, m := range msgs {
		if m.Role == "" && m.Content == "" {
			continue
		}
		if m.Role != "" {
			b.WriteString(m.Role)
			b.WriteString(": ")
		}
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String()), nil
}

// EstimateMessagesTokens sums the estimated token cost of every NON-system
// message, matching CompressMessages' raw-history accounting. Exported so
// consumers (Golem) can make trigger decisions without re-deriving messageCost.
// Panics on a nil estimator, consistent with TrimMessages.
func EstimateMessagesTokens(msgs []Message, estimator TokenEstimator) int {
	if estimator == nil {
		panic("conversation: EstimateMessagesTokens requires non-nil TokenEstimator")
	}
	n := 0
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		n += messageCost(m, estimator)
	}
	return n
}
