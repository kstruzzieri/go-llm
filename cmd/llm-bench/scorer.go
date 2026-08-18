package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

const (
	fallbackJudgeModel       = "gemma4:31b"
	judgeTemperature         = 0.1
	judgeTokenBudget         = 512
	maxJudgeTurnContentBytes = 8192
	maxJudgeAnswerBytes      = 32768
)

// openAICompatTransport is the -judge-transport value that routes the judge
// through provider/openaicompat instead of the default Ollama client.
const openAICompatTransport = "openai-compat"

// claudeCLITransport is the -judge-transport value that routes the judge
// through the local `claude` CLI headless mode (subscription, no API billing).
const claudeCLITransport = "claude-cli"

// Score captures the evaluation dimensions for a single (model, trace) run.
// See docs/llm/benchmark-plan.md for the scoring rationale.
type Score struct {
	ToolSequenceMatch     float64 // [0,1] — how close actual tool calls were to the golden sequence
	ToolArgsValid         float64 // [0,1] — fraction of tool calls with valid arguments
	ToolArgsValidComputed bool    // when false, ToolArgsValid is a placeholder (0.0) — see validateToolArguments
	AnswerQuality         float64 // [0,1] — final-answer quality per the active Scorer
	LatencyMs             int64   // sum of all chat round-trips for this replay
	TurnLatenciesMs       []int64 // per-turn breakdown; len == number of chat round-trips
	ScorerLatencyMs       int64   // wall-clock time spent in the active scorer
	TotalTokens           int     // prompt-eval + generation tokens (kept for back-compat)
	PromptEvalTokens      int     // tokens in the prompt (Ollama prompt_eval_count / OpenAI usage.prompt_tokens)
	GenTokens             int     // tokens generated (Ollama eval_count / OpenAI usage.completion_tokens)
	ThinkingTokens        int     // reasoning tokens when the provider isolates them; see ThinkingTokensComputed
	// ThinkingTokensComputed is false when the provider does not separately
	// expose thinking tokens (e.g. Ollama folds them into GenTokens). It mirrors
	// the ToolArgsValidComputed idiom so the report can distinguish "no thinking"
	// from "not reported".
	ThinkingTokensComputed bool
	Notes                  string
	// Restraint is the tool-restraint signal: 1.0 = held (the golden expected no
	// tool call and the candidate emitted none), 0.0 = diverged (emitted ≥1).
	// RestraintComputed is false when restraint was not testable (the golden
	// expected a tool route); then Restraint is a placeholder 0.0 that
	// aggregation MUST skip — mirrors the ToolArgsValid/ToolArgsValidComputed
	// idiom. Divergence is recoverable as RestraintComputed && Restraint == 0.
	Restraint         float64
	RestraintComputed bool
	// ToolExposedRestraint is the same signal restricted to traces that actually
	// offered tools (len(trace.Tools) > 0) — the stricter "restraint under
	// temptation" denominator. ToolExposedRestraintComputed implies
	// RestraintComputed. It is a companion diagnostic, not a replacement.
	ToolExposedRestraint         float64
	ToolExposedRestraintComputed bool
}

// baseMechanicalScore computes the scorer-independent mechanical fields shared by
// every scorer: tool-sequence match, tool-argument validity, and the restraint
// signals (primary + tool-exposed companion). It returns the raw schema-compile
// error unwrapped so each caller can attach its own "trace %q: compile tool
// schemas" context, preserving existing error messages. AnswerQuality is left to
// the caller.
func baseMechanicalScore(trace Trace, transcript []Turn) (Score, error) {
	toolArgsScore, toolArgsComputed, toolArgsNotes, schemaErr := scoreToolArguments(trace, transcript)
	if schemaErr != nil {
		return Score{}, schemaErr
	}
	restraint, restraintComputed, toolExposedRestraint, toolExposedComputed := restraintScoreFields(trace, transcript)
	return Score{
		ToolSequenceMatch:            toolSequenceScore(trace.Golden.ToolCalls, extractToolNames(transcript)),
		ToolArgsValid:                toolArgsScore,
		ToolArgsValidComputed:        toolArgsComputed,
		Restraint:                    restraint,
		RestraintComputed:            restraintComputed,
		ToolExposedRestraint:         toolExposedRestraint,
		ToolExposedRestraintComputed: toolExposedComputed,
		Notes:                        toolArgsNotes,
	}, nil
}

// Scorer is the pluggable strategy for evaluating a replay result.
//
// The AnswerQuality dimension is load-bearing: how it's computed
// determines whether the harness is trustworthy enough to override public
// benchmark narratives. The three shipped strategies trade cost, quality
// of judgment, and reproducibility differently — see benchmark-plan.md.
type Scorer interface {
	Score(ctx context.Context, trace Trace, actual Result) (Score, error)
}

