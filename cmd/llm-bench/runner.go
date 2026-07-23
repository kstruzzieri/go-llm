package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

// benchKeepAlive is the Ollama keep_alive directive used for benchmark runs.
// Explicitly overrides Ollama's 5-minute default so the outer-loop ordering
// in RunAll actually keeps the model warm across all of a target's traces.
const benchKeepAlive = "30m"

const plainChatToolDivergenceFinal = "__LLM_BENCH_TOOL_DIVERGENCE_8F3E0B2C__"

// Sentinel errors so tests assert on identity rather than message text.
var (
	errNoUserTurn               = errors.New("trace has no user turn")
	errEmptyBaseURL             = errors.New("empty ollama base URL")
	errMissingGolden            = errors.New("scorer requires golden.final_answer_substring")
	errEmptySystem              = errors.New("trace has empty system prompt")
	errNoTurns                  = errors.New("trace has no turns")
	errInvalidTraceTool         = errors.New("trace has invalid tool definition")
	errMissingToolResult        = errors.New("trace is missing frozen tool result")
	errMissingScriptedAssistant = errors.New("trace is missing scripted assistant turn for candidate tool call")
	errToolCallMismatch         = errors.New("candidate tool call does not match trace")
	errUnsupportedTurns         = errors.New("trace has unsupported extra turns")
	errEmptyAssistantReply      = errors.New("candidate returned empty assistant reply (no content, no tool calls)")
	errUnsupportedProv          = errors.New("unsupported provider")
	errEmptyJudgeModel          = errors.New("empty judge model")
	errMissingJudgeCriteria     = errors.New("judge scorer requires golden.final_answer_criteria or golden.final_answer_substring")
	errJudgeSelfPreference      = errors.New("judge model must differ from candidate model")
	errMalformedJudgeResponse   = errors.New("malformed judge response")
	errOffGridJudgeScore        = errors.New("off-grid judge answer_quality")
	errNoAssistantFinalAnswer   = errors.New("judge scorer requires an assistant final answer")
)

// Runner executes traces against a list of candidate models and collects
// per-trace, per-model Results.
type Runner struct {
	OllamaURL string
	Timeout   time.Duration
	// OpenAICompatBaseURL is required when any candidate target uses the
	// openai-compat provider. Ollama candidates ignore it.
	OpenAICompatBaseURL string
	// OpenAICompatAPIKey is an optional Bearer token for openai-compat
	// candidate targets. Prefer LLM_BENCH_CANDIDATE_API_KEY over shell history.
	OpenAICompatAPIKey string
	// PerTurnTimeout bounds a single chatReplayTurn round-trip. Zero means
	// no per-turn bound — only Timeout applies to the whole replay. Set
	// this when running models that may stall mid-loop to keep one bad
	// trace from draining the full per-trace budget.
	PerTurnTimeout time.Duration
	// NumCtx, when non-zero, is passed to Ollama as the context-window
	// directive. Zero leaves the model's configured default in place;
	// long multi-turn replays may silently truncate prompts from the
	// model's perspective without this set.
	NumCtx int
	Scorer Scorer
}

// Result is a single (model, trace) evaluation.
type Result struct {
	Model             string
	TraceID           string
	CandidateProvider string
	Score             Score
	Err               error
	Transcript        []Turn
}

// RunAll replays every trace against every model. Iterates models in the
// outer loop plus an explicit keep_alive directive so the model stays
// warm across all of its traces. Per-target client-construction failures
// and per-trace errors are recorded as Result rows rather than aborting
// the whole run, so partial progress is never discarded.
func (r *Runner) RunAll(ctx context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	results := make([]Result, 0, len(targets)*len(traces))
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return results, err
		}

		transport, err := newCandidateTransport(target, candidateTransportOptions{
			ollamaURL:           r.OllamaURL,
			openAICompatBaseURL: r.OpenAICompatBaseURL,
			openAICompatAPIKey:  r.OpenAICompatAPIKey,
			timeout:             r.Timeout,
		})
		if err != nil {
			for _, trace := range traces {
				results = append(results, Result{
					Model:             target.Display,
					TraceID:           trace.ID,
					CandidateProvider: target.Provider,
					Err:               fmt.Errorf("client for %q: %w", target.Display, err),
				})
			}
			continue
		}

		for _, trace := range traces {
			if err := ctx.Err(); err != nil {
				return results, err
			}
			results = append(results, r.runOne(ctx, transport, target, trace))
		}
	}
	return results, nil
}

