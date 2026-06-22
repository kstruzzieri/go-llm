package agent

import "context"

// TokenBudget is the input-token budget available to the State messages
// (system + transcript), already net of tool-schema tokens.
type TokenBudget struct {
	Input int
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
}

func (rc RecencyCompactor) estimate(s string) int {
	if rc.Estimate != nil {
		return rc.Estimate(s)
	}
	return (len(s) + 3) / 4
}

func (rc RecencyCompactor) messageCost(m Message) int {
	n := rc.estimate(m.Content)
	for _, tc := range m.ToolCalls {
		n += rc.estimate(tc.ID)
		n += rc.estimate(tc.Type)
		n += rc.estimate(tc.Function.Name)
		n += rc.estimate(string(tc.Function.Arguments))
	}
	n += rc.estimate(m.ToolName)
	n += rc.estimate(m.ToolCallID)
	return n
}

func (rc RecencyCompactor) total(st State) int {
	n := rc.estimate(st.System)
	for _, m := range st.Messages {
		n += rc.messageCost(m)
	}
	return n
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
	msgs   []Message
	tokens int
	kind   compactionGroupKind
	drop   bool
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
		for _, m := range st.Messages[i : end+1] {
			tokens += rc.messageCost(m)
		}
		out = append(out, compactionGroup{
			msgs:   st.Messages[i : end+1],
			tokens: tokens,
			kind:   classifyGroup(st.Messages, i, end, firstPinned),
		})
		i = end + 1
	}
	return out
}

func (rc RecencyCompactor) Compact(_ context.Context, st State, b TokenBudget) (State, CompactionReport, error) {
	before := rc.total(st)
	if before <= b.Input {
		return st, CompactionReport{Strategy: "recency", TokensBefore: before, TokensAfter: before}, nil
	}

	groups := rc.groups(st)
	remaining := before
	dropped := 0
	dropKind := func(kind compactionGroupKind) {
		for i := range groups {
			if remaining <= b.Input {
				return
			}
			if groups[i].kind == kind {
				groups[i].drop = true
				remaining -= groups[i].tokens
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

	out := State{System: st.System}
	for _, g := range groups {
		if !g.drop {
			out.Messages = append(out.Messages, g.msgs...)
		}
	}

	after := rc.total(out)
	return out, CompactionReport{
		Strategy:     "recency",
		DroppedCount: dropped,
		TokensBefore: before,
		TokensAfter:  after,
	}, nil
}