type scorerOptions struct {
	ollamaURL    string
	judgeModel   string
	judgeTimeout time.Duration
	// judgeTransport selects the judge backend: "" or "ollama" (default) uses
	// the local Ollama client; "openai-compat" routes through
	// provider/openaicompat for a frontier judge. judgeBaseURL is required for
	// the openai-compat transport; judgeAPIKey is the optional Bearer token.
	judgeTransport string
	judgeBaseURL   string
	judgeAPIKey    string
	// judgeDisableThinking opts the openai-compat judge transport into
	// translating the judge's Think=false directive onto the wire as
	// chat_template_kwargs.enable_thinking=false (a llama.cpp/vLLM template
	// extension). Off by default: api.openai.com rejects unrecognized
	// top-level arguments with HTTP 400. Ignored by other transports (the
	// Ollama client always forwards think natively).
	judgeDisableThinking bool
	// manualLabelsPath is the human labels JSONL consumed by the "manual"
	// scorer (AnswerQuality = expected_answer_quality).
	manualLabelsPath string
	// judgeCache is the optional judge response cache. Callers MUST avoid
	// the typed-nil interface trap: assign only a non-nil concrete
	// implementation (e.g. *sqliteJudgeCache) or leave this field unset.
	// openJudgeCache("") returns a nil *sqliteJudgeCache by design so the
	// caller can decide whether to wrap it in this interface field.
	judgeCache  judgeCacheStore
	bypassCache bool // when true, the scorer skips both cache Get and Put
}

// newScorer returns the Scorer matching the given name.
func newScorer(ctx context.Context, name string, opts scorerOptions) (Scorer, error) {
	switch name {
	case "exact-match":
		return &ExactMatchScorer{}, nil
	case "llm-judge":
		if opts.judgeTimeout < 0 {
			return nil, fmt.Errorf("negative judge timeout %s", opts.judgeTimeout)
		}
		transport, err := newJudgeTransport(opts)
		if err != nil {
			return nil, err
		}
		scorer, err := newLLMJudgeScorer(transport.chat, opts.judgeModel, opts.judgeTimeout)
		if err != nil {
			return nil, err
		}
		scorer.JudgeProvider = transport.providerName
		scorer.ThinkEnforced = transport.thinkEnforced
		if err := validateJudgeModel(ctx, transport.checker, scorer.JudgeModel); err != nil {
			return nil, err
		}
		digest, _ := resolveJudgeDigest(ctx, transport.checker, scorer.JudgeModel)
		scorer.JudgeModelDigest = digest
		scorer.Cache = opts.judgeCache
		scorer.BypassCache = opts.bypassCache
		return scorer, nil
	case "manual":
		if strings.TrimSpace(opts.manualLabelsPath) == "" {
			return nil, fmt.Errorf("manual scorer requires -labels (human labels JSONL)")
		}
		labels, err := loadLabels(opts.manualLabelsPath)
		if err != nil {
			return nil, fmt.Errorf("manual scorer: load labels: %w", err)
		}
		if len(labels) == 0 {
			return nil, fmt.Errorf("manual scorer: no labels in %q", opts.manualLabelsPath)
		}
		return newManualScorer(labels)
	default:
		return nil, fmt.Errorf("unknown scorer %q", name)
	}
}

// judgeTransport bundles the judge's chat client, model checker, and provider
// instance identity so newScorer builds the scorer uniformly regardless of
// backend. The same concrete value satisfies both the chat and checker seams
// for each transport (*ollama.Client; *openAICompatJudgeClient).
type judgeTransport struct {
	chat         judgeChatClient
	checker      judgeModelChecker
	providerName string
	// thinkEnforced records whether this transport actually delivers the
	// judge's Think=false directive to the backend: true for the Ollama
	// client (native think field) and for openai-compat with
	// judgeDisableThinking set; false for openai-compat without the opt-in
	// and for the Claude CLI transport. Folded into the cache key so
	// verdicts judged under different think regimes can never alias.
	thinkEnforced bool
}

// newJudgeTransport resolves opts.judgeTransport to a judge backend. Empty or
// "ollama" (case-insensitive) is the default local path; "openai-compat"
// routes a frontier judge through provider/openaicompat. The returned
// providerName is the provider *instance* identity that gets folded into the
// cache key and provenance. For openai-compat it is endpoint-scoped so two
// base URLs that expose the same model id cannot reuse each other's digest-less
// cached verdicts.
func newJudgeTransport(opts scorerOptions) (judgeTransport, error) {
	switch normalizeModelSelector(opts.judgeTransport) {
	case "", defaultBenchProvider:
		client, err := newOllamaClient(opts.ollamaURL, ollama.WithTimeout(opts.judgeTimeout))
		if err != nil {
			return judgeTransport{}, fmt.Errorf("llm-judge client: %w", err)
		}
		return judgeTransport{chat: client, checker: client, providerName: defaultBenchProvider, thinkEnforced: true}, nil
	case openAICompatTransport:
		baseURL := strings.TrimSpace(opts.judgeBaseURL)
		if baseURL == "" {
			return judgeTransport{}, fmt.Errorf("llm-judge %s transport requires -judge-base-url", openAICompatTransport)
		}
		clientOpts := []openaicompat.ClientOption{}
		if opts.judgeTimeout > 0 {
			clientOpts = append(clientOpts, openaicompat.WithHTTPClient(&http.Client{Timeout: opts.judgeTimeout}))
		}
		if key := strings.TrimSpace(opts.judgeAPIKey); key != "" {
			clientOpts = append(clientOpts, openaicompat.WithAPIKey(key))
		}
		providerName := openAICompatJudgeProviderName(baseURL)
		prov := openaicompat.NewProvider(
			openaicompat.NewClient(baseURL, clientOpts...),
			openaicompat.WithProviderName(providerName),
		)
		adapter := newOpenAICompatJudge(prov)
		adapter.disableThinking = opts.judgeDisableThinking
		return judgeTransport{chat: adapter, checker: adapter, providerName: prov.Name(), thinkEnforced: opts.judgeDisableThinking}, nil
	case claudeCLITransport:
		adapter := newClaudeCLIJudge(opts.judgeModel)
		return judgeTransport{chat: adapter, checker: adapter, providerName: claudeCLIProviderName}, nil
	default:
		return judgeTransport{}, fmt.Errorf("unknown judge transport %q (want %q, %q, or %q)", opts.judgeTransport, defaultBenchProvider, openAICompatTransport, claudeCLITransport)
	}
}