func (r *Runner) runOne(ctx context.Context, transport candidateTransport, target ModelTarget, trace Trace) Result {
	replayCtx, cancel := context.WithTimeout(ctx, r.Timeout)

	opts := replayOptions{PerTurnTimeout: r.PerTurnTimeout, NumCtx: r.NumCtx}
	out, err := replayWith(replayCtx, transport.chat, target.Model, trace, opts)
	cancel()
	if err != nil {
		return Result{Model: target.Display, TraceID: trace.ID, CandidateProvider: transport.providerName, Err: err, Transcript: out.Transcript}
	}

	scoreStart := time.Now()
	score, err := r.Scorer.Score(ctx, trace, Result{
		Model:             target.Display,
		TraceID:           trace.ID,
		CandidateProvider: transport.providerName,
		Transcript:        out.Transcript,
	})
	scorerMs := time.Since(scoreStart).Milliseconds()
	if err != nil {
		return Result{Model: target.Display, TraceID: trace.ID, CandidateProvider: transport.providerName, Err: err, Transcript: out.Transcript}
	}
	// LatencyMs sums per-turn chat round-trips only; scorer work is
	// excluded so llm-judge round-trips can't pollute the model-latency
	// metric. TurnLatenciesMs preserves the per-turn breakdown so a slow
	// first turn (cold load) is distinguishable from a slow tail turn
	// (long context).
	score.LatencyMs = sumInt64(out.TurnLatenciesMs)
	score.TurnLatenciesMs = out.TurnLatenciesMs
	if len(out.Notes) > 0 {
		score.Notes = appendNotes(score.Notes, out.Notes)
	}
	// Latency reflects replay only; ScorerLatencyMs keeps judge/scorer work visible
	// without polluting the target model latency metric.
	score.ScorerLatencyMs = scorerMs
	score.TotalTokens = out.TotalTokens
	score.PromptEvalTokens = out.PromptEvalTokens
	score.GenTokens = out.GenTokens
	score.ThinkingTokens = out.ThinkingTokens
	score.ThinkingTokensComputed = out.ThinkingComputed

	return Result{
		Model:             target.Display,
		TraceID:           trace.ID,
		CandidateProvider: transport.providerName,
		Score:             score,
		Transcript:        out.Transcript,
	}
}

// tokenUsage is the per-turn token breakdown reported by the provider. Gen and
// PromptEval map to Ollama eval_count/prompt_eval_count and to OpenAI-compat
// usage.completion_tokens/prompt_tokens. ThinkingComputed is false when the
// provider does not isolate reasoning tokens (e.g. Ollama folds them into Gen).
type tokenUsage struct {
	PromptEval       int
	Gen              int
	Thinking         int
	ThinkingComputed bool
}

// replayOutput bundles the side effects of a single replay.
type replayOutput struct {
	Transcript       []Turn
	Notes            []string
	TurnLatenciesMs  []int64
	TotalTokens      int // prompt-eval + gen (back-compat)
	PromptEvalTokens int
	GenTokens        int
	ThinkingTokens   int
	ThinkingComputed bool // true once any turn reported isolated thinking tokens
}

// replayOptions are the per-replay knobs threaded down from Runner.
// Zero values preserve current behavior: no per-turn bound, no NumCtx.
type replayOptions struct {
	PerTurnTimeout time.Duration
	NumCtx         int
}

// replay is the legacy entry point retained for tests and callers that
// don't need the Runner-level knobs (PerTurnTimeout, NumCtx).
func replay(ctx context.Context, client *ollama.Client, model string, trace Trace) ([]Turn, error) {
	out, err := replayWith(ctx, ollamaCandidateClient{client: client}, model, trace, replayOptions{})
	return out.Transcript, err
}

