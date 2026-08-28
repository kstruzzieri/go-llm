package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// chunkWithExtras is the recorder's input shape: a full rag.Chunk carrying
// fields the recorder must NOT retain, so a minimal-copy assertion has
// something to detect.
func chunkWithExtras(id, stableKey, source, content string, start, end int) rag.Chunk {
	return rag.Chunk{
		ID: id, StableKey: stableKey, Source: source, Content: content,
		StartLine: start, EndLine: end,
		Language: "go",
		Metadata: map[string]string{"symbol": "Foo"},
	}
}

func sourceFor(c rag.Chunk) agent.RetrievedSource {
	return agent.RetrievedSource{
		StableKey: c.StableKey, Source: c.Source,
		StartLine: c.StartLine, EndLine: c.EndLine,
	}
}

func TestEvidenceRecorderTurnResetAndLookup(t *testing.T) {
	rec := newEvidenceRecorder(1 << 20)
	rec.beginTurn()
	chunk := chunkWithExtras("c1", "sk1", "a.go", "package main", 1, 2)
	rec.record([]rag.SearchResult{{Chunk: chunk, Score: 0.9}})

	got, ok := rec.lookup(sourceFor(chunk))
	if !ok {
		t.Fatal("lookup missed a chunk recorded in this turn")
	}
	// Content is the whole point of the recorder: an identity-only store would
	// satisfy a presence-only assertion.
	if got.Content != "package main" || got.ID != "c1" {
		t.Fatalf("lookup returned %+v; want the recorded id and content", got)
	}
	if got.Source != "a.go" || got.StartLine != 1 || got.EndLine != 2 {
		t.Fatalf("lookup lost the span: %+v", got)
	}

	// A different span under the same key is a different chunk.
	other := sourceFor(chunk)
	other.StartLine, other.EndLine = 9, 9
	if _, ok := rec.lookup(other); ok {
		t.Fatal("a different span must not resolve")
	}

	rec.beginTurn()
	if _, ok := rec.lookup(sourceFor(chunk)); ok {
		t.Fatal("beginTurn must clear the previous turn's evidence")
	}
}

func TestEvidenceRecorderBoundsMinimalCopies(t *testing.T) {
	rec := newEvidenceRecorder(1 << 20)
	rec.beginTurn()
	chunk := chunkWithExtras("c1", "sk1", "a.go", "body", 1, 2)
	rec.record([]rag.SearchResult{{Chunk: chunk}})

	got, ok := rec.lookup(sourceFor(chunk))
	if !ok {
		t.Fatal("lookup missed")
	}
	// Section 5.3: only id, source, span, and content are retained. Retaining
	// the whole chunk would hold metadata maps and embeddings-adjacent fields
	// for the entire turn.
	if got.Language != "" || got.Metadata != nil || got.StableKey != "" {
		t.Fatalf("recorder retained non-minimal chunk fields: %+v", got)
	}
}

func TestEvidenceRecorderRejectsAmbiguousIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second rag.Chunk
	}{
		{"different chunk id", chunkWithExtras("c2", "sk1", "a.go", "body", 1, 2)},
		{"different content", chunkWithExtras("c1", "sk1", "a.go", "OTHER", 1, 2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := newEvidenceRecorder(1 << 20)
			rec.beginTurn()
			first := chunkWithExtras("c1", "sk1", "a.go", "body", 1, 2)
			rec.record([]rag.SearchResult{{Chunk: first}})
			rec.record([]rag.SearchResult{{Chunk: tc.second}})

			if _, ok := rec.lookup(sourceFor(first)); ok {
				t.Fatal("an ambiguous key must not resolve to either chunk")
			}
		})
	}

	// A byte-identical re-record of the same chunk is not ambiguity.
	rec := newEvidenceRecorder(1 << 20)
	rec.beginTurn()
	c := chunkWithExtras("c1", "sk1", "a.go", "body", 1, 2)
	rec.record([]rag.SearchResult{{Chunk: c}})
	rec.record([]rag.SearchResult{{Chunk: c}})
	if _, ok := rec.lookup(sourceFor(c)); !ok {
		t.Fatal("re-recording the identical chunk must stay resolvable")
	}
}

