package tools

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/contextdepth"
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
	groups  []rag.ProgressiveGroup
	err     error
	gotReq  rag.ProgressiveRenderRequest
	// gotK and retrieveCalls make the progressive path observable: without them
	// the clamp is only ever asserted on the flat path, which is not the path
	// whose k^2 projection cost the clamp exists for.
	gotK          int
	retrieveCalls int
}

// Retrieve ignores k and returns the whole fixture — so this fake is also an
// over-returning retriever, which the truncation test relies on.
func (f *fakeProgressive) Retrieve(ctx context.Context, query string, k int) ([]rag.SearchResult, error) {
	f.gotK = k
	f.retrieveCalls++
	return f.results, nil
}
func (f *fakeProgressive) BuildContext(results []rag.SearchResult, maxTokens int) string {
	return "LEGACY-PATH"
}
func (f *fakeProgressive) RenderProgressiveWithGroups(ctx context.Context, req rag.ProgressiveRenderRequest) (string, rag.ProgressiveTrace, []rag.ProgressiveGroup, error) {
	f.gotReq = req
	if f.err != nil {
		// Mirrors the real renderer: every error path yields the ZERO trace and
		// no groups.
		return "", rag.ProgressiveTrace{}, nil, f.err
	}
	return f.out, f.trace, f.groups, nil
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

// The four ladder descriptors rag's projection emits (rag/progressive_groups.go
// repMeta/repAbstract/repOverview/repEvidence, unexported there). Used to BUILD
// the fixture groups; the bridge is asserted to pass them through untouched.
var (
	dAbstract = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationGenerated}
	dOverview = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL1, Kind: contextdepth.RepresentationGenerated}
	dMeta     = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata}
	dEvidence = contextdepth.RepresentationDesc{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim}
)

func alt(content string, reps []contextdepth.RepresentationDesc, ev ...rag.RenderedEvidence) rag.ProgressiveAlternative {
	return rag.ProgressiveAlternative{
		Desc:             contextdepth.AlternativeDesc{Representations: reps},
		Content:          content,
		RenderedEvidence: ev,
	}
}

// progressiveGroupsFixture is the groups half of the retrieve fixture, shaped
// like rag's real projection: a.go is FRESH (two orientation rungs) with two
// evidence prefixes, b.go is stale (one metadata rung) with one.
//
// Its per-alternative evidence deliberately DISAGREES with progressiveFixture's
// trace, which renders only k1 on a.go and nothing on b.go. So an attribution
// built from the whole trace rather than from the alternative in hand is
// detectable here: it would credit k1 everywhere, understating the two-block
// prefixes, crediting orientation-only alternatives, and never mentioning k3.
func progressiveGroupsFixture() []rag.ProgressiveGroup {
	e1 := rag.RenderedEvidence{Source: "a.go", ChunkID: "c1", StableKey: "k1", StartLine: 1, EndLine: 2, Score: 0.9}
	e2 := rag.RenderedEvidence{Source: "a.go", ChunkID: "c2", StableKey: "k2", StartLine: 8, EndLine: 12, Score: 0.7}
	e3 := rag.RenderedEvidence{Source: "b.go", ChunkID: "c3", StableKey: "k3", StartLine: 5, EndLine: 9, Score: 0.6}
	return []rag.ProgressiveGroup{{
		Desc: contextdepth.GroupDesc{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "a.go"}, Rank: 1},
		Alternatives: []rag.ProgressiveAlternative{
			alt("A0", []contextdepth.RepresentationDesc{dAbstract}),
			alt("A0A1", []contextdepth.RepresentationDesc{dAbstract, dOverview}),
			alt("A0+e1", []contextdepth.RepresentationDesc{dAbstract, dEvidence}, e1),
			alt("A0A1+e1", []contextdepth.RepresentationDesc{dAbstract, dOverview, dEvidence}, e1),
			alt("A0+e1e2", []contextdepth.RepresentationDesc{dAbstract, dEvidence, dEvidence}, e1, e2),
			alt("A0A1+e1e2", []contextdepth.RepresentationDesc{dAbstract, dOverview, dEvidence, dEvidence}, e1, e2),
		},
	}, {
		Desc: contextdepth.GroupDesc{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "b.go"}, Rank: 2},
		Alternatives: []rag.ProgressiveAlternative{
			alt("M", []contextdepth.RepresentationDesc{dMeta}),
			alt("M+e3", []contextdepth.RepresentationDesc{dMeta, dEvidence}, e3),
		},
	}}
}

