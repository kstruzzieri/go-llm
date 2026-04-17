package main

import (
	"context"
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
	Model     string
	TraceID   string
	Score     Score
	Err       error
	Transcript []Turn
}

// RunAll replays every trace against every model. Iterates models in the
// outer loop so a warm model stays warm across its traces.
func (r *Runner) RunAll(ctx context.Context, models []string, traces []Trace) ([]Result, error) {
	results := make([]Result, 0, len(models)*len(traces))
	for _, model := range models {
		client, err := newOllamaClient(model, r.OllamaURL)
		if err != nil {
			return nil, fmt.Errorf("client for %q: %w", model, err)
		}
		for _, trace := range traces {
			res := r.runOne(ctx, client, model, trace)
			results = append(results, res)
		}
	}
	return results, nil
}

func (r *Runner) runOne(ctx context.Context, client *ollama.Client, model string, trace Trace) Result {
	runCtx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	start := time.Now()
	transcript, err := replay(runCtx, client, model, trace)
	if err != nil {
		return Result{Model: model, TraceID: trace.ID, Err: err}
	}

	score, err := r.Scorer.Score(runCtx, trace, Result{
		Model:      model,
		TraceID:    trace.ID,
		Transcript: transcript,
	})
	if err != nil {
		return Result{Model: model, TraceID: trace.ID, Err: err, Transcript: transcript}
	}
	score.LatencyMs = time.Since(start).Milliseconds()

	return Result{
		Model:      model,
		TraceID:    trace.ID,
		Score:      score,
		Transcript: transcript,
	}
}

// replay sends the trace's turns to the model and captures the assistant's
// responses. This is a SKELETON — the tool-call loop is not yet wired up.
// Feeding tool results back to the model and extracting real ToolCalls
// from the Ollama response requires more plumbing than this scaffold.
func replay(ctx context.Context, client *ollama.Client, model string, trace Trace) ([]Turn, error) {
	messages := []ollama.ChatMessage{{Role: "system", Content: trace.System}}
	for _, t := range trace.Turns {
		if t.Role == "user" {
			messages = append(messages, ollama.ChatMessage{Role: "user", Content: t.Content})
			break // replay only first user turn for now; tool loop is TODO
		}
	}

	resp, err := client.Chat(ctx, ollama.ChatRequest{
		Model:    model,
		Messages: messages,
	})
	if err != nil {
		return nil, err
	}

	return []Turn{{Role: "assistant", Content: resp.Message.Content}}, nil
}

// newOllamaClient constructs an ollama.Client targeting the configured URL.
// The model argument is unused today but will matter when we route to
// different endpoints per provider (e.g. LM Studio at a different port).
func newOllamaClient(model, baseURL string) (*ollama.Client, error) {
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("empty model")
	}
	return ollama.NewClient(ollama.WithBaseURL(baseURL)), nil
}
