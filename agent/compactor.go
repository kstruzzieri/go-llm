package agent

import (
	"context"
	"math"
)

// TokenBudget is the input-token budget available to the State messages
// (system + transcript), already net of tool-schema tokens. Thresholds carries
// the per-turn pressure bands (zero value => defaults) so Assemble stays a pure
// function of its inputs. #63.
type TokenBudget struct {
	Input      int
	Thresholds PressureThresholds
}

// CompactionReport describes what a Compact call did.
type CompactionReport struct {
	Strategy     string
	DroppedCount int
	TokensBefore int
	TokensAfter  int
}

// Compactor fits a transcript into a token budget.
type Compactor interface {
	Compact(ctx context.Context, state State, budget TokenBudget) (State, CompactionReport, error)
}

// RecencyCompactor drops oldest prior-history groups first, then completed
// tool-call chains, preserves pinned messages and unresolved tool tails, and
// never calls a model.
type RecencyCompactor struct {
	Estimate func(string) int // conversation.TokenEstimator; len/4 default when nil
	// frameToolResults charges the #430 frame envelope per tool message; set
	// only through ContextManager (see its field of the same name).
	frameToolResults bool
}

func (rc RecencyCompactor) estimate(s string) int {
	if rc.Estimate != nil {
		return rc.Estimate(s)
	}
	if s == "" {
		return 0
	}
	return 1 + (len(s)-1)/4
}

func (rc RecencyCompactor) messageCost(m Message) int {
	n, _ := rc.checkedMessageCost(m)
	return n
}

func (rc RecencyCompactor) checkedMessageCost(m Message) (int, bool) {
	n := rc.estimate(m.Content)
	add := func(cost int) bool {
		var ok bool
		n, ok = checkedTokenAdd(n, cost)
		return ok
	}
	for _, tc := range m.ToolCalls {
		if !add(rc.estimate(tc.ID)) || !add(rc.estimate(tc.Type)) ||
			!add(rc.estimate(tc.Function.Name)) || !add(rc.estimate(string(tc.Function.Arguments))) {
			return n, false
		}
	}
	if !add(rc.estimate(m.ToolName)) || !add(rc.estimate(m.ToolCallID)) {
		return n, false
	}
	// #430 (spec D5): when this compactor prices for the Orchestrator, whose
	// request builder frames every tool observation, a tool message costs its
	// raw content and metadata PLUS one frame envelope, priced by the same
	// estimator. The envelope is an additive estimate of the framing bytes,
	// not E(framed content): a non-additive estimator prices content and
	// frame separately by design, and byte caps (OutputCap, the anchor cap,
	// trace Bytes) stay about raw content.
	if rc.frameToolResults && m.Role == "tool" && !add(rc.estimate(toolFrameEnvelope)) {
		return n, false
	}
	return n, true
}

func (rc RecencyCompactor) total(st State) int {
	n, _ := rc.checkedTotal(st)
	return n
}

func (rc RecencyCompactor) checkedTotal(st State) (int, bool) {
	n := rc.estimate(st.System)
	for _, m := range st.Messages {
		cost, costOK := rc.checkedMessageCost(m)
		if !costOK {
			return cost, false
		}
		var ok bool
		n, ok = checkedTokenAdd(n, cost)
		if !ok {
			return n, false
		}
	}
	return n, true
}

type compactionGroupKind int

const (
	groupPinned compactionGroupKind = iota
	groupHistory
	groupCompletedTool
	groupUnresolvedTool
	groupPlainElastic
)

type compactionGroup struct {
	msgs     []Message
	tokens   int
	kind     compactionGroupKind
	drop     bool
	overflow bool
}

// pairableExchange reports whether msgs[i] is a plain Elastic user turn
// immediately followed by a plain Elastic assistant turn (neither pinned, no
// tool calls). Such an exchange is grouped and evicted atomically so a dropped
// question never orphans its answer.
func pairableExchange(msgs []Message, i int) bool {
	if i+1 >= len(msgs) {
		return false
	}
	u, a := msgs[i], msgs[i+1]
	return u.Segment == Elastic && a.Segment == Elastic &&
		u.Role == "user" && a.Role == "assistant" &&
		len(u.ToolCalls) == 0 && len(a.ToolCalls) == 0
}

// chainAt returns the inclusive end index of the message group starting at i:
// an assistant message with ToolCalls plus the contiguous tool results that
// follow it. For a plain message it returns i.
func chainAt(msgs []Message, i int) int {
	if len(msgs[i].ToolCalls) == 0 {
		return i
	}
	end := i
	for end+1 < len(msgs) && msgs[end+1].Role == "tool" {
		end++
	}
	return end
}

