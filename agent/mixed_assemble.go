package agent

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/kstruzzieri/go-llm/contextdepth"
)

// ErrMixedCompactor rejects a ContextManager that opts into mixed assembly AND
// supplies a custom Compactor. The two are alternative assembly strategies for
// the same messages, so the combination has no defined meaning.
//
// It is unconditional: a manager configured this way is wrong whatever any one
// request happens to carry. Gating it on the presence of structured anchors
// would make the failure intermittent and data-dependent — a misconfigured
// consumer would run fine until the first tool attached a ContextSet.
var ErrMixedCompactor = errors.New("agent: ContextManager.Mixed is incompatible with a custom Compactor")

// ContextAssemblyTrace is the per-turn record of what mixed assembly selected,
// rendered and omitted (#331 spec 3.4). Legacy and no-anchor assembly return the
// ZERO value and emit no event; a successful mixed assembly always carries a
// non-nil Subjects slice, which is how a consumer tells the two apart.
//
// It is CONTENT-FREE by construction: descriptors, depths, kinds, costs and a
// fixed decision/reason vocabulary, never rendered block text. Content-free is
// not privacy-free — subject IDs are source paths, memory record IDs and
// conversation indices, and ToolCallID identifies a call — so a consumer must not
// treat a trace as anonymous telemetry.
//
// MaxTokens is the state budget net of tool schemas, and
// EstimatedTokensUsed + EstimatedTokensFree == MaxTokens on every trace this
// returns. Per-subject EstimatedTokens cover each subject's OWN content only.
// Counted in EstimatedTokensUsed and in NO row: message envelopes, the system
// prompt, pinned reservations, the separators between an anchor's fragments,
// and the omission placeholder allocateChain precharges every structured
// anchor with. An admission replaces that placeholder charge with an exact
// delta, so it survives in the total only where it survives in the output —
// a retained anchor whose subjects were ALL omitted, whose rows are then every
// one of them a zero-cost omission while its placeholder text is model-visible
// and paid for. The rows therefore deliberately do not sum to the total.
type ContextAssemblyTrace struct {
	MaxTokens           int
	EstimatedTokensUsed int
	EstimatedTokensFree int
	SelectedSubjects    int
	RenderedSubjects    int
	OmittedSubjects     int
	// VerbatimShortfalls counts anchors whose MinVerbatim preference went unmet,
	// evicted chains included.
	VerbatimShortfalls int
	// Subjects are in stable input order: units in State order, and within a
	// completed chain the chain span followed by its anchors' subjects in group
	// order. The pinned durable summary, when present, is the first row.
	Subjects []ContextSubjectTrace
}

// ContextSubjectTrace is one subject's outcome. Omitted rows carry
// DepthInvalid, zero costs, no representations and a fixed OmissionReason;
// rendered rows carry the chosen alternative's descriptor and a
// base/floor/upgrade Decision with no reason (spec 3.4 D6).
type ContextSubjectTrace struct {
	Subject    contextdepth.SubjectRef
	ToolCallID string `json:",omitempty"` // structured anchor subjects only; "" for conversation
	Lane       int
	// Rank is the producing domain's own rank (ContextGroup's GroupDesc.Rank).
	// It is 0 for every assembler-owned conversation subject, including the
	// durable summary: conversation order IS the rank, and the subject ID — the
	// decimal index of the span's first caller-visible message — already is the
	// ordering key. A synthetic 1-based index here would be a second, driftable
	// source of truth for the same fact.
	Rank            int
	Omitted         bool
	EffectiveDepth  contextdepth.Depth
	Representations []contextdepth.RepresentationDesc
	EstimatedTokens int
	// Bytes is model-visible Content bytes — the quantity the anchor byte cap
	// bounds. It is NOT a byte rendering of EstimatedTokens: the token figure
	// also covers the envelope fields messageCost charges (tool-call id/type/
	// name/arguments, ToolName, ToolCallID), which carry no Content bytes. A
	// chain span whose only text lives in its anchors is the visible case:
	// EstimatedTokens 30, Bytes 0.
	Bytes          int
	Decision       string
	OmissionReason string
}

