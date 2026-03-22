package ollama

import (
	"encoding/json"
	"strings"
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
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
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

func TestObjectParamsSerialization(t *testing.T) {
	params := ObjectParams(
		Param("city", ParamTypeString, "The city name"),
		Param("count", ParamTypeInteger, "Number of results"),
	).Required("city")

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want %q", schema["type"], "object")
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("properties is not a map")
	}
	if len(props) != 2 {
		t.Errorf("expected 2 properties, got %d", len(props))
	}

	cityProp, ok := props["city"].(map[string]any)
	if !ok {
		t.Fatal("city property missing or not a map")
	}
	if cityProp["type"] != "string" {
		t.Errorf("city type = %v, want %q", cityProp["type"], "string")
	}
	if cityProp["description"] != "The city name" {
		t.Errorf("city description = %v, want %q", cityProp["description"], "The city name")
	}

	req, ok := schema["required"].([]any)
	if !ok {
		t.Fatal("required is not an array")
	}
	if len(req) != 1 || req[0] != "city" {
		t.Errorf("required = %v, want [city]", req)
	}
}

func TestParamWithEnum(t *testing.T) {
	params := ObjectParams(
		Param("units", ParamTypeString, "Temperature unit").WithEnum("celsius", "fahrenheit"),
	)

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	props := schema["properties"].(map[string]any)
	unitsProp := props["units"].(map[string]any)

	enumVals, ok := unitsProp["enum"].([]any)
	if !ok {
		t.Fatal("enum field missing or not an array")
	}
	if len(enumVals) != 2 {
		t.Fatalf("expected 2 enum values, got %d", len(enumVals))
	}
	if enumVals[0] != "celsius" || enumVals[1] != "fahrenheit" {
		t.Errorf("enum = %v, want [celsius fahrenheit]", enumVals)
	}
}

func TestObjectParamsNoRequired(t *testing.T) {
	params := ObjectParams(
		Param("query", ParamTypeString, "Search query"),
	)

	data, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	if _, ok := raw["required"]; ok {
		t.Error("expected required field to be omitted when empty")
	}
}

func TestNewTool(t *testing.T) {
	tool := NewTool("get_weather", "Get weather for a city",
		ObjectParams(
			Param("city", ParamTypeString, "The city name"),
			Param("units", ParamTypeString, "Unit").WithEnum("celsius", "fahrenheit"),
		).Required("city"),
	)

	if tool.Type != "function" {
		t.Errorf("tool type = %q, want %q", tool.Type, "function")
	}
	if tool.Function.Name != "get_weather" {
		t.Errorf("function name = %q, want %q", tool.Function.Name, "get_weather")
	}
	if tool.Function.Description != "Get weather for a city" {
		t.Errorf("description = %q, want %q", tool.Function.Description, "Get weather for a city")
	}
	if tool.Function.Parameters == nil {
		t.Fatal("parameters is nil")
	}

	var schema map[string]any
	if err := json.Unmarshal(tool.Function.Parameters, &schema); err != nil {
		t.Fatalf("parameters is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want %q", schema["type"], "object")
	}
}

func TestNewToolRaw(t *testing.T) {
	rawSchema := json.RawMessage(`{"type":"object","properties":{"x":{"type":"number"}},"required":["x"]}`)
	tool, err := NewToolRaw("compute", "Run computation", rawSchema)
	if err != nil {
		t.Fatalf("NewToolRaw: %v", err)
	}

	if tool.Type != "function" {
		t.Errorf("tool type = %q, want %q", tool.Type, "function")
	}
	if tool.Function.Name != "compute" {
		t.Errorf("function name = %q, want %q", tool.Function.Name, "compute")
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded Tool
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if string(decoded.Function.Parameters) != string(rawSchema) {
		t.Errorf("schema not preserved:\ngot:  %s\nwant: %s", decoded.Function.Parameters, rawSchema)
	}
}

func TestNewToolRawInvalidJSON(t *testing.T) {
	_, err := NewToolRaw("bad", "desc", json.RawMessage(`{not valid`))
	if err == nil {
		t.Fatal("expected error for invalid JSON schema")
	}
	if !strings.Contains(err.Error(), "JSON object") {
		t.Errorf("error should mention JSON object: %v", err)
	}
}

func TestNewToolRawNonObject(t *testing.T) {
	cases := []struct {
		name   string
		schema string
	}{
		{"null", `null`},
		{"array", `[1, 2, 3]`},
		{"string", `"hello"`},
		{"number", `42`},
		{"boolean", `true`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewToolRaw("test", "desc", json.RawMessage(tc.schema))
			if err == nil {
				t.Errorf("expected error for non-object schema %s", tc.schema)
			}
		})
	}
}

func TestMustNewToolRawPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for invalid JSON schema")
		}
	}()
	MustNewToolRaw("bad", "desc", json.RawMessage(`{not valid`))
}

func TestToolResultMessage(t *testing.T) {
	msg := ToolResultMessage("get_weather", "11 degrees celsius")
	if msg.Role != "tool" {
		t.Errorf("role = %q, want %q", msg.Role, "tool")
	}
	if msg.Content != "11 degrees celsius" {
		t.Errorf("content = %q, want %q", msg.Content, "11 degrees celsius")
	}
	if msg.ToolName != "get_weather" {
		t.Errorf("tool_name = %q, want %q", msg.ToolName, "get_weather")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["role"] != "tool" {
		t.Error("JSON role != tool")
	}
	if raw["tool_name"] != "get_weather" {
		t.Error("JSON tool_name missing or wrong")
	}
	if _, ok := raw["tool_calls"]; ok {
		t.Error("expected tool_calls to be omitted for tool result message")
	}
}

func TestToolResultMessageWithIndex(t *testing.T) {
	msg := ToolResultMessageWithIndex("get_weather", "sunny", 0)
	if msg.Role != "tool" {
		t.Errorf("role = %q, want %q", msg.Role, "tool")
	}
	if msg.ToolCallIndex == nil || *msg.ToolCallIndex != 0 {
		t.Errorf("tool_call_index = %v, want pointer to 0", msg.ToolCallIndex)
	}

	// Index 0 must serialize (not be omitted by omitempty).
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["tool_call_index"]; !ok {
		t.Error("tool_call_index=0 must be present in JSON, not omitted")
	}

	// Original ToolResultMessage must NOT include tool_call_index.
	plain := ToolResultMessage("get_weather", "sunny")
	data2, _ := json.Marshal(plain)
	var raw2 map[string]any
	json.Unmarshal(data2, &raw2)
	if _, ok := raw2["tool_call_index"]; ok {
		t.Error("plain ToolResultMessage should not include tool_call_index")
	}
}

func TestToolCallArgumentTypes(t *testing.T) {
	input := `{
		"role": "assistant",
		"content": "",
		"tool_calls": [{
			"type": "function",
			"function": {
				"index": 0,
				"name": "process",
				"arguments": {
					"name": "test",
					"count": 42,
					"enabled": true,
					"ratio": 3.14,
					"tags": ["a", "b"],
					"meta": {"key": "val"}
				}
			}
		}]
	}`

	var msg ChatMessage
	if err := json.Unmarshal([]byte(input), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	args := msg.ToolCalls[0].Function.Arguments

	if args["name"] != "test" {
		t.Errorf("name = %v, want %q", args["name"], "test")
	}
	if args["count"] != float64(42) {
		t.Errorf("count = %v (%T), want float64(42)", args["count"], args["count"])
	}
	if args["enabled"] != true {
		t.Errorf("enabled = %v, want true", args["enabled"])
	}
	if args["ratio"] != 3.14 {
		t.Errorf("ratio = %v, want 3.14", args["ratio"])
	}
	tags, ok := args["tags"].([]any)
	if !ok || len(tags) != 2 {
		t.Errorf("tags = %v, want [a b]", args["tags"])
	}
	meta, ok := args["meta"].(map[string]any)
	if !ok || meta["key"] != "val" {
		t.Errorf("meta = %v, want {key: val}", args["meta"])
	}
}
