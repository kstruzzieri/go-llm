package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// compiledToolSchema wraps the v6 schema so the validator can attach
// per-tool metadata in the future (currently a thin pass-through).
type compiledToolSchema struct {
	Name   string
	Schema *jsonschema.Schema
}

// Validate runs JSON Schema validation against the decoded arguments.
func (c *compiledToolSchema) Validate(args any) error {
	if c == nil || c.Schema == nil {
		return fmt.Errorf("nil schema")
	}
	return c.Schema.Validate(args)
}

// toolSchemaByName parses a trace.Tools slice into a name-keyed map of
// compiled JSON schemas. The two recognized shapes mirror
// declaredToolNames in trace.go:
//
//   - MCP-style: {"name": "...", "description": "...", "inputSchema": {...}}
//   - Provider/function-call style: {"type":"function","function":{"name":"...","parameters":{...}}}
//
// An entry with neither shape, an empty name, or an inputSchema that
// fails to compile yields an error wrapped with the offending index
// and (when known) the tool name.
func toolSchemaByName(tools []json.RawMessage) (map[string]*compiledToolSchema, error) {
	out := make(map[string]*compiledToolSchema, len(tools))
	for i, raw := range tools {
		name, schemaRaw, err := extractNameAndInputSchema(raw)
		if err != nil {
			return nil, fmt.Errorf("trace tool[%d]: %w", i, err)
		}
		if name == "" {
			return nil, fmt.Errorf("trace tool[%d]: missing name", i)
		}
		schema, err := compileSchema(name, schemaRaw)
		if err != nil {
			return nil, fmt.Errorf("trace tool[%d] %q: %w", i, name, err)
		}
		out[name] = &compiledToolSchema{Name: name, Schema: schema}
	}
	return out, nil
}

func extractNameAndInputSchema(raw json.RawMessage) (string, json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", nil, err
	}
	if fnRaw, ok := fields["function"]; ok {
		var inner struct {
			Name        string          `json:"name"`
			Parameters  json.RawMessage `json:"parameters"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		if err := json.Unmarshal(fnRaw, &inner); err != nil {
			return "", nil, fmt.Errorf("function: %w", err)
		}
		schemaRaw := inner.Parameters
		if len(schemaRaw) == 0 {
			schemaRaw = inner.InputSchema
		}
		return strings.TrimSpace(inner.Name), schemaRaw, nil
	}
	var name string
	if rawName, ok := fields["name"]; ok {
		if err := json.Unmarshal(rawName, &name); err != nil {
			return "", nil, fmt.Errorf("name: %w", err)
		}
	}
	return strings.TrimSpace(name), fields["inputSchema"], nil
}

func compileSchema(name string, raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		// A tool with no inputSchema accepts any arguments.
		raw = json.RawMessage(`{}`)
	}
	var schemaDoc any
	if err := json.Unmarshal(raw, &schemaDoc); err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	resource := fmt.Sprintf("mem://%s.json", schemaSafeName(name))
	if err := c.AddResource(resource, schemaDoc); err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	schema, err := c.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	return schema, nil
}

func schemaSafeName(name string) string {
	repl := strings.NewReplacer("/", "_", ":", "_", " ", "_")
	return repl.Replace(name)
}
