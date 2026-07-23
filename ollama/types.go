// Package ollama provides an HTTP client for the Ollama REST API,
// supporting chat completions, text generation, embeddings, and model management.
package ollama

import (
	"encoding/json"
	"fmt"
)

// Tool defines a tool the model may call during chat.
type Tool struct {
	Type     string       `json:"type"` // always "function"
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
	ID       string           `json:"id,omitempty"` // unique call ID for result correlation
	Type     string           `json:"type"`         // "function"
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
	Role       string     `json:"role"` // system, user, assistant, tool
	Content    string     `json:"content"`
	Thinking   string     `json:"thinking,omitempty"`     // reasoning text some models emit separately from content in Ollama's thinking mode
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // present when assistant invokes tools
	ToolName   string     `json:"tool_name,omitempty"`    // set when role="tool" (result)
	ToolCallID string     `json:"tool_call_id,omitempty"` // correlates result with originating call
}

// ChatRequest is the request body for the /api/chat endpoint.
type ChatRequest struct {
	Model     string        `json:"model"`
	Messages  []ChatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	Format    string        `json:"format,omitempty"` // e.g. "json" for structured output
	Think     *bool         `json:"think,omitempty"`  // controls reasoning models that support Ollama's thinking mode
	Options   *ModelOptions `json:"options,omitempty"`
	Tools     []Tool        `json:"tools,omitempty"`      // available tools
	KeepAlive string        `json:"keep_alive,omitempty"` // e.g. "5m", "30m", "-1" (keep forever); empty = Ollama default
}

// ChatResponse is the response from the /api/chat endpoint.
// During streaming, each chunk has Done=false until the final response.
type ChatResponse struct {
	Model              string      `json:"model"`
	Message            ChatMessage `json:"message"`
	Done               bool        `json:"done"`
	TotalDuration      int64       `json:"total_duration,omitempty"`       // nanoseconds
	EvalCount          int         `json:"eval_count,omitempty"`           // tokens generated
	EvalDuration       int64       `json:"eval_duration,omitempty"`        // nanoseconds for generation
	LoadDuration       int64       `json:"load_duration,omitempty"`        // nanoseconds to load model
	PromptEvalCount    int         `json:"prompt_eval_count,omitempty"`    // tokens in prompt
	PromptEvalDuration int64       `json:"prompt_eval_duration,omitempty"` // nanoseconds for prompt eval
}

// ModelOptions controls generation parameters sent to Ollama.
type ModelOptions struct {
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	TopK          *int     `json:"top_k,omitempty"`
	NumPredict    int      `json:"num_predict,omitempty"` // max tokens to generate
	NumCtx        int      `json:"num_ctx,omitempty"`     // context window size
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
// Capabilities may be nil when the model or Ollama version does not report capabilities.
type ModelInfo struct {
	Name         string   `json:"name"`
	Size         int64    `json:"size"`
	ParamSize    string   `json:"parameter_size"`
	QuantLevel   string   `json:"quantization_level"`
	Digest       string   `json:"digest"`       // model version hash from /api/show
	Family       string   `json:"family"`       // model family (e.g. "qwen2", "llama")
	Template     string   `json:"template"`     // prompt template; non-empty for chat models
	Capabilities []string `json:"capabilities"` // e.g. ["completion","tools"]; nil if not reported
}

// modelDetails is used when parsing /api/show responses.
type modelDetails struct {
	ParamSize  string `json:"parameter_size"`
	QuantLevel string `json:"quantization_level"`
	Family     string `json:"family"`
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
	Details      modelDetails `json:"details"`
	Digest       string       `json:"digest"`
	Template     string       `json:"template"`
	Capabilities []string     `json:"capabilities"`
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

// APIError represents a non-2xx response from the Ollama API.
// Callers can use errors.As to extract status codes for decision logic.
type APIError struct {
	// StatusCode is the HTTP status code from the Ollama API response.
	StatusCode int
	// Message is the parsed error message from the Ollama JSON response,
	// or the raw response body if JSON parsing fails.
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("ollama: HTTP %d: %s", e.StatusCode, e.Message)
}

// HTTPStatusCode exposes the response status code for router classification
// without requiring the routing layer to depend on Ollama-specific types.
func (e *APIError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	return e.StatusCode
}
