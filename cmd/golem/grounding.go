package main

import (
	"context"
	"sync"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/rag"
)

const (
	// groundingEvidenceMaxBytes bounds retained evidence per TURN. Grounding
	// only ever judges one turn's final prompt, so nothing older needs to
	// survive beginTurn.
	groundingEvidenceMaxBytes = 8 << 20
	// groundingEvidenceEntryOverhead is a conservative per-entry charge for the
	// map entry and string headers the byte accounting cannot see directly. It
	// makes the cap an over-estimate of real retention rather than an
	// under-estimate.
	groundingEvidenceEntryOverhead = 128
)

// evidenceKey identifies one retrieved chunk using only what the presentation
// event carries. rag.RenderedEvidence documents StableKey as "may be blank", so
// the span is part of the identity and never the key alone.
type evidenceKey struct {
	stableKey string
	source    string
	startLine int
	endLine   int
}

// evidenceEntry is the minimal projection analysis.buildEvidenceBlocks needs:
// chunk id, source, span, content. Metadata, language, and stable key are
// deliberately not retained as payload — they are never read downstream and
// would hold a map per chunk for the whole turn.
//
// ambiguous marks a key that was recorded twice with different chunk identity
// or content. Such a key is unresolvable: substituting either candidate would
// judge claims against text the model may not have received.
type evidenceEntry struct {
	id        string
	source    string
	startLine int
	endLine   int
	content   string
	ambiguous bool
}

// evidenceRecorder keeps the chunk text of what golem's retriever returned
// during the CURRENT turn, so grounding can join post-assembly attribution
// (identity only) back to the evidence text analysis.SupportJudge needs.
// Allocated only when -grounding is active.
//
// ponytail: turn-scoped on purpose. session.history() rebuilds model history
// from user/assistant Content alone, so no attributed tool message — and
// therefore no attribution — crosses a turn boundary. That also bounds the
// exactness of the local {StableKey, Source, StartLine, EndLine} key: it is not
// globally unique, so a same-turn collision fails closed rather than guessing.
// If golem ever persists attributed tool messages across turns, stop resetting
// and carry rag.RenderedEvidence.ChunkID through agent.RetrievedSource instead,
// which makes the join direct and drops the collision case entirely.
//
// Retriever calls run from parallel read-only dispatch goroutines (#235), so
// every operation is mutex-guarded.
type evidenceRecorder struct {
	mu       sync.Mutex
	entries  map[evidenceKey]evidenceEntry
	bytes    int
	maxBytes int
}

func newEvidenceRecorder(maxBytes int) *evidenceRecorder {
	return &evidenceRecorder{entries: make(map[evidenceKey]evidenceEntry), maxBytes: maxBytes}
}

// beginTurn drops the previous turn's evidence. Called before Runtime.Run.
func (r *evidenceRecorder) beginTurn() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = make(map[evidenceKey]evidenceEntry)
	r.bytes = 0
}

// record stores the minimal projection of each result. A result whose charge
// would exceed the cap is skipped individually: abandoning the rest of the
// batch would make retention depend on result order.
func (r *evidenceRecorder) record(results []rag.SearchResult) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, res := range results {
		c := res.Chunk
		key := evidenceKey{c.StableKey, c.Source, c.StartLine, c.EndLine}
		next := evidenceEntry{
			id: c.ID, source: c.Source, startLine: c.StartLine,
			endLine: c.EndLine, content: c.Content,
		}
		if prev, seen := r.entries[key]; seen {
			// Already unresolvable, or the same bytes again: nothing to charge.
			if prev.ambiguous || (prev.id == next.id && prev.content == next.content) {
				continue
			}
			prev.ambiguous = true
			r.entries[key] = prev
			continue
		}
		charge := len(c.Content) + len(c.ID) + len(c.Source) + len(c.StableKey) + groundingEvidenceEntryOverhead
		if r.bytes+charge > r.maxBytes {
			continue
		}
		r.entries[key] = next
		r.bytes += charge
	}
}

// lookup resolves one presented identity to its chunk. ok is false when the
// identity was never recorded, was refused by the cap, or is ambiguous — all
// three mean the same thing to the caller: this turn's evidence cannot be
// reconstructed exactly, so no verdict may be produced.
func (r *evidenceRecorder) lookup(s agent.RetrievedSource) (rag.Chunk, bool) {
	if r == nil {
		return rag.Chunk{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[evidenceKey{s.StableKey, s.Source, s.StartLine, s.EndLine}]
	if !ok || e.ambiguous {
		return rag.Chunk{}, false
	}
	return rag.Chunk{
		ID: e.id, Source: e.source, StartLine: e.startLine,
		EndLine: e.endLine, Content: e.content,
	}, true
}

// groundingRetriever is exactly the capability set agenttools.Retrieve requires
// of golem's concrete retriever: its unexported retriever, legacyRenderedRetriever,
// and progressiveRetriever interfaces combined. Naming it here makes the wrapper's
// obligation compile-checked — dropping a method would otherwise silently
// downgrade the tool to the legacy path (or fail every progressive call) with no
// build error, because those interfaces live in another package and are unexported.
type groundingRetriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]rag.SearchResult, error)
	BuildContext(results []rag.SearchResult, maxTokens int) string
	BuildContextWithRenderedCount(results []rag.SearchResult, maxTokens int) (string, int)
	RenderProgressiveWithGroups(ctx context.Context, req rag.ProgressiveRenderRequest) (string, rag.ProgressiveTrace, []rag.ProgressiveGroup, error)
}

