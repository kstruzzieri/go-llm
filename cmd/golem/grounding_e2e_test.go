package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/analysis"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// ---------------------------------------------------------------------------
// End-to-end harness: a scripted model that retrieves and then answers, driven
// through the real runOnce, with grounding wired exactly as main.go wires it.
// ---------------------------------------------------------------------------

func retrieveToolCall(id, query string) provider.ToolCall {
	args, err := json.Marshal(map[string]any{"query": query, "k": 3})
	if err != nil {
		panic(err)
	}
	return provider.ToolCall{
		ID: id, Type: "function",
		Function: provider.ToolCallFunction{Name: "retrieve", Arguments: args},
	}
}

func answerStep(text string) agent.ModelResult {
	return agent.ModelResult{Response: provider.ChatResponse{Content: text, Done: true}}
}

func retrieveStep(id, query string) agent.ModelResult {
	return agent.ModelResult{Response: provider.ChatResponse{
		ToolCalls: []provider.ToolCall{retrieveToolCall(id, query)},
	}}
}

// failingCaller fails the run so the turn never completes.
type failingCaller struct{ err error }

func (f *failingCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	return agent.ModelResult{}, f.err
}

// cancelOnAnswerCaller cancels the turn as it delivers the final answer, so the
// run returns a real answer together with context.Canceled.
type cancelOnAnswerCaller struct {
	inner  *scriptCaller
	cancel context.CancelFunc
}

func (c *cancelOnAnswerCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	res, err := c.inner.Chat(ctx, req, onToken)
	if err == nil && res.Response.Content != "" {
		c.cancel()
	}
	return res, err
}

type groundingE2EOpts struct {
	// script is the model's per-step output. Default: retrieve, then answer.
	script []agent.ModelResult
	// grounding false leaves sess.grounding nil, the flag-off path.
	grounding bool
	// color enables ANSI styling, so a test can see the dim treatment.
	color       bool
	progressive bool
	judge       *stubJudge
	// trace/telemetry enable the observability tiers under a controllable clock.
	trace     bool
	telemetry bool
	// judgeDelay advances the grounding clock while the judge runs, so a test
	// can tell verifier latency apart from agent latency without sleeping.
	judgeDelay time.Duration
	// results overrides the retriever's corpus.
	results []rag.SearchResult
	// recorderBytes overrides the evidence recorder's per-turn budget.
	recorderBytes int
	// callerErr makes the model fail its final step, so the turn does not
	// complete.
	callerErr error
	// cancelOnAnswer cancels the turn's context as the model emits its answer,
	// producing a run that carries BOTH an answer and a cancellation.
	cancelOnAnswer bool
}

type groundingE2E struct {
	sess     *replSession
	judge    *stubJudge
	retr     *fakeGroundingRetriever
	traceDir string
	telePath string
	out      *strings.Builder
	// now is the single clock behind both observability and grounding.
	now time.Time
	// ctx is set when the fixture owns the turn's context.
	ctx context.Context
}

