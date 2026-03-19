package ollama

import (
	"encoding/json"
	"testing"
)

func TestChatRequestToolsSerialization(t *testing.T) {
	req := ChatRequest{
		Model:    "qwen3.5:27b",
		Messages: []ChatMessage{{Role: "user", Content: "What is the weather?"}},
		Tools: []Tool{
			{
				Type: "function",
				Function: ToolFunction{
					Name:        "get_weather",
					Description: "Get weather for a city",
					Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
				},
			},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ChatRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(decoded.Tools))
	}
	if decoded.Tools[0].Type != "function" {
		t.Errorf("tool type = %q, want %q", decoded.Tools[0].Type, "function")
	}
	if decoded.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tool name = %q, want %q", decoded.Tools[0].Function.Name, "get_weather")
	}
}

func TestChatRequestNoToolsOmitted(t *testing.T) {
	req := ChatRequest{
		Model:    "test-model",
		Messages: []ChatMessage{{Role: "user", Content: "Hi"}},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	if _, ok := raw["tools"]; ok {
		t.Error("expected tools field to be omitted when empty")
	}
}

func TestToolCallDeserialization(t *testing.T) {
	input := `{
		"role": "assistant",
		"content": "",
		"tool_calls": [{
			"type": "function",
			"function": {
				"index": 0,
				"name": "get_weather",
				"arguments": {"city": "Tokyo", "units": "celsius"}
			}
		}]
	}`

	var msg ChatMessage
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.Type != "function" {
		t.Errorf("tool call type = %q, want %q", tc.Type, "function")
	}
	if tc.Function.Index != 0 {
		t.Errorf("tool call index = %d, want 0", tc.Function.Index)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("tool call name = %q, want %q", tc.Function.Name, "get_weather")
	}
	city, ok := tc.Function.Arguments["city"]
	if !ok || city != "Tokyo" {
		t.Errorf("arguments[city] = %v, want %q", city, "Tokyo")
	}
}

func TestMultipleToolCallDeserialization(t *testing.T) {
	input := `{
		"role": "assistant",
		"content": "",
		"tool_calls": [
			{"type": "function", "function": {"index": 0, "name": "get_weather", "arguments": {"city": "Tokyo"}}},
			{"type": "function", "function": {"index": 1, "name": "get_weather", "arguments": {"city": "London"}}}
		]
	}`

	var msg ChatMessage
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Function.Index != 0 {
		t.Errorf("first call index = %d, want 0", msg.ToolCalls[0].Function.Index)
	}
	if msg.ToolCalls[1].Function.Index != 1 {
		t.Errorf("second call index = %d, want 1", msg.ToolCalls[1].Function.Index)
	}
	if msg.ToolCalls[1].Function.Arguments["city"] != "London" {
		t.Errorf("second call city = %v, want %q", msg.ToolCalls[1].Function.Arguments["city"], "London")
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	input := `{
		"role": "assistant",
		"content": "",
		"tool_calls": [{
			"type": "function",
			"function": {"index": 0, "name": "get_weather", "arguments": {"city": "Tokyo"}}
		}]
	}`

	var msg ChatMessage
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var roundTripped ChatMessage
	if err := json.Unmarshal(data, &roundTripped); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}
	if len(roundTripped.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call after round-trip, got %d", len(roundTripped.ToolCalls))
	}
	tc := roundTripped.ToolCalls[0]
	if tc.Type != "function" {
		t.Errorf("round-trip type = %q, want %q", tc.Type, "function")
	}
	if tc.Function.Index != 0 {
		t.Errorf("round-trip index = %d, want 0", tc.Function.Index)
	}
	if tc.Function.Name != "get_weather" {
		t.Errorf("round-trip name = %q, want %q", tc.Function.Name, "get_weather")
	}
}
