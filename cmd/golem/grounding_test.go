package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/analysis"
	"github.com/kstruzzieri/go-llm/config"
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

// ---------------------------------------------------------------------------
// Task 2: frozen report, rendering, and the verification service.
// ---------------------------------------------------------------------------

// TestGroundingReportGoldenJSON pins the EXACT bytes #352 will serialize,
// including every nested analysis field name. A structural assertion would let
// a rename in analysis silently change the wire shape golem promised.
func TestGroundingReportGoldenJSON(t *testing.T) {
	rep := groundingReport{
		Status:     groundingPartial,
		Tokens:     850,
		DurationMS: 1200,
		Report: &analysis.SupportReport{
			Status: analysis.StatusPartial,
			Claims: []analysis.ClaimSupport{{
				ID:           "C1",
				Claim:        "the parser rejects tabs",
				Status:       analysis.StatusUnsupported,
				EvidenceIDs:  []string{"E1"},
				Contradicted: true,
				Reason:       "E1 shows tabs accepted",
			}},
			Evidence: []analysis.EvidenceRef{{
				ID: "E1", ChunkID: "c1", Source: "a.go", StartLine: 1, EndLine: 9,
			}},
			MissingEvidence:        []string{"tab handling"},
			MissingEvidenceQueries: []string{"tab handling parser"},
		},
	}
	const want = `{"status":"partial","tokens":850,"duration_ms":1200,` +
		`"report":{"status":"partial",` +
		`"claims":[{"id":"C1","claim":"the parser rejects tabs","status":"unsupported",` +
		`"evidence_ids":["E1"],"contradicted":true,"reason":"E1 shows tabs accepted"}],` +
		`"evidence":[{"id":"E1","chunk_id":"c1","source":"a.go","start_line":1,"end_line":9}],` +
		`"missing_evidence":["tab handling"],` +
		`"missing_evidence_queries":["tab handling parser"]}}`

	got, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("frozen payload drifted\ngot  %s\nwant %s", got, want)
	}
}

func TestGroundingReportOmitsReasonForAVerdictAndReportForASkip(t *testing.T) {
	verdict, err := json.Marshal(groundingReport{
		Status: groundingSupported,
		Report: &analysis.SupportReport{Status: analysis.StatusSupported},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(verdict), `"reason"`) {
		t.Fatalf("a verdict must omit reason: %s", verdict)
	}

	skip, err := json.Marshal(groundingReport{Status: groundingSkipped, Reason: groundingReasonNoFinalEvidence})
	if err != nil {
		t.Fatal(err)
	}
	if string(skip) != `{"status":"skipped","reason":"no_final_evidence"}` {
		t.Fatalf("skip payload = %s", skip)
	}
}

func TestGroundingSummaryLine(t *testing.T) {
	verdict := func(status analysis.SupportStatus, supported, total, evidence int) *analysis.SupportReport {
		r := &analysis.SupportReport{Status: status}
		for i := range total {
			s := analysis.StatusUnsupported
			if i < supported {
				s = analysis.StatusSupported
			}
			r.Claims = append(r.Claims, analysis.ClaimSupport{Status: s})
		}
		for i := range evidence {
			r.Evidence = append(r.Evidence, analysis.EvidenceRef{ID: fmt.Sprintf("E%d", i+1)})
		}
		return r
	}
	for _, tc := range []struct {
		name string
		rep  groundingReport
		diag string
		want string
	}{
		{name: "supported", want: "grounding · supported · 2/2 claims · 3 evidence · 1.2s · 850 tok",
			rep: groundingReport{Status: groundingSupported, Tokens: 850, DurationMS: 1200,
				Report: verdict(analysis.StatusSupported, 2, 2, 3)}},
		{name: "partial", want: "grounding · partial · 1/2 claims · 1 evidence · 0.5s · 12 tok",
			rep: groundingReport{Status: groundingPartial, Tokens: 12, DurationMS: 500,
				Report: verdict(analysis.StatusPartial, 1, 2, 1)}},
		{name: "unsupported", want: "grounding · unsupported · 0/1 claims · 1 evidence · 0.1s · 7 tok",
			rep: groundingReport{Status: groundingUnsupported, Tokens: 7, DurationMS: 100,
				Report: verdict(analysis.StatusUnsupported, 0, 1, 1)}},
		{name: "zero tokens omits the token field", want: "grounding · supported · 1/1 claims · 1 evidence · 0.1s",
			rep: groundingReport{Status: groundingSupported, DurationMS: 100,
				Report: verdict(analysis.StatusSupported, 1, 1, 1)}},
		{name: "skip", want: "grounding · skipped · no_final_evidence",
			rep: groundingReport{Status: groundingSkipped, Reason: groundingReasonNoFinalEvidence}},
		{name: "error with a multiline diagnostic",
			want: "grounding · error · judge_failed: route verify: no provider reachable",
			rep:  groundingReport{Status: groundingError, Reason: groundingReasonJudgeFailed},
			diag: "route verify:\nno provider reachable\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := groundingSummaryLine(tc.rep, tc.diag)
			if got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Fatalf("summary must be exactly one line: %q", got)
			}
		})
	}
}

