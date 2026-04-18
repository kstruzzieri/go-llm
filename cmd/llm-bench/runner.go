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
	errNoUserTurn       = errors.New("trace has no user turn")
	errMultiUserTurn    = errors.New("multi-turn replay not yet supported")
	errEmptyBaseURL     = errors.New("empty ollama base URL")
	errMissingGolden    = errors.New("scorer requires golden.final_answer_substring")
	errEmptySystem      = errors.New("trace has empty system prompt")
	errNoTurns          = errors.New("trace has no turns")
	errInvalidTraceTool = errors.New("trace has invalid tool definition")
	errUnsupportedProv  = errors.New("unsupported provider")
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

// replay sends the trace's turns to the model and captures the assistant's
// responses. This is a SKELETON — the tool-call loop is not yet wired up.
// Feeding tool results back to the model and extracting real ToolCalls
// from the Ollama response requires more plumbing than this scaffold.
//
// Until the tool loop lands, replay refuses multi-user-turn traces rather
// than silently scoring only the first turn, which would produce
// misleading aggregates.
func replay(ctx context.Context, client *ollama.Client, model string, trace Trace) ([]Turn, error) {
	var firstUser *Turn
	userTurns := 0
	for i := range trace.Turns {
		if trace.Turns[i].Role == "user" {
			userTurns++
			if firstUser == nil {
				firstUser = &trace.Turns[i]
			}
		}
	}
	if firstUser == nil {
		return nil, fmt.Errorf("trace %q: %w", trace.ID, errNoUserTurn)
	}
	if userTurns > 1 {
		return nil, fmt.Errorf("trace %q has %d user turns: %w", trace.ID, userTurns, errMultiUserTurn)
	}

	messages := []ollama.ChatMessage{
		{Role: "system", Content: trace.System},
		{Role: "user", Content: firstUser.Content},
	}
	tools, err := decodeTraceTools(trace.Tools)
	if err != nil {
		return nil, fmt.Errorf("trace %q: %w", trace.ID, err)
	}

	resp, err := client.Chat(ctx, ollama.ChatRequest{
		Model:     model,
		Messages:  messages,
		Tools:     tools,
		KeepAlive: benchKeepAlive,
	})
	if err != nil {
		return nil, err
	}

	turn, err := assistantTurnFromMessage(resp.Message)
	if err != nil {
		return nil, err
	}

	return []Turn{turn}, nil
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