// replayWith sends user turns to the model, replaces scripted assistant
// turns with candidate responses, and feeds captured tool-result turns
// back after matching candidate tool calls.
//
// Tool results are frozen replay fixtures: the candidate chooses whether
// and which tool to call, but llm-bench never executes tools. The match
// is strict and lock-step — the candidate must emit the same number of
// tool calls in the same order, with the same names, as the scripted
// assistant turn it is replacing. Argument-shape divergence is not
// rejected here (schema validation is scorer-side follow-up).
//
// Divergence handling:
//   - Candidate emits tool calls when the scripted turn was plain text:
//     errToolCallMismatch.
//   - Candidate emits tool calls when there is no scripted assistant turn
//     after the user turn: for a captured plain-chat trace (Golden.ToolCalls
//     empty) this is recorded as a scored divergence (Notes annotation, replay
//     ends, low quality); for a trace that expected a tool route it is the
//     errMissingScriptedAssistant malformed-fixture error.
//   - Candidate emits plain text when the scripted route used tools:
//     scripted tool group is skipped and a Notes annotation records the
//     bypass; the candidate's reply replaces the scripted final answer.
//   - Candidate emits a fully empty reply (no content, no tool calls):
//     errEmptyAssistantReply.
//
// All accumulated divergence notes are appended to the resulting Score's
// Notes so aggregate consumers can see why a candidate diverged.
func replayWith(ctx context.Context, client candidateChatClient, model string, trace Trace, opts replayOptions) (replayOutput, error) {
	if !hasUserTurn(trace.Turns) {
		return replayOutput{}, fmt.Errorf("trace %q: %w", trace.ID, errNoUserTurn)
	}

	messages := []ollama.ChatMessage{
		{Role: "system", Content: trace.System},
	}
	// Tools are always exposed exactly as captured: a faithful replay must
	// present the candidate with the same tool temptation the original workflow
	// did. Whether the candidate (mis)uses them is the signal we measure, not
	// something to suppress.
	tools, err := decodeTraceTools(trace.Tools)
	if err != nil {
		return replayOutput{}, fmt.Errorf("trace %q: %w", trace.ID, err)
	}

	out := replayOutput{}
	for i := 0; i < len(trace.Turns); {
		turn := trace.Turns[i]
		if turn.Role != "user" {
			return out, fmt.Errorf("trace %q turn %d role %q: %w", trace.ID, i, turn.Role, errUnsupportedTurns)
		}
		messages = append(messages, ollama.ChatMessage{Role: "user", Content: turn.Content})
		userIndex := i
		i++

		for {
			var expected Turn
			expectedIndex := -1
			if i < len(trace.Turns) && trace.Turns[i].Role == "assistant" {
				expected = trace.Turns[i]
				expectedIndex = i
				i++
			}

			msg, actualTurn, latencyMs, usage, err := chatReplayTurn(ctx, client, model, messages, tools, opts)
			out.TurnLatenciesMs = append(out.TurnLatenciesMs, latencyMs)
			if err != nil {
				return out, err
			}
			out.PromptEvalTokens += usage.PromptEval
			out.GenTokens += usage.Gen
			out.TotalTokens += usage.PromptEval + usage.Gen
			if usage.ThinkingComputed {
				out.ThinkingTokens += usage.Thinking
				out.ThinkingComputed = true
			}
			messages = append(messages, msg)
			out.Transcript = append(out.Transcript, actualTurn)

			if len(msg.ToolCalls) == 0 {
				if msg.Content == "" {
					return out, fmt.Errorf("trace %q turn %d: %w", trace.ID, userIndex, errEmptyAssistantReply)
				}
				if len(expected.ToolCalls) > 0 {
					skipped, next := skipScriptedToolLoop(trace.Turns, i)
					if skipped > 0 {
						out.Notes = append(out.Notes,
							fmt.Sprintf("trace %q user turn %d: candidate bypassed scripted tool route (%d turn(s) skipped from index %d)",
								trace.ID, userIndex, skipped, i))
					}
					i = next
				}
				break
			}

			if expectedIndex == -1 {
				if len(trace.Golden.ToolCalls) == 0 {
					return scorePlainChatToolDivergence(out, trace.ID, userIndex, len(msg.ToolCalls), "no scripted tool route"), nil
				}
				// A trace that genuinely expected a tool route but lacks the
				// scripted assistant turn is a malformed fixture: surface it.
				return out, fmt.Errorf("trace %q user turn %d: candidate emitted %d tool call(s) with no scripted assistant turn to match: %w",
					trace.ID, userIndex, len(msg.ToolCalls), errMissingScriptedAssistant)
			}
			if len(expected.ToolCalls) == 0 {
				if len(trace.Golden.ToolCalls) == 0 {
					return scorePlainChatToolDivergence(out, trace.ID, userIndex, len(msg.ToolCalls), "scripted plain-text reply"), nil
				}
				return out, fmt.Errorf("trace %q assistant turn %d: candidate emitted %d tool call(s) for plain-text scripted reply: %w",
					trace.ID, expectedIndex, len(msg.ToolCalls), errToolCallMismatch)
			}

			toolMessages, toolTurns, next, err := frozenToolResults(trace.ID, expectedIndex, expected, trace.Turns, i, msg.ToolCalls)
			if err != nil {
				return out, err
			}
			i = next
			messages = append(messages, toolMessages...)
			out.Transcript = append(out.Transcript, toolTurns...)
		}
	}

	if note := thinkMarkupResidueNote(lastAssistantContent(out.Transcript)); note != "" {
		out.Notes = append(out.Notes, note)
	}

	return out, nil
}