func TestGroundingSummaryLineBoundsTheDiagnostic(t *testing.T) {
	// A multi-byte rune straddling the cap must not be split into invalid UTF-8.
	diag := strings.Repeat("é", 300)
	got := groundingSummaryLine(groundingReport{Status: groundingError, Reason: groundingReasonJudgeFailed}, diag)
	if !utf8.ValidString(got) {
		t.Fatal("summary line must remain valid UTF-8")
	}
	if len(got) > groundingDiagnosticMaxBytes+64 {
		t.Fatalf("diagnostic not bounded: %d bytes", len(got))
	}
}

// stubJudge replaces analysis.SupportJudge so the service's own policy is
// testable without a model.
type stubJudge struct {
	rep      *analysis.SupportReport
	err      error
	block    chan struct{}
	calls    int
	answer   string
	evidence []rag.SearchResult
	maxChars int
}

func (s *stubJudge) Judge(ctx context.Context, answer string, evidence []rag.SearchResult, maxEvidenceChars int) (*analysis.SupportReport, error) {
	s.calls++
	s.answer, s.evidence, s.maxChars = answer, evidence, maxEvidenceChars
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.rep, s.err
}

func testGroundingService(j *stubJudge, rec *evidenceRecorder) *groundingService {
	return &groundingService{
		rec:     rec,
		timeout: 2 * time.Second,
		now:     time.Now,
		newJudge: func() (groundingJudge, func() int, error) {
			return j, func() int { return 42 }, nil
		},
	}
}

// groundedFixture wires a recorder and a collector that presented n chunks on
// the final step, in order.
func groundedFixture(t *testing.T, n int) (*evidenceRecorder, *groundingCollector) {
	t.Helper()
	rec := newEvidenceRecorder(1 << 20)
	rec.beginTurn()
	c := &groundingCollector{}
	_ = c.OnToolResult(context.Background(), agent.ToolResultEvent{Call: retrieveCall("t1"), Invoked: true})
	srcs := make([]agent.RetrievedSource, 0, n)
	for i := range n {
		chunk := chunkWithExtras(
			fmt.Sprintf("c%d", i), fmt.Sprintf("sk%d", i),
			fmt.Sprintf("f%d.go", i), fmt.Sprintf("body-%d", i), 1, 2)
		rec.record([]rag.SearchResult{{Chunk: chunk}})
		s := sourceFor(chunk)
		s.Score = float64(i) / 10
		srcs = append(srcs, s)
	}
	_ = c.OnRetrievalPresentation(context.Background(), presentation(0, srcs...))
	_ = c.OnStep(context.Background(), agent.StepEvent{Index: 0})
	return rec, c
}