func openAICompatJudgeProviderName(baseURL string) string {
	return openAICompatEndpointProviderName(baseURL)
}

func openAICompatCandidateProviderName(baseURL string) string {
	return openAICompatEndpointProviderName(baseURL)
}

func openAICompatEndpointProviderName(baseURL string) string {
	canonical := canonicalOpenAICompatBaseURL(baseURL)
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%s:%s", openAICompatTransport, hex.EncodeToString(sum[:])[:12])
}

func canonicalOpenAICompatBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawPath = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

// ExactMatchScorer is a dependency-free baseline. Answer quality is scored
// as 1.0 iff the golden substring appears in the assistant's response,
// 0.0 otherwise. Use it to bootstrap the harness; graduate to `llm-judge`
// before drawing conclusions.
type ExactMatchScorer struct{}

// Score implements Scorer. ToolArgsValid is populated by validating each
// candidate tool call against the JSON Schema declared in trace.Tools;
// ToolArgsValidComputed records whether the score is meaningful (see
// validateToolArguments for the truth table).
func (s *ExactMatchScorer) Score(_ context.Context, trace Trace, actual Result) (Score, error) {
	needle := strings.TrimSpace(trace.Golden.FinalAnswerSubstring)
	if needle == "" {
		return Score{}, fmt.Errorf("trace %q: %w", trace.ID, errMissingGolden)
	}

	score, schemaErr := baseMechanicalScore(trace, actual.Transcript)
	if schemaErr != nil {
		return Score{}, fmt.Errorf("trace %q: compile tool schemas: %w", trace.ID, schemaErr)
	}

	finalText := lastAssistantContent(actual.Transcript)
	if strings.Contains(finalText, needle) {
		score.AnswerQuality = 1.0
	} else {
		score.AnswerQuality = 0.0
	}

	return score, nil
}

// CaptureScorer lets capture-only flows replay candidates through Runner
// without making scoring prerequisites part of artifact collection.
type CaptureScorer struct{}

func (s *CaptureScorer) Score(_ context.Context, trace Trace, actual Result) (Score, error) {
	restraint, restraintComputed, toolExposedRestraint, toolExposedComputed := restraintScoreFields(trace, actual.Transcript)
	return Score{
		ToolSequenceMatch:            toolSequenceScore(trace.Golden.ToolCalls, extractToolNames(actual.Transcript)),
		Restraint:                    restraint,
		RestraintComputed:            restraintComputed,
		ToolExposedRestraint:         toolExposedRestraint,
		ToolExposedRestraintComputed: toolExposedComputed,
		Notes:                        "capture mode: scoring skipped",
	}, nil
}

type judgeChatClient interface {
	Chat(ctx context.Context, req ollama.ChatRequest) (*ollama.ChatResponse, error)
}

type judgeModelChecker interface {
	AvailableModels(ctx context.Context) ([]string, error)
	ShowModel(ctx context.Context, name string) (*ollama.ModelInfo, error)
}

// resolveJudgeDigest returns the judge model's content digest, or empty
// string if the provider's /api/show response is unavailable or omits one.
// Errors from /api/show are deliberately swallowed: a missing digest is
// degraded R1 mitigation, not a hard failure.
func resolveJudgeDigest(ctx context.Context, checker judgeModelChecker, judgeModel string) (string, error) {
	info, err := checker.ShowModel(ctx, modelSelectorWithoutBenchProvider(judgeModel))
	if err != nil {
		return "", nil
	}
	if info == nil {
		return "", nil
	}
	return info.Digest, nil
}

// LLMJudgeScorer asks a separate local Ollama model to score final-answer
// quality against the trace's golden rubric. Replay latency and target-model
// tokens remain owned by Runner.runOne.
//
// Cache is the optional judge response cache. It is typed as the
// judgeCacheStore interface so callers MUST assign only non-nil concrete
// values; assigning a typed-nil pointer (e.g. the (*sqliteJudgeCache)(nil)
// returned by openJudgeCache("")) produces a non-nil interface that will
// panic on use. Wiring code is responsible for guarding against the
// typed-nil interface trap before assignment.
type LLMJudgeScorer struct {
	Client           judgeChatClient
	JudgeProvider    string // provider instance identity (e.g. "ollama", "openai-compat:<endpoint-id>"); folded into the cache key and persisted for provenance
	JudgeModel       string
	JudgeModelDigest string // optional; empty when /api/show was unavailable
	// ThinkEnforced records whether the transport delivers the judge's
	// Think=false directive to the backend (see judgeTransport.thinkEnforced).
	// Part of the cache key: a verdict judged by a freely-thinking judge and
	// one judged with thinking disabled are different measurements.
	ThinkEnforced bool
	JudgeTimeout  time.Duration
	Cache         judgeCacheStore  // optional; nil disables caching
	BypassCache   bool             // when true, Score skips both Get and Put
	Clock         func() time.Time // injectable; defaults to time.Now().UTC()
}

