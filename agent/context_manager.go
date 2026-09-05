package agent

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

// checkedTokenAdd is the single arithmetic boundary for estimator-driven token
// counts. Overflow saturates for diagnostics and reports false so admission can
// fail closed instead of treating a wrapped negative as spare capacity.
func checkedTokenAdd(a, b int) (int, bool) {
	if (b > 0 && a > math.MaxInt-b) || (b < 0 && a < math.MinInt-b) {
		return math.MaxInt, false
	}
	return a + b, true
}

func checkedTokenSub(a, b int) (int, bool) {
	if (b > 0 && a < math.MinInt+b) || (b < 0 && a > math.MaxInt+b) {
		return math.MaxInt, false
	}
	return a - b, true
}

func saturatedTokenAdd(a, b int) int {
	n, _ := checkedTokenAdd(a, b)
	return n
}

// DefaultInputCeiling is a conservative fallback when Budget.InputCeiling is 0.
// Exported so consumers (e.g. Golem's compression policy) derive their own
// token thresholds from the same source rather than duplicating the literal.
const DefaultInputCeiling = 8192

// ContextManager assembles and bounds the prompt each turn. It is a pure
// function over State, tool-schema token count, and budget.
type ContextManager struct {
	Compactor Compactor
	// Estimate is the token estimator; len/4 when nil. It must be pure and
	// deterministic. The mixed path additionally enforces the same fallback
	// rag.ProgressiveRenderRequest.Estimate documents — a non-positive result
	// for non-empty text falls back to the default heuristic — because its cost
	// accounting is an exact identity rather than an approximation. The legacy
	// path is unchanged.
	Estimate func(string) int
	// frameToolResults charges one #430 frame envelope per role "tool" message
	// during fitting. Only the Orchestrator sets it (agent.New), because only
	// its request builder frames observations on the wire; a standalone
	// manager (llm-bench's corpus builder, prefilled replay) sends what it
	// assembles unframed and prices raw content. Unexported on purpose: the
	// charge follows the transport, and callers do not choose the transport.
	frameToolResults bool
	// Mixed opts into structured mixed-budget assembly. It requires the DEFAULT
	// compactor: Mixed together with a non-nil Compactor is a configuration
	// error (ErrMixedCompactor) whatever any one request carries, because the
	// two are alternative strategies for the same messages and an
	// anchor-dependent error would surface intermittently.
	//
	// With the default compactor, any request whose State carries no structured
	// anchors keeps the legacy Compactor path and model-visible messages
	// byte-identical. Its readers are dispatch, which skips the tool-result deep
	// copy when mixed assembly is off, and AssembleWithTrace (#331).
	//
	// MEMORY: On, dispatch clones each tool result's ContextSet onto its anchor
	// Message and NOTHING ever clears it, so every producer's whole capability
	// projection is retained for the rest of the run — including alternatives no
	// allocation will ever choose again. For agent/tools' Retrieve that is
	// quadratic in k: 1.35 MB per call at the ceiling MaxK of 20 over 2 KB
	// chunks, so a 20-step run holds ~27 MB (measured by rag's
	// TestProgressiveGroupsProjectionBytes). With Mixed OFF the clone never
	// happens and the projection is garbage as soon as dispatch returns.
	//
	// Releasing the sets of units the allocator evicted would reclaim most of it
	// under pressure, and eviction IS permanent for a run. It is deliberately not
	// done: mixedUnit.msgs aliases State.Messages, so any release writes the
	// caller's canonical State, and mixed assembly's contract — pinned per budget
	// by TestMixedTraceProperties, whose state snapshot marshals every message's
	// Context — is that assembling a State does not mutate it. Budget the memory;
	// do not buy it with a non-idempotent Assemble.
	Mixed bool
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
	if s == "" {
		return 0
	}
	return 1 + (len(s)-1)/4
}

func normalizeContextManager(m ContextManager) ContextManager {
	switch c := m.Compactor.(type) {
	case nil:
		m.Compactor = m.costCompactor()
	case RecencyCompactor:
		// A caller-supplied built-in compactor prices messages the way this
		// manager's final validation does, frame envelope included.
		c.frameToolResults = m.frameToolResults
		m.Compactor = c
	}
	return m
}