// Implicit alternatives for the two assembler-owned subject kinds (spec 3.5).
// A conversation span is retained whole or not at all, so a retained one is
// verbatim evidence at L2; the durable summary is a stored compact rendering at
// L1. Rows clone these, so a trace consumer cannot mutate them.
var (
	spanRepresentations    = []contextdepth.RepresentationDesc{{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim}}
	summaryRepresentations = []contextdepth.RepresentationDesc{{Depth: contextdepth.DepthL1, Kind: contextdepth.RepresentationCompact}}
)

// hasStructuredAnchor reports whether any message carries a structured payload.
// It is the mixed-mode gate: a State with no payload has nothing to allocate, so
// it takes the legacy path with its model-visible bytes unchanged. It inspects
// EVERY message rather than just completed tool chains, because a set riding a
// pinned span or an unresolved chain still has to be stripped from the
// materialized copy (spec 4.2).
func hasStructuredAnchor(st State) bool {
	for _, msg := range st.Messages {
		if msg.Context != nil {
			return true
		}
	}
	return false
}

// AssembleWithTrace bounds the transcript to fit the budget and reports what
// mixed assembly decided. It is the full form of Assemble, which discards the
// trace; the returned Pressure is populated on every path Assemble populates it.
//
// The trace is the ZERO value whenever mixed assembly did not run: Mixed off, no
// structured anchors, or any error.
func (m ContextManager) AssembleWithTrace(ctx context.Context, st State, toolSchemaTokens int, budget TokenBudget) (State, Pressure, ContextAssemblyTrace, error) {
	// Checked BEFORE the anchor short-circuit. Ordering it after would let a
	// misconfigured manager take the legacy path on requests that happen to
	// carry no structured payload, so the configuration error would surface
	// intermittently, driven by tool output rather than by configuration.
	if m.Mixed && m.Compactor != nil {
		return st, Pressure{}, ContextAssemblyTrace{}, ErrMixedCompactor
	}
	if !m.Mixed || !hasStructuredAnchor(st) {
		out, pressure, err := m.assembleLegacy(ctx, st, toolSchemaTokens, budget)
		return out, pressure, ContextAssemblyTrace{}, err
	}
	return m.assembleMixed(ctx, st, toolSchemaTokens, budget)
}

