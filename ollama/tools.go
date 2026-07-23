package ollama

import (
	"encoding/json"
	"fmt"
)

// JSON Schema type constants for use with Param.
const (
	ParamTypeString  = "string"
	ParamTypeNumber  = "number"
	ParamTypeInteger = "integer"
	ParamTypeBoolean = "boolean"
	ParamTypeArray   = "array"
	ParamTypeObject  = "object"
)

// ParamDef describes a single property in a tool's parameter schema.
type ParamDef struct {
	Name        string
	Type        string // Use ParamType* constants
	Description string
	Enum        []string // optional: constrained values
}

// Param creates a ParamDef.
func Param(name, typ, description string) ParamDef {
	return ParamDef{Name: name, Type: typ, Description: description}
}

// WithEnum sets allowed values for this parameter. Returns ParamDef for chaining.
func (p ParamDef) WithEnum(values ...string) ParamDef {
	p.Enum = values
	return p
}

// ToolParams holds the built schema and tracks required fields.
type ToolParams struct {
	params   []ParamDef
	required []string
}

// ObjectParams builds a JSON Schema object type from parameter definitions.
func ObjectParams(params ...ParamDef) ToolParams {
	return ToolParams{params: params}
}

// Required marks parameter names as required. Returns ToolParams for chaining.
func (tp ToolParams) Required(names ...string) ToolParams {
	tp.required = append(tp.required, names...)
	return tp
}

// MarshalJSON produces the JSON Schema for this parameter set.
// The error return satisfies the json.Marshaler interface; in practice
// marshaling cannot fail because all inputs are Go strings and string slices.
func (tp ToolParams) MarshalJSON() ([]byte, error) {
	properties := make(map[string]any, len(tp.params))
	for _, p := range tp.params {
		prop := map[string]any{
			"type":        p.Type,
			"description": p.Description,
		}
		if len(p.Enum) > 0 {
			prop["enum"] = p.Enum
		}
		properties[p.Name] = prop
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(tp.required) > 0 {
		schema["required"] = tp.required
	}
	return json.Marshal(schema)
}

// NewTool creates a Tool with builder-generated parameters.
// Panics if params cannot be marshaled to JSON (programming error).
func NewTool(name, description string, params ToolParams) Tool {
	schema, err := json.Marshal(params)
	if err != nil {
		panic(fmt.Sprintf("ollama: marshal tool params: %v", err))
	}
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  schema,
		},
	}
}

// NewToolRaw creates a Tool with a raw JSON Schema — for MCP passthrough.
// Returns an error if schema is not a valid JSON object.
func NewToolRaw(name, description string, schema json.RawMessage) (Tool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(schema, &obj); err != nil || obj == nil {
		return Tool{}, fmt.Errorf("ollama: NewToolRaw %q: schema must be a JSON object", name)
	}
	return Tool{
		Type: "function",
		Function: ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  schema,
		},
	}, nil
}

// MustNewToolRaw is like NewToolRaw but panics on error.
// Use for hardcoded schemas known to be valid at compile time.
func MustNewToolRaw(name, description string, schema json.RawMessage) Tool {
	t, err := NewToolRaw(name, description, schema)
	if err != nil {
		panic(err)
	}
	return t
}

// ToolResultMessage creates a ChatMessage for feeding a tool's result back to the model.
// For parallel tool calls, use ToolResultMessageFor to correlate each result
// with its originating call via tool_call_id.
func ToolResultMessage(toolName, content string) ChatMessage {
	return ChatMessage{Role: "tool", Content: content, ToolName: toolName}
}

// ToolResultMessageFor creates a ChatMessage for a tool result that correlates
// with a specific tool call via its ID (from ToolCall.ID). Use this when the model
// invokes the same tool multiple times in one turn and results must be unambiguous.
func ToolResultMessageFor(call ToolCall, content string) ChatMessage {
	return ChatMessage{
		Role:       "tool",
		Content:    content,
		ToolName:   call.Function.Name,
		ToolCallID: call.ID,
	}
}
