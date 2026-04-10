// Package provider defines the provider-agnostic abstraction layer for LLM
// backends. It contains all shared types, interfaces, and capability flags
// that allow consumers to work with different model providers (Ollama, OpenAI,
// Anthropic, etc.) through a single unified API.
package provider

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Capability bitmask
// ---------------------------------------------------------------------------

// Capability is a bitmask describing what a provider or model supports.
// Use bitwise OR to combine capabilities and the Has method to test them.
type Capability uint32

const (
	// CapChat indicates the provider supports multi-turn chat completions.
	CapChat Capability = 1 << iota
	// CapGenerate indicates the provider supports raw text generation.
	CapGenerate
	// CapEmbed indicates the provider supports embedding generation.
	CapEmbed
	// CapStream indicates the provider supports streaming responses.
	CapStream
	// CapToolCall indicates the provider supports tool/function calling.
	CapToolCall
	// CapThinking indicates the provider supports extended thinking mode.
	CapThinking
)

// capNames maps each single-bit capability to its string representation.
var capNames = []struct {
	bit  Capability
	name string
}{
	{CapChat, "chat"},
	{CapGenerate, "generate"},
	{CapEmbed, "embed"},
	{CapStream, "stream"},
	{CapToolCall, "tool_call"},
	{CapThinking, "thinking"},
}

// Has reports whether c includes every bit in flag.
func (c Capability) Has(flag Capability) bool {
	return c&flag == flag
}