func TestGroundingVerifySilentCases(t *testing.T) {
	rec, c := groundedFixture(t, 1)
	for _, tc := range []struct {
		name   string
		answer string
		coll   *groundingCollector
	}{
		{"no retrieve call at all", "an answer", &groundingCollector{}},
		{"empty answer", "   \n  ", c},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := &stubJudge{}
			rep, diag, show := testGroundingService(j, rec).verify(context.Background(), tc.answer, tc.coll)
			if show || diag != "" || rep.Status != "" || j.calls != 0 {
				t.Fatalf("must be silent: rep=%+v diag=%q show=%v calls=%d", rep, diag, show, j.calls)
			}
		})
	}
}

// TestGroundingVerifyVisibleSkipWhenFinalPromptHadNoEvidence covers AC5: the
// turn retrieved, but the answering prompt carried nothing. Reporting a verdict
// here would judge claims against evidence from an earlier step.
func TestGroundingVerifyVisibleSkipWhenFinalPromptHadNoEvidence(t *testing.T) {
	c := &groundingCollector{}
	_ = c.OnToolResult(context.Background(), agent.ToolResultEvent{Call: retrieveCall("t1"), Invoked: true})
	_ = c.OnStep(context.Background(), agent.StepEvent{Index: 0})

	j := &stubJudge{}
	rep, _, show := testGroundingService(j, newEvidenceRecorder(1<<20)).verify(context.Background(), "a", c)
	if !show || rep.Status != groundingSkipped || rep.Reason != groundingReasonNoFinalEvidence {
		t.Fatalf("rep=%+v show=%v", rep, show)
	}
	if j.calls != 0 {
		t.Fatal("a skip must make no judge call")
	}
}

// TestGroundingVerifyPrefersNoFinalEvidenceOverIncomplete pins the precedence
// between the two skip reasons when both apply. An empty final prompt is the
// more accurate description: with no attribution on the answering step there is
// nothing for a truncated observation to have over-credited, and reporting
// evidence_incomplete would send a reader looking for a capture bug that is not
// there.
func TestGroundingVerifyPrefersNoFinalEvidenceOverIncomplete(t *testing.T) {
	c := &groundingCollector{}
	_ = c.OnToolResult(context.Background(), agent.ToolResultEvent{
		Call: retrieveCall("t1"), Invoked: true, Result: agent.ToolResult{Truncated: true}})
	_ = c.OnStep(context.Background(), agent.StepEvent{Index: 0})

	j := &stubJudge{}
	rep, _, show := testGroundingService(j, newEvidenceRecorder(1<<20)).verify(context.Background(), "a", c)
	if !show || rep.Status != groundingSkipped || rep.Reason != groundingReasonNoFinalEvidence {
		t.Fatalf("rep=%+v show=%v, want skipped/%s", rep, show, groundingReasonNoFinalEvidence)
	}
	if j.calls != 0 {
		t.Fatal("either skip reason must make zero judge calls")
	}
}

func TestGroundingVerifyJoinsEvidenceInPresentationOrder(t *testing.T) {
	rec, c := groundedFixture(t, 3)
	j := &stubJudge{rep: &analysis.SupportReport{
		Status:   analysis.StatusSupported,
		Evidence: []analysis.EvidenceRef{{ID: "E1"}, {ID: "E2"}, {ID: "E3"}},
	}}
	rep, diag, show := testGroundingService(j, rec).verify(context.Background(), "the answer", c)
	if !show || diag != "" || rep.Status != groundingSupported || rep.Reason != "" {
		t.Fatalf("rep=%+v diag=%q show=%v", rep, diag, show)
	}
	if j.answer != "the answer" {
		t.Fatalf("judge received answer %q", j.answer)
	}
	if len(j.evidence) != 3 {
		t.Fatalf("judge received %d evidence blocks", len(j.evidence))
	}
	for i, e := range j.evidence {
		if want := fmt.Sprintf("body-%d", i); e.Chunk.Content != want {
			t.Fatalf("evidence[%d] = %q, want %q (presentation order)", i, e.Chunk.Content, want)
		}
		if want := float64(i) / 10; e.Score != want {
			t.Fatalf("evidence[%d] score = %v, want the presented %v", i, e.Score, want)
		}
	}
	// The recorder budget, not analysis's 6000-char default: five 2 KB chunks
	// already exceed the default, and a silent tail drop manufactures
	// unsupported verdicts.
	if j.maxChars != groundingEvidenceMaxBytes {
		t.Fatalf("evidence cap = %d, want the recorder budget %d", j.maxChars, groundingEvidenceMaxBytes)
	}
	if rep.Tokens != 42 {
		t.Fatalf("verifier tokens = %d, want 42", rep.Tokens)
	}
	if rep.Report == nil || rep.Report.Status != analysis.StatusSupported {
		t.Fatalf("verdict not carried: %+v", rep.Report)
	}
}

