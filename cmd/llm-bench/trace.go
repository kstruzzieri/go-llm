package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Trace is a replayable conversation captured from a real MCP / chat session.
// See docs/llm/benchmark-plan.md for the format and capture strategy.
type Trace struct {
	ID         string            `json:"id"`
	Source     string            `json:"source"`
	CapturedAt time.Time         `json:"captured_at"`
	System     string            `json:"system"`
	Tools      []json.RawMessage `json:"tools"`
	Turns      []Turn            `json:"turns"`
	Golden     Golden            `json:"golden"`
}

// Turn represents a single role/content pair, optionally with tool calls or
// tool results. Preserved verbatim so the replay is deterministic.
type Turn struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// ToolCall is a minimal representation of a tool invocation.
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Golden is the rubric the replay is scored against.
type Golden struct {
	ToolCalls            []string `json:"tool_calls"`
	FinalAnswerCriteria  string   `json:"final_answer_criteria"`
	FinalAnswerSubstring string   `json:"final_answer_substring,omitempty"`
}

// loadTraces reads each path as a JSON Trace and returns the full set.
// Validates structural minimums up front so malformed traces surface at
// load time, not deep in the replay loop.
func loadTraces(paths []string) ([]Trace, error) {
	traces := make([]Trace, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", path, err)
		}
		var t Trace
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("parse %q: %w", path, err)
		}
		if err := validateTrace(t); err != nil {
			return nil, fmt.Errorf("trace %q (%s): %w", path, t.ID, err)
		}
		traces = append(traces, t)
	}
	return traces, nil
}

func validateTrace(t Trace) error {
	if t.ID == "" {
		return fmt.Errorf("missing id")
	}
	if t.System == "" {
		return errEmptySystem
	}
	if len(t.Turns) == 0 {
		return errNoTurns
	}
	return nil
}