func TestEvidenceRecorderRecordsLaterSmallEntryAfterOversizedEntry(t *testing.T) {
	// Budget fits exactly one small entry plus overhead, never the big one.
	small := chunkWithExtras("s", "sks", "s.go", "tiny", 1, 1)
	big := chunkWithExtras("b", "skb", "b.go", strings.Repeat("x", 4096), 1, 1)
	rec := newEvidenceRecorder(groundingEvidenceEntryOverhead + 64)
	rec.beginTurn()
	rec.record([]rag.SearchResult{{Chunk: big}, {Chunk: small}})

	if _, ok := rec.lookup(sourceFor(big)); ok {
		t.Fatal("an oversized entry must be refused")
	}
	// Section 5.4: refusing one entry must not abandon the rest of the batch.
	if _, ok := rec.lookup(sourceFor(small)); !ok {
		t.Fatal("a later small entry must still be recorded after a refusal")
	}
}

func TestEvidenceRecorderConcurrentRecordAndLookup(t *testing.T) {
	rec := newEvidenceRecorder(1 << 20)
	rec.beginTurn()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i)
			c := chunkWithExtras(key, key, key+".go", key, 1, 1)
			rec.record([]rag.SearchResult{{Chunk: c}})
			if _, ok := rec.lookup(sourceFor(c)); !ok {
				t.Errorf("chunk %s vanished", key)
			}
		}(i)
	}
	wg.Wait()
}

// fakeGroundingRetriever implements every method golem's concrete retriever
// exposes to agenttools.Retrieve. renderCount bounds how many results the
// legacy renderer claims to have emitted, which is what drives the tool's
// attribution truncation.
type fakeGroundingRetriever struct {
	results     []rag.SearchResult
	renderCount int
	gotK        int
	progressive bool
}

func (f *fakeGroundingRetriever) Retrieve(_ context.Context, _ string, k int) ([]rag.SearchResult, error) {
	f.gotK = k
	return f.results, nil
}

func (f *fakeGroundingRetriever) BuildContext(r []rag.SearchResult, maxTokens int) string {
	s, _ := f.BuildContextWithRenderedCount(r, maxTokens)
	return s
}

func (f *fakeGroundingRetriever) BuildContextWithRenderedCount(_ []rag.SearchResult, _ int) (string, int) {
	return "legacy-context", f.renderCount
}

func (f *fakeGroundingRetriever) RenderProgressiveWithGroups(_ context.Context, _ rag.ProgressiveRenderRequest) (string, rag.ProgressiveTrace, []rag.ProgressiveGroup, error) {
	f.progressive = true
	trace := rag.ProgressiveTrace{Sources: []rag.ProgressiveSourceTrace{{
		Source: f.results[0].Chunk.Source,
		RenderedEvidence: []rag.RenderedEvidence{{
			Source:    f.results[0].Chunk.Source,
			ChunkID:   f.results[0].Chunk.ID,
			StableKey: f.results[0].Chunk.StableKey,
			StartLine: f.results[0].Chunk.StartLine,
			EndLine:   f.results[0].Chunk.EndLine,
		}},
	}}}
	return "progressive-context", trace, nil, nil
}

func threeResults() []rag.SearchResult {
	out := make([]rag.SearchResult, 3)
	for i := range out {
		out[i] = rag.SearchResult{
			Chunk: chunkWithExtras(
				fmt.Sprintf("c%d", i), fmt.Sprintf("sk%d", i),
				fmt.Sprintf("f%d.go", i), fmt.Sprintf("body-%d", i), 1, 2),
			Score: float64(i),
		}
	}
	return out
}

// TestRecordingRetrieverPreservesLegacyRenderedCount proves the wrapper keeps
// the optional BuildContextWithRenderedCount capability. Dropping it is silent:
// agent/tools' interfaces are unexported, so the tool would fall back to
// BuildContext and credit every retrieved result, which is exactly the
// over-crediting grounding must never judge against.
func TestRecordingRetrieverPreservesLegacyRenderedCount(t *testing.T) {
	inner := &fakeGroundingRetriever{results: threeResults(), renderCount: 1}
	rec := newEvidenceRecorder(1 << 20)
	rec.beginTurn()
	tool := agenttools.Retrieve{R: &recordingRetriever{inner: inner, rec: rec}, MaxTokens: 64}

	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q","k":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "legacy-context" {
		t.Fatalf("legacy render not forwarded: %q", out.Content)
	}
	if out.Attrib == nil || len(out.Attrib.Sources) != 1 || out.Attrib.Sources[0].StableKey != "sk0" {
		t.Fatalf("attribution credited %+v; want only the rendered prefix", out.Attrib)
	}
	for _, want := range []string{"body-0", "body-1", "body-2"} {
		found := false
		for _, r := range threeResults() {
			if r.Chunk.Content != want {
				continue
			}
			if _, ok := rec.lookup(sourceFor(r.Chunk)); ok {
				found = true
			}
		}
		if !found {
			t.Fatalf("wrapper did not record %q", want)
		}
	}
}