func newGroundingE2E(t *testing.T, opts groundingE2EOpts) *groundingE2E {
	t.Helper()
	root := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)

	results := opts.results
	if results == nil {
		results = threeResults()
	}
	retr := &fakeGroundingRetriever{results: results, renderCount: len(results)}
	recorderBytes := opts.recorderBytes
	if recorderBytes == 0 {
		recorderBytes = groundingEvidenceMaxBytes
	}
	rec := newEvidenceRecorder(recorderBytes)
	tool := &agenttools.Retrieve{
		R: &recordingRetriever{inner: retr, rec: rec}, K: 3, MaxTokens: 2048,
		Progressive: opts.progressive,
	}

	script := opts.script
	if script == nil {
		script = []agent.ModelResult{retrieveStep("t1", "q"), answerStep("the answer")}
	}
	var caller agent.ModelCaller = &scriptCaller{responses: script}
	if opts.callerErr != nil {
		caller = &failingCaller{err: opts.callerErr}
	}
	var turnCtx context.Context
	if opts.cancelOnAnswer {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		turnCtx = ctx
		caller = &cancelOnAnswerCaller{inner: &scriptCaller{responses: script}, cancel: cancel}
	}
	system := buildSystemPrompt(false, false)
	// Golem couples both halves of -progressive: the renderer and mixed assembly.
	orch := agent.New(caller, agent.ContextManager{Mixed: opts.progressive})

	e := &groundingE2E{judge: opts.judge, retr: retr, out: &strings.Builder{},
		now: time.Unix(0, 0), ctx: turnCtx}
	e.sess = &replSession{
		orch:       orch,
		runtime:    newTestRuntime(t, root, system, orch, []agent.Tool{tool}),
		tools:      []agent.Tool{tool},
		baseSystem: system,
		maxSteps:   16,
		clock:      func() time.Time { return e.now },
		grants:     newApprovalGrants(),
		mixed:      opts.progressive,
		color:      opts.color,
	}

	if opts.grounding {
		judge := opts.judge
		if judge == nil {
			judge = &stubJudge{rep: &analysis.SupportReport{
				Status:   analysis.StatusSupported,
				Claims:   []analysis.ClaimSupport{{ID: "C1", Status: analysis.StatusSupported}},
				Evidence: evidenceRefsFor(results),
			}}
			e.judge = judge
		}
		e.sess.grounding = &groundingService{
			rec: rec,
			// Generous: no test exercises the production ceiling here, and the
			// timeout policy itself is covered by the service tests.
			timeout: 2 * time.Second,
			// Advancing the SHARED clock is what lets a test tell a timestamp
			// taken before grounding from one taken after it.
			now: func() time.Time {
				now := e.now
				e.now = e.now.Add(opts.judgeDelay)
				return now
			},
			newJudge: func() (groundingJudge, func() int, error) {
				return judge, func() int { return 42 }, nil
			},
		}
	}

	if opts.trace || opts.telemetry {
		obs, err := newObserv(func(k string) string {
			if k == "XDG_DATA_HOME" {
				return dataDir
			}
			return os.Getenv(k)
		}, root, opts.trace, opts.telemetry, func() time.Time { return e.now })
		if err != nil {
			t.Fatalf("newObserv: %v", err)
		}
		e.sess.obs = obs
		e.traceDir = obs.traceDir
		e.telePath = obs.telemetryPath
	}
	return e
}

func evidenceRefsFor(results []rag.SearchResult) []analysis.EvidenceRef {
	refs := make([]analysis.EvidenceRef, 0, len(results))
	for i, r := range results {
		refs = append(refs, analysis.EvidenceRef{
			ID: fmt.Sprintf("E%d", i+1), ChunkID: r.Chunk.ID, Source: r.Chunk.Source,
			StartLine: r.Chunk.StartLine, EndLine: r.Chunk.EndLine,
		})
	}
	return refs
}

func (e *groundingE2E) run(t *testing.T, ctx context.Context, interrupts <-chan struct{}) agent.Result {
	t.Helper()
	res, err := runOnce(ctx, e.out, interrupts, e.sess, "answer this", nil)
	if err != nil {
		t.Fatalf("runOnce: %v", err)
	}
	return res
}

// readTrace returns the single trace record written for the turn.
func (e *groundingE2E) readTrace(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	entries, err := os.ReadDir(e.traceDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one trace file, got %d", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(e.traceDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("trace is not valid JSON: %v\n%s", err, raw)
	}
	return rec
}

// ---------------------------------------------------------------------------
// Acceptance criteria
// ---------------------------------------------------------------------------

// AC1: the flag off must change nothing. Both arms run the identical fixture;
// only sess.grounding differs.
func TestGroundingFlagOffChangesNothing(t *testing.T) {
	off := newGroundingE2E(t, groundingE2EOpts{trace: true})
	offRes := off.run(t, context.Background(), nil)
	offTrace := off.readTrace(t)

	on := newGroundingE2E(t, groundingE2EOpts{grounding: true, trace: true})
	onRes := on.run(t, context.Background(), nil)

	if strings.Contains(off.out.String(), "grounding") {
		t.Fatalf("flag off printed a grounding line:\n%s", off.out.String())
	}
	if _, ok := offTrace["grounding"]; ok {
		t.Fatal("flag off wrote a grounding trace key")
	}
	if off.judge != nil {
		t.Fatal("flag off must construct no judge")
	}
	// The answer and its usage must be identical with the flag on: verification
	// is post-run and never feeds back into the agent result.
	if offRes.Answer != onRes.Answer || offRes.Usage != onRes.Usage {
		t.Fatalf("flag changed the run: %q/%+v vs %q/%+v",
			offRes.Answer, offRes.Usage, onRes.Answer, onRes.Usage)
	}
	if !strings.Contains(on.out.String(), "grounding · supported") {
		t.Fatalf("flag on printed no verdict:\n%s", on.out.String())
	}
}