func scorePlainChatToolDivergence(out replayOutput, traceID string, userIndex, toolCallCount int, reason string) replayOutput {
	// A captured plain-chat trace (no expected tool route) has no frozen tool
	// result to feed back, so replay cannot continue deterministically once the
	// candidate reaches for a tool. That choice is a real divergence (failed
	// restraint), not a harness error: record it as a scored divergence so
	// exactly one artifact per (trace, model) pair is still written.
	//
	// The tool call IS the divergent answer. Replace any prose the candidate
	// emitted alongside it with a deterministic marker so a stray sentence that
	// happens to contain the golden substring cannot be scored as a correct
	// final answer. The marker is non-empty so llm-judge can still score the
	// failed-restraint output instead of rejecting the replay as malformed. The
	// tool call stays on the turn for forensics and the Note records the
	// divergence.
	if n := len(out.Transcript); n > 0 {
		out.Transcript[n-1].Content = plainChatToolDivergenceFinal
	}
	out.Notes = append(out.Notes,
		fmt.Sprintf("trace %q user turn %d: candidate called %d tool(s) on a plain-chat trace (%s); scored as divergence",
			traceID, userIndex, toolCallCount, reason))
	return out
}

func hasUserTurn(turns []Turn) bool {
	for _, turn := range turns {
		if turn.Role == "user" {
			return true
		}
	}
	return false
}

func chatReplayTurn(ctx context.Context, client candidateChatClient, model string, messages []ollama.ChatMessage, tools []ollama.Tool, opts replayOptions) (ollama.ChatMessage, Turn, int64, tokenUsage, error) {
	turnCtx := ctx
	if opts.PerTurnTimeout > 0 {
		var cancel context.CancelFunc
		turnCtx, cancel = context.WithTimeout(ctx, opts.PerTurnTimeout)
		defer cancel()
	}

	req := ollama.ChatRequest{
		Model:     model,
		Messages:  messages,
		Tools:     tools,
		KeepAlive: benchKeepAlive,
	}
	if opts.NumCtx > 0 {
		req.Options = &ollama.ModelOptions{NumCtx: opts.NumCtx}
	}

	start := time.Now()
	resp, err := client.Chat(turnCtx, req)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return ollama.ChatMessage{}, Turn{}, latencyMs, tokenUsage{}, err
	}

	msg := normalizeAssistantMessage(resp.Message)
	turn, err := assistantTurnFromMessage(msg)
	if err != nil {
		return ollama.ChatMessage{}, Turn{}, latencyMs, tokenUsage{}, err
	}
	// Capture reasoning text in the transcript turn (kept separate from Content
	// and out of the history message msg, which feeds later turns). See #160.
	turn.Thinking = resp.Thinking
	usage := tokenUsage{
		PromptEval:       resp.PromptEvalCount,
		Gen:              resp.EvalCount,
		Thinking:         resp.ThinkingTokens,
		ThinkingComputed: resp.ThinkingTokensComputed,
	}
	return msg, turn, latencyMs, usage, nil
}

