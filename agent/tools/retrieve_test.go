package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/rag"
)

type fakeRetriever struct {
	results []rag.SearchResult
}

func (f fakeRetriever) Retrieve(context.Context, string, int) ([]rag.SearchResult, error) {
	return f.results, nil
}
func (f fakeRetriever) BuildContext(results []rag.SearchResult, _ int) string {
	out := ""
	for _, r := range results {
		out += r.Chunk.Content + "\n"
	}
	return out
}

func TestRetrieveReturnsContextAndAttribution(t *testing.T) {
	fr := fakeRetriever{results: []rag.SearchResult{
		{Chunk: rag.Chunk{StableKey: "k1", Source: "a.go", StartLine: 1, EndLine: 5, Content: "hello"}, Score: 0.9},
	}}
	tool := Retrieve{R: fr, K: 4, MaxTokens: 1000}

	if tool.Spec().Name != "retrieve" {
		t.Fatalf("spec name = %q", tool.Spec().Name)
	}
	if tool.Effect().Class != agent.Read || tool.Effect().Approval != agent.ApprovalNever {
		t.Fatalf("retrieve must be Read/ApprovalNever, got %+v", tool.Effect())
	}

	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"hi"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.Content == "" || out.IsError {
		t.Fatalf("expected content, got %+v", out)
	}
	if out.Attrib == nil || len(out.Attrib.Sources) != 1 || out.Attrib.Sources[0].StableKey != "k1" {
		t.Fatalf("expected attribution for k1, got %+v", out.Attrib)
	}
}

func TestRetrieveMalformedArgsIsError(t *testing.T) {
	tool := Retrieve{R: fakeRetriever{}, K: 4}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":}`))
	if err != nil {
		t.Fatalf("Invoke should not hard-error: %v", err)
	}
	if !out.IsError {
		t.Fatal("malformed args must yield IsError observation")
	}
}

type capturingRetriever struct {
	gotK         int
	gotMaxTokens int
	err          error
}

func (c *capturingRetriever) Retrieve(_ context.Context, _ string, k int) ([]rag.SearchResult, error) {
	c.gotK = k
	if c.err != nil {
		return nil, c.err
	}
	return []rag.SearchResult{{Chunk: rag.Chunk{StableKey: "k", Content: "x"}}}, nil
}
func (c *capturingRetriever) BuildContext(_ []rag.SearchResult, maxTokens int) string {
	c.gotMaxTokens = maxTokens
	return "x"
}

func TestRetrieveEmptyQueryIsError(t *testing.T) {
	tool := Retrieve{R: &capturingRetriever{}, K: 4}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":""}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.IsError {
		t.Fatal("empty query must yield an IsError observation")
	}
}

func TestRetrieveKAndMaxTokensDefaults(t *testing.T) {
	cr := &capturingRetriever{}
	tool := Retrieve{R: cr} // K and MaxTokens both zero -> defaults
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"hi"}`)); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if cr.gotK != defaultRetrieveK {
		t.Fatalf("k = %d, want default %d", cr.gotK, defaultRetrieveK)
	}
	if cr.gotMaxTokens != defaultRetrieveMaxTokens {
		t.Fatalf("maxTokens = %d, want default %d", cr.gotMaxTokens, defaultRetrieveMaxTokens)
	}
}

func TestRetrieveBackendErrorIsError(t *testing.T) {
	tool := Retrieve{R: &capturingRetriever{err: errors.New("boom")}, K: 4}
	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"hi"}`))
	if err != nil {
		t.Fatalf("Invoke should not hard-error: %v", err)
	}
	if !out.IsError {
		t.Fatal("backend error must yield an IsError observation")
	}
}

type fakeProgressive struct {
	results []rag.SearchResult
	out     string
	trace   rag.ProgressiveTrace
	err     error
	gotReq  rag.ProgressiveRenderRequest
}

func (f *fakeProgressive) Retrieve(ctx context.Context, query string, k int) ([]rag.SearchResult, error) {
	return f.results, nil
}
func (f *fakeProgressive) BuildContext(results []rag.SearchResult, maxTokens int) string {
	return "LEGACY-PATH"
}
func (f *fakeProgressive) RenderProgressive(ctx context.Context, req rag.ProgressiveRenderRequest) (string, rag.ProgressiveTrace, error) {
	f.gotReq = req
	if f.err != nil {
		// Mirrors the real renderer: every error path yields the ZERO trace.
		return "", rag.ProgressiveTrace{}, f.err
	}
	return f.out, f.trace, nil
}

func TestRetrieveEffectOutputCap(t *testing.T) {
	effect := Retrieve{}.Effect()
	if effect.OutputCap != RetrieveOutputCap {
		t.Fatalf("Effect().OutputCap = %d, want %d", effect.OutputCap, RetrieveOutputCap)
	}
	if RetrieveOutputCap != 64*1024 {
		t.Fatalf("cap must match the runtime default it makes explicit")
	}
}