// now returns the scorer's notion of the current time. Injected via Clock
// in tests so cache CreatedAt/LastUsedAt timestamps are deterministic.
func (s *LLMJudgeScorer) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now().UTC()
}

func newLLMJudgeScorer(client judgeChatClient, judgeModel string, judgeTimeout time.Duration) (*LLMJudgeScorer, error) {
	if client == nil {
		return nil, fmt.Errorf("nil judge client")
	}
	judgeModel = strings.TrimSpace(judgeModel)
	if judgeModel == "" {
		return nil, errEmptyJudgeModel
	}
	if judgeTimeout < 0 {
		return nil, fmt.Errorf("negative judge timeout %s", judgeTimeout)
	}
	return &LLMJudgeScorer{
		Client:       client,
		JudgeModel:   judgeModel,
		JudgeTimeout: judgeTimeout,
	}, nil
}

func validateJudgeModel(ctx context.Context, checker judgeModelChecker, judgeModel string) error {
	if checker == nil {
		return fmt.Errorf("llm-judge model validation requires a model checker")
	}
	models, err := checker.AvailableModels(ctx)
	if err != nil {
		return fmt.Errorf("validate judge model %q: %w", judgeModel, err)
	}
	for _, model := range models {
		if sameModelSelector(judgeModel, model) {
			return nil
		}
	}
	return fmt.Errorf("judge model %q is not available from the configured judge transport; pass a -judge-model the active -judge-transport exposes (or fix -judge-transport)", judgeModel)
}

// buildJudgeCall produces the judge ChatRequest and the partial Score that
// will be merged with the judge's verdict. Pure over (trace, actual): no I/O
// and no mutation. The cache key is derived from req; baseScore carries the
// freshly-computed ToolSequenceMatch/Notes that must be recomputed each run
// even on a cache hit so tool-loop changes are not masked by stale cache.
func (s *LLMJudgeScorer) buildJudgeCall(trace Trace, actual Result) (ollama.ChatRequest, Score, error) {
	if sameModelSelector(s.JudgeModel, actual.Model) {
		return ollama.ChatRequest{}, Score{}, fmt.Errorf("trace %q model %q: %w", trace.ID, actual.Model, errJudgeSelfPreference)
	}

	criteria := strings.TrimSpace(trace.Golden.FinalAnswerCriteria)
	substring := strings.TrimSpace(trace.Golden.FinalAnswerSubstring)
	if criteria == "" && substring == "" {
		return ollama.ChatRequest{}, Score{}, fmt.Errorf("trace %q: %w", trace.ID, errMissingJudgeCriteria)
	}

	baseScore, schemaErr := baseMechanicalScore(trace, actual.Transcript)
	if schemaErr != nil {
		return ollama.ChatRequest{}, Score{}, fmt.Errorf("trace %q: compile tool schemas: %w", trace.ID, schemaErr)
	}

	if strings.TrimSpace(lastAssistantContent(actual.Transcript)) == "" {
		return ollama.ChatRequest{}, Score{}, fmt.Errorf("trace %q: %w", trace.ID, errNoAssistantFinalAnswer)
	}

	prompt, err := buildJudgePrompt(trace, actual)
	if err != nil {
		return ollama.ChatRequest{}, Score{}, fmt.Errorf("trace %q: build judge prompt: %w", trace.ID, err)
	}

	think := false
	req := ollama.ChatRequest{
		Model: modelSelectorWithoutBenchProvider(s.JudgeModel),
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: judgeSystemPrompt},
			{Role: "user", Content: prompt},
		},
		Format: "json",
		Think:  &think,
		Options: &ollama.ModelOptions{
			Temperature: provider.Ptr(judgeTemperature),
			NumPredict:  judgeTokenBudget,
		},
		KeepAlive: benchKeepAlive,
	}
	return req, baseScore, nil
}

// callJudge issues the judge ChatRequest and returns the raw response
// content. The only I/O step; honors JudgeTimeout. Errors are wrapped
// with the judge model identity so callers (Score and the future cache
// wrapper) MUST NOT re-attribute model identity in their own wraps —
// doing so produces double-prefixed messages.
func (s *LLMJudgeScorer) callJudge(ctx context.Context, req ollama.ChatRequest) (string, error) {
	judgeCtx := ctx
	var cancel context.CancelFunc
	if s.JudgeTimeout > 0 {
		judgeCtx, cancel = context.WithTimeout(ctx, s.JudgeTimeout)
		defer cancel()
	}
	resp, err := s.Client.Chat(judgeCtx, req)
	if err != nil {
		return "", fmt.Errorf("judge model %q: %w", s.JudgeModel, err)
	}
	if resp == nil {
		return "", fmt.Errorf("judge model %q: %w: nil response", s.JudgeModel, errMalformedJudgeResponse)
	}
	return resp.Message.Content, nil
}

