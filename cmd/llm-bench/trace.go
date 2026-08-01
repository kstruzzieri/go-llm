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

// validRestraintDifficulties is the closed set for Golden.Difficulty ("" allowed).
var validRestraintDifficulties = map[string]struct{}{
	"obvious": {}, "tempting": {}, "adversarial": {},
}

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
	// AssemblyEval is present only on assembly-corpus traces (#331); nil for
	// every other trace and omitted from their JSON.
	AssemblyEval *AssemblyEval `json:"assembly_eval,omitempty"`
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

// ToolCall is a minimal representation of a tool invocation. ID is set on
// prefilled-mode assembly traces (#331 slice 3c) so tool-result turns can
// reference the originating assistant call via ToolCallID; it is omitempty,
// so legacy traces and their hashes are unaffected.
type ToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Golden is the rubric the replay is scored against.
type Golden struct {
	ToolCalls            []string `json:"tool_calls"`
	FinalAnswerCriteria  string   `json:"final_answer_criteria"`
	FinalAnswerSubstring string   `json:"final_answer_substring,omitempty"`
	// Difficulty tiers a golden-empty restraint trace: "obvious" (no tool
	// plainly needed), "tempting" (an unneeded tool is offered), "adversarial"
	// (looks tool-needed but the baked context already suffices). Empty on
	// non-restraint or legacy traces. Single source of truth for restraint
	// stratification — reaches computeRestraintPairing via Artifact.Trace.
	Difficulty string `json:"difficulty,omitempty"`
	// RestraintRationale records why no tool call is correct (audit only).
	RestraintRationale string `json:"restraint_rationale,omitempty"`
	// FailureMode is a short tag of what the trace tests, e.g.
	// "context-already-answers", "tempting-search-tool" (audit only).
	FailureMode string `json:"failure_mode,omitempty"`
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
	if d := t.Golden.Difficulty; d != "" {
		if _, ok := validRestraintDifficulties[d]; !ok {
			return fmt.Errorf("golden.difficulty %q invalid (want obvious, tempting, adversarial, or empty)", d)
		}
	}
	if t.AssemblyEval != nil {
		ae := t.AssemblyEval
		if ae.PairID == "" {
			return fmt.Errorf("assembly_eval: blank pair_id")
		}
		switch ae.Mode {
		case AssemblyFlat, AssemblyProgressive:
			if len(ae.CandidateIDs) == 0 {
				return fmt.Errorf("assembly_eval: empty candidate_ids")
			}
			if ae.EstimatedPromptTokens <= 0 {
				return fmt.Errorf("assembly_eval: non-positive estimated_prompt_tokens")
			}
		case AssemblyLegacy, AssemblyMixed:
			if ae.Budget <= 0 {
				return fmt.Errorf("assembly_eval: %s arm requires positive budget", ae.Mode)
			}
			if ae.StateDigest == "" {
				return fmt.Errorf("assembly_eval: %s arm requires state_digest", ae.Mode)
			}
		case AssemblyTopline:
			// pair_id is the only requirement; topline arms are unpaired
			// descriptive ceilings with no budget or digest contract.
		default:
			return fmt.Errorf("assembly_eval: unknown mode %q", ae.Mode)
		}
		if prefilledAssemblyMode(t) {
			if err := validatePrefilledTurns(t.Turns); err != nil {
				return err
			}
		}
	}
	return nil
}

// prefilledAssemblyMode reports whether a trace carries a prefilled
// assembled history (#331 slice 3c legacy/mixed/topline arms). Prefilled
// traces are replayed with a single generation call over their verbatim
// Turns; every other trace (including 3a flat/progressive) keeps the legacy
// scripted-replacement replay and turn rules.
func prefilledAssemblyMode(t Trace) bool {
	if t.AssemblyEval == nil {
		return false
	}
	switch t.AssemblyEval.Mode {
	case AssemblyLegacy, AssemblyMixed, AssemblyTopline:
		return true
	}
	return false
}

// anyPrefilledAssemblyTrace reports whether any trace in the set is a
// prefilled assembly arm (legacy/mixed/topline).
func anyPrefilledAssemblyTrace(traces []Trace) bool {
	for _, t := range traces {
		if prefilledAssemblyMode(t) {
			return true
		}
	}
	return false
}

// validatePrefilledTurns enforces the prefilled-history turn rules: roles
// user/assistant/tool only; each tool turn answers exactly one earlier
// assistant tool call by ToolCallID; every declared assistant tool call is
// answered exactly once (IDs required non-empty); and the final turn is a
// user question with non-empty content (the single generation prompt the
// candidate answers).
func validatePrefilledTurns(turns []Turn) error {
	declared := map[string]struct{}{} // assistant tool-call IDs seen so far
	var declaredOrder []string
	answered := map[string]struct{}{}
	for i, turn := range turns {
		switch turn.Role {
		case "user":
		case "assistant":
			for j, call := range turn.ToolCalls {
				id := strings.TrimSpace(call.ID)
				if id == "" {
					// A blank-ID call could never be answered by any tool
					// turn, so it is the same unanswered-call hazard the
					// post-loop check below rejects.
					return fmt.Errorf("turn %d tool_call %d: prefilled assistant tool call requires non-empty id", i, j)
				}
				if _, ok := declared[id]; ok {
					return fmt.Errorf("turn %d tool_call %d: duplicate assistant tool-call id %q", i, j, id)
				}
				declared[id] = struct{}{}
				declaredOrder = append(declaredOrder, id)
			}
		case "tool":
			id := strings.TrimSpace(turn.ToolCallID)
			if id == "" {
				return fmt.Errorf("turn %d: prefilled tool turn requires non-empty tool_call_id", i)
			}
			if _, ok := declared[id]; !ok {
				return fmt.Errorf("turn %d: tool_call_id %q does not reference a preceding assistant tool call", i, id)
			}
			if _, ok := answered[id]; ok {
				return fmt.Errorf("turn %d: assistant tool call %q already answered", i, id)
			}
			answered[id] = struct{}{}
		default:
			return fmt.Errorf("turn %d role %q: prefilled traces allow only user, assistant, tool", i, turn.Role)
		}
	}
	// Every declared call must be answered: llama.cpp strict chat templates
	// reject histories with unanswered assistant tool calls at capture time,
	// and both 3b assemblers keep call/result chains atomic, so an assembled
	// State never legitimately contains one.
	for _, id := range declaredOrder {
		if _, ok := answered[id]; !ok {
			return fmt.Errorf("assistant tool call %q is never answered by a tool turn (assembled chains are atomic; built corpora always answer every declared call)", id)
		}
	}
	final := turns[len(turns)-1]
	if final.Role != "user" || strings.TrimSpace(final.Content) == "" {
		return fmt.Errorf("prefilled final turn must be role user with non-empty content, got role %q", final.Role)
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
