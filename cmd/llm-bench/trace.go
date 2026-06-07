package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// errToolNameNotDeclared signals a scripted assistant tool call whose
// name doesn't appear in trace.Tools — the candidate would never see a
// schema for the tool it's supposed to invoke.
var errToolNameNotDeclared = errors.New("trace tool call references undeclared tool name")

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
//
// Thinking is the reasoning text a candidate separated from its answer
// (captured on assistant transcript turns); it is kept distinct from Content so
// the scored answer stays clean. It is omitempty, so trace fixtures that never
// set it are unaffected.
type Turn struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	Thinking   string          `json:"thinking,omitempty"`
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
	if err := validateToolNamesDeclared(t); err != nil {
		return err
	}
	return nil
}

// validateToolNamesDeclared rejects a trace whose scripted assistant
// tool calls reference a tool name not declared in trace.Tools. Empty
// trace.Tools is a permitted shape (traces with no tools at all); the
// check only applies once any tool schema is declared.
func validateToolNamesDeclared(t Trace) error {
	if len(t.Tools) == 0 {
		return nil
	}
	declared, err := declaredToolNames(t.Tools)
	if err != nil {
		return err
	}
	for i, turn := range t.Turns {
		if turn.Role != "assistant" {
			continue
		}
		for j, call := range turn.ToolCalls {
			name := strings.TrimSpace(call.Name)
			if name == "" {
				continue
			}
			if _, ok := declared[name]; !ok {
				return fmt.Errorf("turn %d tool_call %d %q: %w", i, j, name, errToolNameNotDeclared)
			}
		}
	}
	return nil
}

// declaredToolNames extracts the tool names from trace.Tools using a
// minimal decode that tolerates both the provider-tool shape ("function"
// wrapper) and the MCP-style shape (top-level "name").
func declaredToolNames(rawTools []json.RawMessage) (map[string]struct{}, error) {
	names := make(map[string]struct{}, len(rawTools))
	for i, raw := range rawTools {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("trace tool[%d]: %w", i, err)
		}
		if fn, ok := fields["function"]; ok {
			var inner struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(fn, &inner); err != nil {
				return nil, fmt.Errorf("trace tool[%d].function: %w", i, err)
			}
			if n := strings.TrimSpace(inner.Name); n != "" {
				names[n] = struct{}{}
			}
			continue
		}
		if raw, ok := fields["name"]; ok {
			var n string
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, fmt.Errorf("trace tool[%d].name: %w", i, err)
			}
			if n = strings.TrimSpace(n); n != "" {
				names[n] = struct{}{}
			}
		}
	}
	return names, nil
}