func normalizeAssistantMessage(msg ollama.ChatMessage) ollama.ChatMessage {
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	return msg
}

// frozenToolResults pairs each candidate tool call with the corresponding
// frozen tool-result turn from the trace and produces both the wire
// messages and the transcript turns. The candidate's call ID is the
// authoritative correlator: an empty candidate ID stays empty rather
// than borrowing the captured trace's ID (which belonged to a different
// conversation and would mislead any model that routes by ID).
func frozenToolResults(traceID string, expectedIndex int, expected Turn, turns []Turn, start int, calls []ollama.ToolCall) ([]ollama.ChatMessage, []Turn, int, error) {
	if len(calls) != len(expected.ToolCalls) {
		return nil, nil, start, fmt.Errorf("trace %q assistant turn %d: got %d candidate tool calls for %d frozen calls: %w",
			traceID, expectedIndex, len(calls), len(expected.ToolCalls), errToolCallMismatch)
	}
	if start+len(expected.ToolCalls) > len(turns) {
		return nil, nil, start, fmt.Errorf("trace %q assistant turn %d: %w", traceID, expectedIndex, errMissingToolResult)
	}

	messages := make([]ollama.ChatMessage, 0, len(calls))
	transcript := make([]Turn, 0, len(calls))
	for i, call := range calls {
		expectedCall := expected.ToolCalls[i]
		callName := strings.TrimSpace(call.Function.Name)
		if callName == "" || callName != strings.TrimSpace(expectedCall.Name) {
			return nil, nil, start, fmt.Errorf("trace %q assistant turn %d tool %d: got %q, want %q: %w",
				traceID, expectedIndex, i, callName, expectedCall.Name, errToolCallMismatch)
		}

		result := turns[start+i]
		if result.Role != "tool" {
			return nil, nil, start, fmt.Errorf("trace %q assistant turn %d tool %d: %w", traceID, expectedIndex, i, errMissingToolResult)
		}
		resultName := strings.TrimSpace(result.Name)
		if resultName != "" && resultName != callName {
			return nil, nil, start, fmt.Errorf("trace %q assistant turn %d tool %d result: got %q, want %q: %w",
				traceID, expectedIndex, i, resultName, callName, errToolCallMismatch)
		}

		messages = append(messages, ollama.ChatMessage{
			Role:       "tool",
			Content:    result.Content,
			ToolName:   callName,
			ToolCallID: call.ID,
		})
		transcript = append(transcript, Turn{
			Role:       "tool",
			Content:    result.Content,
			Name:       callName,
			ToolCallID: call.ID,
			Raw:        result.Raw,
		})
	}

	next := start + len(expected.ToolCalls)
	if next < len(turns) && turns[next].Role == "tool" {
		return nil, nil, start, fmt.Errorf("trace %q assistant turn %d: extra frozen tool result: %w",
			traceID, expectedIndex, errUnsupportedTurns)
	}
	return messages, transcript, next, nil
}

// skipScriptedToolLoop fast-forwards past a scripted tool group when the
// candidate diverged from the scripted route by replying with plain text.
// Returns the number of turns skipped (for Notes annotation) and the new
// index. Skips up to the next user turn since the outer replay loop only
// accepts user turns at the top level.
func skipScriptedToolLoop(turns []Turn, start int) (int, int) {
	i := start
	for i < len(turns) && turns[i].Role != "user" {
		i++
	}
	return i - start, i
}