func TestGroundingVerifySkipsOnIncompleteEvidence(t *testing.T) {
	base := func(t *testing.T) (*evidenceRecorder, *groundingCollector) { return groundedFixture(t, 2) }

	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T) (*evidenceRecorder, *groundingCollector)
	}{
		{"a presented identity was never recorded", func(t *testing.T) (*evidenceRecorder, *groundingCollector) {
			rec, c := base(t)
			_ = c.OnRetrievalPresentation(context.Background(), presentation(0, src("never-recorded")))
			_ = c.OnStep(context.Background(), agent.StepEvent{Index: 0})
			return rec, c
		}},
		{"a presented identity is ambiguous", func(t *testing.T) (*evidenceRecorder, *groundingCollector) {
			rec, c := base(t)
			rec.record([]rag.SearchResult{{Chunk: chunkWithExtras("other", "sk0", "f0.go", "DIFFERENT", 1, 2)}})
			return rec, c
		}},
		{"a successful retrieve observation was truncated", func(t *testing.T) (*evidenceRecorder, *groundingCollector) {
			rec, c := base(t)
			_ = c.OnToolResult(context.Background(), agent.ToolResultEvent{
				Call: retrieveCall("t2"), Invoked: true, Result: agent.ToolResult{Truncated: true}})
			return rec, c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, c := tc.prepare(t)
			j := &stubJudge{rep: &analysis.SupportReport{Status: analysis.StatusSupported}}
			rep, _, show := testGroundingService(j, rec).verify(context.Background(), "a", c)
			if !show || rep.Status != groundingSkipped || rep.Reason != groundingReasonEvidenceIncomplete {
				t.Fatalf("rep=%+v show=%v", rep, show)
			}
			// The invariant: never a verdict over a silently reduced subset.
			if j.calls != 0 {
				t.Fatalf("incomplete evidence must make zero judge calls, made %d", j.calls)
			}
		})
	}
}

func TestGroundingVerifyFailOpenOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		judge      func() *stubJudge
		cancel     bool
		timeout    time.Duration
		wantStatus string
		wantReason string
		wantDiag   bool
	}{
		{name: "judge error", wantStatus: groundingError, wantReason: groundingReasonJudgeFailed, wantDiag: true,
			judge: func() *stubJudge { return &stubJudge{err: analysis.ErrSupportVerifyMalformed} }},
		{name: "nil report without an error", wantStatus: groundingError, wantReason: groundingReasonJudgeFailed,
			judge: func() *stubJudge { return &stubJudge{} }},
		{name: "timeout", wantStatus: groundingError, wantReason: groundingReasonTimeout, timeout: 10 * time.Millisecond,
			judge: func() *stubJudge { return &stubJudge{block: make(chan struct{})} }},
		{name: "caller cancellation", wantStatus: groundingSkipped, wantReason: groundingReasonCanceled, cancel: true,
			judge: func() *stubJudge { return &stubJudge{block: make(chan struct{})} }},
		{name: "judge dropped evidence blocks", wantStatus: groundingError, wantReason: groundingReasonEvidenceTruncated,
			judge: func() *stubJudge {
				return &stubJudge{rep: &analysis.SupportReport{Status: analysis.StatusSupported,
					Evidence: []analysis.EvidenceRef{{ID: "E1"}}}} // 1 ref for 2 joined blocks
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, c := groundedFixture(t, 2)
			j := tc.judge()
			if j.block != nil {
				defer close(j.block)
			}
			svc := testGroundingService(j, rec)
			if tc.timeout > 0 {
				svc.timeout = tc.timeout
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				go func() { time.Sleep(10 * time.Millisecond); cancel() }()
			}
			rep, diag, show := svc.verify(ctx, "an answer", c)
			if !show || rep.Status != tc.wantStatus || rep.Reason != tc.wantReason {
				t.Fatalf("rep=%+v show=%v, want %s/%s", rep, show, tc.wantStatus, tc.wantReason)
			}
			if rep.Report != nil {
				t.Fatalf("a non-verdict outcome must carry no report: %+v", rep.Report)
			}
			if tc.wantDiag && diag == "" {
				t.Fatal("a provider/judge failure must supply a terminal diagnostic")
			}
			// Stable codes only: raw error prose must never reach the payload.
			if strings.ContainsAny(rep.Reason, " :") {
				t.Fatalf("reason %q is not a stable code", rep.Reason)
			}
		})
	}
}