// materializeJudgement parses raw judge response content and merges it
// into baseScore. Pure; no I/O. Used identically by cache-hit and
// cache-miss paths so AnswerQuality/Notes derive from the judge text but
// ToolSequenceMatch comes from baseScore (recomputed fresh each call).
// Parse errors are wrapped with the judge model identity; callers MUST
// NOT re-attribute.
//
// The parsed judgeResponse is returned alongside the merged Score so the
// cache-put path can persist the verbatim justification without round-
// tripping it through the joined Notes string (which uses "; " as a
// separator and would silently truncate justifications that contain that
// substring — common in real judge output like "covered A; missed B").
func materializeJudgement(base Score, judgeModel, rawContent string) (Score, judgeResponse, error) {
	judgement, err := parseJudgeResponse(rawContent)
	if err != nil {
		return Score{}, judgeResponse{}, fmt.Errorf("judge model %q: %w", judgeModel, err)
	}
	base.AnswerQuality = judgement.AnswerQuality
	base.Notes = joinScoreNotes(base.Notes, fmt.Sprintf("llm-judge=%s: %s", judgeModel, judgement.Justification))
	return base, judgement, nil
}

// Score composes: build → check cache (if enabled) → callJudge or hit →
// materialize → put-to-cache (if enabled).
//
// Cache-hit path MUST pass `base` (fresh from buildJudgeCall on THIS call's
// trace + actual) into materializeJudgement — not a base reconstructed
// from the cached entry. This is the load-bearing invariant covered by
// TestLLMJudgeScorer_CacheHit_ReusesContentButRecomputesToolSequenceMatch:
// AnswerQuality and the judge justification are reused from the cached
// response content, but ToolSequenceMatch / Notes are always recomputed
// from the current actual transcript so tool-loop regressions can never
// be masked by a stale cache entry.
//
// Cache errors (Get and Put) and malformed cache-hit payloads are non-fatal:
// they are logged to stderr and bypassed. The judge call still proceeds on a
// Get error or malformed hit; the result is still returned on a Put error.
func (s *LLMJudgeScorer) Score(ctx context.Context, trace Trace, actual Result) (Score, error) {
	req, base, err := s.buildJudgeCall(trace, actual)
	if err != nil {
		return Score{}, err
	}
	if hit, ok := s.scoreFromCache(ctx, base, req); ok {
		return hit, nil
	}
	repairReq := repairOffGridJudgeRequest(req)
	if hit, ok := s.scoreFromCacheIfPresent(ctx, base, repairReq); ok {
		return hit, nil
	}
	content, err := s.callJudge(ctx, req)
	if err != nil {
		return Score{}, err
	}
	requestForCache := req
	materialized, judgement, err := materializeJudgement(base, s.JudgeModel, content)
	if errors.Is(err, errOffGridJudgeScore) {
		if hit, ok := s.scoreFromCache(ctx, base, repairReq); ok {
			return hit, nil
		}
		content, err = s.callJudge(ctx, repairReq)
		if err != nil {
			return Score{}, err
		}
		materialized, judgement, err = materializeJudgement(base, s.JudgeModel, content)
		requestForCache = repairReq
	}
	if err != nil {
		return Score{}, err
	}
	if s.Cache != nil && !s.BypassCache {
		cacheKey := s.cacheKeyForJudgeRequest(requestForCache)
		sum := sha256.Sum256([]byte(judgeUserPromptOf(requestForCache)))
		now := s.now()
		if putErr := s.Cache.Put(ctx, judgeCacheEntry{
			CacheKey:         cacheKey,
			JudgeProvider:    s.JudgeProvider,
			JudgeModel:       s.JudgeModel,
			JudgeModelDigest: s.JudgeModelDigest,
			TraceID:          trace.ID,
			CandidateModel:   actual.Model,
			PromptHash:       hex.EncodeToString(sum[:]),
			RequestJSON:      prettyRequestJSON(requestForCache),
			ResponseContent:  content,
			AnswerQuality:    materialized.AnswerQuality,
			Justification:    judgement.Justification,
			CreatedAt:        now,
			LastUsedAt:       now,
		}); putErr != nil {
			fmt.Fprintf(os.Stderr, "llm-bench: judge cache put bypassed: %v\n", putErr)
		}
	}
	return materialized, nil
}

func (s *LLMJudgeScorer) scoreFromCache(ctx context.Context, base Score, req ollama.ChatRequest) (Score, bool) {
	if s.Cache == nil || s.BypassCache {
		return Score{}, false
	}
	cacheKey := s.cacheKeyForJudgeRequest(req)
	hit, ok, getErr := s.Cache.Get(ctx, cacheKey)
	return s.materializeCacheLookup(base, hit, ok, getErr)
}

func (s *LLMJudgeScorer) scoreFromCacheIfPresent(ctx context.Context, base Score, req ollama.ChatRequest) (Score, bool) {
	if s.Cache == nil || s.BypassCache {
		return Score{}, false
	}
	presenceStore, ok := s.Cache.(judgeCachePresenceStore)
	if !ok {
		return Score{}, false
	}
	cacheKey := s.cacheKeyForJudgeRequest(req)
	hit, ok, getErr := presenceStore.GetIfPresent(ctx, cacheKey)
	return s.materializeCacheLookup(base, hit, ok, getErr)
}