// AC2 + AC10: a legacy retrieval-backed answer renders one line and persists the
// complete report, with the rendered status and the stored status agreeing.
func TestGroundingLegacyAnswerRendersLineAndPersistsReport(t *testing.T) {
	e := newGroundingE2E(t, groundingE2EOpts{grounding: true, trace: true})
	e.run(t, context.Background(), nil)

	line := e.out.String()
	if got := strings.Count(line, "grounding · "); got != 1 {
		t.Fatalf("want exactly one grounding line, got %d:\n%s", got, line)
	}
	if !strings.Contains(line, "grounding · supported · 1/1 claims · 3 evidence") {
		t.Fatalf("summary line wrong:\n%s", line)
	}
	if e.judge.calls != 1 {
		t.Fatalf("judge calls = %d, want 1", e.judge.calls)
	}
	if len(e.judge.evidence) != 3 {
		t.Fatalf("judged against %d evidence blocks, want 3", len(e.judge.evidence))
	}

	rec := e.readTrace(t)
	rawGround, ok := rec["grounding"]
	if !ok {
		t.Fatalf("trace has no grounding key: %v", rec)
	}
	var stored groundingReport
	if err := json.Unmarshal(rawGround, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status != groundingSupported || stored.Report == nil {
		t.Fatalf("stored report = %+v", stored)
	}
	if !strings.Contains(line, stored.Status) {
		t.Fatalf("rendered line %q does not carry the stored status %q", line, stored.Status)
	}
}

// AC3: the judge must see what post-assembly attribution names, not the raw
// retrieval set. The fake renderer attributes one of three retrieved results,
// so a grounding path that reached past attribution to the retriever's own
// output would judge three. (The fixture returns no capability groups, so the
// assembly itself stays on the legacy arm; what it pins is that attribution —
// progressive here — is the only source of evidence identity.)
func TestGroundingProgressiveJudgesChosenAlternativesOnly(t *testing.T) {
	e := newGroundingE2E(t, groundingE2EOpts{grounding: true, progressive: true})
	e.run(t, context.Background(), nil)

	if !e.retr.progressive {
		t.Fatal("fixture did not exercise the progressive renderer")
	}
	if e.judge.calls != 1 {
		t.Fatalf("judge calls = %d, want 1", e.judge.calls)
	}
	// The fake renderer attributes exactly one source; the corpus holds three.
	// Judging all three would mean grounding fell back to the raw retrieval set
	// instead of the chosen alternatives.
	if len(e.judge.evidence) != 1 {
		t.Fatalf("judged against %d blocks, want the 1 chosen alternative", len(e.judge.evidence))
	}
	if got := e.judge.evidence[0].Chunk.Content; got != "body-0" {
		t.Fatalf("judged wrong evidence %q", got)
	}
}

// AC4: a turn that never retrieves is silent end to end.
func TestGroundingSilentWhenTurnDidNotRetrieve(t *testing.T) {
	e := newGroundingE2E(t, groundingE2EOpts{
		grounding: true, trace: true,
		script: []agent.ModelResult{answerStep("no retrieval needed")},
	})
	e.run(t, context.Background(), nil)

	if strings.Contains(e.out.String(), "grounding") {
		t.Fatalf("a non-retrieval turn printed a grounding line:\n%s", e.out.String())
	}
	if e.judge.calls != 0 {
		t.Fatalf("judge calls = %d, want 0", e.judge.calls)
	}
	if _, ok := e.readTrace(t)["grounding"]; ok {
		t.Fatal("a non-retrieval turn wrote a grounding trace key")
	}
}

// AC5: retrieval on an earlier step whose evidence did not survive into the
// answering prompt must not be judged. Two retrieve calls, then an answer whose
// context assembly is irrelevant: what matters is that the ANSWER step is the
// one whose attribution counts.
func TestGroundingSkipsWhenFinalPromptCarriesNoEvidence(t *testing.T) {
	// A retriever that returns nothing produces a successful retrieve call with
	// no attribution, so the answering prompt carries no retrieval evidence.
	e := newGroundingE2E(t, groundingE2EOpts{
		grounding: true, trace: true,
		results: []rag.SearchResult{},
	})
	e.run(t, context.Background(), nil)

	if e.judge.calls != 0 {
		t.Fatalf("judge calls = %d, want 0", e.judge.calls)
	}
	// A visible skip, not silence: the turn DID retrieve, so hiding the outcome
	// would misreport it as a turn that never used retrieval at all.
	if !strings.Contains(e.out.String(), "grounding · skipped · "+groundingReasonNoFinalEvidence) {
		t.Fatalf("want a visible %s skip:\n%s", groundingReasonNoFinalEvidence, e.out.String())
	}
}

// AC6: an identity the recorder cannot resolve fails closed, with no judge call.
func TestGroundingSkipsOnUnresolvableEvidence(t *testing.T) {
	// Two results share one identity tuple but carry different text — what a
	// mid-run reindex looks like from the recorder's side. Neither candidate may
	// be substituted, so the whole turn fails closed.
	e := newGroundingE2E(t, groundingE2EOpts{grounding: true, trace: true, results: []rag.SearchResult{
		{Chunk: chunkWithExtras("c0", "sk0", "f0.go", "body-0", 1, 2)},
		{Chunk: chunkWithExtras("c0b", "sk0", "f0.go", "DIFFERENT", 1, 2)},
	}})
	e.run(t, context.Background(), nil)

	if e.judge.calls != 0 {
		t.Fatalf("an unresolvable identity must make zero judge calls, made %d", e.judge.calls)
	}
	if !strings.Contains(e.out.String(), groundingReasonEvidenceIncomplete) {
		t.Fatalf("want %s:\n%s", groundingReasonEvidenceIncomplete, e.out.String())
	}
	rec := e.readTrace(t)
	if string(rec["status"]) != `"completed"` {
		t.Fatalf("main trace status = %s, want completed", rec["status"])
	}
}

// AC7: a judge failure leaves the agent result, main status, and exit path
// untouched.
func TestGroundingJudgeFailureLeavesTheTurnIntact(t *testing.T) {
	e := newGroundingE2E(t, groundingE2EOpts{
		grounding: true, trace: true,
		judge: &stubJudge{err: errors.New("provider unreachable")},
	})
	res := e.run(t, context.Background(), nil)

	if res.Answer != "the answer" {
		t.Fatalf("answer lost: %q", res.Answer)
	}
	out := e.out.String()
	if !strings.Contains(out, "grounding · error · judge_failed: provider unreachable") {
		t.Fatalf("diagnostic missing:\n%s", out)
	}
	rec := e.readTrace(t)
	if string(rec["status"]) != `"completed"` || string(rec["partial"]) != "false" {
		t.Fatalf("verifier failure changed the main run: status=%s partial=%s", rec["status"], rec["partial"])
	}
}

// AC7 (cancellation): Ctrl-C during the judge cancels only grounding. The answer
// is retained, the run is still a success, and the trace still says completed.
func TestGroundingCancellationDuringJudgeKeepsTheAnswer(t *testing.T) {
	blocked := make(chan struct{})
	started := make(chan struct{})
	judge := &stubJudge{block: blocked, started: started,
		rep: &analysis.SupportReport{Status: analysis.StatusSupported}}
	e := newGroundingE2E(t, groundingE2EOpts{grounding: true, trace: true, judge: judge})

	interrupts := make(chan struct{}, 1)
	done := make(chan agent.Result, 1)
	go func() {
		res, err := runOnce(context.Background(), e.out, interrupts, e.sess, "answer this", nil)
		if err != nil {
			t.Errorf("runOnce: %v", err)
		}
		done <- res
	}()
	// Wait until the judge is actually blocking, then interrupt. Signalled, not
	// polled: judge.calls is written on the run goroutine.
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("judge never ran")
	}
	interrupts <- struct{}{}

	var res agent.Result
	select {
	case res = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt did not cancel grounding promptly")
	}
	close(blocked)

	if res.Answer != "the answer" {
		t.Fatalf("cancelling grounding lost the answer: %q", res.Answer)
	}
	out := e.out.String()
	if !strings.Contains(out, "grounding · skipped · "+groundingReasonCanceled) {
		t.Fatalf("want a canceled grounding line:\n%s", out)
	}
	if strings.Contains(out, "canceled\n") && strings.Contains(out, "error:") {
		t.Fatalf("the turn itself must not report cancellation:\n%s", out)
	}
	rec := e.readTrace(t)
	if string(rec["status"]) != `"completed"` {
		t.Fatalf("main trace status = %s, want completed", rec["status"])
	}
}