// assembleMixed runs the structured pipeline: build candidates, allocate,
// materialize, trace (#331 spec 4.1).
func (m ContextManager) assembleMixed(ctx context.Context, st State, toolSchemaTokens int, budget TokenBudget) (State, Pressure, ContextAssemblyTrace, error) {
	// Value receiver: wrapping the estimator ONCE, here, is what makes the ledger
	// exact. buildMixedUnits' span and envelope costs (via messageCost), the
	// allocator's deltas and the final recompute all read this one field, so
	// there is no second estimator channel to drift out of alignment.
	m.Estimate = guardedEstimate(m.Estimate)
	thresholds := budget.Thresholds.normalize()

	hadSummary := strings.TrimSpace(st.DurableSummary) != ""
	stMat := materializeDurableSummary(st)

	pinned := m.pinnedTokens(stMat, toolSchemaTokens)
	if pinned > budget.Input {
		return stMat, Pressure{
			UsedPct:     usedFraction(pinned, budget.Input),
			InputTokens: pinned,
			InputBudget: budget.Input,
			Level:       LevelCritical,
			Cause:       m.pinnedOverflowCause(stMat, toolSchemaTokens),
			Mitigation:  MitigationHalt,
		}, ContextAssemblyTrace{}, ErrContextExhausted
	}

	// Units come from the PRE-materialization State. materializeDurableSummary
	// prepends a pinned message at index 0, which makes firstPinnedIndex return 0
	// and silently reclassifies every prior-history exchange as a current-run
	// plain one — buildMixedUnits' documented precondition.
	units, err := m.buildMixedUnits(st)
	if err != nil {
		return stMat, Pressure{}, ContextAssemblyTrace{}, err
	}

	// sysTokens covers only what the units do NOT represent: the system prompt
	// and the materialized durable summary. Pinned MESSAGES are unitPinned units
	// charged in the allocator's must-fit step, so adding pinnedTokens here would
	// double-charge the whole pinned span and manufacture an exhaustion.
	sysTokens := m.estimate(stMat.System)
	var summary *Message
	if hadSummary {
		// materializeDurableSummary prepends the summary under the same
		// non-blank test hadSummary used, so index 0 is it. Copied rather than
		// pointed at, so nothing downstream can reach stMat.Messages.
		msg := stMat.Messages[0]
		summary = &msg
		sysTokens += m.messageCost(msg)
	}

	stateBudget := budget.Input - toolSchemaTokens
	alloc, err := m.allocateMixed(ctx, units, stateBudget, sysTokens)
	if err != nil {
		// Must-fit overflow. alloc.used is the FULL reservation cost — pinned
		// messages, the durable summary AND unresolved chains — so the pressure
		// row reports what actually did not fit rather than the pinned subset
		// (spec 5).
		reserved := alloc.used + toolSchemaTokens
		return stMat, Pressure{
			UsedPct:     usedFraction(reserved, budget.Input),
			InputTokens: reserved,
			InputBudget: budget.Input,
			Level:       LevelCritical,
			Cause:       m.mustFitCause(stMat, units, toolSchemaTokens, alloc.used),
			Mitigation:  MitigationHalt,
		}, ContextAssemblyTrace{}, err
	}

	out := materializeMixed(stMat.System, summary, units)
	used := m.totalTokens(out, 0)
	after := used + toolSchemaTokens
	// Both causes shed model-visible content, so both drive the mitigation and
	// the compaction event. They stay separate COUNTS: folding anchor omissions
	// into Evicted would report "5 groups evicted" for one retained anchor that
	// lost five of its sources, which is a different and much worse fact.
	shed := alloc.evictedGroups > 0 || alloc.anchorOmissions > 0
	compactions := 0
	if shed {
		compactions = 1
	}
	exhausted := after > budget.Input
	usedPct := usedFraction(after, budget.Input)
	level, mitigation := thresholds.Classify(usedPct, exhausted, shed)
	pressure := Pressure{
		UsedPct:         usedPct,
		Evicted:         alloc.evictedGroups,
		Compactions:     compactions,
		AnchorOmissions: alloc.anchorOmissions,
		InputTokens:     after,
		InputBudget:     budget.Input,
		Level:           level,
		Cause:           m.dominantCause(out, toolSchemaTokens),
		Mitigation:      mitigation,
	}
	if exhausted {
		// Unreachable for a pure estimator: exact-delta accounting makes this
		// recompute equal the admission ledger, which allocateMixed kept inside
		// stateBudget. An estimator that re-priced the same text could still land
		// here, and failing closed beats returning a trace whose
		// EstimatedTokensUsed + EstimatedTokensFree == MaxTokens identity is a lie.
		return out, pressure, ContextAssemblyTrace{}, ErrContextExhausted
	}
	return out, pressure, m.fillMixedTrace(units, summary, alloc, stateBudget, used), nil
}

// mustFitCause attributes a must-fit overflow across the THREE buckets mixed
// assembly reserves from: pinned spans, tool schemas, and unresolved tool
// chains. The third is why this exists rather than reusing pinnedOverflowCause
// alone — an unresolved chain is a new must-fit reservation on a new path, and
// reporting a 450-token tool tail as CausePinned points the operator at the
// wrong lever.
//
// Both sides of the comparison are WHOLE-SPAN reservation cost: allocUsed is the
// allocator's ledger (sysTokens + every pinned and unresolved unit), so
// allocUsed-unresolved is the pinned remainder measured exactly as unresolved
// is. Weighing it against pinnedTokens instead would compare a span total to a
// pinned-MESSAGE total, and those differ: chainAt bundles an Elastic tool result
// under a Pinned assistant into one span and classifyGroup calls the whole span
// pinned, so the pinned reservation would be understated and a smaller
// unresolved chain would win. Ties resolve to the pinned/schema split, matching
// dominantCause's earliest-bucket precedence.
func (m ContextManager) mustFitCause(stMat State, units []*mixedUnit, toolSchemaTokens, allocUsed int) PressureCause {
	unresolved := 0
	for _, u := range units {
		if u.kind == unitUnresolved {
			unresolved += u.baseTokens
		}
	}
	if unresolved > toolSchemaTokens && unresolved > allocUsed-unresolved {
		return CauseToolOutput
	}
	return m.pinnedOverflowCause(stMat, toolSchemaTokens)
}