func (s *LLMJudgeScorer) materializeCacheLookup(base Score, hit judgeCacheEntry, ok bool, getErr error) (Score, bool) {
	if getErr != nil {
		fmt.Fprintf(os.Stderr, "llm-bench: judge cache get bypassed: %v\n", getErr)
		return Score{}, false
	}
	if !ok {
		return Score{}, false
	}
	matHit, _, hitErr := materializeJudgement(base, s.JudgeModel, hit.ResponseContent)
	if hitErr != nil {
		fmt.Fprintf(os.Stderr, "llm-bench: judge cache hit bypassed: %v\n", hitErr)
		return Score{}, false
	}
	return matHit, true
}

func (s *LLMJudgeScorer) cacheKeyForJudgeRequest(req ollama.ChatRequest) string {
	return canonicalCacheKey(judgeCacheRequest{
		Version:          judgeCacheKeyVersion,
		JudgeProvider:    normalizeModelSelector(s.JudgeProvider),
		JudgeModel:       normalizeModelSelector(s.JudgeModel),
		JudgeModelDigest: s.JudgeModelDigest,
		SystemPrompt:     judgeSystemPrompt,
		UserPrompt:       judgeUserPromptOf(req),
		Format:           req.Format,
		Think:            req.Think,
		ThinkEnforced:    s.ThinkEnforced,
		Temperature:      judgeTemperature,
		NumPredict:       judgeTokenBudget,
	})
}

// judgeUserPromptOf extracts the user prompt that buildJudgeCall composed.
// We rely on the convention that buildJudgeCall always emits
// [system, user] in that order; this helper returns the first message with
// role "user" so a future ordering tweak surfaces obviously rather than
// silently mixing system and user content into the cache key.
func judgeUserPromptOf(req ollama.ChatRequest) string {
	for _, m := range req.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

// prettyRequestJSON serializes the judge ChatRequest for cache-audit
// inspection. Best-effort: returns "" on marshal failure rather than
// failing the Score call, since the cache row is observational only.
func prettyRequestJSON(req ollama.ChatRequest) string {
	raw, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return ""
	}
	return string(raw)
}

const judgeSystemPrompt = "You are an impartial evaluator for local LLM benchmark replays.\n" +
	"Score only the candidate assistant's final answer against the golden rubric.\n" +
	"Return only valid JSON with this schema:\n" +
	"{\"answer_quality\":0.5,\"justification\":\"short reason\"}\n\n" +
	"answer_quality must be exactly one of `0.0`, `0.5`, or `1.0`:\n" +
	"0.0 means the answer is wrong, fabricated, absent, or materially misleading.\n" +
	"0.5 means the answer is partially correct but missing important requirements, or contains a contained technical flaw.\n" +
	"1.0 means the answer fully satisfies the rubric with no material technical error.\n" +
	"Penalize unsupported claims, missing requested details, contradictions, and material technical errors.\n" +
	"Do not reward style, verbosity, or model identity."

const offGridJudgeRepairInstruction = "Repair instruction: answer_quality must be exactly one of 0.0, 0.5, or 1.0. Return only valid JSON matching {\"answer_quality\":0.5,\"justification\":\"short reason\"}."

func repairOffGridJudgeRequest(req ollama.ChatRequest) ollama.ChatRequest {
	repaired := req
	repaired.Messages = append([]ollama.ChatMessage(nil), req.Messages...)
	for i := len(repaired.Messages) - 1; i >= 0; i-- {
		if repaired.Messages[i].Role == "user" {
			repaired.Messages[i].Content = strings.TrimSpace(repaired.Messages[i].Content) + "\n\n" + offGridJudgeRepairInstruction
			return repaired
		}
	}
	repaired.Messages = append(repaired.Messages, ollama.ChatMessage{Role: "user", Content: offGridJudgeRepairInstruction})
	return repaired
}

type judgePromptPayload struct {
	TraceID                       string      `json:"trace_id"`
	CandidateModel                string      `json:"candidate_model"`
	System                        string      `json:"system"`
	SystemTruncated               bool        `json:"system_truncated,omitempty"`
	PromptTurns                   []judgeTurn `json:"prompt_turns"`
	GoldenToolCalls               []string    `json:"golden_tool_calls,omitempty"`
	ActualToolCalls               []string    `json:"actual_tool_calls,omitempty"`
	FinalAnswerCriteria           string      `json:"final_answer_criteria,omitempty"`
	FinalAnswerSubstring          string      `json:"final_answer_substring,omitempty"`
	FinalAnswerSubstringTruncated bool        `json:"final_answer_substring_truncated,omitempty"`
	ActualFinalAnswer             string      `json:"actual_final_answer"`
	ActualFinalAnswerTruncated    bool        `json:"actual_final_answer_truncated,omitempty"`
	ActualReplayTranscript        []judgeTurn `json:"actual_replay_transcript"`
}

type judgeTurn struct {
	Role             string   `json:"role"`
	Content          string   `json:"content,omitempty"`
	ContentTruncated bool     `json:"content_truncated,omitempty"`
	ToolCalls        []string `json:"tool_calls,omitempty"`
	ToolCallID       string   `json:"tool_call_id,omitempty"`
	Name             string   `json:"name,omitempty"`
}

