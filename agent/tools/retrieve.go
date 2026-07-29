// Package tools provides built-in read-only agent.Tool implementations.
package tools

import (
	"context"
	"encoding/json"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/rag"
)

const (
	defaultRetrieveK         = 5
	defaultRetrieveMaxTokens = 2048
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
// *rag.Retriever satisfies it.
type progressiveRetriever interface {
	retriever
	RenderProgressive(ctx context.Context, req rag.ProgressiveRenderRequest) (string, rag.ProgressiveTrace, error)
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
	MaxTokens int // token budget for BuildContext AND RenderProgressive; 0 => a sane default
	// Progressive opts into rag.RenderProgressive when R supports it
	// (#189 slice 1). Off => the legacy BuildContext path, byte-identical
	// to before. MinFullResults and Estimate pass through to the renderer.
	Progressive    bool
	MinFullResults int
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
	maxTokens := t.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultRetrieveMaxTokens
	}

	results, err := t.R.Retrieve(ctx, args.Query, k)
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "retrieval failed: " + err.Error()}, nil
	}
	if pr, ok := t.R.(progressiveRetriever); ok && t.Progressive {
		content, trace, err := pr.RenderProgressive(ctx, rag.ProgressiveRenderRequest{
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
			for _, ev := range src.RenderedEvidence {
				attrib.Sources = append(attrib.Sources, agent.RetrievedSource{
					StableKey: ev.StableKey,
					Source:    ev.Source,
					StartLine: ev.StartLine,
					EndLine:   ev.EndLine,
					Score:     ev.Score,
				})
			}
		}
		return agent.ToolResult{Content: content, Attrib: attrib}, nil
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