func decodeTraceTools(rawTools []json.RawMessage) ([]ollama.Tool, error) {
	if len(rawTools) == 0 {
		return nil, nil
	}

	tools := make([]ollama.Tool, 0, len(rawTools))
	for i, raw := range rawTools {
		tool, err := decodeTraceTool(raw)
		if err != nil {
			return nil, fmt.Errorf("tool[%d]: %w", i, err)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func decodeTraceTool(raw json.RawMessage) (ollama.Tool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		if err != nil {
			return ollama.Tool{}, fmt.Errorf("%w: invalid JSON object: %v", errInvalidTraceTool, err)
		}
		return ollama.Tool{}, fmt.Errorf("%w: invalid JSON object", errInvalidTraceTool)
	}

	if _, ok := fields["function"]; ok {
		var tool ollama.Tool
		if err := json.Unmarshal(raw, &tool); err != nil {
			return ollama.Tool{}, fmt.Errorf("%w: decode provider tool: %v", errInvalidTraceTool, err)
		}
		if tool.Type != "" && tool.Type != "function" {
			return ollama.Tool{}, fmt.Errorf("%w: unsupported type %q", errInvalidTraceTool, tool.Type)
		}
		if strings.TrimSpace(tool.Function.Name) == "" {
			return ollama.Tool{}, fmt.Errorf("%w: missing function.name", errInvalidTraceTool)
		}
		normalized, err := ollama.NewToolRaw(tool.Function.Name, tool.Function.Description, tool.Function.Parameters)
		if err != nil {
			return ollama.Tool{}, fmt.Errorf("%w: %v", errInvalidTraceTool, err)
		}
		return normalized, nil
	}

	if _, ok := fields["name"]; ok {
		var tool struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		if err := json.Unmarshal(raw, &tool); err != nil {
			return ollama.Tool{}, fmt.Errorf("%w: decode MCP tool: %v", errInvalidTraceTool, err)
		}
		if len(tool.InputSchema) == 0 {
			tool.InputSchema = fields["input_schema"]
		}
		if strings.TrimSpace(tool.Name) == "" {
			return ollama.Tool{}, fmt.Errorf("%w: missing name", errInvalidTraceTool)
		}
		normalized, err := ollama.NewToolRaw(tool.Name, tool.Description, tool.InputSchema)
		if err != nil {
			return ollama.Tool{}, fmt.Errorf("%w: %v", errInvalidTraceTool, err)
		}
		return normalized, nil
	}

	return ollama.Tool{}, fmt.Errorf("%w: expected provider tool or MCP tool shape", errInvalidTraceTool)
}

func assistantTurnFromMessage(msg ollama.ChatMessage) (Turn, error) {
	role := msg.Role
	if role == "" {
		role = "assistant"
	}

	toolCalls := make([]ToolCall, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		args, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			return Turn{}, fmt.Errorf("marshal tool call arguments for %q: %w", tc.Function.Name, err)
		}
		toolCalls = append(toolCalls, ToolCall{
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(args),
		})
	}

	return Turn{
		Role:      role,
		Content:   msg.Content,
		ToolCalls: toolCalls,
	}, nil
}

// newOllamaClient constructs an ollama.Client targeting the configured URL.
// Multi-provider routing (e.g. LM Studio at a different port) is a
// follow-up that will introduce a client factory keyed by target.Provider.
func newOllamaClient(baseURL string, opts ...ollama.Option) (*ollama.Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errEmptyBaseURL
	}
	clientOpts := append([]ollama.Option{ollama.WithBaseURL(baseURL)}, opts...)
	return ollama.NewClient(clientOpts...), nil
}

func sumInt64(xs []int64) int64 {
	var total int64
	for _, x := range xs {
		total += x
	}
	return total
}

func appendNotes(existing string, extras []string) string {
	if len(extras) == 0 {
		return existing
	}
	joined := strings.Join(extras, "; ")
	if existing == "" {
		return joined
	}
	return existing + "; " + joined
}

// thinkMarkupResidueNote returns a divergence note when a candidate's final
// answer still contains a "<think" marker after think extraction — for example
// an unclosed or malformed tag the default extractor's <think>...</think>
// regex can't match, or a backend that emits reasoning inline instead of
// separating it. The empty string means no residue. It is advisory: callers
// append it to Score.Notes so a reviewer can discount answers polluted by
// serving-stack reasoning formatting rather than model output.
func thinkMarkupResidueNote(finalAnswer string) string {
	if strings.Contains(finalAnswer, "<think") {
		return "candidate final answer retains <think> reasoning markup; serving backend did not separate reasoning"
	}
	return ""
}