// progressiveFixture is the shared two-source fixture: a.go renders one
// evidence block, b.go is orientation-only. Both halves are load-bearing —
// the b.go RESULT makes the retrieved set larger than the rendered set, and
// the b.go TRACE ENTRY makes the trace source list larger than the rendered
// set. A mutation that attributes either one is caught by the count check.
func progressiveFixture() *fakeProgressive {
	return &fakeProgressive{
		results: []rag.SearchResult{
			{Chunk: rag.Chunk{ID: "c1", Source: "a.go", StableKey: "k1", StartLine: 1, EndLine: 2}, Score: 0.9},
			{Chunk: rag.Chunk{ID: "c2", Source: "b.go", StableKey: "k2", StartLine: 5, EndLine: 9}, Score: 0.8},
		},
		out: "rendered",
		trace: rag.ProgressiveTrace{
			Sources: []rag.ProgressiveSourceTrace{{
				Source: "a.go",
				RenderedEvidence: []rag.RenderedEvidence{
					{Source: "a.go", ChunkID: "c1", StableKey: "k1", StartLine: 1, EndLine: 2, Score: 0.9},
				},
			}, {
				Source: "b.go", // orientation-only: no evidence, no attribution
			}},
		},
	}
}

func TestRetrieveProgressiveAttributionEqualsRenderedSet(t *testing.T) {
	fake := progressiveFixture()
	tool := Retrieve{R: fake, Progressive: true}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Content != "rendered" {
		t.Fatalf("content = %q", res.Content)
	}
	if res.Attrib == nil || len(res.Attrib.Sources) != 1 {
		t.Fatalf("attribution must equal the rendered set exactly: %+v", res.Attrib)
	}
	got := res.Attrib.Sources[0]
	if got.StableKey != "k1" || got.Source != "a.go" || got.StartLine != 1 || got.EndLine != 2 || got.Score != 0.9 {
		t.Fatalf("attribution fields wrong: %+v", got)
	}
	// MaxBytes must ride the exported cap so capOutput can never truncate.
	if fake.gotReq.MaxBytes != RetrieveOutputCap {
		t.Fatalf("MaxBytes = %d, want RetrieveOutputCap", fake.gotReq.MaxBytes)
	}
}

func TestRetrieveProgressiveIsOptIn(t *testing.T) {
	// Same capable retriever, Progressive unset: the legacy path, unchanged.
	fake := progressiveFixture()
	tool := Retrieve{R: fake}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Content != "LEGACY-PATH" {
		t.Fatalf("Progressive off must use BuildContext, got %q", res.Content)
	}
	if res.Attrib == nil || len(res.Attrib.Sources) != 2 {
		t.Fatalf("legacy path attributes every retrieved result: %+v", res.Attrib)
	}
	if fake.gotReq.MaxBytes != 0 {
		t.Fatal("Progressive off must not call RenderProgressive")
	}
}

func TestRetrieveProgressiveRenderErrorIsErrorWithoutAttribution(t *testing.T) {
	// The renderer returns a ZERO trace on every error path, so reading the
	// trace here would credit sources nothing rendered. Nothing may be
	// attributed on this path.
	fake := progressiveFixture()
	fake.err = errors.New("boom")
	tool := Retrieve{R: fake, Progressive: true}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("invoke should not hard-error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("render failure must yield an IsError observation: %+v", res)
	}
	if res.Attrib != nil {
		t.Fatalf("error path must attribute nothing, got %+v", res.Attrib)
	}
}

func TestRetrieveProgressiveRendersNothingAttributesNothing(t *testing.T) {
	// Render succeeded but emitted no evidence: attribution is present and
	// empty, matching the legacy path's shape for a zero-result retrieval.
	fake := progressiveFixture()
	fake.out = ""
	fake.trace = rag.ProgressiveTrace{Sources: []rag.ProgressiveSourceTrace{{Source: "a.go"}, {Source: "b.go"}}}
	tool := Retrieve{R: fake, Progressive: true}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.IsError {
		t.Fatalf("empty render is not an error: %+v", res)
	}
	if res.Attrib == nil || len(res.Attrib.Sources) != 0 {
		t.Fatalf("orientation-only render must attribute nothing: %+v", res.Attrib)
	}
}

type fakeRetrieverLegacy struct{}

func (fakeRetrieverLegacy) Retrieve(context.Context, string, int) ([]rag.SearchResult, error) {
	return []rag.SearchResult{{Chunk: rag.Chunk{Source: "x.go", Content: "c"}, Score: 1}}, nil
}
func (fakeRetrieverLegacy) BuildContext([]rag.SearchResult, int) string { return "LEGACY" }

func TestRetrieveProgressiveFallsBackWithoutCapability(t *testing.T) {
	// Progressive requested but R lacks RenderProgressive: legacy path.
	tool := Retrieve{R: fakeRetrieverLegacy{}, Progressive: true}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Content != "LEGACY" {
		t.Fatalf("must fall back to BuildContext, got %q", res.Content)
	}
	if res.Attrib == nil || len(res.Attrib.Sources) != 1 || res.Attrib.Sources[0].Source != "x.go" {
		t.Fatalf("fallback keeps the legacy attribution semantics: %+v", res.Attrib)
	}
}