// materializeMixed renders the assembled copy: the pinned durable summary (when
// present) followed by every retained unit's messages, with each structured
// anchor's Content replaced by the exact string the allocator charged.
//
// It NEVER writes through mixedUnit.msgs, which ALIASES the caller's
// State.Messages. Every message is range-copied and the copy is what gets
// rewritten, so the canonical State — and Result.Messages with it — keeps the
// original anchors and their fallback Content (spec 4.2).
func materializeMixed(system string, summary *Message, units []*mixedUnit) State {
	out := State{System: system}
	if summary != nil {
		out.Messages = append(out.Messages, detachContext(*summary))
	}
	for _, u := range units {
		if materializeSkip(u) {
			continue
		}
		for i, msg := range u.msgs {
			if a := anchorFor(u, i); a != nil {
				// a.content is anchorJoined's output for the committed
				// assignment — the same function, and therefore the same bytes,
				// admission was charged for.
				msg.Content = a.content
				msg.Attrib = anchorAttrib(a)
			}
			out.Messages = append(out.Messages, detachContext(msg))
		}
	}
	return out
}

// materializeSkip reports whether a unit contributes no messages at all.
func materializeSkip(u *mixedUnit) bool {
	switch u.kind {
	case unitPlainSpan, unitHistorySpan:
		return u.subject.omitted
	case unitChain:
		// An evicted chain's anchors still hold the omission placeholder
		// allocateChain pre-assigned BEFORE its fit check — content that was
		// never charged to the ledger. Emitting it would put model-visible text
		// behind no budget and break the trace's Used+Free==MaxTokens identity.
		return u.evicted
	default:
		// unitPinned and unitUnresolved are must-fit: never dropped.
		return false
	}
}

// detachContext returns msg with the runtime-only structured payload removed.
// msg is a VALUE parameter, so the result never aliases State.Messages. Spec 4.2
// requires the materialized copy carry no runtime Context: it is assembly input,
// not transcript, and a set riding a pinned span or an unresolved chain never
// passed through validateContextSet at all (spec 4.3 validates completed chains
// only), so it must not travel any further.
func detachContext(msg Message) Message {
	msg.Context = nil
	return msg
}

// anchorFor returns the structured anchor at index i of the unit's messages, or
// nil when that message is an ordinary one — including a tool result with no
// Context, which stays byte-identical to legacy mode even in mixed mode. Linear
// because a chain holds one anchor per parallel tool call: single digits.
func anchorFor(u *mixedUnit, i int) *mixedAnchor {
	for _, a := range u.anchors {
		if a.msgIdx == i {
			return a
		}
	}
	return nil
}

// anchorAttrib rebuilds a materialized anchor's attribution from the CHOSEN
// alternatives only (spec 4.2). The fallback ToolResult.Attrib is legacy-mode
// truth: in mixed mode it would credit sources whose evidence was not admitted.
// Nil when nothing chosen carries attribution.
func anchorAttrib(a *mixedAnchor) *RetrievalAttribution {
	var sources []RetrievedSource
	for _, s := range a.subjects {
		if s.chosen < 0 {
			continue
		}
		if at := s.alts[s.chosen].Attrib; at != nil {
			sources = append(sources, at.Sources...)
		}
	}
	if len(sources) == 0 {
		return nil
	}
	return &RetrievalAttribution{Sources: sources}
}