// AC8: one-shot stdout carries the answer and nothing else.
func TestGroundingOneShotKeepsStdoutAnswerOnly(t *testing.T) {
	e := newGroundingE2E(t, groundingE2EOpts{grounding: true})
	var stdout strings.Builder
	if err := runOneShot(context.Background(), &stdout, e.out, nil, e.sess, "answer this"); err != nil {
		t.Fatalf("runOneShot: %v", err)
	}
	if stdout.String() != "the answer\n" {
		t.Fatalf("stdout = %q, want the answer plus one newline", stdout.String())
	}
	if !strings.Contains(e.out.String(), "grounding · supported") {
		t.Fatalf("grounding line missing from stderr:\n%s", e.out.String())
	}
}

// AC9: verifier usage is reported only in the grounding payload. It must not
// reach the agent result, the footer, or telemetry, and the verifier's wall
// time must stay out of the recorded agent-run duration.
func TestGroundingUsageAndLatencyStayOutOfTheAgentRun(t *testing.T) {
	e := newGroundingE2E(t, groundingE2EOpts{
		grounding: true, trace: true, telemetry: true,
		judgeDelay: 5 * time.Second,
	})
	res := e.run(t, context.Background(), nil)

	if res.Usage.TotalTokens != 0 {
		t.Fatalf("verifier tokens leaked into res.Usage: %+v", res.Usage)
	}
	rec := e.readTrace(t)
	var stored groundingReport
	if err := json.Unmarshal(rec["grounding"], &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Tokens != 42 {
		t.Fatalf("grounding.tokens = %d, want 42", stored.Tokens)
	}
	if stored.DurationMS != (5 * time.Second).Milliseconds() {
		t.Fatalf("grounding.duration_ms = %d, want the verifier's own elapsed time", stored.DurationMS)
	}

	raw, err := os.ReadFile(e.telePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "grounding") {
		t.Fatalf("telemetry must carry no grounding content:\n%s", raw)
	}
	sawRunSpan := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var span struct {
			Kind       string `json:"kind"`
			DurationMS int64  `json:"duration_ms"`
			Usage      struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(line), &span); err != nil {
			continue
		}
		if span.Kind != "run" {
			continue
		}
		sawRunSpan = true
		if span.DurationMS != 0 {
			t.Fatalf("the root run span absorbed grounding latency: %d ms", span.DurationMS)
		}
		if span.Usage.TotalTokens != 0 {
			t.Fatalf("the root run span absorbed verifier tokens: %+v", span.Usage)
		}
	}
	if !sawRunSpan {
		t.Fatalf("no root run span in telemetry; the assertions above checked nothing:\n%s", raw)
	}
	// The end time is the PRE-grounding tick. The judge advanced the shared
	// clock by 5s twice, so a timestamp taken after grounding would read 10s.
	if got, want := string(rec["ended_at"]), `"1970-01-01T00:00:00Z"`; got != want {
		t.Fatalf("ended_at = %s, want the pre-grounding snapshot %s", got, want)
	}
	if got := e.out.String(); !strings.Contains(got, "done · 2 steps · 0.0s · 0 tok") {
		t.Fatalf("run footer absorbed grounding latency:\n%s", got)
	}
}

