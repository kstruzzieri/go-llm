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

// Sentinel errors so tests assert on identity rather than message text.
var (
	errNoUserTurn        = errors.New("trace has no user turn")
	errEmptyBaseURL      = errors.New("empty ollama base URL")
	errMissingGolden     = errors.New("scorer requires golden.final_answer_substring")
	errEmptySystem       = errors.New("trace has empty system prompt")
	errNoTurns           = errors.New("trace has no turns")
	errInvalidTraceTool  = errors.New("trace has invalid tool definition")
	errMissingToolResult = errors.New("trace is missing frozen tool result")
	errToolCallMismatch  = errors.New("candidate tool call does not match trace")
	errUnsupportedTurns  = errors.New("trace has unsupported extra turns")
	errUnsupportedProv   = errors.New("unsupported provider")
)

// Runner executes traces against a list of candidate models and collects
// per-trace, per-model Results.
type Runner struct {
	OllamaURL string
	Timeout   time.Duration
	Scorer    Scorer
}

// Result is a single (model, trace) evaluation.
type Result struct {
	Model      string
	TraceID    string
	Score      Score
	Err        error
	Transcript []Turn
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

		client, err := newOllamaClient(r.OllamaURL)
		if err != nil {
			for _, trace := range traces {
				results = append(results, Result{
					Model:   target.Display,
					TraceID: trace.ID,
					Err:     fmt.Errorf("client for %q: %w", target.Display, err),
				})
			}
			continue
		}

		for _, trace := range traces {
			if err := ctx.Err(); err != nil {
				return results, err
			}
			results = append(results, r.runOne(ctx, client, target, trace))
		}
	}
	return results, nil
}

func (r *Runner) runOne(ctx context.Context, client *ollama.Client, target ModelTarget, trace Trace) Result {
	runCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	replayStart := time.Now()
	transcript, err := replay(runCtx, client, target.Model, trace)
	replayMs := time.Since(replayStart).Milliseconds()
	if err != nil {
		return Result{Model: target.Display, TraceID: trace.ID, Err: err}
	}

	score, err := r.Scorer.Score(runCtx, trace, Result{
		Model:      target.Display,
		TraceID:    trace.ID,
		Transcript: transcript,
	})
	if err != nil {
		return Result{Model: target.Display, TraceID: trace.ID, Err: err, Transcript: transcript}
	}
	// Latency reflects replay only, not scorer work. When llm-judge lands,
	// the judge round-trip must not pollute the model-latency metric.
	score.LatencyMs = replayMs

	return Result{
		Model:      target.Display,
		TraceID:    trace.ID,
		Score:      score,
		Transcript: transcript,
	}
}

// replay sends user turns to the model, replaces scripted assistant turns with
// candidate responses, and feeds captured tool-result turns back after matching
// candidate tool calls. Tool results are frozen replay fixtures: the candidate
// chooses whether and which tool to call, but llm-bench never executes tools.
func replay(ctx context.Context, client *ollama.Client, model string, trace Trace) ([]Turn, error) {
	if !hasUserTurn(trace.Turns) {
		return nil, fmt.Errorf("trace %q: %w", trace.ID, errNoUserTurn)
	}

	messages := []ollama.ChatMessage{
		{Role: "system", Content: trace.System},
	}
	tools, err := decodeTraceTools(trace.Tools)
	if err != nil {
		return nil, fmt.Errorf("trace %q: %w", trace.ID, err)
	}

	var transcript []Turn
	for i := 0; i < len(trace.Turns); {
		turn := trace.Turns[i]
		if turn.Role != "user" {
			return nil, fmt.Errorf("trace %q turn %d role %q: %w", trace.ID, i, turn.Role, errUnsupportedTurns)
		}
		messages = append(messages, ollama.ChatMessage{Role: "user", Content: turn.Content})
		i++

		for {
			var expected Turn
			expectedIndex := -1
			if i < len(trace.Turns) && trace.Turns[i].Role == "assistant" {
				expected = trace.Turns[i]
				expectedIndex = i
				i++
			}

			msg, actualTurn, err := chatReplayTurn(ctx, client, model, messages, tools)
			if err != nil {
				return nil, err
			}
			messages = append(messages, msg)
			transcript = append(transcript, actualTurn)

			if len(msg.ToolCalls) == 0 {
				if len(expected.ToolCalls) > 0 {
					i = skipScriptedToolLoop(trace.Turns, i)
				}
				break
			}

			if len(expected.ToolCalls) == 0 {
				return nil, fmt.Errorf("trace %q turn %d: %w", trace.ID, i, errMissingToolResult)
			}

			toolMessages, toolTurns, next, err := frozenToolResults(trace.ID, expectedIndex, expected, trace.Turns, i, msg.ToolCalls)
			if err != nil {
				return nil, err
			}
			i = next
			messages = append(messages, toolMessages...)
			transcript = append(transcript, toolTurns...)
		}
	}

	return transcript, nil
}

func hasUserTurn(turns []Turn) bool {
	for _, turn := range turns {
		if turn.Role == "user" {
			return true
		}
	}
	return false
}

func chatReplayTurn(ctx context.Context, client *ollama.Client, model string, messages []ollama.ChatMessage, tools []ollama.Tool) (ollama.ChatMessage, Turn, error) {
	resp, err := client.Chat(ctx, ollama.ChatRequest{
		Model:     model,
		Messages:  messages,
		Tools:     tools,
		KeepAlive: benchKeepAlive,
	})
	if err != nil {
		return ollama.ChatMessage{}, Turn{}, err
	}

	msg := normalizeAssistantMessage(resp.Message)
	turn, err := assistantTurnFromMessage(msg)
	if err != nil {
		return ollama.ChatMessage{}, Turn{}, err
	}
	return msg, turn, nil
}

func normalizeAssistantMessage(msg ollama.ChatMessage) ollama.ChatMessage {
	if msg.Role == "" {
		msg.Role = "assistant"
	}
	return msg
}

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

		callID := call.ID
		if callID == "" {
			callID = result.ToolCallID
		}
		messages = append(messages, ollama.ChatMessage{
			Role:       "tool",
			Content:    result.Content,
			ToolName:   callName,
			ToolCallID: callID,
		})
		transcript = append(transcript, Turn{
			Role:       "tool",
			Content:    result.Content,
			Name:       callName,
			ToolCallID: callID,
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

func skipScriptedToolLoop(turns []Turn, start int) int {
	for start < len(turns) && turns[start].Role != "user" {
		start++
	}
	return start
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
func newOllamaClient(baseURL string) (*ollama.Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errEmptyBaseURL
	}
	return ollama.NewClient(ollama.WithBaseURL(baseURL)), nil
}
