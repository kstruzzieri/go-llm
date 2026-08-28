package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/analysis"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
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
	// ponytail: one refusal invalidates this turn; use bounded per-key
	// tombstones only if cap-driven false skips become measurable.
	capped bool
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
	r.capped = false
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
			r.capped = true
			continue
		}
		r.entries[key] = next
		r.bytes += charge
	}
}

func (r *evidenceRecorder) evidenceComplete() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.capped
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
	successfulRetrieve := e.Call.Function.Name == "retrieve" && e.Invoked && !e.Denied &&
		!e.Result.IsError && e.Result.Attrib != nil
	if !successfulRetrieve {
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

// ---------------------------------------------------------------------------
// Frozen report, rendering, and the verification service.
// ---------------------------------------------------------------------------

// Grounding statuses. The first three mirror analysis.SupportStatus; the last
// two are outcomes a verdict cannot express and a machine consumer must still
// be able to distinguish. Frozen for #352, which serializes these verbatim.
const (
	groundingSupported   = "supported"
	groundingPartial     = "partial"
	groundingUnsupported = "unsupported"
	groundingSkipped     = "skipped"
	groundingError       = "error"
)

// Stable machine reason codes. Raw provider and router error text never enters
// the payload: it is unbounded, provider-specific prose that would freeze into
// a contract #352 has to keep honoring. Diagnostics travel beside the report
// and end up on stderr instead.
const (
	groundingReasonNoFinalEvidence    = "no_final_evidence"
	groundingReasonEvidenceIncomplete = "evidence_incomplete"
	groundingReasonCanceled           = "canceled"
	groundingReasonTimeout            = "timeout"
	groundingReasonJudgeFailed        = "judge_failed"
	groundingReasonEvidenceTruncated  = "evidence_truncated"
)

const (
	// groundingTimeout bounds the whole two-stage judge.
	groundingTimeout = 60 * time.Second
	// groundingDiagnosticMaxBytes bounds the terminal diagnostic appended to a
	// non-verdict line. Provider errors can be arbitrarily long.
	groundingDiagnosticMaxBytes = 160
)

// groundingReport is the frozen result shape: rendered as one line today and
// serialized verbatim by #352. It carries no payload version because the
// containing trace and event protocols are already versioned, and an exact
// golden test pins these bytes.
type groundingReport struct {
	Status     string                  `json:"status"`
	Reason     string                  `json:"reason,omitempty"`
	Tokens     int                     `json:"tokens,omitempty"`
	DurationMS int64                   `json:"duration_ms,omitempty"`
	Report     *analysis.SupportReport `json:"report,omitempty"`
}

// groundingJudge is the analysis seam. maxEvidenceChars is an explicit
// parameter rather than an analysis.SupportOption because a SupportOption
// closes over an unexported config that no test outside analysis can read
// back, so passing the wrong budget would be invisible.
type groundingJudge interface {
	Judge(ctx context.Context, answer string, evidence []rag.SearchResult, maxEvidenceChars int) (*analysis.SupportReport, error)
}

// supportJudgeAdapter is the one-line translation from the explicit parameter
// to the analysis option.
type supportJudgeAdapter struct{ judge *analysis.SupportJudge }

func (a supportJudgeAdapter) Judge(ctx context.Context, answer string, evidence []rag.SearchResult, maxEvidenceChars int) (*analysis.SupportReport, error) {
	return a.judge.Judge(ctx, answer, evidence, analysis.WithMaxEvidenceChars(maxEvidenceChars))
}

// groundingRouteFunc routes and executes one verifier chat. It is the seam the
// routing tests drive; production supplies router.Route + RoutePlan.ExecuteChat.
type groundingRouteFunc func(ctx context.Context, rr provider.RoutingRequest) (*provider.ChatResponse, error)

// newGroundingChat builds the analysis.ChatFunc the judge's two stages call,
// plus a reader for their combined token usage. Each stage keeps its own use
// case and chain: collapsing them would route claim extraction through the
// verify chain. StrictChain suppresses the router's safety-net tail, and
// background priority keeps a verifier from displacing the primary agent model
// under slot admission.
func newGroundingChat(route groundingRouteFunc, chains map[string][]string) (analysis.ChatFunc, func() int) {
	var tokens int
	chat := func(ctx context.Context, useCase string, req provider.ChatRequest) (*provider.ChatResponse, error) {
		resp, err := route(ctx, provider.RoutingRequest{
			UseCase:        useCase,
			RequiredCaps:   provider.CapChat,
			Messages:       req.Messages,
			Options:        req.Options,
			ExpectedOutput: provider.DefaultExpectedOutput(useCase),
			Priority:       provider.PriorityBackground,
			PreferredChain: chains[useCase],
			StrictChain:    true,
		})
		if resp != nil {
			tokens += resp.Usage.TotalTokens
		}
		return resp, err
	}
	return chat, func() int { return tokens }
}

// groundingService runs claim-support verification over one finished turn.
// newJudge builds a judge and a usage reader per call, so token accounting
// never leaks across turns.
type groundingService struct {
	rec      *evidenceRecorder
	timeout  time.Duration
	now      func() time.Time
	newJudge func() (groundingJudge, func() int, error)
}

// verify returns the report, a terminal-only diagnostic, and whether anything
// should be shown. show=false is the silent case: the turn ran no successful
// retrieve, or produced no answer to judge.
//
// Every path that cannot reconstruct the model's exact evidence returns without
// calling the judge. A verdict over a silently reduced evidence subset would
// report "unsupported" for claims the model actually had support for.
func (s *groundingService) verify(ctx context.Context, answer string, c *groundingCollector) (groundingReport, string, bool) {
	if s == nil || c == nil {
		return groundingReport{}, "", false
	}
	if strings.TrimSpace(answer) == "" || !c.sawRetrieveCall() {
		return groundingReport{}, "", false
	}
	sources := c.finalSources()
	if len(sources) == 0 {
		// Checked before completeness: with nothing in the answer's prompt there
		// is no attribution to be incomplete about.
		return groundingReport{Status: groundingSkipped, Reason: groundingReasonNoFinalEvidence}, "", true
	}
	if !c.evidenceComplete() || !s.rec.evidenceComplete() {
		return groundingReport{Status: groundingSkipped, Reason: groundingReasonEvidenceIncomplete}, "", true
	}
	evidence := make([]rag.SearchResult, 0, len(sources))
	for _, src := range sources {
		chunk, ok := s.rec.lookup(src)
		if !ok {
			return groundingReport{Status: groundingSkipped, Reason: groundingReasonEvidenceIncomplete}, "", true
		}
		evidence = append(evidence, rag.SearchResult{Chunk: chunk, Score: src.Score})
	}

	judge, usage, err := s.newJudge()
	if err != nil {
		return groundingReport{Status: groundingError, Reason: groundingReasonJudgeFailed}, err.Error(), true
	}
	jctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	started := s.now()
	// The judge's evidence budget equals the recorder's retained-body budget, so
	// analysis never silently drops the tail; any shortfall is treated as a hard
	// failure below rather than published as a subset verdict.
	report, judgeErr := judge.Judge(jctx, answer, evidence, groundingEvidenceMaxBytes)
	rep := groundingReport{
		Tokens:     usage(),
		DurationMS: s.now().Sub(started).Milliseconds(),
	}
	switch {
	case judgeErr != nil && ctx.Err() != nil:
		// The caller's context died: an interrupt, not a verifier fault.
		rep.Status, rep.Reason = groundingSkipped, groundingReasonCanceled
	case errors.Is(judgeErr, context.DeadlineExceeded):
		rep.Status, rep.Reason = groundingError, groundingReasonTimeout
	case judgeErr != nil:
		rep.Status, rep.Reason = groundingError, groundingReasonJudgeFailed
		return rep, judgeErr.Error(), true
	case report == nil:
		rep.Status, rep.Reason = groundingError, groundingReasonJudgeFailed
	case len(report.Evidence) < len(evidence):
		// analysis returns refs only for blocks it actually included, so a
		// shortfall means the verdict covers less than the model read.
		rep.Status, rep.Reason = groundingError, groundingReasonEvidenceTruncated
	default:
		rep.Status, rep.Report = string(report.Status), report
	}
	return rep, "", true
}

// groundingSummaryLine renders the single line the CLI prints. diagnostic is
// terminal-only detail for a non-verdict outcome and is ignored for a verdict.
func groundingSummaryLine(rep groundingReport, diagnostic string) string {
	if rep.Report == nil {
		line := fmt.Sprintf("grounding · %s · %s", rep.Status, rep.Reason)
		if d := boundGroundingText(diagnostic, groundingDiagnosticMaxBytes); d != "" {
			line += ": " + d
		}
		return line
	}
	supported := 0
	for _, claim := range rep.Report.Claims {
		if claim.Status == analysis.StatusSupported {
			supported++
		}
	}
	line := fmt.Sprintf("grounding · %s · %d/%d claims · %d evidence · %.1fs",
		rep.Status, supported, len(rep.Report.Claims), len(rep.Report.Evidence),
		float64(rep.DurationMS)/1000)
	if rep.Tokens > 0 {
		line += fmt.Sprintf(" · %d tok", rep.Tokens)
	}
	return line
}

// boundGroundingText flattens text to one line and truncates it on a rune
// boundary so a multi-byte rune straddling the cap cannot produce invalid UTF-8.
// Every space-like and control rune collapses to an ASCII space: the exotic
// separators (U+0085, U+2028, U+2029) break the one-line guarantee just as
// surely as a newline, and unicode.IsSpace already covers them.
func boundGroundingText(text string, maxBytes int) string {
	flat := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text))
	if len(flat) <= maxBytes {
		return flat
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(flat[end]) {
		end--
	}
	return strings.TrimSpace(flat[:end])
}

