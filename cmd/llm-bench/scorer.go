package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/ollama"
)

const (
	fallbackJudgeModel       = "gemma4:31b"
	judgeTemperature         = 0.1
	judgeTokenBudget         = 512
	maxJudgeTurnContentBytes = 8192
	maxJudgeAnswerBytes      = 32768
)

// Score captures the evaluation dimensions for a single (model, trace) run.
// See docs/llm/benchmark-plan.md for the scoring rationale.
type Score struct {
	ToolSequenceMatch float64 // [0,1] — how close actual tool calls were to the golden sequence
	ToolArgsValid     float64 // [0,1] — fraction of tool calls with valid arguments
	AnswerQuality     float64 // [0,1] — final-answer quality per the active Scorer
	LatencyMs         int64   // sum of all chat round-trips for this replay
	TurnLatenciesMs   []int64 // per-turn breakdown; len == number of chat round-trips
	ScorerLatencyMs   int64   // wall-clock time spent in the active scorer
	TotalTokens       int
	Notes             string
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
		client, err := newOllamaClient(opts.ollamaURL, ollama.WithTimeout(opts.judgeTimeout))
		if err != nil {
			return nil, fmt.Errorf("llm-judge client: %w", err)
		}
		scorer, err := newLLMJudgeScorer(client, opts.judgeModel, opts.judgeTimeout)
		if err != nil {
			return nil, err
		}
		if err := validateJudgeModel(ctx, client, scorer.JudgeModel); err != nil {
			return nil, err
		}
		digest, _ := resolveJudgeDigest(ctx, client, scorer.JudgeModel)
		scorer.JudgeModelDigest = digest
		scorer.Cache = opts.judgeCache
		scorer.BypassCache = opts.bypassCache
		return scorer, nil
	case "manual":
		return nil, fmt.Errorf("manual scorer not yet implemented")
	default:
		return nil, fmt.Errorf("unknown scorer %q", name)
	}
}

// ExactMatchScorer is a dependency-free baseline. Answer quality is scored
// as 1.0 iff the golden substring appears in the assistant's response,
// 0.0 otherwise. Use it to bootstrap the harness; graduate to `llm-judge`
// before drawing conclusions.
type ExactMatchScorer struct{}

