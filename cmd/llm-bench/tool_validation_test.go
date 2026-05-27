package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolSchemaByNameParsesMCPShape(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{
			"name": "read_file",
			"description": "read file",
			"inputSchema": {
				"type": "object",
				"properties": {"path": {"type": "string"}},
				"required": ["path"]
			}
		}`),
	}
	schemas, err := toolSchemaByName(tools)
	if err != nil {
		t.Fatalf("toolSchemaByName: %v", err)
	}
	if _, ok := schemas["read_file"]; !ok {
		t.Fatalf("missing schema for read_file (got keys %v)", keys(schemas))
	}
	if err := schemas["read_file"].Validate(map[string]any{"path": "foo"}); err != nil {
		t.Fatalf("validate good input: %v", err)
	}
	if err := schemas["read_file"].Validate(map[string]any{}); err == nil {
		t.Fatalf("validate missing-required: want error, got nil")
	}
}

func TestToolSchemaByNameParsesMCPSnakeCaseInputSchema(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{
			"name": "read_file",
			"description": "read file",
			"input_schema": {
				"type": "object",
				"properties": {"path": {"type": "string"}},
				"required": ["path"]
			}
		}`),
	}
	schemas, err := toolSchemaByName(tools)
	if err != nil {
		t.Fatalf("toolSchemaByName: %v", err)
	}
	if _, ok := schemas["read_file"]; !ok {
		t.Fatalf("missing schema for read_file (got keys %v)", keys(schemas))
	}
	if err := schemas["read_file"].Validate(map[string]any{"path": "foo"}); err != nil {
		t.Fatalf("validate good input: %v", err)
	}
	if err := schemas["read_file"].Validate(map[string]any{}); err == nil {
		t.Fatalf("validate missing-required: want error, got nil")
	}
}

func TestToolSchemaByNameParsesProviderShape(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{
			"type": "function",
			"function": {
				"name": "list_dir",
				"description": "list",
				"parameters": {"type": "object", "properties": {"dir": {"type": "string"}}}
			}
		}`),
	}
	schemas, err := toolSchemaByName(tools)
	if err != nil {
		t.Fatalf("toolSchemaByName: %v", err)
	}
	if _, ok := schemas["list_dir"]; !ok {
		t.Fatalf("missing schema for list_dir (got keys %v)", keys(schemas))
	}
}

func TestToolSchemaByNameMissingNameIsError(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"description":"no name","inputSchema":{}}`)}
	if _, err := toolSchemaByName(tools); err == nil {
		t.Fatalf("want error for missing name, got nil")
	}
}

func TestToolSchemaByNameNilTools(t *testing.T) {
	schemas, err := toolSchemaByName(nil)
	if err != nil {
		t.Fatalf("toolSchemaByName(nil): %v", err)
	}
	if len(schemas) != 0 {
		t.Fatalf("want empty map, got %v", keys(schemas))
	}
}

func TestToolSchemaByNameInvalidSchemaIsError(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"name":"broken","inputSchema":{"$ref":"nowhere://x"}}`)}
	if _, err := toolSchemaByName(tools); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("want compile error citing 'broken', got %v", err)
	}
}

func keys(m map[string]*compiledToolSchema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestValidateToolArgumentsVacuousTrueWhenNoCallsExpectedOrMade(t *testing.T) {
	trace := Trace{Tools: nil, Golden: Golden{ToolCalls: nil}}
	score, computed, notes := validateToolArguments(nil, trace, []Turn{
		{Role: "assistant", Content: "answer"},
	})
	if score != 1.0 || !computed {
		t.Fatalf("score=%v computed=%v; want 1.0/true", score, computed)
	}
	if !strings.Contains(notes, "no tool calls expected or made") {
		t.Fatalf("notes=%q; want 'no tool calls expected or made'", notes)
	}
}

func TestValidateToolArgumentsNotComputedWhenGoldenExpectedButActualEmpty(t *testing.T) {
	trace := Trace{Tools: nil, Golden: Golden{ToolCalls: []string{"read_file"}}}
	score, computed, notes := validateToolArguments(nil, trace, []Turn{
		{Role: "assistant", Content: "no tool calls"},
	})
	if score != 0.0 || computed {
		t.Fatalf("score=%v computed=%v; want 0.0/false", score, computed)
	}
	if !strings.Contains(notes, "expected tool calls but replay made none") {
		t.Fatalf("notes=%q; want 'expected tool calls but replay made none'", notes)
	}
}

func TestValidateToolArgumentsNotComputedWhenTraceHasNoSchemas(t *testing.T) {
	trace := Trace{Tools: nil, Golden: Golden{ToolCalls: []string{"read_file"}}}
	actual := []Turn{{Role: "assistant", ToolCalls: []ToolCall{{Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}}}}
	score, computed, notes := validateToolArguments(nil, trace, actual)
	if score != 0.0 || computed {
		t.Fatalf("score=%v computed=%v; want 0.0/false", score, computed)
	}
	if !strings.Contains(notes, "trace has no tool schemas") {
		t.Fatalf("notes=%q; want 'trace has no tool schemas'", notes)
	}
}

func TestValidateToolArgumentsUnknownSchemaCountsAsInvalid(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"name":"read_file","inputSchema":{"type":"object"}}`)}
	schemas, err := toolSchemaByName(tools)
	if err != nil {
		t.Fatalf("toolSchemaByName: %v", err)
	}
	trace := Trace{Tools: tools, Golden: Golden{ToolCalls: []string{"read_file"}}}
	actual := []Turn{
		{Role: "assistant", ToolCalls: []ToolCall{
			{Name: "read_file", Arguments: json.RawMessage(`{}`)},
			{Name: "undeclared_tool", Arguments: json.RawMessage(`{}`)},
		}},
	}
	score, computed, notes := validateToolArguments(schemas, trace, actual)
	if !computed {
		t.Fatalf("computed=false; want true")
	}
	if score != 0.5 {
		t.Fatalf("score=%v; want 0.5 (1 valid of 2 calls)", score)
	}
	if !strings.Contains(notes, "no schema for undeclared_tool") {
		t.Fatalf("notes=%q; want 'no schema for undeclared_tool'", notes)
	}
}

