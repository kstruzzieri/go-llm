// Package ollama provides an HTTP client for the Ollama REST API,
// supporting chat completions, text generation, embeddings, and model management.
package ollama

import "encoding/json"

// Tool defines a tool the model may call during chat.
type Tool struct {
	Type     string       `json:"type"`     // always "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// ToolCall represents a tool invocation returned by the model.
type ToolCall struct {
	Type     string           `json:"type"`     // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds the index, name, and parsed arguments of a tool call.
// Index identifies the call for parallel tool invocations, enabling correlation
// between tool calls and their results.
type ToolCallFunction struct {
	Index     int            `json:"index"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// ChatMessage represents a single message in a chat conversation.
type ChatMessage struct {
	Role          string     `json:"role"`                      // system, user, assistant, tool
	Content       string     `json:"content"`
	ToolCalls     []ToolCall `json:"tool_calls,omitempty"`      // present when assistant invokes tools
	ToolName      string     `json:"tool_name,omitempty"`       // set when role="tool" (result)
	ToolCallIndex *int       `json:"tool_call_index,omitempty"` // correlates result with parallel call
}

// ChatRequest is the request body for the /api/chat endpoint.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Options  *ModelOptions `json:"options,omitempty"`
	Tools    []Tool        `json:"tools,omitempty"` // available tools
}

// ChatResponse is the response from the /api/chat endpoint.
// During streaming, each chunk has Done=false until the final response.
type ChatResponse struct {
	Model         string      `json:"model"`
	Message       ChatMessage `json:"message"`
	Done          bool        `json:"done"`
	TotalDuration int64       `json:"total_duration,omitempty"` // nanoseconds
	EvalCount     int         `json:"eval_count,omitempty"`     // tokens generated
	EvalDuration  int64       `json:"eval_duration,omitempty"`  // nanoseconds for generation
}

// ModelOptions controls generation parameters sent to Ollama.
type ModelOptions struct {
	Temperature   float64  `json:"temperature,omitempty"`
	TopP          float64  `json:"top_p,omitempty"`
	NumPredict    int      `json:"num_predict,omitempty"`    // max tokens to generate
	NumCtx        int      `json:"num_ctx,omitempty"`        // context window size
	Stop          []string `json:"stop,omitempty"`
	RepeatPenalty float64  `json:"repeat_penalty,omitempty"`
}

// EmbedRequest is the request body for the /api/embed endpoint.
type EmbedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// EmbedResponse is the response from the /api/embed endpoint.
type EmbedResponse struct {
	Embeddings [][]float64 `json:"embeddings"`
}

// GenerateRequest is the request body for the /api/generate endpoint.
type GenerateRequest struct {
	Model   string        `json:"model"`
	Prompt  string        `json:"prompt"`
	Stream  bool          `json:"stream"`
	Options *ModelOptions `json:"options,omitempty"`
	System  string        `json:"system,omitempty"`
	Suffix  string        `json:"suffix,omitempty"` // for Fill-in-the-Middle
}

// GenerateResponse is the response from the /api/generate endpoint.
type GenerateResponse struct {
	Model         string `json:"model"`
	Response      string `json:"response"`
	Done          bool   `json:"done"`
	TotalDuration int64  `json:"total_duration,omitempty"`
	EvalCount     int    `json:"eval_count,omitempty"`
	EvalDuration  int64  `json:"eval_duration,omitempty"`
}

// ModelInfo holds metadata about an Ollama model.
type ModelInfo struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ParamSize  string `json:"parameter_size"`
	QuantLevel string `json:"quantization_level"`
}

// modelDetails is used when parsing /api/show responses.
type modelDetails struct {
	ParamSize  string `json:"parameter_size"`
	QuantLevel string `json:"quantization_level"`
}

// listModelsResponse wraps the /api/tags response.
type listModelsResponse struct {
	Models []listModelEntry `json:"models"`
}

// listModelEntry represents a single model in the /api/tags response.
type listModelEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// showModelResponse wraps the /api/show response.
type showModelResponse struct {
	Details modelDetails `json:"details"`
}

// pullModelRequest is the request body for /api/pull.
type pullModelRequest struct {
	Name   string `json:"name"`
	Stream bool   `json:"stream"`
}

// pullModelResponse is a streaming response from /api/pull.
type pullModelResponse struct {
	Status    string `json:"status"`
	Completed int64  `json:"completed,omitempty"`
	Total     int64  `json:"total,omitempty"`
	Done      bool   `json:"-"` // derived from status
}