var _ groundingRetriever = (*rag.Retriever)(nil)

// recordingRetriever forwards every capability untouched and records the
// results on the way past.
type recordingRetriever struct {
	inner groundingRetriever
	rec   *evidenceRecorder
}

var _ groundingRetriever = (*recordingRetriever)(nil)

// Retrieve records at most the requested k prefix. agenttools.Retrieve
// truncates an over-returning retriever to k before rendering, so recording
// the full return would retain evidence the model never received.
func (w *recordingRetriever) Retrieve(ctx context.Context, query string, k int) ([]rag.SearchResult, error) {
	results, err := w.inner.Retrieve(ctx, query, k)
	if err != nil {
		return nil, err
	}
	recorded := results
	if k > 0 && len(recorded) > k {
		recorded = recorded[:k]
	}
	w.rec.record(recorded)
	return results, nil
}

func (w *recordingRetriever) BuildContext(results []rag.SearchResult, maxTokens int) string {
	return w.inner.BuildContext(results, maxTokens)
}

func (w *recordingRetriever) BuildContextWithRenderedCount(results []rag.SearchResult, maxTokens int) (string, int) {
	return w.inner.BuildContextWithRenderedCount(results, maxTokens)
}

func (w *recordingRetriever) RenderProgressiveWithGroups(ctx context.Context, req rag.ProgressiveRenderRequest) (string, rag.ProgressiveTrace, []rag.ProgressiveGroup, error) {
	return w.inner.RenderProgressiveWithGroups(ctx, req)
}

// groundingCollector captures the retrieval attribution of the prompt that
// produced the FINAL answer.
//
// Presentation events fire before a step's model call; OnStep reports that call
// completed. Committing on OnStep — including when the step presented nothing —
// is what makes "last completed step" the answer step. Tracking the highest
// step that merely happened to expose retrieval would let an evidence-free
// answer inherit an earlier step's evidence.
//
// Observer callbacks are serial in the orchestrator goroutine, so no locking.
type groundingCollector struct {
	pending     []agent.RetrievedSource
	pendingStep int
	havePending bool
	committed   []agent.RetrievedSource
	retrieves   int
	truncated   bool
}

var (
	_ agent.Observer                      = (*groundingCollector)(nil)
	_ agent.ToolResultObserver            = (*groundingCollector)(nil)
	_ agent.RetrievalPresentationObserver = (*groundingCollector)(nil)
)

func (c *groundingCollector) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (c *groundingCollector) OnToken(context.Context, agent.TokenEvent) error       { return nil }

func (c *groundingCollector) OnStep(_ context.Context, e agent.StepEvent) error {
	if c.havePending && c.pendingStep == e.Index {
		c.committed = c.pending
	} else {
		c.committed = nil
	}
	c.pending, c.havePending, c.pendingStep = nil, false, 0
	return nil
}

func (c *groundingCollector) OnRetrievalPresentation(_ context.Context, e agent.RetrievalPresentationEvent) error {
	if len(e.Attribution.Sources) == 0 {
		return nil
	}
	if !c.havePending || c.pendingStep != e.Step {
		c.pending, c.havePending, c.pendingStep = nil, true, e.Step
	}
	c.pending = append(c.pending, e.Attribution.Sources...)
	return nil
}

func (c *groundingCollector) OnToolResult(_ context.Context, e agent.ToolResultEvent) error {
	if e.Call.Function.Name != "retrieve" || !e.Invoked || e.Denied || e.Result.IsError {
		return nil
	}
	c.retrieves++
	// A capped observation leaves attribution crediting evidence the model
	// never read. RetrieveOutputCap (64 KiB) sits far above both renderers'
	// budgets so this cannot fire today; record it rather than trust it.
	if e.Result.Truncated {
		c.truncated = true
	}
	return nil
}

// finalSources returns the committed step's distinct identities in
// first-presentation order.
func (c *groundingCollector) finalSources() []agent.RetrievedSource {
	if len(c.committed) == 0 {
		return nil
	}
	seen := make(map[evidenceKey]bool, len(c.committed))
	out := make([]agent.RetrievedSource, 0, len(c.committed))
	for _, s := range c.committed {
		key := evidenceKey{s.StableKey, s.Source, s.StartLine, s.EndLine}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

// retrieved reports whether the answer's prompt carried retrieval evidence.
func (c *groundingCollector) retrieved() bool { return len(c.finalSources()) > 0 }

// sawRetrieveCall reports whether this turn ran the retrieve tool successfully.
func (c *groundingCollector) sawRetrieveCall() bool { return c.retrieves > 0 }

// evidenceComplete reports whether every successful retrieve observation was
// whole. False forces a visible skip with no judge call.
func (c *groundingCollector) evidenceComplete() bool { return !c.truncated }
