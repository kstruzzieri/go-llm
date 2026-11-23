package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func validRecallTrace() Trace {
	tool := func(n string) json.RawMessage {
		return json.RawMessage(`{"type":"function","function":{"name":"` + n + `","description":"d","parameters":{"type":"object"}}}`)
	}
	return Trace{
		ID: "recall-a-1", Source: "recall", System: "sys",
		Tools: []json.RawMessage{tool("agent_memory_search"), tool("read_file")},
		Turns: []Turn{
			{Role: "user", Content: "Which retry limit applies?"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "agent_memory_search", Arguments: json.RawMessage(`{"query":"retry"}`)}}},
			{Role: "tool", ToolCallID: "call_1", Name: "agent_memory_search", Content: "record"},
		},
		Recall: &RecallEval{
			FamilyID: "f1", Template: "t", TaskStyle: "s", Split: "pilot", Trace: "A", Arm: "1",
			Cases: []string{"S1", "S2"}, NamedFile: "retry.go",
			Value: RecallValue{Type: "number", Identifier: "maxRetries", Lesson: "5", Changed: "7"},
		},
	}
}

func TestValidateRecallTrace(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Trace)
		want   string // empty = valid
	}{
		{"valid trace A", func(*Trace) {}, ""},
		{"valid S7", func(tr *Trace) { tr.Recall.Arm = "2"; tr.Recall.Cases = []string{"S7"} }, ""},
		{"valid trace B S1 arm 2", func(tr *Trace) { tr.Recall.Trace = "B"; tr.Recall.Arm = "2"; tr.Recall.Cases = []string{"S1"} }, ""},
		{"valid trace B S2 arm P", func(tr *Trace) { tr.Recall.Trace = "B"; tr.Recall.Arm = "P"; tr.Recall.Cases = []string{"S2"} }, ""},
		{"valid repeat", func(tr *Trace) {
			tr.Recall.Trace = "B"
			tr.Recall.Arm = "1"
			tr.Recall.Cases = []string{"S2"}
			tr.Recall.Repeat = true
		}, ""},
		{"beside injection", func(tr *Trace) { tr.Injection = &InjectionEval{} }, "carries"},
		{"beside assembly", func(tr *Trace) { tr.AssemblyEval = &AssemblyEval{} }, "carries"},
		{"beside benign", func(tr *Trace) { tr.Benign = &BenignEval{} }, "carries"},
		{"last turn not tool", func(tr *Trace) { tr.Turns = append(tr.Turns, Turn{Role: "assistant", Content: "x"}) }, "last turn"},
		{"unknown arm", func(tr *Trace) { tr.Recall.Arm = "3" }, "arm"},
		{"unknown trace", func(tr *Trace) { tr.Recall.Trace = "C" }, "trace"},
		{"unknown split", func(tr *Trace) { tr.Recall.Split = "x" }, "split"},
		{"unknown value type", func(tr *Trace) { tr.Recall.Value.Type = "x" }, "value.type"},
		{"empty lesson", func(tr *Trace) { tr.Recall.Value.Lesson = "" }, "non-empty"},
		{"empty identifier", func(tr *Trace) { tr.Recall.Value.Identifier = "" }, "non-empty"},
		{"changed equals lesson", func(tr *Trace) { tr.Recall.Value.Changed = "5" }, "differ"},
		{"S7 on arm 1", func(tr *Trace) { tr.Recall.Cases = []string{"S7"} }, "cases"},
		{"A arm 0 one case", func(tr *Trace) { tr.Recall.Arm = "0"; tr.Recall.Cases = []string{"S1"} }, "cases"},
		{"B both cases", func(tr *Trace) { tr.Recall.Trace = "B" }, "cases"},
		{"B arm 0", func(tr *Trace) { tr.Recall.Trace = "B"; tr.Recall.Arm = "0"; tr.Recall.Cases = []string{"S1"} }, "arm"},
		{"B S1 arm P", func(tr *Trace) { tr.Recall.Trace = "B"; tr.Recall.Arm = "P"; tr.Recall.Cases = []string{"S1"} }, "arm"},
		{"A arm P", func(tr *Trace) { tr.Recall.Arm = "P" }, "arm"},
		{"repeat on A", func(tr *Trace) { tr.Recall.Repeat = true }, "repeat"},
		{"repeat on B arm 2", func(tr *Trace) {
			tr.Recall.Trace = "B"
			tr.Recall.Arm = "2"
			tr.Recall.Cases = []string{"S2"}
			tr.Recall.Repeat = true
		}, "repeat"},
		{"repeat on B S1", func(tr *Trace) {
			tr.Recall.Trace = "B"
			tr.Recall.Cases = []string{"S1"}
			tr.Recall.Repeat = true
		}, "repeat"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := validRecallTrace()
			tc.mutate(&tr)
			err := validateTrace(tr)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want containing %q", err, tc.want)
			}
		})
	}
}

func TestReplayWithRecallSendsToolsAndKeepsCalls(t *testing.T) {
	c := &fakeInjectionClient{resp: candidateChatResponse{Message: ollama.ChatMessage{Role: "assistant", ToolCalls: []ollama.ToolCall{{
		Function: ollama.ToolCallFunction{Name: "read_file", Arguments: map[string]any{"path": "retry.go"}},
	}}}}}
	out, err := replayWith(context.Background(), c, "m", validRecallTrace(), replayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.got.Tools) == 0 || c.got.Messages[len(c.got.Messages)-1].Role != "tool" {
		t.Fatalf("wire: %d tools, last role %q", len(c.got.Tools), c.got.Messages[len(c.got.Messages)-1].Role)
	}
	if len(out.Transcript) != 1 || len(out.Transcript[0].ToolCalls) != 1 {
		t.Fatalf("transcript %+v; want one turn with one call", out.Transcript)
	}
}

func TestCaptureProvenanceIncludesRecall(t *testing.T) {
	if captureProvenance(validRecallTrace(), Result{}, 0, map[string]int{}, map[string]string{}, map[string]string{}) == nil {
		t.Fatal("recall trace got no capture provenance")
	}
}

func TestInjectionTurnsUnchanged(t *testing.T) {
	bad := map[string][]Turn{
		"short":      {{Role: "user", Content: "x"}},
		"last":       append(validInjectionTrace().Turns, Turn{Role: "assistant", Content: "d"}),
		"unanswered": {validInjectionTrace().Turns[0], {Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "read_file"}}}, {Role: "tool", ToolCallID: "zz"}},
	}
	for name, turns := range bad {
		t.Run(name, func(t *testing.T) {
			err := validateInjectionTurns(turns)
			if err == nil || !errors.Is(err, errInjection) || !strings.HasPrefix(err.Error(), "injection: ") {
				t.Fatalf("err = %v; want injection-wrapped", err)
			}
		})
	}
	if err := validateInjectionTurns(validInjectionTrace().Turns); err != nil {
		t.Fatal(err)
	}
}
