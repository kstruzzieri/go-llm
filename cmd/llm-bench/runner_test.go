package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

func TestAssistantTurnFromMessagePreservesToolCalls(t *testing.T) {
	msg := ollama.ChatMessage{
		Role:    "assistant",
		Content: "Calling a tool",
		ToolCalls: []ollama.ToolCall{
			{
				Type: "function",
				Function: ollama.ToolCallFunction{
					Name: "read_file",
					Arguments: map[string]any{
						"path": "provider/router.go",
					},
				},
			},
		},
	}

	turn, err := assistantTurnFromMessage(msg)
	if err != nil {
		t.Fatalf("assistantTurnFromMessage() error: %v", err)
	}

	if turn.Role != "assistant" {
		t.Fatalf("Role = %q, want assistant", turn.Role)
	}
	if got := extractToolNames([]Turn{turn}); !reflect.DeepEqual(got, []string{"read_file"}) {
		t.Fatalf("extractToolNames() = %v, want [read_file]", got)
	}

	var args map[string]any
	if err := json.Unmarshal(turn.ToolCalls[0].Arguments, &args); err != nil {
		t.Fatalf("unmarshal tool call args: %v", err)
	}
	if got := args["path"]; got != "provider/router.go" {
		t.Fatalf("tool arg path = %v, want provider/router.go", got)
	}
}