func TestRetrieveProgressiveAttachesGroups(t *testing.T) {
	fake := progressiveFixture()
	fake.groups = progressiveGroupsFixture()
	tool := Retrieve{R: fake, Progressive: true}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	// The flat rendering stays the canonical fallback, unchanged bytes, and the
	// anchor attribution still equals the RENDERED set (one block) — not the
	// groups, which declare three.
	if res.Content != "rendered" {
		t.Fatalf("content = %q, want the flat rendering unchanged", res.Content)
	}
	if res.Attrib == nil || len(res.Attrib.Sources) != 1 || res.Attrib.Sources[0].StableKey != "k1" {
		t.Fatalf("anchor attribution must still equal the rendered set: %+v", res.Attrib)
	}
	if res.Context == nil {
		t.Fatal("progressive groups must reach ToolResult.Context")
	}
	if res.Context.MinVerbatim != 1 {
		t.Fatalf("MinVerbatim = %d, want the renderer-normalized floor 1", res.Context.MinVerbatim)
	}

	// Expected per-alternative attribution, stated literally: nil sources means
	// the alternative must carry NO attribution at all.
	a1 := agent.RetrievedSource{StableKey: "k1", Source: "a.go", StartLine: 1, EndLine: 2, Score: 0.9}
	a2 := agent.RetrievedSource{StableKey: "k2", Source: "a.go", StartLine: 8, EndLine: 12, Score: 0.7}
	b3 := agent.RetrievedSource{StableKey: "k3", Source: "b.go", StartLine: 5, EndLine: 9, Score: 0.6}
	want := [][][]agent.RetrievedSource{
		{nil, nil, {a1}, {a1}, {a1, a2}, {a1, a2}},
		{nil, {b3}},
	}

	groups := progressiveGroupsFixture()
	if len(res.Context.Groups) != len(groups) {
		t.Fatalf("got %d groups, want %d", len(res.Context.Groups), len(groups))
	}
	for i, got := range res.Context.Groups {
		src := groups[i]
		if got.Desc != src.Desc {
			t.Errorf("group %d: Desc = %+v, want %+v", i, got.Desc, src.Desc)
		}
		if len(got.Alternatives) != len(src.Alternatives) {
			t.Fatalf("group %d: got %d alternatives, want %d", i, len(got.Alternatives), len(src.Alternatives))
		}
		for j, ga := range got.Alternatives {
			sa := src.Alternatives[j]
			if ga.Content != sa.Content {
				t.Errorf("group %d alt %d: content = %q, want %q", i, j, ga.Content, sa.Content)
			}
			if !slices.Equal(ga.Desc.Representations, sa.Desc.Representations) {
				t.Errorf("group %d alt %d: reps = %+v, want %+v", i, j, ga.Desc.Representations, sa.Desc.Representations)
			}
			switch wantSources := want[i][j]; {
			case wantSources == nil:
				if ga.Attrib != nil {
					t.Errorf("group %d alt %d: orientation-only alternative must carry no attribution, got %+v",
						i, j, ga.Attrib.Sources)
				}
			case ga.Attrib == nil:
				t.Errorf("group %d alt %d: evidence-bearing alternative lost its attribution", i, j)
			default:
				if !reflect.DeepEqual(ga.Attrib.Sources, wantSources) {
					t.Errorf("group %d alt %d: attribution = %+v, want %+v", i, j, ga.Attrib.Sources, wantSources)
				}
			}
		}
	}
}

func TestRetrieveProgressiveMinVerbatimIsNormalizedFloor(t *testing.T) {
	// The floor is reported in the renderer's normalized units (0 => 1), so a
	// consumer never has to re-derive rag's normalization. Negative values
	// cannot reach here: the renderer rejects them.
	for _, tc := range []struct{ minFull, want int }{{0, 1}, {1, 1}, {2, 2}, {7, 7}} {
		fake := progressiveFixture()
		fake.groups = progressiveGroupsFixture()
		tool := Retrieve{R: fake, Progressive: true, MinFullResults: tc.minFull}
		res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
		if err != nil {
			t.Fatalf("MinFullResults=%d: invoke: %v", tc.minFull, err)
		}
		if res.Context == nil {
			t.Fatalf("MinFullResults=%d: no context set", tc.minFull)
		}
		if res.Context.MinVerbatim != tc.want {
			t.Errorf("MinFullResults=%d: MinVerbatim = %d, want %d", tc.minFull, res.Context.MinVerbatim, tc.want)
		}
	}
}

