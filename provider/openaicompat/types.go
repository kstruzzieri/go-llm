package openaicompat

import "encoding/json"

// ---------------------------------------------------------------------------
// Chat completion wire types (/v1/chat/completions)
// ---------------------------------------------------------------------------

// chatRequest is the outbound shape for POST /v1/chat/completions. Only the
// fields go-llm sends are declared; OpenAI ignores unknown request fields.
type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
	Tools       []chatTool    `json:"tools,omitempty"`
}

// chatMessage is the wire shape of one message in a chat request or response.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
}

// chatTool is the OpenAI function-calling tool descriptor.
type chatTool struct {
	Type     string       `json:"type"` // always "function"
	Function chatFunction `json:"function"`
}

// chatFunction describes a callable function.
type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// chatToolCall is one tool-call entry inside an assistant message.
//
// Index is a pointer-with-omitempty for two reasons: OpenAI's streaming
// deltas include it to correlate arg fragments across chunks (the
// streamToolCallAccumulator depends on this for fragmented assembly),
// but outbound requests MUST NOT carry it — `index` is a response-only
// field in OpenAI's spec, and strict server-side schema validators may
// reject requests that include it. Nil pointer + omitempty produces the
// correct shape in both directions; toWireToolCalls deliberately never
// sets Index when converting provider.ToolCall back to the wire form.
type chatToolCall struct {
	Index    *int                 `json:"index,omitempty"` // response-only; see type doc
	ID       string               `json:"id,omitempty"`
	Type     string               `json:"type"` // "function"
	Function chatToolCallFunction `json:"function"`
}

// chatToolCallFunction is the function descriptor inside a tool_calls entry.
//
// Arguments is OpenAI's JSON-string envelope around the actual tool-call
// payload (e.g. the wire carries `"arguments": "{\"q\":\"go\"}"` for a
// search-style tool). On INBOUND we normalize via
// normalizeToolCallArguments — object/array literals are unwrapped to the
// canonical raw-JSON tool-call shape, scalar string literals stay wrapped
// to preserve type. On OUTBOUND we re-encode via encodeToolCallArguments
// so the wire shape matches OpenAI's canonical form regardless of which
// representation a caller passed in.
type chatToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// chatResponse is the non-streaming /v1/chat/completions response body.
type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   usage        `json:"usage"`
}

// chatChoice is one completion choice in a chat response. The first choice
// (index 0) is consumed; multi-choice responses are not exposed to the
// provider-level API.
type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// chatChunk is one frame of a streaming /v1/chat/completions response.
type chatChunk struct {
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *usage            `json:"usage,omitempty"` // present on final chunk when stream_options.include_usage is set
}

// chatChunkChoice is one streaming-chunk choice. FinishReason is a pointer
// so interim chunks deserialize without spuriously matching the zero value
// — the final chunk is the one with a non-nil FinishReason.
type chatChunkChoice struct {
	Index        int         `json:"index"`
	Delta        chatMessage `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// ---------------------------------------------------------------------------
// Completion wire types (/v1/completions, used for FIM via suffix)
// ---------------------------------------------------------------------------

// completionRequest is POST /v1/completions. The Suffix field is what makes
// this distinct from chat: when set, vLLM and llama.cpp run native FIM.
type completionRequest struct {
	Model       string   `json:"model"`
	Prompt      string   `json:"prompt"`
	Suffix      string   `json:"suffix,omitempty"`
	Stream      bool     `json:"stream,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

// completionResponse is the non-streaming /v1/completions response body.
type completionResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   usage              `json:"usage"`
}

// completionChoice is one choice in a completion response.
type completionChoice struct {
	Index        int    `json:"index"`
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason"`
}

// completionChunk is one frame of a streaming /v1/completions response.
type completionChunk struct {
	ID      string                  `json:"id"`
	Model   string                  `json:"model"`
	Choices []completionChunkChoice `json:"choices"`
	Usage   *usage                  `json:"usage,omitempty"`
}

// completionChunkChoice is one streaming-chunk choice. Same FinishReason
// pointer convention as chatChunkChoice.
type completionChunkChoice struct {
	Index        int     `json:"index"`
	Text         string  `json:"text"`
	FinishReason *string `json:"finish_reason"`
}

// ---------------------------------------------------------------------------
// Embedding wire types (/v1/embeddings)
// ---------------------------------------------------------------------------

// embedRequest is POST /v1/embeddings. Input accepts either a single string
// or an array of strings on the wire; we always send an array for uniform
// response handling.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse is the /v1/embeddings response body.
type embedResponse struct {
	Model string         `json:"model"`
	Data  []embeddingDoc `json:"data"`
	Usage usage          `json:"usage"`
}

// embeddingDoc is one entry in the response data array. Index correlates
// the embedding back to its position in the request Input array.
type embeddingDoc struct {
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

// ---------------------------------------------------------------------------
// Models wire types (/v1/models)
// ---------------------------------------------------------------------------

// modelsResponse is the GET /v1/models response body.
type modelsResponse struct {
	Data []modelEntry `json:"data"`
}

// modelEntry is one model in the /v1/models listing. OpenAI-compat servers
// expose only the ID reliably; family/quant/context-window are NOT in the
// spec and would have to be inferred per-server.
type modelEntry struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
}

// ---------------------------------------------------------------------------
// Shared subtypes
// ---------------------------------------------------------------------------

// usage is OpenAI's token accounting shape. Embedding responses omit
// completion_tokens; chat/completion responses populate all three.
//
// CompletionTokensDetails is an OpenAI extension (also emitted by some
// llama-server / vLLM builds) that breaks completion_tokens down further.
// It is a pointer so absence (the server omits the block) is distinguishable
// from a present-but-zero reasoning count; servers that don't report it leave
// it nil and the reasoning accessor yields zero.
type usage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// completionTokensDetails carries the reasoning-token count reported by
// reasoning models via usage.completion_tokens_details.reasoning_tokens.
type completionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// reasoningTokens returns the reasoning-token count the server reported, or 0
// when it omitted the completion_tokens_details block entirely.
func (u usage) reasoningTokens() int {
	if u.CompletionTokensDetails == nil {
		return 0
	}
	return u.CompletionTokensDetails.ReasoningTokens
}

func (u usage) reasoningTokensReported() bool {
	return u.CompletionTokensDetails != nil
}

// errorEnvelope is the OpenAI-shape error response body. Servers return
// this with non-2xx HTTP statuses to surface model/auth/format problems.
type errorEnvelope struct {
	Error errorPayload `json:"error"`
}

// errorPayload is the nested error object inside errorEnvelope.
type errorPayload struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}