// costCompactor is the one place a ContextManager turns itself into the
// message-cost seam, so every path (pinned, total, mixed envelopes, the
// default compactor) prices a message identically.
func (m ContextManager) costCompactor() RecencyCompactor {
	return RecencyCompactor{Estimate: m.Estimate, frameToolResults: m.frameToolResults}
}

func (m ContextManager) messageCost(msg Message) int {
	return m.costCompactor().messageCost(msg)
}

func (m ContextManager) checkedMessageCost(msg Message) (int, bool) {
	return m.costCompactor().checkedMessageCost(msg)
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
	n, _ := m.checkedPinnedTokens(st, toolSchemaTokens)
	return n
}

func (m ContextManager) checkedPinnedTokens(st State, toolSchemaTokens int) (int, bool) {
	n, ok := checkedTokenAdd(m.estimate(st.System), toolSchemaTokens)
	if !ok {
		return n, false
	}
	for _, msg := range st.Messages {
		if msg.Segment == Pinned {
			cost, costOK := m.checkedMessageCost(msg)
			if !costOK {
				return cost, false
			}
			n, ok = checkedTokenAdd(n, cost)
			if !ok {
				return n, false
			}
		}
	}
	return n, true
}

func (m ContextManager) totalTokens(st State, toolSchemaTokens int) int {
	n, _ := m.checkedTotalTokens(st, toolSchemaTokens)
	return n
}

func (m ContextManager) checkedTotalTokens(st State, toolSchemaTokens int) (int, bool) {
	n, ok := checkedTokenAdd(m.estimate(st.System), toolSchemaTokens)
	if !ok {
		return n, false
	}
	for _, msg := range st.Messages {
		cost, costOK := m.checkedMessageCost(msg)
		if !costOK {
			return cost, false
		}
		n, ok = checkedTokenAdd(n, cost)
		if !ok {
			return n, false
		}
	}
	return n, true
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
//
// It is AssembleWithTrace with the trace discarded; callers that want the
// mixed-assembly trace call that instead.
func (m ContextManager) Assemble(ctx context.Context, st State, toolSchemaTokens int, budget TokenBudget) (State, Pressure, error) {
	out, pressure, _, err := m.AssembleWithTrace(ctx, st, toolSchemaTokens, budget)
	return out, pressure, err
}

// assembleLegacy is the Compactor-driven path: unchanged behavior, and the only
// place the default compactor is installed. Doing it here rather than in
// agent.New is what keeps a mixed manager's nil Compactor distinguishable from
// a custom one.
func (m ContextManager) assembleLegacy(ctx context.Context, st State, toolSchemaTokens int, budget TokenBudget) (State, Pressure, error) {
	m = normalizeContextManager(m)
	thresholds := budget.Thresholds.normalize()
	st = materializeDurableSummary(st)

	pinned, pinnedOK := m.checkedPinnedTokens(st, toolSchemaTokens)
	if !pinnedOK || pinned > budget.Input {
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

	stateInput, stateBudgetOK := checkedTokenSub(budget.Input, toolSchemaTokens)
	if !stateBudgetOK {
		return st, Pressure{
			UsedPct: 1, InputTokens: stateInput, InputBudget: budget.Input,
			Level: LevelCritical, Cause: CauseToolSchema, Mitigation: MitigationHalt,
		}, ErrContextExhausted
	}
	stateBudget := TokenBudget{Input: stateInput}
	out, report, err := m.Compactor.Compact(ctx, st, stateBudget)
	if err != nil {
		if errors.Is(err, ErrContextExhausted) {
			tokens := saturatedTokenAdd(report.TokensAfter, toolSchemaTokens)
			return st, Pressure{
				UsedPct: usedFraction(tokens, budget.Input), InputTokens: tokens, InputBudget: budget.Input,
				Level: LevelCritical, Cause: m.dominantCause(st, toolSchemaTokens), Mitigation: MitigationHalt,
			}, err
		}
		return st, Pressure{}, err
	}
	after, afterOK := m.checkedTotalTokens(out, toolSchemaTokens)
	used := usedFraction(after, budget.Input)
	evicted := report.DroppedCount > 0
	compactions := 0
	if evicted {
		compactions = 1
	}
	exhausted := !afterOK || after > budget.Input
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
			pinnedMsgs = saturatedTokenAdd(pinnedMsgs, m.messageCost(msg))
		}
	}
	if toolSchemaTokens > pinnedMsgs {
		return CauseToolSchema
	}
	return CausePinned
}