// groundingNoChainWarning is the startup notice when the verifier's side-task
// use-cases resolve to nothing. Same shape as the -progressive summary notice:
// the flag is a no-op, not a fatal misconfiguration.
const groundingNoChainWarning = "-grounding had no effect: no verify/extract, analysis, " +
	"or chat default is configured, so no verifier model could be selected"

// newGroundingService builds the production service. Returns (nil, warning)
// when either stage's chain cannot resolve, so the caller can disable the
// feature with one notice instead of failing every turn.
func newGroundingService(cfg *config.Config, router *provider.Router, rec *evidenceRecorder) (*groundingService, string) {
	if cfg == nil || router == nil || rec == nil {
		return nil, groundingNoChainWarning
	}
	chains := make(map[string][]string, 2)
	for _, useCase := range []string{config.UseCaseExtract, config.UseCaseVerify} {
		chain, err := cfg.RoleFallbackChain(useCase)
		if err != nil || len(chain) == 0 {
			return nil, groundingNoChainWarning
		}
		chains[useCase] = chain
	}
	route := func(ctx context.Context, rr provider.RoutingRequest) (*provider.ChatResponse, error) {
		plan, err := router.Route(ctx, rr)
		if err != nil {
			return nil, err
		}
		return plan.ExecuteChat(ctx)
	}
	return &groundingService{
		rec:     rec,
		timeout: groundingTimeout,
		now:     time.Now,
		newJudge: func() (groundingJudge, func() int, error) {
			chat, tokens := newGroundingChat(route, chains)
			judge, err := analysis.NewSupportJudgeWithChat(chat, "")
			if err != nil {
				return nil, nil, err
			}
			// Model pin left empty on purpose: each stage routes by its own
			// use-case chain, so the two may land on different models.
			return supportJudgeAdapter{judge: judge}, tokens, nil
		},
	}, ""
}

// groundingNoRetrieveWarning is the startup notice when -grounding is on but no
// retrieve tool is mounted. A warming auto-index wrapper still counts as
// available: its later generation is built through the same recorder.
const groundingNoRetrieveWarning = "-grounding had no effect: the retrieve tool is not available, " +
	"so no turn can produce retrieval evidence to verify"

// groundingStartupWarning returns the single startup notice for -grounding, or
// "" when the feature is off or usable. Retrieval unavailability wins over an
// unresolved verifier chain: two lines about one disabled flag read like two
// separate problems, and retrieval is the one the user acts on first.
func groundingStartupWarning(enabled, retrievalAvailable bool, chainWarn string) string {
	switch {
	case !enabled:
		return ""
	case !retrievalAvailable:
		return groundingNoRetrieveWarning
	default:
		return chainWarn
	}
}

// beginTurn clears the previous turn's captured evidence. Nil-safe so the
// caller needs no second check beyond the one that decides to observe at all.
func (s *groundingService) beginTurn() {
	if s == nil {
		return
	}
	s.rec.beginTurn()
}
