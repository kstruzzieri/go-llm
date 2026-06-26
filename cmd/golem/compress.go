package main

import (
	"context"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
)

// compressPolicy decides when to compress a session's history into its durable
// summary. It lives on replSession (which owns runtime wiring); session stays
// persistence-only.
type compressPolicy struct {
	summarize          conversation.Summarizer
	estimate           conversation.TokenEstimator
	maxHistoryTokens   int // fire when model-visible history (summary prompt + raw) exceeds this
	minRecentExchanges int
	enabled            bool
}

// maybeCompress runs the post-turn compression policy. Best-effort: on a
// summarizer/model error the session is left unchanged and the error is returned
// for the REPL to surface — a turn is never lost to a failed summary.
func (sess *replSession) maybeCompress(ctx context.Context) error {
	p := sess.compress
	if !p.enabled || sess.session == nil || p.summarize == nil {
		return nil
	}
	conv := sess.session.currentConversation()

	// Trigger accounting lives here because Golem (not conversation/) knows the
	// exact summary prompt agent will inject next turn.
	summaryPrompt := agent.DurableSummaryPrompt(sess.session.historySummary())
	historyCost := p.estimate(summaryPrompt) +
		conversation.EstimateMessagesTokens(conv.Messages, p.estimate)
	if historyCost <= p.maxHistoryTokens {
		return nil // model-visible history still fits; no model call
	}

	// Reserve headroom for the regenerated summary (bounded by NumPredict) when
	// bounding the retained raw history.
	maxRawTokens := p.maxHistoryTokens - agent.DefaultSummaryOutputReserve
	if maxRawTokens < 0 {
		maxRawTokens = 0
	}
	out, err := conversation.CompressMessages(ctx, conv, maxRawTokens, p.minRecentExchanges, p.estimate, p.summarize)
	if err != nil {
		return err
	}
	if len(out.Messages) == len(conv.Messages) {
		return nil // nothing was evicted (floor covered everything)
	}
	return sess.session.applyCompacted(ctx, out)
}
