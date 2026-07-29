// Package tools provides built-in read-only agent.Tool implementations.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/rag"
)

const (
	defaultRetrieveK         = 5
	defaultRetrieveMaxTokens = 2048
	defaultRetrieveMaxK      = 20
	// maxRetrieveMaxK is the largest MaxK the carriers can hold. rag projects
	// (k+1) prefixes x rungs alternatives per source, 2 rungs for a fresh one,
	// so all k results landing on ONE fresh source yields 2(k+1) alternatives.
	// SOURCE OF TRUTH for the 64 it must stay under: maxContextAlternatives in
	// package agent (agent/context_set.go) — unexported, and agent/tools is a
	// different package, so the derived ceiling is restated here and pinned by
	// TestRetrieveMaxKWithinCarrierBound. Exceeding it is a hard mixed-mode
	// validation failure, not a degradation.
	maxRetrieveMaxK = 31
)

// RetrieveOutputCap bounds retrieve tool output. It is set explicitly on the
// Effect AND passed to the progressive renderer as its byte ceiling, so the
// runtime's post-Invoke capOutput (agent/dispatch.go:193) can never truncate
// a block the trace and attribution describe as fully rendered. The value
// matches the runtime's unexported default (agent/types.go:14).
const RetrieveOutputCap = 64 * 1024

// retriever is the minimal slice of *rag.Retriever the tool needs; abstracting
// it keeps the tool unit-testable with a fake.
type retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]rag.SearchResult, error)
	BuildContext(results []rag.SearchResult, maxTokens int) string
}

// progressiveRetriever is the optional capability the progressive path needs.
// *rag.Retriever satisfies it. The groups variant is the only one used: it
// returns the same output and trace as RenderProgressive plus the capability
// projection, so requiring both would let a retriever satisfy the interface
// while being unable to feed mixed assembly.
type progressiveRetriever interface {
	retriever
	RenderProgressiveWithGroups(ctx context.Context, req rag.ProgressiveRenderRequest) (string, rag.ProgressiveTrace, []rag.ProgressiveGroup, error)
}

// Without this assertion a rename or signature change in rag drifts SILENTLY:
// the interface is local, every test here uses a fake, and the type switch in
// Invoke degrades to the legacy BuildContext path with the over-crediting
// attribution — no build error, no failing test, in rag, agent, agent/tools or
// cmd/golem. Same convention as rag/progressive.go's progressiveStoreReader
// assertion (DEV-11) and DEV-20's atomicSourceReplacerWithVectorSpaceID.
var _ progressiveRetriever = (*rag.Retriever)(nil)

// Retrieve is the reference read-only retrieval built-in. #95 swaps SearchMulti
// behind the same retriever seam without changing the agent.Tool contract.
type Retrieve struct {
	R         retriever
	K         int // default top-k when the call omits k
	MaxTokens int // token budget for BuildContext AND the progressive renderer; 0 => a sane default
	// Progressive opts into the progressive renderer when R supports it
	// (#189 slice 1). Off => the legacy BuildContext path, byte-identical
	// to before. MinFullResults and Estimate pass through to the renderer.
	//
	// On, it also builds the ToolResult.Context capability projection
	// UNCONDITIONALLY — the tool cannot see ContextManager.Mixed, so a consumer
	// that sets Progressive without Mixed pays the full projection and dispatch
	// then discards it (agent/dispatch.go). That cost is accepted, not gated:
	// it is one transient allocation per call, bounded by MaxK at
	// O(rungs x k^2/2) block copies (<= ~1000 for the ceiling MaxK of 31),
	// freed as soon as dispatch drops the result. Gating it would mean
	// plumbing an assembly-mode signal into every tool — new API for a cost
	// smaller than the retrieval it accompanies. Golem couples both flags
	// behind -progressive, so the shipped path never pays it.
	Progressive    bool
	MinFullResults int
	// MaxK bounds the model-supplied k. The progressive groups projection
	// renders every evidence prefix, so its cost grows with k^2; unlike the
	// flat path there is no token/byte ceiling on the projection itself.
	// 0 => defaultRetrieveMaxK. A value above maxRetrieveMaxK is rejected by
	// Invoke: it could project more alternatives than the agent carriers
	// accept.
	MaxK int
	// Estimate is the token estimator for the progressive path; nil => the
	// renderer's heuristic. It must be pure and deterministic: the renderer
	// calls it twice on the same text (pinned pre-check, upgrade delta) and
	// assumes the two agree. A NEGATIVE return also falls back to the
	// heuristic, so a tokenizer that returns -1 as an error sentinel degrades
	// silently rather than failing (rag/progressive.go:49-59).
	Estimate func(string) int
}

type retrieveArgs struct {
	Query string `json:"query"`
	K     int    `json:"k"`
}

