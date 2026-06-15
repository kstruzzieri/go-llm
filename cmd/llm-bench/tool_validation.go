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
//     or {"name": "...", "description": "...", "input_schema": {...}}
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

func scoreToolArguments(trace Trace, actual []Turn) (float64, bool, string, error) {
	actualCalls := assistantToolCalls(actual)
	schemas, err := toolSchemaByNameForCalls(trace.Tools, actualCalls)
	if err != nil {
		return 0, false, "", err
	}
	score, computed, notes := validateToolArguments(schemas, trace, actual)
	return score, computed, notes, nil
}

// toolSchemaByNameForCalls compiles only schemas for tools the candidate
// actually called. Full MCP snapshots can include unused schemas with external
// refs or other unsupported features; those should not invalidate scoring for
// unrelated calls.
func toolSchemaByNameForCalls(tools []json.RawMessage, calls []ToolCall) (map[string]*compiledToolSchema, error) {
	wanted := make(map[string]struct{})
	for _, call := range calls {
		name := strings.TrimSpace(call.Name)
		if name != "" {
			wanted[name] = struct{}{}
		}
	}
	out := make(map[string]*compiledToolSchema, len(wanted))
	if len(wanted) == 0 {
		return out, nil
	}

	for i, raw := range tools {
		name, schemaRaw, err := extractNameAndInputSchema(raw)
		if err != nil {
			continue
		}
		if _, ok := wanted[name]; !ok {
			continue
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
	schemaRaw := fields["inputSchema"]
	if len(schemaRaw) == 0 {
		schemaRaw = fields["input_schema"]
	}
	return strings.TrimSpace(name), schemaRaw, nil
}

// denyAllLoader refuses any external $ref. Tool schemas originate from
// MCP servers the user may not fully trust; allowing network resolution
// would turn capture into a confused-deputy SSRF gadget. jsonschema/v6
// already returns "no URLLoader set" by default, but the library's
// default behavior is documented as subject to change — wiring this
// loader explicitly pins the security posture.
type denyAllLoader struct{}

func (denyAllLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("external $ref %q not allowed", url)
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
	c.UseLoader(denyAllLoader{})
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

// validateToolArguments produces the ToolArgsValid score for one replay.
//
// The score is the fraction of candidate tool calls whose arguments
// validate against the schema declared in trace.Tools. The semantics
// table (see spec §4.3) collapses to:
//
//   - No calls expected, none made → (1.0, computed=true)
//   - Calls expected, none made     → (0.0, computed=false)
//   - Calls made, no schemas at all → (0.0, computed=false)
//   - Calls made, partial schema coverage → score over all calls (unknown counts as invalid), computed=true
//   - All calls pass                → (1.0, computed=true)
//
// The notes field is a "; "-joined list of human-readable diagnostics
// for individual call failures; on the vacuous-true and not-computed
// paths it explains why ToolArgsValid is what it is. Callers should
// treat notes as informational only — the load-bearing signals are
// (score, computed).
func validateToolArguments(schemas map[string]*compiledToolSchema, trace Trace, actual []Turn) (float64, bool, string) {
	actualCalls := assistantToolCalls(actual)
	if len(actualCalls) == 0 {
		if len(trace.Golden.ToolCalls) == 0 {
			return 1.0, true, "no tool calls expected or made"
		}
		return 0.0, false, "ToolArgsValid not computed (expected tool calls but replay made none)"
	}
	if len(schemas) == 0 {
		return 0.0, false, "ToolArgsValid not computed (trace has no tool schemas)"
	}

	var notes []string
	valid := 0
	for _, call := range actualCalls {
		schema, ok := schemas[call.Name]
		if !ok {
			notes = append(notes, fmt.Sprintf("no schema for %s", call.Name))
			continue
		}
		args, err := decodeArguments(call.Arguments)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s{unparseable args: %v}", call.Name, err))
			continue
		}
		if err := schema.Validate(args); err != nil {
			notes = append(notes, fmt.Sprintf("%s{%s}", call.Name, summarizeValidationError(err)))
			continue
		}
		valid++
	}
	score := float64(valid) / float64(len(actualCalls))
	return score, true, strings.Join(notes, "; ")
}

func assistantToolCalls(turns []Turn) []ToolCall {
	var out []ToolCall
	for _, t := range turns {
		if t.Role != "assistant" {
			continue
		}
		out = append(out, t.ToolCalls...)
	}
	return out
}

// restraintSignals reports whether tool-restraint was testable for this trace
// and, if so, whether the candidate held it (emitted no tool call) or diverged
// (emitted ≥1 tool call). Restraint is only testable when the golden expected NO
// tool call; a tool-route trace tests tool correctness, not restraint, and
// returns computed=false. On a golden-empty trace ANY assistant tool call is a
// divergence — equivalent to what replayWith flags, but recomputed from the
// transcript so the metric never depends on parsing Score.Notes.
func restraintSignals(trace Trace, transcript []Turn) (restraint float64, computed bool) {
	if len(trace.Golden.ToolCalls) != 0 {
		return 0, false
	}
	if len(assistantToolCalls(transcript)) > 0 {
		return 0, true
	}
	return 1, true
}

func decodeArguments(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// summarizeValidationError compresses a jsonschema/v6 ValidationError
// into a one-line note suitable for the Score.Notes field. The library
// returns multi-line tree output by default; we want something that
// reads well next to other "; "-joined notes.
func summarizeValidationError(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}
