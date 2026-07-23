package agent

import (
	"context"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

// DefaultInputCeiling is a conservative fallback when Budget.InputCeiling is 0.
// Exported so consumers (e.g. Golem's compression policy) derive their own
// token thresholds from the same source rather than duplicating the literal.
const DefaultInputCeiling = 8192

// ContextManager assembles and bounds the prompt each turn. It is a pure
// function over State, tool-schema token count, and budget.
type ContextManager struct {
	Compactor Compactor
	Estimate  func(string) int // token estimator; len/4 when nil
}

// DurableSummaryPrompt renders the durable summary as the pinned system message
// injected ahead of raw history. Exported so Golem's trigger accounting counts
// the exact text agent injects (no drift between estimate and reality).
func DurableSummaryPrompt(summary string) string {
	return "Previous conversation summary:\n" + summary
}

func (m ContextManager) estimate(s string) int {
	if m.Estimate != nil {
		return m.Estimate(s)
	}
	return (len(s) + 3) / 4
}

func normalizeContextManager(m ContextManager) ContextManager {
	if m.Compactor == nil {
		m.Compactor = RecencyCompactor{Estimate: m.Estimate}
	}
	return m
}

func (m ContextManager) messageCost(msg Message) int {
	return RecencyCompactor{Estimate: m.Estimate}.messageCost(msg)
}

// turnBudget resolves the per-turn input ceiling from the run Budget, applying
// the conservative default when unset. Tool-schema accounting happens in
// Assemble, which subtracts the pinned schema cost from this ceiling.
func turnBudget(b Budget) TokenBudget {
	ceiling := b.InputCeiling
	if ceiling <= 0 {
		ceiling = DefaultInputCeiling
	}
	if b.OutputReserve > 0 {
		ceiling -= b.OutputReserve
	}
	if ceiling < 0 {
		ceiling = 0
	}
	return TokenBudget{Input: ceiling, Thresholds: b.Pressure.normalize()}
}

func (m ContextManager) pinnedTokens(st State, toolSchemaTokens int) int {
	n := m.estimate(st.System) + toolSchemaTokens
	for _, msg := range st.Messages {
		if msg.Segment == Pinned {
			n += m.messageCost(msg)
		}
	}
	return n
}

func (m ContextManager) totalTokens(st State, toolSchemaTokens int) int {
	n := m.estimate(st.System) + toolSchemaTokens
	for _, msg := range st.Messages {
		n += m.messageCost(msg)
	}
	return n
}

func materializeDurableSummary(st State) State {
	summary := strings.TrimSpace(st.DurableSummary)
	if summary == "" {
		return st
	}
	out := st
	out.DurableSummary = ""
	out.Messages = make([]Message, 0, len(st.Messages)+1)
	out.Messages = append(out.Messages, Message{
		ChatMessage: provider.ChatMessage{Role: "system", Content: DurableSummaryPrompt(summary)},
		Segment:     Pinned,
	})
	out.Messages = append(out.Messages, st.Messages...)
	return out
}

// Assemble bounds the transcript to fit the budget. toolSchemaTokens is the
// pinned cost of the active tool schemas (not stored in State). The returned
// Pressure is fully populated on every path — including the exhaustion error
// paths — so the orchestrator can emit it before the model call.
func (m ContextManager) Assemble(ctx context.Context, st State, toolSchemaTokens int, budget TokenBudget) (State, Pressure, error) {
	m = normalizeContextManager(m)
	thresholds := budget.Thresholds.normalize()
	st = materializeDurableSummary(st)

	pinned := m.pinnedTokens(st, toolSchemaTokens)
	if pinned > budget.Input {
		// Pinned segment (system + goal + tool schemas) alone exceeds the ceiling:
		// a hard runtime exhaustion. Report pinned tokens as the input cost.
		p := Pressure{
			UsedPct:     usedFraction(pinned, budget.Input),
			InputTokens: pinned,
			InputBudget: budget.Input,
			Level:       LevelCritical,
			Cause:       m.pinnedOverflowCause(st, toolSchemaTokens),
			Mitigation:  MitigationHalt,
		}
		return st, p, ErrContextExhausted
	}

	stateBudget := TokenBudget{Input: budget.Input - toolSchemaTokens}
	out, report, err := m.Compactor.Compact(ctx, st, stateBudget)
	if err != nil {
		return st, Pressure{}, err
	}
	after := m.totalTokens(out, toolSchemaTokens)
	used := usedFraction(after, budget.Input)
	evicted := report.DroppedCount > 0
	compactions := 0
	if evicted {
		compactions = 1
	}
	exhausted := after > budget.Input
	level, mitigation := thresholds.Classify(used, exhausted, evicted)
	pressure := Pressure{
		UsedPct:     used,
		Evicted:     report.DroppedCount,
		Compactions: compactions,
		InputTokens: after,
		InputBudget: budget.Input,
		Level:       level,
		Cause:       m.dominantCause(out, toolSchemaTokens),
		Mitigation:  mitigation,
	}
	if exhausted {
		return out, pressure, ErrContextExhausted
	}
	return out, pressure, nil
}

// usedFraction is after/budget, guarding a zero/negative budget.
func usedFraction(tokens, budget int) float64 {
	if budget <= 0 {
		if tokens > 0 {
			return 1
		}
		return 0
	}
	return float64(tokens) / float64(budget)
}

// pinnedOverflowCause attributes a pinned-overflow exhaustion to either the tool
// schemas or the pinned messages (system + goal). It deliberately ignores elastic
// history, which is irrelevant to a pinned-segment overflow.
func (m ContextManager) pinnedOverflowCause(st State, toolSchemaTokens int) PressureCause {
	pinnedMsgs := m.estimate(st.System)
	for _, msg := range st.Messages {
		if msg.Segment == Pinned {
			pinnedMsgs += m.messageCost(msg)
		}
	}
	if toolSchemaTokens > pinnedMsgs {
		return CauseToolSchema
	}
	return CausePinned
}
