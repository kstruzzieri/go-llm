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

// CompressMessages folds the oldest history into the durable summary when the
// estimated cost of non-system raw messages exceeds maxTokens. It retains the
// most recent exchanges that fit maxTokens, never fewer than minRecentExchanges,
// and never splits a tool-call/result chain (it reuses trimByExchangesKeep).
// Returns conv unchanged (no model call) when raw history already fits and
// there is no durable summary to rewrite, or when the floor keeps all messages
// and there is no durable summary to rewrite.
//
// Safety invariant: raw messages are evicted ONLY after the summary that absorbs
// them is produced successfully. Any summarizer error (including blank output)
// aborts the whole operation and the caller keeps its prior state.
func CompressMessages(
	ctx context.Context,
	conv Conversation,
	maxTokens, minRecentExchanges int,
	estimator TokenEstimator,
	summarize Summarizer,
) (Conversation, error) {
	if estimator == nil {
		panic("conversation: CompressMessages requires non-nil TokenEstimator")
	}
	if summarize == nil {
		return Conversation{}, errNilSummarizer
	}
	prior, priorCount := "", 0
	if conv.DurableSummary != nil {
		prior = conv.DurableSummary.Content
		priorCount = conv.DurableSummary.MessageCount
	}
	hasPrior := strings.TrimSpace(prior) != ""
	if EstimateMessagesTokens(conv.Messages, estimator) <= maxTokens && !hasPrior {
		return conv, nil
	}
	if minRecentExchanges < 0 {
		minRecentExchanges = 0
	}

	// Grow the retained window up from the hard floor while it still fits the
	// budget and there are older exchanges left to add.
	// ponytail: O(n^2) over exchanges; fine for chat-history sizes.
	keep := trimByExchangesKeep(conv.Messages, minRecentExchanges)
	for next := minRecentExchanges + 1; ; next++ {
		grown := trimByExchangesKeep(conv.Messages, next)
		if sameKeepMask(grown, keep) {
			break // nothing older left to add: floor already keeps everything
		}
		if keptRawCost(conv.Messages, grown, estimator) > maxTokens {
			break // adding the next-oldest exchange overflows the budget
		}
		keep = grown
	}

	recent := make([]Message, 0, len(conv.Messages))
	old := make([]Message, 0, len(conv.Messages))
	for i, m := range conv.Messages {
		if !keep[i] && m.Role != "system" {
			old = append(old, m)
		} else {
			recent = append(recent, m)
		}
	}
	if len(old) == 0 {
		if !hasPrior {
			return conv, nil
		}
	}
	newSummary, err := summarize(ctx, prior, old)
	if err != nil {
		return Conversation{}, err
	}
	if strings.TrimSpace(newSummary) == "" {
		return Conversation{}, ErrEmptySummary
	}

	out := conv
	out.Messages = recent
	out.DurableSummary = &DurableSummary{
		Content:      strings.TrimSpace(newSummary),
		MessageCount: priorCount + len(old),
	}
	return out, nil
}

func sameKeepMask(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func keptRawCost(msgs []Message, keep []bool, estimator TokenEstimator) int {
	n := 0
	for i, m := range msgs {
		if keep[i] && m.Role != "system" {
			n += messageCost(m, estimator)
		}
	}
	return n
}
