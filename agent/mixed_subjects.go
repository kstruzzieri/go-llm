package agent

import (
	"fmt"
	"strconv"

	"github.com/kstruzzieri/go-llm/contextdepth"
)

// Mixed-assembly trace vocabulary (#331 spec 3.4). Exported because trace
// consumers compare against these exact strings. They are fixed constants,
// never payload-derived: a decision or reason that varied with content would
// make traces unaggregatable.
const (
	// DecisionBase marks the cheapest measured alternative admitted for a
	// subject during base admission.
	DecisionBase = "base"
	// DecisionFloor marks an alternative admitted to meet an anchor's
	// preferred verbatim-component count (ContextSet.MinVerbatim).
	DecisionFloor = "floor"
	// DecisionUpgrade marks a later-declared alternative admitted with budget
	// left over after base admission.
	DecisionUpgrade = "upgrade"
	// DecisionOmitted marks a selected subject that reached no alternative.
	DecisionOmitted = "omitted"

	// OmitTokenBudget: the subject's cheapest alternative did not fit the
	// remaining token budget.
	OmitTokenBudget = "token_budget"
	// OmitByteCap: the subject's cheapest alternative would breach the
	// owning anchor's model-visible byte cap.
	OmitByteCap = "byte_cap"
	// OmitChainEvicted: the whole tool chain owning the subject was evicted,
	// so none of its subjects could be rendered.
	OmitChainEvicted = "chain_evicted"
)

// omittedObservation is the fixed minimal content a retained anchor carries
// when every one of its subjects was omitted (#331 spec 4.2).
const omittedObservation = "context omitted for budget"

// durableSummarySubjectID names the pinned durable-summary subject in traces.
const durableSummarySubjectID = "durable-summary"

// Retention lanes, derived from — not invented alongside — RecencyCompactor's
// eviction order. Compact drops groupHistory, then groupCompletedTool, then
// groupPlainElastic (agent/compactor.go), so retention priority is the
// reverse: plain > tool chains > history. Smaller lane number = retained
// earlier. Producer GroupDesc.Lane values are deliberately ignored; there is
// no lane-tuning API (#331 spec 4.1).
const (
	lanePlain   = 0 // current-run plain elastic exchanges
	laneTool    = 1 // current-run tool context (structured anchors and chain spans)
	laneHistory = 2 // prior raw-history exchanges
)

// mixedUnitKind classifies one atomic message group for mixed assembly. The
// kinds map 1:1 onto the compactor's compactionGroupKind so the two cannot
// disagree about what is droppable.
type mixedUnitKind int

const (
	unitPinned mixedUnitKind = iota
	unitUnresolved
	unitPlainSpan
	unitHistorySpan
	unitChain
)

// mixedSubject is one allocatable subject: either a structured anchor group
// (alts set) or a whole conversation span (span=true). Fields past alts are
// written by the allocator, not the builder.
type mixedSubject struct {
	ref        contextdepth.SubjectRef
	toolCallID string // producing tool call; "" for conversation spans
	lane       int
	rank       int
	alts       []ContextAlternative
	span       bool
	spanTokens int
	chosen     int // index into alts; -1 = none
	omitted    bool
	decision   string
	reason     string
}

// mixedAnchor is one structured tool-result message within a chain.
// content/tokens carry the exact-delta bookkeeping the allocator uses:
// content starts as the omission placeholder (precharged) and every admission
// replaces it, so admission cost always equals the final measured cost
// (#331 spec 4.1). The builder leaves them zero.
type mixedAnchor struct {
	msgIdx      int // index into the owning unit's msgs
	callID      string
	set         *ContextSet
	cap         int // Message.OutputCap: bound on final model-visible Content bytes
	minVerbatim int
	verbatimGot int
	content     string
	tokens      int
	subjects    []*mixedSubject
}

// mixedUnit is one atomic message group in State order. msgs ALIASES the input
// State.Messages span — the builder never copies or mutates a message, so a
// consumer that rewrites Content (materialization) must copy first or it
// mutates the canonical State (#331 spec 4.2).
type mixedUnit struct {
	kind       mixedUnitKind
	msgs       []Message
	baseTokens int
	anchors    []*mixedAnchor
	subject    *mixedSubject // plain/history spans only
	evicted    bool          // chains
}