// Guard the trace tier's own contract from the golem side: agenttrace stores the
// payload verbatim, so a stringified encoding would silently ship to #352.
func TestGroundingTracePayloadIsAnObject(t *testing.T) {
	e := newGroundingE2E(t, groundingE2EOpts{grounding: true, trace: true})
	e.run(t, context.Background(), nil)
	raw := e.readTrace(t)["grounding"]
	if len(raw) == 0 || raw[0] != '{' {
		t.Fatalf("grounding must be embedded as an object, got %s", raw)
	}
	var probe agenttrace.TraceRecord
	if err := json.Unmarshal([]byte(`{"grounding":`+string(raw)+`}`), &probe); err != nil {
		t.Fatalf("stored payload does not round-trip through TraceRecord: %v", err)
	}
}

// The recorder is turn-scoped. Its budget bounds ONE turn, so a session that
// never reset it would accumulate across turns until an ordinary turn tripped
// the cap and reported evidence_incomplete forever after — a slow, silent
// degradation with no error anywhere.
func TestGroundingRecorderIsResetEachTurn(t *testing.T) {
	results := threeResults()
	budget := 0
	for _, r := range results {
		budget += len(r.Chunk.Content) + len(r.Chunk.ID) + len(r.Chunk.Source) +
			len(r.Chunk.StableKey) + groundingEvidenceEntryOverhead
	}
	// Exactly one turn's worth of evidence fits.
	e := newGroundingE2E(t, groundingE2EOpts{grounding: true, recorderBytes: budget})

	for turn := range 2 {
		e.out.Reset()
		// A fresh corpus each turn: re-recording identical keys is free, so only
		// new evidence can expose a recorder that never forgets.
		e.retr.results = threeResultsFrom(turn * 3)
		e.retr.renderCount = len(e.retr.results)
		e.sess.runtime = newTestRuntime(t, t.TempDir(), e.sess.baseSystem,
			agent.New(&scriptCaller{responses: []agent.ModelResult{
				retrieveStep("t1", "q"), answerStep("the answer"),
			}}, agent.ContextManager{}), e.sess.tools)
		e.run(t, context.Background(), nil)
		if !strings.Contains(e.out.String(), "grounding · supported") {
			t.Fatalf("turn %d did not verify:\n%s", turn+1, e.out.String())
		}
	}
	if e.judge.calls != 2 {
		t.Fatalf("judge calls = %d, want one per turn", e.judge.calls)
	}
}