// fillMixedTrace renders the trace from the same structures materialization
// read. used is the recompute of the materialized copy, which equals the
// admission ledger for any pure estimator (spec 4.1 exact-delta accounting).
func (m ContextManager) fillMixedTrace(units []*mixedUnit, summary *Message,
	alloc *mixedAllocState, maxTokens, used int) ContextAssemblyTrace {

	tr := ContextAssemblyTrace{
		MaxTokens:           maxTokens,
		EstimatedTokensUsed: used,
		EstimatedTokensFree: maxTokens - used,
		VerbatimShortfalls:  alloc.verbatimShortfalls,
		// Always non-nil, even with no subjects: a nil slice marshals as null,
		// and the orchestrator's zero-trace guard keys on Subjects != nil.
		Subjects: make([]ContextSubjectTrace, 0, len(units)),
	}
	if summary != nil {
		// The summary is a must-fit reservation, never omitted, and sits outside
		// the lanes; it is traced in lane 0 (spec 4.1).
		tr.Subjects = append(tr.Subjects, ContextSubjectTrace{
			Subject:         contextdepth.SubjectRef{Domain: contextdepth.DomainConversation, ID: durableSummarySubjectID},
			Lane:            lanePlain,
			EffectiveDepth:  contextdepth.DepthL1,
			Representations: slices.Clone(summaryRepresentations),
			EstimatedTokens: m.messageCost(*summary),
			Bytes:           len(summary.Content),
			Decision:        DecisionBase,
		})
	}
	for _, u := range units {
		if u.subject != nil {
			tr.Subjects = append(tr.Subjects, spanRow(u))
		}
		for _, a := range u.anchors {
			for _, s := range a.subjects {
				tr.Subjects = append(tr.Subjects, m.anchorRow(s))
			}
		}
	}
	tr.SelectedSubjects = len(tr.Subjects)
	for _, s := range tr.Subjects {
		if s.Omitted {
			tr.OmittedSubjects++
		} else {
			tr.RenderedSubjects++
		}
	}
	return tr
}

// spanRow traces one whole-span conversation subject.
func spanRow(u *mixedUnit) ContextSubjectTrace {
	s := u.subject
	row := newSubjectRow(s)
	if s.omitted {
		return row
	}
	row.EffectiveDepth = contextdepth.DepthL2
	row.Representations = slices.Clone(spanRepresentations)
	row.EstimatedTokens = s.spanTokens
	row.Bytes = spanBytes(u)
	return row
}

// anchorRow traces one structured-anchor subject at the alternative admission
// settled on.
func (m ContextManager) anchorRow(s *mixedSubject) ContextSubjectTrace {
	row := newSubjectRow(s)
	// chosen < 0 without omitted would be an allocator bug; guarding here keeps
	// it a visibly wrong row (DepthInvalid with a non-omitted decision, which the
	// D6 assertions catch) instead of an index panic.
	if s.omitted || s.chosen < 0 {
		return row
	}
	alt := s.alts[s.chosen]
	row.EffectiveDepth = alt.Desc.Depth()
	row.Representations = slices.Clone(alt.Desc.Representations)
	row.EstimatedTokens = m.estimate(alt.Content)
	row.Bytes = len(alt.Content)
	return row
}

// newSubjectRow fills the fields every row carries however the subject was
// decided. Representations starts as an EMPTY slice, not nil, so an omitted row
// still marshals [].
func newSubjectRow(s *mixedSubject) ContextSubjectTrace {
	return ContextSubjectTrace{
		Subject:         s.ref,
		ToolCallID:      s.toolCallID,
		Lane:            s.lane,
		Rank:            s.rank,
		Omitted:         s.omitted,
		Decision:        s.decision,
		OmissionReason:  s.reason,
		Representations: []contextdepth.RepresentationDesc{},
	}
}

// spanBytes is the model-visible content bytes the span itself contributes. An
// ANCHOR's Content is allocated, so its bytes belong to its own subject rows —
// mirroring chainAnchors, which charges the anchor's envelope with Content
// emptied. The test is anchor membership, not merely a non-nil Context: a set
// riding a message no anchor was built for (an orphan tool result grouped as a
// plain span) is charged verbatim, so its bytes are the span's.
func spanBytes(u *mixedUnit) int {
	n := 0
	for i, msg := range u.msgs {
		if anchorFor(u, i) != nil {
			continue
		}
		n += len(msg.Content)
	}
	return n
}
