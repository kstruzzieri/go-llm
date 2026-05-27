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