func TestRetrieveProgressiveEmptyResults(t *testing.T) {
	// No results => rag returns no groups => no set. A non-nil set with zero
	// groups is a hard validation failure downstream (agent/context_set.go).
	fake := &fakeProgressive{out: ""}
	tool := Retrieve{R: fake, Progressive: true}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Context != nil {
		t.Fatalf("zero groups must leave Context nil, got %+v", res.Context)
	}
}

func TestRetrieveLegacyNoContext(t *testing.T) {
	// Same capable retriever WITH groups available, Progressive off: the legacy
	// path is untouched — no set, and the byte-identical BuildContext output.
	fake := progressiveFixture()
	fake.groups = progressiveGroupsFixture()
	tool := Retrieve{R: fake}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.Context != nil {
		t.Fatalf("legacy path must attach no context set, got %+v", res.Context)
	}
	if res.Content != "LEGACY-PATH" {
		t.Fatalf("content = %q, want the legacy BuildContext output", res.Content)
	}
	if res.Attrib == nil || len(res.Attrib.Sources) != 2 {
		t.Fatalf("legacy path attributes every retrieved result: %+v", res.Attrib)
	}
}

func TestRetrieveClampsK(t *testing.T) {
	tests := []struct {
		name  string
		toolK int
		maxK  int
		args  string
		wantK int
	}{
		{"model k above default cap", 0, 0, `{"query":"q","k":500}`, defaultRetrieveMaxK},
		{"model k above explicit cap", 0, 7, `{"query":"q","k":500}`, 7},
		{"model k below cap unchanged", 0, 0, `{"query":"q","k":3}`, 3},
		{"model k at cap unchanged", 0, 0, `{"query":"q","k":20}`, 20},
		{"tool default k is clamped too", 500, 0, `{"query":"q"}`, defaultRetrieveMaxK},
		{"explicit cap at the carrier ceiling", 0, maxRetrieveMaxK, `{"query":"q","k":500}`, maxRetrieveMaxK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cr := &capturingRetriever{}
			tool := Retrieve{R: cr, K: tc.toolK, MaxK: tc.maxK}
			if _, err := tool.Invoke(context.Background(), json.RawMessage(tc.args)); err != nil {
				t.Fatalf("invoke: %v", err)
			}
			// gotK is what the BACKEND was asked for: clamping the results after
			// retrieval would still let a {"k":500} call cost 500 lookups and,
			// on the progressive path, 500 prefix families.
			if cr.gotK != tc.wantK {
				t.Fatalf("backend received k = %d, want %d", cr.gotK, tc.wantK)
			}
		})
	}
}

func TestRetrieveClampsKInProgressiveMode(t *testing.T) {
	// The clamp exists FOR this path: the groups projection renders every
	// evidence prefix, so an unclamped k of 500 materializes ~500 prefix
	// families per source. The flat table above cannot observe that path at all.
	for _, tc := range []struct {
		name        string
		maxK, wantK int
	}{
		{"default cap", 0, defaultRetrieveMaxK},
		{"explicit cap", 9, 9},
		{"cap at the carrier ceiling", maxRetrieveMaxK, maxRetrieveMaxK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := progressiveFixture()
			fake.groups = progressiveGroupsFixture()
			tool := Retrieve{R: fake, Progressive: true, MaxK: tc.maxK}
			if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q","k":500}`)); err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if fake.gotK != tc.wantK {
				t.Fatalf("progressive backend received k = %d, want %d", fake.gotK, tc.wantK)
			}
		})
	}
}

func TestRetrieveTruncatesOverReturningRetriever(t *testing.T) {
	// R is an interface: nothing forces an implementation to honor k, and the
	// alternative-count ceiling is derived from k. fakeProgressive ignores k and
	// returns its whole fixture, so it IS such a retriever — the renderer must
	// still see at most k results, or the projection can exceed the carrier
	// bound through a seam maxRetrieveMaxK cannot guard.
	fake := progressiveFixture() // two results
	fake.groups = progressiveGroupsFixture()
	tool := Retrieve{R: fake, Progressive: true}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q","k":1}`)); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if got := len(fake.gotReq.Results); got != 1 {
		t.Fatalf("renderer received %d results for k=1: the projection bound is derived from k", got)
	}
}