func TestValidateToolArgumentsRejectsBadArgs(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{
		"name":"read_file",
		"inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
	}`)}
	schemas, _ := toolSchemaByName(tools)
	trace := Trace{Tools: tools, Golden: Golden{ToolCalls: []string{"read_file"}}}
	actual := []Turn{
		{Role: "assistant", ToolCalls: []ToolCall{
			{Name: "read_file", Arguments: json.RawMessage(`{}`)},
		}},
	}
	score, computed, notes := validateToolArguments(schemas, trace, actual)
	if !computed {
		t.Fatalf("computed=false; want true")
	}
	if score != 0.0 {
		t.Fatalf("score=%v; want 0.0 (0 valid of 1)", score)
	}
	if !strings.Contains(notes, "read_file") {
		t.Fatalf("notes=%q; want a note mentioning read_file", notes)
	}
}

func TestValidateToolArgumentsUsesMCPSnakeCaseInputSchema(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{
		"name":"read_file",
		"input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
	}`)}
	schemas, _ := toolSchemaByName(tools)
	trace := Trace{Tools: tools, Golden: Golden{ToolCalls: []string{"read_file"}}}
	actual := []Turn{
		{Role: "assistant", ToolCalls: []ToolCall{
			{Name: "read_file", Arguments: json.RawMessage(`{}`)},
		}},
	}
	score, computed, notes := validateToolArguments(schemas, trace, actual)
	if !computed {
		t.Fatalf("computed=false; want true")
	}
	if score != 0.0 {
		t.Fatalf("score=%v; want 0.0 (missing required arg)", score)
	}
	if !strings.Contains(notes, "read_file") {
		t.Fatalf("notes=%q; want a note mentioning read_file", notes)
	}
}

func TestValidateToolArgumentsAllValid(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{
		"name":"read_file",
		"inputSchema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}
	}`)}
	schemas, _ := toolSchemaByName(tools)
	trace := Trace{Tools: tools, Golden: Golden{ToolCalls: []string{"read_file"}}}
	actual := []Turn{
		{Role: "assistant", ToolCalls: []ToolCall{
			{Name: "read_file", Arguments: json.RawMessage(`{"path":"foo"}`)},
		}},
	}
	score, computed, notes := validateToolArguments(schemas, trace, actual)
	if score != 1.0 || !computed {
		t.Fatalf("score=%v computed=%v; want 1.0/true", score, computed)
	}
	_ = notes
}

func TestValidateToolArgumentsHandlesNilArguments(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"name":"ping","inputSchema":{"type":"object"}}`)}
	schemas, _ := toolSchemaByName(tools)
	trace := Trace{Tools: tools, Golden: Golden{ToolCalls: []string{"ping"}}}
	actual := []Turn{
		{Role: "assistant", ToolCalls: []ToolCall{
			{Name: "ping", Arguments: nil},
		}},
	}
	score, computed, _ := validateToolArguments(schemas, trace, actual)
	if score != 1.0 || !computed {
		t.Fatalf("score=%v computed=%v; want 1.0/true (nil args = {})", score, computed)
	}
}
