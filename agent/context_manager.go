package agent

import (
	"context"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

// defaultInputCeiling is a conservative fallback when Budget.InputCeiling is 0.
const defaultInputCeiling = 8192

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
		ceiling = defaultInputCeiling
	}
	if b.OutputReserve > 0 {
		ceiling -= b.OutputReserve
	}
	if ceiling < 0 {
		ceiling = 0
	}
	return TokenBudget{Input: ceiling}
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
// pinned cost of the active tool schemas (not stored in State).
func (m ContextManager) Assemble(ctx context.Context, st State, toolSchemaTokens int, budget TokenBudget) (State, Pressure, error) {
	m = normalizeContextManager(m)
	st = materializeDurableSummary(st)
	if m.pinnedTokens(st, toolSchemaTokens) > budget.Input {
		return st, Pressure{}, ErrContextExhausted
	}
	// Compactor fits the State messages into the budget left after tool schemas.
	stateBudget := TokenBudget{Input: budget.Input - toolSchemaTokens}
	out, report, err := m.Compactor.Compact(ctx, st, stateBudget)
	if err != nil {
		return st, Pressure{}, err
	}
	after := m.totalTokens(out, toolSchemaTokens)
	used := 0.0
	if budget.Input > 0 {
		used = float64(after) / float64(budget.Input)
	}
	compactions := 0
	if report.DroppedCount > 0 {
		compactions = 1
	}
	pressure := Pressure{UsedPct: used, Evicted: report.DroppedCount, Compactions: compactions}
	if after > budget.Input {
		return out, pressure, ErrContextExhausted
	}
	return out, pressure, nil
}