// TestGroundingChatRoutesEachStageIndependently pins D5. The two stages are
// distinct use cases with distinct chains; collapsing them would silently route
// claim extraction through the verify chain.
func TestGroundingChatRoutesEachStageIndependently(t *testing.T) {
	var got []provider.RoutingRequest
	route := func(_ context.Context, rr provider.RoutingRequest) (*provider.ChatResponse, error) {
		got = append(got, rr)
		return &provider.ChatResponse{Usage: provider.Usage{TotalTokens: 11}}, nil
	}
	chains := map[string][]string{
		config.UseCaseExtract: {"ollama:extract-model"},
		config.UseCaseVerify:  {"ollama:verify-model"},
	}
	chat, tokens := newGroundingChat(route, chains)

	for _, uc := range []string{config.UseCaseExtract, config.UseCaseVerify} {
		if _, err := chat(context.Background(), uc, provider.ChatRequest{
			Messages: []provider.ChatMessage{{Role: "user", Content: "x"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 {
		t.Fatalf("want 2 routed requests, got %d", len(got))
	}
	for i, uc := range []string{config.UseCaseExtract, config.UseCaseVerify} {
		rr := got[i]
		if rr.UseCase != uc {
			t.Fatalf("request %d use case = %q, want %q", i, rr.UseCase, uc)
		}
		if len(rr.PreferredChain) != 1 || rr.PreferredChain[0] != chains[uc][0] {
			t.Fatalf("request %d chain = %v, want %v", i, rr.PreferredChain, chains[uc])
		}
		if !rr.StrictChain {
			t.Fatalf("request %d must route strictly; a safety-net tail can swap the primary model", i)
		}
		if rr.RequiredCaps != provider.CapChat {
			t.Fatalf("request %d caps = %v, want CapChat", i, rr.RequiredCaps)
		}
		if rr.ExpectedOutput != provider.DefaultExpectedOutput(uc) {
			t.Fatalf("request %d expected output = %d", i, rr.ExpectedOutput)
		}
		// PriorityBackground is the zero value, so this assertion pins intent
		// rather than distinguishing "set" from "defaulted".
		if rr.Priority != provider.PriorityBackground {
			t.Fatalf("request %d priority = %v", i, rr.Priority)
		}
		if rr.Model != "" {
			t.Fatalf("request %d pinned a model: %q", i, rr.Model)
		}
	}
	if n := tokens(); n != 22 {
		t.Fatalf("combined stage usage = %d, want 22", n)
	}
}

func TestGroundingChatPropagatesRouteFailure(t *testing.T) {
	route := func(context.Context, provider.RoutingRequest) (*provider.ChatResponse, error) {
		return nil, errors.New("no provider reachable")
	}
	chat, tokens := newGroundingChat(route, map[string][]string{config.UseCaseVerify: {"m"}})
	if _, err := chat(context.Background(), config.UseCaseVerify, provider.ChatRequest{}); err == nil {
		t.Fatal("route failure must propagate")
	}
	if n := tokens(); n != 0 {
		t.Fatalf("a failed call must contribute no tokens, got %d", n)
	}
}