// buildMixedUnits walks st.Messages with the compactor's own grouping helpers
// and returns the allocation units in stable input order.
//
// PRECONDITION — st MUST be the PRE-materialization State (the caller-visible
// one, with DurableSummary still a field). materializeDurableSummary prepends
// the summary as a PINNED message at index 0, which makes firstPinnedIndex
// return 0; priorHistoryGroup requires end < firstPinned, so every prior-history
// exchange would then be classified as a current-run plain exchange and its
// conversation subject ID would be shifted by one. The history lane silently
// disappears — nothing at the call site reveals the mistake. The caller
// materializes the summary separately and charges it as a pinned reservation
// (#331 spec 4.1 step 1).
func (m ContextManager) buildMixedUnits(st State) ([]*mixedUnit, error) {
	units := make([]*mixedUnit, 0, len(st.Messages))
	firstPinned := firstPinnedIndex(st.Messages)
	for i := 0; i < len(st.Messages); {
		end := chainAt(st.Messages, i)
		if end == i && pairableExchange(st.Messages, i) {
			end = i + 1 // the user->assistant exchange is allocated atomically
		}
		span := st.Messages[i : end+1]
		u := &mixedUnit{msgs: span}
		// Verbatim cost of the whole span. Correct for every unit EXCEPT a
		// completed chain, whose structured anchors' content is allocated
		// rather than charged, so the chain branch discards it and computes
		// its own base. It is therefore ASSIGNED per branch, never up front:
		// a wrong baseTokens living in the struct until a later overwrite is
		// one stray early return away from shipping.
		verbatim := m.spanCost(span)
		switch classifyGroup(st.Messages, i, end, firstPinned) {
		case groupPinned:
			u.kind, u.baseTokens = unitPinned, verbatim
		case groupUnresolvedTool:
			u.kind, u.baseTokens = unitUnresolved, verbatim
		case groupPlainElastic:
			u.kind, u.baseTokens = unitPlainSpan, verbatim
			u.subject = spanSubject(i, lanePlain, verbatim)
		case groupHistory:
			u.kind, u.baseTokens = unitHistorySpan, verbatim
			u.subject = spanSubject(i, laneHistory, verbatim)
		case groupCompletedTool:
			u.kind = unitChain
			if err := validateChainBijection(span); err != nil {
				return nil, err
			}
			anchors, base, err := m.chainAnchors(span)
			if err != nil {
				return nil, err
			}
			u.anchors, u.baseTokens = anchors, base
		}
		units = append(units, u)
		i = end + 1
	}
	return units, nil
}

// spanSubject builds the conversation subject for one whole span. The ID is
// the decimal index of the span's FIRST message in the caller-visible
// State.Messages (#331 spec 3.4); Rank is unused because conversation order
// IS the rank.
func spanSubject(firstMsg, lane, tokens int) *mixedSubject {
	return &mixedSubject{
		ref:        contextdepth.SubjectRef{Domain: contextdepth.DomainConversation, ID: strconv.Itoa(firstMsg)},
		lane:       lane,
		span:       true,
		spanTokens: tokens,
		chosen:     -1,
	}
}

// validateChainBijection checks that a completed chain's assistant call IDs
// and tool results are in bijection. classifyGroup's completion test counts
// results only (agent/compactor.go), so it cannot see a duplicate or unknown
// call ID — and mixed assembly keys anchors by call ID, which requires exactly
// one result per call (#331 spec 4.3).
func validateChainBijection(span []Message) error {
	ids := map[string]bool{}
	for _, tc := range span[0].ToolCalls {
		if tc.ID == "" {
			return fmt.Errorf("agent: mixed assembly: assistant call with blank tool-call ID")
		}
		if ids[tc.ID] {
			return fmt.Errorf("agent: mixed assembly: duplicate tool-call ID %q in one assistant message", tc.ID)
		}
		ids[tc.ID] = true
	}
	answered := map[string]bool{}
	for j := 1; j < len(span); j++ {
		id := span[j].ToolCallID
		if id == "" || !ids[id] {
			return fmt.Errorf("agent: mixed assembly: tool result with unknown or blank call ID %q", id)
		}
		if answered[id] {
			return fmt.Errorf("agent: mixed assembly: multiple tool results for call ID %q", id)
		}
		answered[id] = true
	}
	return nil
}

// chainAnchors extracts the structured anchors of one completed chain and
// returns the chain's base footprint: the assistant call, every unstructured
// tool result verbatim, and every structured anchor's ENVELOPE — the message
// with Content emptied, because an anchor's content is allocated, not assumed
// (#331 spec 4.1 step 4).
//
// Duplicate subjects are NOT re-checked here. Chain bijection gives one tool
// result per call ID, so all groups of one ContextSet share one call ID and
// the spec's (ToolCallID, Domain, ID) triple is fully determined within the
// set — validateContextSet owns it. Duplicates ACROSS calls are legal.
func (m ContextManager) chainAnchors(span []Message) ([]*mixedAnchor, int, error) {
	base := m.messageCost(span[0])
	var anchors []*mixedAnchor
	for j := 1; j < len(span); j++ {
		msg := span[j]
		if msg.Context == nil {
			base += m.messageCost(msg) // ordinary tool result: verbatim, as today
			continue
		}
		if err := validateContextSet(msg.ToolCallID, msg.Context); err != nil {
			return nil, 0, err
		}
		envelope := msg
		envelope.Content = ""
		base += m.messageCost(envelope)

		a := &mixedAnchor{
			msgIdx:      j,
			callID:      msg.ToolCallID,
			set:         msg.Context,
			cap:         msg.OutputCap,
			minVerbatim: msg.Context.MinVerbatim,
		}
		for _, g := range msg.Context.Groups {
			a.subjects = append(a.subjects, &mixedSubject{
				ref:        g.Desc.Subject,
				toolCallID: msg.ToolCallID,
				lane:       laneTool,
				rank:       g.Desc.Rank,
				alts:       g.Alternatives,
				chosen:     -1,
			})
		}
		anchors = append(anchors, a)
	}
	return anchors, base, nil
}

// spanCost is the messageCost of a whole message span.
func (m ContextManager) spanCost(span []Message) int {
	n := 0
	for _, msg := range span {
		n += m.messageCost(msg)
	}
	return n
}
