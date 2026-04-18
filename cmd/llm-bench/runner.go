package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
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
// outer loop so a warm model stays warm across its traces.
func (r *Runner) RunAll(ctx context.Context, targets []ModelTarget, traces []Trace) ([]Result, error) {
	results := make([]Result, 0, len(targets)*len(traces))
	for _, target := range targets {
		client, err := newOllamaClient(r.OllamaURL)
		if err != nil {
			return nil, fmt.Errorf("client for %q: %w", target.Display, err)
		}
		for _, trace := range traces {
			res := r.runOne(ctx, client, target, trace)
			results = append(results, res)
		}
	}
	return results, nil
}

func (r *Runner) runOne(ctx context.Context, client *ollama.Client, target ModelTarget, trace Trace) Result {
	runCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	start := time.Now()
	transcript, err := replay(runCtx, client, target.Model, trace)
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
	score.LatencyMs = time.Since(start).Milliseconds()

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
		return nil, fmt.Errorf("trace %q has no user turn", trace.ID)
	}
	if userTurns > 1 {
		return nil, fmt.Errorf("trace %q has %d user turns; multi-turn replay not yet supported", trace.ID, userTurns)
	}

	messages := []ollama.ChatMessage{
		{Role: "system", Content: trace.System},
		{Role: "user", Content: firstUser.Content},
	}

	resp, err := client.Chat(ctx, ollama.ChatRequest{
		Model:    model,
		Messages: messages,
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
		return nil, fmt.Errorf("empty ollama base URL")
	}
	return ollama.NewClient(ollama.WithBaseURL(baseURL)), nil
}