// TestRecordingRetrieverPreservesProgressiveGroups proves the wrapper still
// satisfies the progressive capability. Without RenderProgressiveWithGroups the
// tool fails every call outright.
func TestRecordingRetrieverPreservesProgressiveGroups(t *testing.T) {
	inner := &fakeGroundingRetriever{results: threeResults(), renderCount: 3}
	rec := newEvidenceRecorder(1 << 20)
	rec.beginTurn()
	tool := agenttools.Retrieve{R: &recordingRetriever{inner: inner, rec: rec}, MaxTokens: 64, Progressive: true}

	out, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"q","k":3}`))
	if err != nil {
		t.Fatalf("progressive Invoke rejected the wrapper: %v", err)
	}
	if !inner.progressive || out.Content != "progressive-context" {
		t.Fatalf("progressive render not forwarded: %+v", out)
	}
	if out.Attrib == nil || len(out.Attrib.Sources) != 1 || out.Attrib.Sources[0].StableKey != "sk0" {
		t.Fatalf("progressive attribution = %+v; want the single rendered evidence", out.Attrib)
	}
	if _, ok := rec.lookup(sourceFor(threeResults()[0].Chunk)); !ok {
		t.Fatal("wrapper did not record under the progressive path")
	}
}

// TestRecordingRetrieverRecordsOnlyTheRequestedPrefix pins Section 5.2: the
// tool truncates an over-returning retriever to k, so recording the whole
// return would retain evidence the model never received.
func TestRecordingRetrieverRecordsOnlyTheRequestedPrefix(t *testing.T) {
	inner := &fakeGroundingRetriever{results: threeResults(), renderCount: 3}
	rec := newEvidenceRecorder(1 << 20)
	rec.beginTurn()
	w := &recordingRetriever{inner: inner, rec: rec}

	got, err := w.Retrieve(context.Background(), "q", 2)
	if err != nil || len(got) != 3 {
		t.Fatalf("wrapper must return the inner result unchanged: %d %v", len(got), err)
	}
	all := threeResults()
	if _, ok := rec.lookup(sourceFor(all[1].Chunk)); !ok {
		t.Fatal("the requested prefix must be recorded")
	}
	if _, ok := rec.lookup(sourceFor(all[2].Chunk)); ok {
		t.Fatal("results beyond the requested k must not be recorded")
	}
}

func retrieveCall(id string) provider.ToolCall {
	call := provider.ToolCall{ID: id}
	call.Function.Name = "retrieve"
	return call
}

func presentation(step int, srcs ...agent.RetrievedSource) agent.RetrievalPresentationEvent {
	return agent.RetrievalPresentationEvent{
		Step:        step,
		Attribution: agent.RetrievalAttribution{Sources: srcs},
	}
}

func src(key string) agent.RetrievedSource {
	return agent.RetrievedSource{StableKey: key, Source: key + ".go", StartLine: 1, EndLine: 2}
}

func TestGroundingCollectorCommitsTheCompletedStep(t *testing.T) {
	ctx := context.Background()
	c := &groundingCollector{}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Step 0 presents two messages: both belong to the same assembled prompt.
	must(c.OnRetrievalPresentation(ctx, presentation(0, src("a"))))
	must(c.OnRetrievalPresentation(ctx, presentation(0, src("b"))))
	must(c.OnStep(ctx, agent.StepEvent{Index: 0}))

	got := c.finalSources()
	if len(got) != 2 || got[0].StableKey != "a" || got[1].StableKey != "b" {
		t.Fatalf("same-step presentations must accumulate in order: %+v", got)
	}
}

// TestGroundingCollectorEvidenceFreeFinalStepClearsEarlierEvidence is the
// defect the review caught: tracking the highest step that happened to expose
// retrieval would let an evidence-free answer inherit an earlier step's
// evidence and be judged against context the answering model never had.
func TestGroundingCollectorEvidenceFreeFinalStepClearsEarlierEvidence(t *testing.T) {
	ctx := context.Background()
	c := &groundingCollector{}
	_ = c.OnRetrievalPresentation(ctx, presentation(0, src("a")))
	_ = c.OnStep(ctx, agent.StepEvent{Index: 0})
	// Step 1 assembled no attributed message at all.
	_ = c.OnStep(ctx, agent.StepEvent{Index: 1})

	if got := c.finalSources(); len(got) != 0 {
		t.Fatalf("an evidence-free final step must present no evidence, got %+v", got)
	}
	if c.retrieved() {
		t.Fatal("retrieved() must be false when the final step carried no evidence")
	}
}

// TestGroundingCollectorIgnoresPendingFromAnotherStep pins the fail-closed
// half of the commit rule. The orchestrator always emits OnStep(N) after step
// N's presentations, so this sequence does not occur today — which is exactly
// why the guard needs its own test: without one, deleting it changes nothing
// that any other assertion can see, and a future early-return in the step loop
// would silently let one step's evidence be committed as another's.
//
// Committing an EMPTY set is the required outcome. Evidence the answering step
// did not receive must never reach the judge.
func TestGroundingCollectorIgnoresPendingFromAnotherStep(t *testing.T) {
	ctx := context.Background()
	c := &groundingCollector{}
	_ = c.OnRetrievalPresentation(ctx, presentation(0, src("a")))
	// Step 0's OnStep never arrives; step 1 completes instead.
	_ = c.OnStep(ctx, agent.StepEvent{Index: 1})

	if got := c.finalSources(); len(got) != 0 {
		t.Fatalf("pending evidence from step 0 must not commit as step 1's: %+v", got)
	}
}

func TestGroundingCollectorDedupesInFirstPresentationOrder(t *testing.T) {
	ctx := context.Background()
	c := &groundingCollector{}
	_ = c.OnRetrievalPresentation(ctx, presentation(0, src("b"), src("a")))
	_ = c.OnRetrievalPresentation(ctx, presentation(0, src("a"), src("c")))
	_ = c.OnStep(ctx, agent.StepEvent{Index: 0})

	got := c.finalSources()
	if len(got) != 3 {
		t.Fatalf("want 3 distinct sources, got %+v", got)
	}
	if got[0].StableKey != "b" || got[1].StableKey != "a" || got[2].StableKey != "c" {
		t.Fatalf("dedupe must keep first-presentation order: %+v", got)
	}
}

func TestGroundingCollectorCountsOnlySuccessfulRetrieveCalls(t *testing.T) {
	ctx := context.Background()
	other := provider.ToolCall{ID: "t0"}
	other.Function.Name = "read_file"

	for _, tc := range []struct {
		name string
		e    agent.ToolResultEvent
		want bool
	}{
		{"other tool", agent.ToolResultEvent{Call: other, Invoked: true}, false},
		{"not invoked", agent.ToolResultEvent{Call: retrieveCall("t1")}, false},
		{"denied", agent.ToolResultEvent{Call: retrieveCall("t1"), Invoked: true, Denied: true}, false},
		{"tool error", agent.ToolResultEvent{Call: retrieveCall("t1"), Invoked: true,
			Result: agent.ToolResult{IsError: true}}, false},
		{"successful", agent.ToolResultEvent{Call: retrieveCall("t1"), Invoked: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &groundingCollector{}
			if err := c.OnToolResult(ctx, tc.e); err != nil {
				t.Fatal(err)
			}
			if got := c.sawRetrieveCall(); got != tc.want {
				t.Fatalf("sawRetrieveCall() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGroundingCollectorTruncatedRetrievePoisonsCompleteness(t *testing.T) {
	ctx := context.Background()
	c := &groundingCollector{}
	if !c.evidenceComplete() {
		t.Fatal("a fresh collector must report complete evidence")
	}
	_ = c.OnToolResult(ctx, agent.ToolResultEvent{
		Call: retrieveCall("t1"), Invoked: true,
		Result: agent.ToolResult{Truncated: true},
	})
	if c.evidenceComplete() {
		t.Fatal("a truncated successful retrieve must poison completeness")
	}
}