func firstPinnedIndex(msgs []Message) int {
	for i, m := range msgs {
		if m.Segment == Pinned {
			return i
		}
	}
	return -1
}

func priorHistoryGroup(start, end, firstPinned int) bool {
	return firstPinned >= 0 && end < firstPinned
}

func classifyGroup(msgs []Message, start, end, firstPinned int) compactionGroupKind {
	for _, m := range msgs[start : end+1] {
		if m.Segment == Pinned {
			return groupPinned
		}
	}
	if priorHistoryGroup(start, end, firstPinned) && len(msgs[start].ToolCalls) == 0 {
		return groupHistory
	}
	if len(msgs[start].ToolCalls) == 0 {
		return groupPlainElastic
	}
	// A tool chain is completed (safe to evict) once every tool_call in the
	// assistant message has a matching tool result within the chain: the model
	// has the observations, and the whole atomic group drops together without
	// orphaning a tool_call_id. A chain missing results is an unresolved tail
	// (e.g. mid-dispatch) and must never be dropped. chainAt bundles the
	// assistant with its contiguous tool results, so the result count is end-start.
	resultCount := end - start
	if resultCount >= len(msgs[start].ToolCalls) {
		return groupCompletedTool
	}
	return groupUnresolvedTool
}

func (rc RecencyCompactor) groups(st State) []compactionGroup {
	out := make([]compactionGroup, 0, len(st.Messages))
	firstPinned := firstPinnedIndex(st.Messages)
	for i := 0; i < len(st.Messages); {
		end := chainAt(st.Messages, i)
		if end == i && pairableExchange(st.Messages, i) {
			end = i + 1 // evict the user->assistant exchange atomically
		}
		tokens := 0
		overflow := false
		for _, m := range st.Messages[i : end+1] {
			cost, costOK := rc.checkedMessageCost(m)
			var addOK bool
			tokens, addOK = checkedTokenAdd(tokens, cost)
			if !costOK || !addOK {
				overflow = true
				tokens = int(^uint(0) >> 1)
				break
			}
		}
		out = append(out, compactionGroup{
			msgs:     st.Messages[i : end+1],
			tokens:   tokens,
			kind:     classifyGroup(st.Messages, i, end, firstPinned),
			overflow: overflow,
		})
		i = end + 1
	}
	return out
}

func (rc RecencyCompactor) Compact(_ context.Context, st State, b TokenBudget) (State, CompactionReport, error) {
	before, ok := rc.checkedTotal(st)
	if !ok {
		return st, CompactionReport{Strategy: "recency", TokensBefore: before, TokensAfter: before}, ErrContextExhausted
	}
	if before <= b.Input {
		return st, CompactionReport{Strategy: "recency", TokensBefore: before, TokensAfter: before}, nil
	}

	groups := rc.groups(st)
	for _, g := range groups {
		if g.overflow {
			return st, CompactionReport{Strategy: "recency", TokensBefore: before, TokensAfter: math.MaxInt}, ErrContextExhausted
		}
	}
	remaining := before
	dropped := 0
	arithmeticOK := true
	dropKind := func(kind compactionGroupKind) {
		for i := range groups {
			if !arithmeticOK || remaining <= b.Input {
				return
			}
			if groups[i].kind == kind {
				groups[i].drop = true
				remaining, arithmeticOK = checkedTokenSub(remaining, groups[i].tokens)
				dropped++
			}
		}
	}

	// Prior session history is context only, so it yields before current-run
	// tool observations. Completed tool-call chains are verbose and
	// reconstructable from later assistant observations, so they yield before
	// current-run plain Elastic messages.
	dropKind(groupHistory)
	dropKind(groupCompletedTool)
	dropKind(groupPlainElastic)
	if !arithmeticOK {
		return st, CompactionReport{Strategy: "recency", DroppedCount: dropped, TokensBefore: before, TokensAfter: math.MaxInt}, ErrContextExhausted
	}

	out := State{System: st.System}
	for _, g := range groups {
		if !g.drop {
			out.Messages = append(out.Messages, g.msgs...)
		}
	}

	after, ok := rc.checkedTotal(out)
	if !ok {
		return out, CompactionReport{Strategy: "recency", DroppedCount: dropped, TokensBefore: before, TokensAfter: math.MaxInt}, ErrContextExhausted
	}
	return out, CompactionReport{
		Strategy:     "recency",
		DroppedCount: dropped,
		TokensBefore: before,
		TokensAfter:  after,
	}, nil
}