func buildJudgePrompt(trace Trace, actual Result) (string, error) {
	finalAnswer, finalAnswerTruncated := truncateForJudge(lastAssistantContent(actual.Transcript), maxJudgeAnswerBytes)
	system, systemTruncated := truncateForJudge(trace.System, maxJudgeTurnContentBytes)
	finalAnswerSubstring, finalAnswerSubstringTruncated := truncateForJudge(strings.TrimSpace(trace.Golden.FinalAnswerSubstring), maxJudgeAnswerBytes)
	payload := judgePromptPayload{
		TraceID:                       trace.ID,
		CandidateModel:                actual.Model,
		System:                        system,
		SystemTruncated:               systemTruncated,
		PromptTurns:                   compactTurnsForJudge(trace.Turns),
		GoldenToolCalls:               trace.Golden.ToolCalls,
		ActualToolCalls:               extractToolNames(actual.Transcript),
		FinalAnswerCriteria:           strings.TrimSpace(trace.Golden.FinalAnswerCriteria),
		FinalAnswerSubstring:          finalAnswerSubstring,
		FinalAnswerSubstringTruncated: finalAnswerSubstringTruncated,
		ActualFinalAnswer:             finalAnswer,
		ActualFinalAnswerTruncated:    finalAnswerTruncated,
		ActualReplayTranscript:        compactTurnsForJudge(actual.Transcript),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("Evaluate this benchmark replay. Use final_answer_criteria when present; ")
	b.WriteString("otherwise use final_answer_substring as the expected answer excerpt. ")
	b.WriteString("Score the final answer only; tool-call quality is scored separately and is included only as context.\n\n")
	b.Write(data)
	return b.String(), nil
}

func compactTurnsForJudge(turns []Turn) []judgeTurn {
	if len(turns) == 0 {
		return nil
	}
	out := make([]judgeTurn, 0, len(turns))
	for _, turn := range turns {
		content, truncated := truncateForJudge(turn.Content, maxJudgeTurnContentBytes)
		out = append(out, judgeTurn{
			Role:             turn.Role,
			Content:          content,
			ContentTruncated: truncated,
			ToolCalls:        extractToolNames([]Turn{turn}),
			ToolCallID:       turn.ToolCallID,
			Name:             turn.Name,
		})
	}
	return out
}

func truncateForJudge(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut <= 0 {
		cut = limit
	}
	return s[:cut] + "\n[truncated for judge prompt]", true
}

type judgeResponse struct {
	AnswerQuality float64
	Justification string
}

// parseJudgeResponse extracts the verdict from a judge response. A frontier
// judge sometimes precedes the verdict with other balanced {...} blobs — an
// echoed struct literal, `{}`, a quoted example — so the FIRST object is not
// reliably the verdict (observed live as "missing answer_quality"). We scan
// each candidate object and select the one that actually carries
// answer_quality; when multiple verdict-shaped objects are present, the last
// one wins so quoted candidate text cannot self-score before the judge's final
// verdict. Comment stripping is handled by firstJSONObject.
func parseJudgeResponse(content string) (judgeResponse, error) {
	raw := strings.TrimSpace(content)
	if raw == "" {
		return judgeResponse{}, fmt.Errorf("%w: empty response", errMalformedJudgeResponse)
	}

	sawObject := false
	sawVerdictObject := false
	var lastVerdict judgeResponse
	haveLastVerdict := false
	var lastVerdictErr error
	for pos := 0; pos < len(raw); {
		idx := strings.IndexByte(raw[pos:], '{')
		if idx < 0 {
			break
		}
		abs := pos + idx
		obj, consumed := firstJSONObjectWithEnd(raw[abs:])
		if obj == "" {
			pos = abs + 1
			continue
		}
		sawObject = true
		nextPos := abs + consumed
		if nextPos <= pos {
			nextPos = abs + 1
		}
		var parsed struct {
			AnswerQuality *float64 `json:"answer_quality"`
			Justification string   `json:"justification"`
			Notes         string   `json:"notes"`
		}
		if err := json.Unmarshal([]byte(obj), &parsed); err != nil || parsed.AnswerQuality == nil {
			// Not the verdict object (malformed, or a non-verdict blob like an
			// echoed struct literal). Resume scanning after this object.
			pos = nextPos
			continue
		}
		sawVerdictObject = true
		quality := *parsed.AnswerQuality
		if quality < 0 || quality > 1 {
			lastVerdictErr = fmt.Errorf("%w: answer_quality %.3f outside [0,1]", errMalformedJudgeResponse, quality)
			haveLastVerdict = false
			pos = nextPos
			continue
		}
		if !validJudgeAnswerQuality(quality) {
			lastVerdictErr = fmt.Errorf("%w: answer_quality %.3f must be exactly one of 0.0, 0.5, or 1.0", errOffGridJudgeScore, quality)
			haveLastVerdict = false
			pos = nextPos
			continue
		}
		justification := strings.TrimSpace(parsed.Justification)
		if justification == "" {
			justification = strings.TrimSpace(parsed.Notes)
		}
		if justification == "" {
			justification = "no justification returned"
		}
		lastVerdict = judgeResponse{
			AnswerQuality: quality,
			Justification: normalizeNote(justification),
		}
		haveLastVerdict = true
		lastVerdictErr = nil
		pos = nextPos
	}

	if haveLastVerdict {
		return lastVerdict, nil
	}
	if !sawObject {
		return judgeResponse{}, fmt.Errorf("%w: missing JSON object", errMalformedJudgeResponse)
	}
	if sawVerdictObject && lastVerdictErr != nil {
		return judgeResponse{}, lastVerdictErr
	}
	return judgeResponse{}, fmt.Errorf("%w: missing answer_quality", errMalformedJudgeResponse)
}

func validJudgeAnswerQuality(v float64) bool {
	return v == 0 || v == 0.5 || v == 1
}

// firstJSONObject extracts the first balanced {...} object from s and returns
// it with JSON-illegal // line comments and /* */ block comments stripped.
// LLM judges (especially when the judged content itself contains code) sometimes
// emit comments — observed live with a frontier judge producing
// `{"answer_quality":1.0, // matches\n ...}`, which json.Unmarshal rejects.
// Comments are stripped only OUTSIDE string literals, so a // or /* that lives
// inside the justification string is preserved verbatim. Comment-aware scanning
// also prevents a brace inside a comment from corrupting the depth count.
func firstJSONObject(s string) string {
	obj, _ := firstJSONObjectWithEnd(s)
	return obj
}

func firstJSONObjectWithEnd(s string) (string, int) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", 0
	}

	var b strings.Builder
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
			b.WriteByte(ch)
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}

		// Outside a string: skip // line and /* */ block comments so the
		// extracted object is valid JSON and braces inside comments do not
		// affect depth.
		if ch == '/' && i+1 < len(s) && s[i+1] == '/' {
			i += 2
			for i < len(s) && s[i] != '\n' {
				i++
			}
			continue // loop i++ steps past the newline (or EOF)
		}
		if ch == '/' && i+1 < len(s) && s[i+1] == '*' {
			i += 2
			for i+1 < len(s) && (s[i] != '*' || s[i+1] != '/') {
				i++
			}
			i++      // position on the closing '/'
			continue // loop i++ steps past it
		}

		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
		}
		b.WriteByte(ch)
		if ch == '}' && depth == 0 {
			return b.String(), i + 1
		}
	}
	return "", 0
}