// A run that never completed must not be judged. The empty-answer guard covers
// an outright provider failure; this covers the harder case, an interrupt that
// lands after the model produced an answer. The run then carries a real answer
// AND a cancellation, so only the completed-status gate keeps the verifier off
// a turn the user abandoned.
func TestGroundingSkippedWhenTheRunDidNotComplete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		build   func(t *testing.T, e *groundingE2E) *groundingE2E
		wantErr bool
	}{
		{name: "provider failure leaves no answer", wantErr: true,
			build: func(t *testing.T, _ *groundingE2E) *groundingE2E {
				return newGroundingE2E(t, groundingE2EOpts{grounding: true, trace: true,
					callerErr: errors.New("provider exploded")})
			}},
		{name: "cancellation after the answer", wantErr: true,
			build: func(t *testing.T, _ *groundingE2E) *groundingE2E {
				return newGroundingE2E(t, groundingE2EOpts{grounding: true, trace: true,
					cancelOnAnswer: true})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.build(t, nil)
			ctx := e.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			_, err := runOnce(ctx, e.out, nil, e.sess, "answer this", nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("runOnce err = %v, wantErr %v", err, tc.wantErr)
			}
			if e.judge.calls != 0 {
				t.Fatalf("an incomplete run must make no judge call, made %d", e.judge.calls)
			}
			if strings.Contains(e.out.String(), "grounding") {
				t.Fatalf("an incomplete run printed a grounding line:\n%s", e.out.String())
			}
		})
	}
}

// The verdict is a status line about the turn, not part of the answer, so it
// gets the same dim treatment as the run footer. With color off the two
// renderings are byte-identical, so only a color-enabled run can see this.
func TestGroundingSummaryLineIsDimmedLikeTheFooter(t *testing.T) {
	e := newGroundingE2E(t, groundingE2EOpts{grounding: true, color: true})
	e.run(t, context.Background(), nil)

	out := e.out.String()
	if !strings.Contains(out, "\x1b[2mgrounding · supported") {
		t.Fatalf("grounding line is not dim:\n%q", out)
	}
	if !strings.Contains(out, "\x1b[2mdone · ") {
		t.Fatalf("fixture did not produce a dim footer to compare against:\n%q", out)
	}
}
