//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestReplayAttempt_FrozenResultNameMismatch(t *testing.T) {
	for _, tc := range []struct {
		name, candidate, result, code string
	}{
		{"fixture mismatch", "rag_search", "different_tool", attemptCodeInput},
		{"candidate mismatch", "other_tool", "rag_search", attemptCodeReplayDivergence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := loadFixtureTrace(t, fixtureToolRouteTrace)
			trace.Turns[2].Name = tc.result
			data, err := json.Marshal(trace)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "trace.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			srv := newCompatServer(t, compatExchange{Body: compatToolCall(tc.candidate, `{"query":"retry budget"}`, "")})
			_, rec := runAttempt(t, context.Background(), attemptTestOptions(t, srv.URL, path))
			assertAttemptError(t, rec, tc.code, attemptStageReplay, 1)
		})
	}
}

func TestFrozenToolResultNameMismatchPreservesLegacyError(t *testing.T) {
	trace := loadFixtureTrace(t, fixtureToolRouteTrace)
	trace.Turns[2].Name = "different_tool"
	calls := []ollama.ToolCall{{Function: ollama.ToolCallFunction{Name: "rag_search"}}}
	_, _, _, err := frozenToolResults("fixture", 1, trace.Turns[1], trace.Turns, 2, calls)
	const want = `trace "fixture" assistant turn 1 tool 0 result: got "different_tool", want "rag_search": candidate tool call does not match trace`
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if !errors.Is(err, errToolCallMismatch) {
		t.Fatalf("error no longer wraps errToolCallMismatch: %v", err)
	}
}