func sameModelSelector(a, b string) bool {
	a = normalizeModelSelector(a)
	b = normalizeModelSelector(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return modelSelectorWithoutBenchProvider(a) == modelSelectorWithoutBenchProvider(b)
}

func normalizeModelSelector(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func modelSelectorWithoutBenchProvider(s string) string {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	for _, provider := range []string{defaultBenchProvider, openAICompatTransport} {
		prefix := provider + "/"
		if strings.HasPrefix(lower, prefix) {
			return strings.TrimSpace(s[len(prefix):])
		}
	}
	return s
}

func canonicalCandidateModelKey(s string) string {
	selector := strings.TrimSpace(s)
	provider, model, found := strings.Cut(selector, "/")
	if !found {
		return normalizeModelSelector(selector)
	}
	provider = normalizeModelSelector(provider)
	model = normalizeModelSelector(model)
	switch provider {
	case defaultBenchProvider:
		if nestedProvider, _, ok := strings.Cut(model, "/"); ok && supportedCandidateProvider(nestedProvider) {
			return provider + "/" + model
		}
		return model
	case openAICompatTransport:
		return provider + "/" + model
	default:
		return normalizeModelSelector(selector)
	}
}

func joinScoreNotes(parts ...string) string {
	var cleaned []string
	for _, part := range parts {
		part = normalizeNote(part)
		if part != "" {
			cleaned = append(cleaned, part)
		}
	}
	return strings.Join(cleaned, "; ")
}

func normalizeNote(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// toolSequenceScore computes a simple Jaccard overlap between the expected
// and actual tool-call sequences, ignoring order. The tool loop now
// records real ordered calls, so order-sensitive scoring (e.g.
// Levenshtein) is a meaningful follow-up — deferred pending the
// cost/benefit call on whether a coarser sequence-match-vs-divergence
// signal is enough for the benchmark's purpose.
func toolSequenceScore(expected, actual []string) float64 {
	if len(expected) == 0 && len(actual) == 0 {
		return 1.0
	}
	if len(expected) == 0 || len(actual) == 0 {
		return 0.0
	}
	expSet := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		expSet[e] = struct{}{}
	}
	actSet := make(map[string]struct{}, len(actual))
	for _, a := range actual {
		actSet[a] = struct{}{}
	}
	union := make(map[string]struct{}, len(expected)+len(actual))
	for k := range expSet {
		union[k] = struct{}{}
	}
	intersect := 0
	for a := range actSet {
		union[a] = struct{}{}
		if _, ok := expSet[a]; ok {
			intersect++
		}
	}
	if len(union) == 0 {
		return 0.0
	}
	return float64(intersect) / float64(len(union))
}

func extractToolNames(turns []Turn) []string {
	var names []string
	for _, t := range turns {
		for _, tc := range t.ToolCalls {
			names = append(names, tc.Name)
		}
	}
	return names
}

// lastAssistantContent returns the content of the final assistant turn, or
// "" if there is no assistant turn. It does NOT walk backward past an
// empty-content assistant turn to find a prior non-empty one — doing so
// would return stale content if the last action was a tool call with no
// final answer. Callers can distinguish "no final answer" from "answer
// didn't match" via an empty return.
func lastAssistantContent(turns []Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" {
			return turns[i].Content
		}
	}
	return ""
}