func (Retrieve) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        "retrieve",
		Description: "Retrieve relevant code/context chunks for a natural-language query.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "query":{"type":"string","description":"what to search for"},
    "k":{"type":"integer","description":"max results"}
  },
  "required":["query"]
}`),
	}
}

func (t Retrieve) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever, OutputCap: RetrieveOutputCap}
}

func (t Retrieve) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	// Configuration, not model input: a hard error, not an IsError observation
	// the model can neither fix nor learn from. Checked here because
	// agent/tools has no constructors — every tool is a struct literal — and
	// the alternative is a mixed-assembly failure much later whose message
	// talks about alternative counts rather than about MaxK.
	if t.MaxK > maxRetrieveMaxK {
		return agent.ToolResult{}, fmt.Errorf("tools: retrieve: MaxK must be <= %d, got %d", maxRetrieveMaxK, t.MaxK)
	}
	var args retrieveArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
	}
	if args.Query == "" {
		return agent.ToolResult{IsError: true, Content: "query is required"}, nil
	}
	k := args.K
	if k <= 0 {
		k = t.K
	}
	if k <= 0 {
		k = defaultRetrieveK
	}
	// Clamped BEFORE the backend call: clamping the results afterwards would
	// still let one {"k":500} call pay for 500 lookups and 500 prefix families.
	maxK := t.MaxK
	if maxK <= 0 {
		maxK = defaultRetrieveMaxK
	}
	if k > maxK {
		k = maxK
	}
	maxTokens := t.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultRetrieveMaxTokens
	}

	results, err := t.R.Retrieve(ctx, args.Query, k)
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "retrieval failed: " + err.Error()}, nil
	}
	if pr, ok := t.R.(progressiveRetriever); ok && t.Progressive {
		content, trace, groups, err := pr.RenderProgressiveWithGroups(ctx, rag.ProgressiveRenderRequest{
			Results:        results,
			MaxTokens:      maxTokens,
			MaxBytes:       RetrieveOutputCap,
			MinFullResults: t.MinFullResults,
			Estimate:       t.Estimate,
		})
		if err != nil {
			// The renderer returns a ZERO trace on every error path, so no
			// trace field may be read here: doing so would attribute sources
			// that rendered nothing.
			return agent.ToolResult{IsError: true, Content: "retrieval render failed: " + err.Error()}, nil
		}
		// Attribution equals the rendered set exactly — never the raw
		// retrieval results (fixes the over-crediting at the legacy path's
		// expense of precision; see #189 spec section 11). A source that got
		// orientation only has no RenderedEvidence and contributes nothing.
		// Every output-derived trace field is recomputed from surviving blocks
		// after the defensive trim (#331 spec 3.6), so attribution built from
		// RenderedEvidence and the token/omission counters all describe exactly
		// the rendered output.
		attrib := &agent.RetrievalAttribution{}
		for _, src := range trace.Sources {
			attrib.Sources = appendEvidence(attrib.Sources, src.RenderedEvidence)
		}
		return agent.ToolResult{Content: content, Attrib: attrib, Context: bridgeGroups(groups, t.MinFullResults)}, nil
	}

	content := t.R.BuildContext(results, maxTokens)

	// KNOWN OVER-CREDITING, DELIBERATELY RETAINED: BuildContext stops at the
	// first entry that would exceed its char budget (rag/retriever.go:948) yet
	// every retrieved result is attributed here, so the model can be credited
	// with evidence it never saw (#189 spec section 11). BuildContext is frozen
	// per spec section 4 and byte-identity with the pre-#189 path is the goal;
	// set Progressive to get attribution equal to what was actually rendered.
	attrib := &agent.RetrievalAttribution{Sources: make([]agent.RetrievedSource, 0, len(results))}
	for _, r := range results {
		attrib.Sources = append(attrib.Sources, agent.RetrievedSource{
			StableKey: r.Chunk.StableKey,
			Source:    r.Chunk.Source,
			StartLine: r.Chunk.StartLine,
			EndLine:   r.Chunk.EndLine,
			Score:     r.Score,
		})
	}
	return agent.ToolResult{Content: content, Attrib: attrib}, nil
}

// appendEvidence maps rendered evidence onto attribution entries, preserving
// order. Both the anchor attribution (the whole rendered set) and each group
// alternative's attribution are built from RenderedEvidence, so the mapping
// lives in one place: a field added to either type must reach both.
func appendEvidence(dst []agent.RetrievedSource, ev []rag.RenderedEvidence) []agent.RetrievedSource {
	for _, e := range ev {
		dst = append(dst, agent.RetrievedSource{
			StableKey: e.StableKey,
			Source:    e.Source,
			StartLine: e.StartLine,
			EndLine:   e.EndLine,
			Score:     e.Score,
		})
	}
	return dst
}

// bridgeGroups carries rag's capability projection across into the agent
// carriers: one group per source, descriptors and content passed through
// untouched, and per-alternative attribution built from THAT alternative's
// RenderedEvidence only. Orientation-only alternatives get nil Attrib — the
// carriers reject attribution on an alternative with no verbatim component,
// and crediting evidence a rendering does not contain is the over-crediting
// the progressive path exists to avoid.
//
// minFull is the caller's raw MinFullResults, normalized to the renderer's
// units (0 => 1) so a consumer never re-derives rag's normalization. It is the
// preferred verbatim-component COUNT; the mixed assembler's selection policy
// is its own (spec 3.5), not a reproduction of rag's flat floor scan.
//
// Zero groups => nil set: a non-nil set with no groups is a hard validation
// failure in agent.
func bridgeGroups(groups []rag.ProgressiveGroup, minFull int) *agent.ContextSet {
	if len(groups) == 0 {
		return nil
	}
	if minFull <= 0 {
		minFull = 1
	}
	set := &agent.ContextSet{MinVerbatim: minFull, Groups: make([]agent.ContextGroup, len(groups))}
	for i, g := range groups {
		cg := agent.ContextGroup{Desc: g.Desc, Alternatives: make([]agent.ContextAlternative, len(g.Alternatives))}
		for j, a := range g.Alternatives {
			ca := agent.ContextAlternative{Desc: a.Desc, Content: a.Content}
			if len(a.RenderedEvidence) > 0 {
				ca.Attrib = &agent.RetrievalAttribution{
					Sources: appendEvidence(make([]agent.RetrievedSource, 0, len(a.RenderedEvidence)), a.RenderedEvidence),
				}
			}
			cg.Alternatives[j] = ca
		}
		set.Groups[i] = cg
	}
	return set
}
