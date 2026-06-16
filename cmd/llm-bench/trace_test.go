package main

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestValidateTrace(t *testing.T) {
	tests := []struct {
		name    string
		trace   Trace
		wantErr error
	}{
		{
			name:    "ok",
			trace:   Trace{ID: "t1", System: "sys", Turns: []Turn{{Role: "user", Content: "q"}}},
			wantErr: nil,
		},
		{
			name:    "missing id",
			trace:   Trace{System: "sys", Turns: []Turn{{Role: "user"}}},
			wantErr: nil, // validateTrace returns fmt.Errorf, not a sentinel, for missing id — covered by substring
		},
		{
			name:    "empty system",
			trace:   Trace{ID: "t2", Turns: []Turn{{Role: "user"}}},
			wantErr: errEmptySystem,
		},
		{
			name:    "no turns",
			trace:   Trace{ID: "t3", System: "sys"},
			wantErr: errNoTurns,
		},
		{
			name: "tool name not declared",
			trace: Trace{
				ID:     "t4",
				System: "sys",
				Tools: []json.RawMessage{
					json.RawMessage(`{"name":"read_file","inputSchema":{"type":"object"}}`),
				},
				Turns: []Turn{
					{Role: "user", Content: "q"},
					{Role: "assistant", ToolCalls: []ToolCall{{Name: "write_file"}}},
				},
			},
			wantErr: errToolNameNotDeclared,
		},
		{
			name: "tool name declared via provider shape",
			trace: Trace{
				ID:     "t5",
				System: "sys",
				Tools: []json.RawMessage{
					json.RawMessage(`{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}`),
				},
				Turns: []Turn{
					{Role: "user", Content: "q"},
					{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file"}}},
				},
			},
			wantErr: nil,
		},
		{
			name: "no tools declared bypasses cross-check",
			trace: Trace{
				ID:     "t6",
				System: "sys",
				Turns: []Turn{
					{Role: "user", Content: "q"},
					{Role: "assistant", ToolCalls: []ToolCall{{Name: "anything"}}},
				},
			},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTrace(tt.trace)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			// nil wantErr cases: ok case must pass, missing-id case must error.
			if tt.name == "ok" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.name == "missing id" && err == nil {
				t.Fatal("expected error for missing id, got nil")
			}
		})
	}
}

func TestGoldenAuditFieldsParse(t *testing.T) {
	const raw = `{
	  "id":"conversation-rw-x","source":"s","system":"ctx",
	  "turns":[{"role":"user","content":"q"}],
	  "golden":{
	    "tool_calls":[],
	    "final_answer_criteria":"answer from context",
	    "difficulty":"adversarial",
	    "restraint_rationale":"context already contains the answer",
	    "failure_mode":"context-already-answers"
	  }
	}`
	var tr Trace
	if err := json.Unmarshal([]byte(raw), &tr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tr.Golden.Difficulty != "adversarial" {
		t.Errorf("Difficulty = %q, want adversarial", tr.Golden.Difficulty)
	}
	if tr.Golden.RestraintRationale == "" || tr.Golden.FailureMode == "" {
		t.Errorf("rationale/failure_mode not parsed: %+v", tr.Golden)
	}
}

func TestGoldenAuditFieldsBackwardCompat(t *testing.T) {
	const raw = `{"tool_calls":[],"final_answer_criteria":"x"}`
	var g Golden
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if g.Difficulty != "" || g.RestraintRationale != "" || g.FailureMode != "" {
		t.Errorf("legacy golden gained non-empty audit fields: %+v", g)
	}
}