func TestRetrieveMaxKRejectionSkipsRetrieval(t *testing.T) {
	// The ceiling is checked before the backend call, so a misconfigured tool
	// costs nothing.
	fake := progressiveFixture()
	tool := Retrieve{R: fake, Progressive: true, MaxK: 500}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`)); err == nil {
		t.Fatal("MaxK over the ceiling must be rejected")
	}
	if fake.retrieveCalls != 0 {
		t.Fatalf("rejected configuration still called the backend %d time(s)", fake.retrieveCalls)
	}
}

func TestRetrieveMaxKWithinCarrierBound(t *testing.T) {
	// SOURCE OF TRUTH: maxContextAlternatives in package agent
	// (agent/context_set.go), unexported and unreachable from this package.
	// Restated here so the two move together; if this literal ever disagrees
	// with agent's, mixed assembly rejects a projection the tool produced.
	const carrierMaxAlternatives = 64
	// Worst case: every result lands on ONE fresh source, which rag renders at
	// two orientation rungs, giving (k+1) prefixes x 2 rungs alternatives.
	if got := 2 * (maxRetrieveMaxK + 1); got > carrierMaxAlternatives {
		t.Fatalf("maxRetrieveMaxK=%d yields %d alternatives for one fresh source, over the carrier limit %d",
			maxRetrieveMaxK, got, carrierMaxAlternatives)
	}
	// And the ceiling must actually be the largest value that fits, or the tool
	// is rejecting configurations assembly would accept.
	if got := 2 * (maxRetrieveMaxK + 2); got <= carrierMaxAlternatives {
		t.Fatalf("maxRetrieveMaxK=%d is below the carrier limit %d: %d alternatives would still fit",
			maxRetrieveMaxK, carrierMaxAlternatives, got)
	}
	if defaultRetrieveMaxK > maxRetrieveMaxK {
		t.Fatalf("default MaxK %d exceeds its own ceiling %d", defaultRetrieveMaxK, maxRetrieveMaxK)
	}
}

func TestRetrieveRejectsMaxKAboveCarrierBound(t *testing.T) {
	// A MaxK over the ceiling is a programmer misconfiguration whose only other
	// symptom is a hard mixed-assembly validation failure much later, with a
	// message about alternative counts rather than about MaxK. Reject it here,
	// as a hard error, at the same severity assembly would.
	//
	// The ceiling binds EXACTLY where groups get built: it comes from agent's
	// maxContextAlternatives, which nothing but the projection can violate. The
	// two negative cases below pin it to the branch predicate rather than to the
	// Progressive field alone.
	tests := []struct {
		name    string
		tool    Retrieve
		wantErr bool
	}{
		{"progressive, one over the ceiling", Retrieve{R: progressiveFixture(), Progressive: true, MaxK: maxRetrieveMaxK + 1}, true},
		{"progressive, far over the ceiling", Retrieve{R: progressiveFixture(), Progressive: true, MaxK: 500}, true},
		{"progressive, at the ceiling", Retrieve{R: progressiveFixture(), Progressive: true, MaxK: maxRetrieveMaxK}, false},
		{"capable retriever, progressive off", Retrieve{R: progressiveFixture(), MaxK: 500}, false},
		{"progressive on, retriever not capable", Retrieve{R: fakeRetrieverLegacy{}, Progressive: true, MaxK: 500}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.tool.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("MaxK=%d must be rejected, got result %+v", tc.tool.MaxK, res)
			case tc.wantErr && !strings.Contains(err.Error(), "MaxK"):
				t.Errorf("error must name the field, got %v", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("MaxK=%d must be accepted here: %v", tc.tool.MaxK, err)
			}
		})
	}
}

func TestRetrieveLegacyMaxKAboveCarrierBoundIsHonored(t *testing.T) {
	// The flat path builds no groups, so agent's carrier bound cannot constrain
	// it: a legacy consumer asking for 500 results must GET 500. Caging the flat
	// path at 31 would be a functional regression on a shipped path, enforced by
	// a constraint that has nothing to do with it.
	cr := &capturingRetriever{}
	tool := Retrieve{R: cr, MaxK: 500}
	if _, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q","k":500}`)); err != nil {
		t.Fatalf("legacy MaxK above the carrier bound must be honored: %v", err)
	}
	if cr.gotK != 500 {
		t.Fatalf("backend received k = %d, want 500", cr.gotK)
	}
}

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