// String returns a human-readable representation such as "chat|stream".
// When no bits are set it returns "none".
func (c Capability) String() string {
	if c == 0 {
		return "none"
	}
	var parts []string
	for _, cn := range capNames {
		if c.Has(cn.bit) {
			parts = append(parts, cn.name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// ---------------------------------------------------------------------------
// ModelKey
// ---------------------------------------------------------------------------

// ModelKey uniquely identifies a model by its provider and model name.
type ModelKey struct {
	// Provider is the backend name (e.g. "ollama", "openai").
	Provider string
	// Model is the model identifier (e.g. "qwen3:8b", "gpt-4o").
	Model string
}

// String returns the key in "provider/model" format.
func (k ModelKey) String() string {
	return k.Provider + "/" + k.Model
}

// ---------------------------------------------------------------------------
// Provider interface
// ---------------------------------------------------------------------------

// Provider is the core abstraction for an LLM backend. Every provider must
// implement at minimum Name, Capabilities, Health, and Models. The generation
// methods (Chat, ChatStream, Generate, GenerateStream, Embed) may return
// an error if the corresponding capability is not supported.
type Provider interface {
	// Name returns the provider identifier (e.g. "ollama").
	Name() string

	// Capabilities returns the bitmask of features this provider supports.
	Capabilities() Capability

	// Health checks whether the provider backend is reachable and ready.
	Health(ctx context.Context) error

	// Models returns the list of models available from this provider.
	Models(ctx context.Context) ([]ModelInfo, error)

	// Chat sends a chat completion request and returns the full response.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

	// ChatStream sends a chat completion request and invokes fn for each
	// partial response chunk. The final chunk has Done set to true.
	ChatStream(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error

	// Generate sends a raw text generation request and returns the response.
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)

	// GenerateStream sends a generation request and invokes fn for each
	// partial response chunk. The final chunk has Done set to true.
	GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateResponse) error) error

	// Embed generates vector embeddings for the given input texts.
	Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error)
}

// ---------------------------------------------------------------------------
// Chat types
// ---------------------------------------------------------------------------

// ChatMessage represents a single message in a multi-turn conversation.
type ChatMessage struct {
	Role       string     `json:"role"` // system, user, assistant, tool
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // present when assistant invokes tools
	ToolName   string     `json:"tool_name,omitempty"`    // set when role="tool"
	ToolCallID string     `json:"tool_call_id,omitempty"` // correlates result with call
}

// Tool defines a tool the model may invoke during chat.
type Tool struct {
	Type     string       `json:"type"` // always "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable function available to the model.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// ToolCall represents a tool invocation returned by the model.
type ToolCall struct {
	ID       string           `json:"id,omitempty"` // unique call ID
	Type     string           `json:"type"`         // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction holds the name and parsed arguments of a tool call.
type ToolCallFunction struct {
	Index     int             `json:"index"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ChatRequest is the provider-agnostic request for a chat completion.
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Options  ModelOptions  `json:"options,omitempty"`
	Tools    []Tool        `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
}

// ChatResponse is the provider-agnostic response from a chat completion.
type ChatResponse struct {
	Model     string      `json:"model"`
	Provider  string      `json:"provider"`
	Content   string      `json:"content"`
	Thinking  string      `json:"thinking,omitempty"`
	ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
	Done      bool        `json:"done"`
	Partial   bool        `json:"partial,omitempty"`
	Usage     Usage       `json:"usage,omitempty"`
	Latency   LatencyInfo `json:"latency,omitempty"`
}

// ---------------------------------------------------------------------------
// Generate types
// ---------------------------------------------------------------------------

// GenerateRequest is the provider-agnostic request for raw text generation.
type GenerateRequest struct {
	Model   string       `json:"model"`
	Prompt  string       `json:"prompt"`
	System  string       `json:"system,omitempty"`
	Suffix  string       `json:"suffix,omitempty"` // presence triggers FIM mode
	Options ModelOptions `json:"options,omitempty"`
	Stream  bool         `json:"stream"`
}

// GenerateResponse is the provider-agnostic response from text generation.
type GenerateResponse struct {
	Model      string                `json:"model"`
	Provider   string                `json:"provider"`
	Response   string                `json:"response"`
	Done       bool                  `json:"done"`
	Partial    bool                  `json:"partial,omitempty"`
	Usage      Usage                 `json:"usage,omitempty"`
	Latency    LatencyInfo           `json:"latency,omitempty"`
	Confidence *CompletionConfidence `json:"confidence,omitempty"`
}

// ---------------------------------------------------------------------------
// Embed types
// ---------------------------------------------------------------------------

// EmbedRequest is the provider-agnostic request for embedding generation.
type EmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// EmbedResponse is the provider-agnostic response from embedding generation.
type EmbedResponse struct {
	Model      string      `json:"model"`
	Provider   string      `json:"provider"`
	Embeddings [][]float64 `json:"embeddings"`
	Usage      Usage       `json:"usage,omitempty"`
}

// ---------------------------------------------------------------------------
// Model configuration
// ---------------------------------------------------------------------------

// ModelOptions controls generation parameters. Pointer fields are optional;
// nil means "use the provider's default". Use the Ptr helper to set values.
type ModelOptions struct {
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	NumPredict    int      `json:"num_predict,omitempty"`
	NumCtx        int      `json:"num_ctx,omitempty"`
	Stop          []string `json:"stop,omitempty"`
	RepeatPenalty *float64 `json:"repeat_penalty,omitempty"`
	Think         *bool    `json:"think,omitempty"` // only used when ThinkMode is ThinkToggle
}

// ModelInfo holds metadata about a model available from a provider.
type ModelInfo struct {
	Name          string   `json:"name"`
	Family        string   `json:"family,omitempty"`
	ParameterSize string   `json:"parameter_size,omitempty"`
	QuantLevel    string   `json:"quantization_level,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
	ContextWindow int      `json:"context_window,omitempty"`
	Digest        string   `json:"digest,omitempty"`
}

// ---------------------------------------------------------------------------
// Usage and latency
// ---------------------------------------------------------------------------

// Usage reports token consumption for a request.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// LatencyInfo captures timing breakdowns from the provider backend.
type LatencyInfo struct {
	LoadDuration       time.Duration `json:"load_duration"`
	PromptEvalDuration time.Duration `json:"prompt_eval_duration"`
	GenerationDuration time.Duration `json:"generation_duration"`
}

// ---------------------------------------------------------------------------
// Thinking / extended reasoning
// ---------------------------------------------------------------------------

// ThinkMode controls whether the model uses extended thinking (chain-of-thought).
type ThinkMode int

const (
	// ThinkNone disables thinking entirely.
	ThinkNone ThinkMode = iota
	// ThinkAlways forces thinking on every request.
	ThinkAlways
	// ThinkToggle lets the caller explicitly enable/disable per request.
	ThinkToggle
	// ThinkAuto lets the provider decide based on query complexity.
	ThinkAuto
)

// String returns the human-readable name of the think mode.
func (m ThinkMode) String() string {
	switch m {
	case ThinkNone:
		return "none"
	case ThinkAlways:
		return "always"
	case ThinkToggle:
		return "toggle"
	case ThinkAuto:
		return "auto"
	default:
		return "unknown"
	}
}

// ThinkTags defines the delimiters used to wrap thinking blocks in model output.
type ThinkTags struct {
	Open  string
	Close string
}

// DefaultThinkTags returns the standard thinking delimiters used by most models.
func DefaultThinkTags() ThinkTags {
	return ThinkTags{Open: "<think>", Close: "</think>"}
}

// ThinkBudgetAction specifies what to do when a thinking budget is exceeded.
type ThinkBudgetAction int

const (
	// ThinkBudgetSkip silently omits the thinking content when the budget is exceeded.
	ThinkBudgetSkip ThinkBudgetAction = iota
	// ThinkBudgetCancel cancels the entire request when the budget is exceeded.
	ThinkBudgetCancel
)

// ThinkBudget constrains the resources spent on extended thinking.
type ThinkBudget struct {
	MaxTokens  int               `json:"max_tokens,omitempty"`
	MaxTime    time.Duration     `json:"max_time,omitempty"`
	OnExceeded ThinkBudgetAction `json:"on_exceeded,omitempty"`
}

// ThinkMetrics captures statistics about a thinking/reasoning response.
type ThinkMetrics struct {
	ThinkingTokens int           `json:"thinking_tokens"`
	ContentTokens  int           `json:"content_tokens"`
	ThinkRatio     float64       `json:"think_ratio"`
	ThinkDuration  time.Duration `json:"think_duration"`
}

// ---------------------------------------------------------------------------
// FIM (Fill-in-the-Middle)
// ---------------------------------------------------------------------------

// FIMConfig holds the token markers and budget for Fill-in-the-Middle completion.
type FIMConfig struct {
	Prefix          string `json:"prefix"`
	Suffix          string `json:"suffix"`
	Middle          string `json:"middle"`
	PrefixBudgetPct int    `json:"prefix_budget_pct,omitempty"`
}

// ---------------------------------------------------------------------------
// Completion confidence
// ---------------------------------------------------------------------------

// CompletionConfidence scores how confident the model is in a generation result.
type CompletionConfidence struct {
	Score      float64 `json:"score"`
	AvgLogProb float64 `json:"avg_log_prob"`
	MinLogProb float64 `json:"min_log_prob"`
	DropOffIdx int     `json:"drop_off_idx"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// Ptr returns a pointer to v. It is a convenience for setting optional pointer
// fields in ModelOptions and similar structs.
func Ptr[T any](v T) *T {
	return &v
}

// estimateTokens provides a rough token count estimate using the len/4 heuristic.
// This is intentionally imprecise and suitable only for budget estimation.
func estimateTokens(s string) int {
	n := len(s) / 4
	if n == 0 && len(s) > 0 {
		return 1
	}
	return n
}