// Score implements Scorer. ToolArgsValid is left unset (zero) until replay
// validates candidate arguments against trace.Tools schemas. The Notes field
// records this so aggregate consumers can distinguish "not scored" from
// "scored zero".
func (s *ExactMatchScorer) Score(_ context.Context, trace Trace, actual Result) (Score, error) {
	needle := strings.TrimSpace(trace.Golden.FinalAnswerSubstring)
	if needle == "" {
		return Score{}, fmt.Errorf("trace %q: %w", trace.ID, errMissingGolden)
	}

	score := Score{
		ToolSequenceMatch: toolSequenceScore(trace.Golden.ToolCalls, extractToolNames(actual.Transcript)),
		Notes:             "ToolArgsValid not computed (schema validation pending; see benchmark-plan.md metrics)",
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
	return Score{
		ToolSequenceMatch: toolSequenceScore(trace.Golden.ToolCalls, extractToolNames(actual.Transcript)),
		Notes:             "capture mode: scoring skipped",
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
	info, err := checker.ShowModel(ctx, judgeModel)
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
	JudgeModel       string
	JudgeModelDigest string // optional; empty when /api/show was unavailable
	JudgeTimeout     time.Duration
	Cache            judgeCacheStore  // optional; nil disables caching
	BypassCache      bool             // when true, Score skips both Get and Put
	Clock            func() time.Time // injectable; defaults to time.Now().UTC()
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
	return fmt.Errorf("judge model %q is not available from the configured Ollama server; pull it or pass -judge-model/-judge-ollama-url", judgeModel)
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

	baseScore := Score{
		ToolSequenceMatch: toolSequenceScore(trace.Golden.ToolCalls, extractToolNames(actual.Transcript)),
		Notes:             "ToolArgsValid not computed (schema validation pending; see benchmark-plan.md metrics)",
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
		Model: s.JudgeModel,
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: judgeSystemPrompt},
			{Role: "user", Content: prompt},
		},
		Format: "json",
		Think:  &think,
		Options: &ollama.ModelOptions{
			Temperature: judgeTemperature,
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
	var cacheKey string
	if s.Cache != nil && !s.BypassCache {
		cacheKey = canonicalCacheKey(judgeCacheRequest{
			Version:          judgeCacheKeyVersion,
			JudgeModel:       normalizeModelSelector(s.JudgeModel),
			JudgeModelDigest: s.JudgeModelDigest,
			SystemPrompt:     judgeSystemPrompt,
			UserPrompt:       judgeUserPromptOf(req),
			Format:           req.Format,
			Think:            req.Think,
			Temperature:      judgeTemperature,
			NumPredict:       judgeTokenBudget,
		})
		if hit, ok, getErr := s.Cache.Get(ctx, cacheKey); getErr != nil {
			fmt.Fprintf(os.Stderr, "llm-bench: judge cache get bypassed: %v\n", getErr)
		} else if ok {
			matHit, _, hitErr := materializeJudgement(base, s.JudgeModel, hit.ResponseContent)
			if hitErr != nil {
				fmt.Fprintf(os.Stderr, "llm-bench: judge cache hit bypassed: %v\n", hitErr)
			} else {
				return matHit, nil
			}
		}
	}
	content, err := s.callJudge(ctx, req)
	if err != nil {
		return Score{}, err
	}
	materialized, judgement, err := materializeJudgement(base, s.JudgeModel, content)
	if err != nil {
		return Score{}, err
	}
	if s.Cache != nil && !s.BypassCache && cacheKey != "" {
		sum := sha256.Sum256([]byte(judgeUserPromptOf(req)))
		now := s.now()
		if putErr := s.Cache.Put(ctx, judgeCacheEntry{
			CacheKey:         cacheKey,
			JudgeModel:       s.JudgeModel,
			JudgeModelDigest: s.JudgeModelDigest,
			TraceID:          trace.ID,
			CandidateModel:   actual.Model,
			PromptHash:       hex.EncodeToString(sum[:]),
			RequestJSON:      prettyRequestJSON(req),
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

const judgeSystemPrompt = `You are an impartial evaluator for local LLM benchmark replays.
Score only the candidate assistant's final answer against the golden rubric.
Return only valid JSON with this schema:
{"answer_quality":0.5,"justification":"short reason"}

answer_quality must be a number from 0 to 1:
0.0 means the answer is wrong or absent.
0.5 means the answer is partially correct but misses important requirements.
1.0 means the answer fully satisfies the rubric.
Penalize unsupported claims, missing requested details, and contradictions.
Do not reward style, verbosity, or model identity.`

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

func parseJudgeResponse(content string) (judgeResponse, error) {
	raw := strings.TrimSpace(content)
	if raw == "" {
		return judgeResponse{}, fmt.Errorf("%w: empty response", errMalformedJudgeResponse)
	}
	raw = firstJSONObject(raw)
	if raw == "" {
		return judgeResponse{}, fmt.Errorf("%w: missing JSON object", errMalformedJudgeResponse)
	}

	var parsed struct {
		AnswerQuality *float64 `json:"answer_quality"`
		Score         *float64 `json:"score"`
		Justification string   `json:"justification"`
		Notes         string   `json:"notes"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return judgeResponse{}, fmt.Errorf("%w: %v", errMalformedJudgeResponse, err)
	}
	quality := parsed.AnswerQuality
	if quality == nil {
		quality = parsed.Score
	}
	if quality == nil {
		return judgeResponse{}, fmt.Errorf("%w: missing answer_quality", errMalformedJudgeResponse)
	}
	if *quality < 0 || *quality > 1 {
		return judgeResponse{}, fmt.Errorf("%w: answer_quality %.3f outside [0,1]", errMalformedJudgeResponse, *quality)
	}

	justification := strings.TrimSpace(parsed.Justification)
	if justification == "" {
		justification = strings.TrimSpace(parsed.Notes)
	}
	if justification == "" {
		justification = "no justification returned"
	}

	return judgeResponse{
		AnswerQuality: *quality,
		Justification: normalizeNote(justification),
	}, nil
}

func firstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
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

		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
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
	prefix := defaultBenchProvider + "/"
	if strings.HasPrefix(s, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(s, prefix))
	}
	return s
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
