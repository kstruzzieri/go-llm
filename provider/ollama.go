// provider/ollama.go

package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
	"golang.org/x/sync/singleflight"
)

// ---------------------------------------------------------------------------
// OllamaOption — functional options for OllamaProvider
// ---------------------------------------------------------------------------

// OllamaOption configures an OllamaProvider.
type OllamaOption func(*OllamaProvider)

// WithThinkMode sets the ThinkMode used when constructing per-request ThinkParsers.
func WithThinkMode(mode ThinkMode) OllamaOption {
	return func(p *OllamaProvider) {
		p.thinkMode = mode
	}
}

// WithThinkTags overrides the default think tag delimiters.
func WithThinkTags(tags ThinkTags) OllamaOption {
	return func(p *OllamaProvider) {
		p.thinkTags = tags
	}
}

// WithThinkBudget sets a budget constraint for thinking extraction.
func WithThinkBudget(budget *ThinkBudget) OllamaOption {
	return func(p *OllamaProvider) {
		p.thinkBudget = budget
	}
}

// ---------------------------------------------------------------------------
// OllamaProvider
// ---------------------------------------------------------------------------

// OllamaProvider implements the Provider interface by delegating to an
// ollama.Client. It adds think-tag extraction on chat responses (streaming
// and non-streaming) and graceful cancellation with partial results on all
// streaming methods.
type OllamaProvider struct {
	client      *ollama.Client
	thinkMode   ThinkMode
	thinkTags   ThinkTags
	thinkBudget *ThinkBudget
	embedGroup  singleflight.Group
}

// NewOllamaProvider creates a Provider backed by the given ollama.Client.
// Options configure think-tag parsing behavior. By default, ThinkAuto mode
// with standard <think></think> tags is used.
func NewOllamaProvider(client *ollama.Client, opts ...OllamaOption) *OllamaProvider {
	p := &OllamaProvider{
		client:    client,
		thinkMode: ThinkAuto,
		thinkTags: DefaultThinkTags(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Name returns the canonical provider identifier "ollama".
func (p *OllamaProvider) Name() string {
	return "ollama"
}

// Capabilities returns the bitmask of features the Ollama backend supports.
func (p *OllamaProvider) Capabilities() Capability {
	return CapChat | CapGenerate | CapEmbed | CapStream | CapToolCall
}

// Health checks whether the Ollama server is reachable and responsive.
func (p *OllamaProvider) Health(ctx context.Context) error {
	if p.client.IsAvailable(ctx) {
		return nil
	}
	return fmt.Errorf("provider: ollama: server is not available")
}

// Models returns the list of models available from the Ollama server,
// enriched with detailed metadata from /api/show for each model.
func (p *OllamaProvider) Models(ctx context.Context) ([]ModelInfo, error) {
	oModels, err := p.client.ListModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("provider: ollama: list models: %w", err)
	}

	models := make([]ModelInfo, len(oModels))
	for i, m := range oModels {
		// Fetch detailed info for each model.
		detail, showErr := p.client.ShowModel(ctx, m.Name)
		if showErr != nil {
			// Fallback: use basic info from list.
			models[i] = ModelInfo{
				Name: m.Name,
			}
			continue
		}
		models[i] = ModelInfo{
			Name:          detail.Name,
			Family:        detail.Family,
			ParameterSize: detail.ParamSize,
			QuantLevel:    detail.QuantLevel,
			Capabilities:  detail.Capabilities,
			Digest:        detail.Digest,
		}
	}
	return models, nil
}

// ---------------------------------------------------------------------------
// Chat (non-streaming)
// ---------------------------------------------------------------------------

// Chat sends a non-streaming chat request, translating between provider and
// ollama types. The response content is processed through ExtractThinking to
// separate any reasoning from the final answer.
func (p *OllamaProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	oReq := toOllamaChatRequest(req)
	oReq.Stream = false

	oResp, err := p.client.Chat(ctx, oReq)
	if err != nil {
		return nil, fmt.Errorf("provider: ollama: chat: %w", err)
	}

	// Extract thinking from the complete response using regex.
	content, thinking := ExtractThinking(oResp.Message.Content, p.thinkTags)

	return &ChatResponse{
		Model:     oResp.Model,
		Provider:  "ollama",
		Content:   content,
		Thinking:  thinking,
		ToolCalls: toProviderToolCalls(oResp.Message.ToolCalls),
		Done:      true,
		Usage: Usage{
			PromptTokens:     oResp.PromptEvalCount,
			CompletionTokens: oResp.EvalCount,
			TotalTokens:      oResp.PromptEvalCount + oResp.EvalCount,
		},
		Latency: LatencyInfo{
			LoadDuration:       time.Duration(oResp.LoadDuration),
			PromptEvalDuration: time.Duration(oResp.PromptEvalDuration),
			GenerationDuration: time.Duration(oResp.EvalDuration),
		},
	}, nil
}

// ---------------------------------------------------------------------------
// ChatStream (streaming with think-tag extraction)
// ---------------------------------------------------------------------------

// ChatStream sends a streaming chat request. Each chunk is processed through
// a per-request ThinkParser to separate reasoning from content in real time.
//
// If the context is cancelled after at least one chunk has been received, a
// final synthetic chunk with Partial=true and Done=true is emitted containing
// accumulated content so far, then the context error is returned.
func (p *OllamaProvider) ChatStream(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error {
	if fn == nil {
		return fmt.Errorf("provider: ollama: chat stream: callback function is required")
	}

	oReq := toOllamaChatRequest(req)
	oReq.Stream = true

	// Build a per-request ThinkParser with callbacks that emit chunks.
	var lastModel string
	var lastToolCalls []ToolCall
	var lastUsage Usage
	var lastLatency LatencyInfo
	var thinkingBuf strings.Builder
	var contentBuf strings.Builder
	chunksReceived := 0
	var callbackErr error
	lastDone := false

	parser := NewThinkParser(ThinkParserConfig{
		Mode: p.thinkMode,
		Tags: p.thinkTags,
		OnThinking: func(s string) error {
			thinkingBuf.WriteString(s)
			resp := ChatResponse{
				Model:    lastModel,
				Provider: "ollama",
				Thinking: s,
			}
			if err := fn(resp); err != nil {
				callbackErr = err
				return err
			}
			return nil
		},
		OnContent: func(s string) error {
			contentBuf.WriteString(s)
			resp := ChatResponse{
				Model:    lastModel,
				Provider: "ollama",
				Content:  s,
			}
			if err := fn(resp); err != nil {
				callbackErr = err
				return err
			}
			return nil
		},
		Budget: p.thinkBudget,
	})

	streamErr := p.client.ChatStream(ctx, oReq, func(oResp ollama.ChatResponse) error {
		chunksReceived++
		lastModel = oResp.Model

		if len(oResp.Message.ToolCalls) > 0 {
			lastToolCalls = toProviderToolCalls(oResp.Message.ToolCalls)
		}

		// Feed content through the think parser.
		if oResp.Message.Content != "" {
			if err := parser.Process(oResp.Message.Content); err != nil {
				return err
			}
		}

		// On the final chunk, flush the parser and emit Done with usage/latency.
		if oResp.Done {
			lastDone = true
			lastUsage = Usage{
				PromptTokens:     oResp.PromptEvalCount,
				CompletionTokens: oResp.EvalCount,
				TotalTokens:      oResp.PromptEvalCount + oResp.EvalCount,
			}
			lastLatency = LatencyInfo{
				LoadDuration:       time.Duration(oResp.LoadDuration),
				PromptEvalDuration: time.Duration(oResp.PromptEvalDuration),
				GenerationDuration: time.Duration(oResp.EvalDuration),
			}

			if err := parser.Flush(); err != nil {
				return err
			}

			doneResp := ChatResponse{
				Model:     oResp.Model,
				Provider:  "ollama",
				ToolCalls: lastToolCalls,
				Done:      true,
				Usage:     lastUsage,
				Latency:   lastLatency,
			}
			return fn(doneResp)
		}

		// Emit tool calls as they arrive (separate from content/thinking).
		if len(oResp.Message.ToolCalls) > 0 {
			tcResp := ChatResponse{
				Model:     oResp.Model,
				Provider:  "ollama",
				ToolCalls: toProviderToolCalls(oResp.Message.ToolCalls),
			}
			return fn(tcResp)
		}

		return nil
	})

	// Graceful cancellation: if context was cancelled after receiving chunks,
	// emit a partial result so consumers can use what was generated.
	if streamErr != nil && ctx.Err() != nil && chunksReceived > 0 && !lastDone {
		// Flush any remaining parser state.
		_ = parser.Flush()

		partial := ChatResponse{
			Model:     req.Model,
			Provider:  "ollama",
			Content:   contentBuf.String(),
			Thinking:  thinkingBuf.String(),
			ToolCalls: lastToolCalls,
			Done:      true,
			Partial:   true,
			Usage:     lastUsage,
			Latency:   lastLatency,
		}
		// Best-effort delivery of partial result; ignore callback error.
		_ = fn(partial)
		return fmt.Errorf("provider: ollama: chat stream: %w", ctx.Err())
	}

	if streamErr != nil {
		// If the error originated from the user callback, return it directly
		// to avoid double-wrapping.
		if callbackErr != nil {
			return fmt.Errorf("provider: ollama: chat stream: %w", streamErr)
		}
		return fmt.Errorf("provider: ollama: chat stream: %w", streamErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Generate (non-streaming)
// ---------------------------------------------------------------------------

// Generate sends a non-streaming text generation request, translating between
// provider and ollama types.
func (p *OllamaProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	oReq := toOllamaGenerateRequest(req)
	oReq.Stream = false

	oResp, err := p.client.Generate(ctx, oReq)
	if err != nil {
		return nil, fmt.Errorf("provider: ollama: generate: %w", err)
	}

	return &GenerateResponse{
		Model:    oResp.Model,
		Provider: "ollama",
		Response: oResp.Response,
		Done:     true,
		Usage: Usage{
			CompletionTokens: oResp.EvalCount,
		},
		Latency: LatencyInfo{
			GenerationDuration: time.Duration(oResp.EvalDuration),
		},
	}, nil
}

// ---------------------------------------------------------------------------
// GenerateStream (streaming with graceful cancellation)
// ---------------------------------------------------------------------------

// GenerateStream sends a streaming text generation request. Each chunk is
// forwarded to fn with the provider-level GenerateResponse type.
//
// If the context is cancelled after at least one chunk has been received, a
// final synthetic chunk with Partial=true and Done=true is emitted containing
// the accumulated response so far, then the context error is returned.
func (p *OllamaProvider) GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateResponse) error) error {
	if fn == nil {
		return fmt.Errorf("provider: ollama: generate stream: callback function is required")
	}

	oReq := toOllamaGenerateRequest(req)
	oReq.Stream = true

	var accumulated strings.Builder
	var lastUsage Usage
	var lastLatency LatencyInfo
	chunksReceived := 0
	lastDone := false
	var callbackErr error

	streamErr := p.client.GenerateStream(ctx, oReq, func(oResp ollama.GenerateResponse) error {
		chunksReceived++
		accumulated.WriteString(oResp.Response)

		if oResp.Done {
			lastDone = true
			lastUsage = Usage{
				CompletionTokens: oResp.EvalCount,
			}
			lastLatency = LatencyInfo{
				GenerationDuration: time.Duration(oResp.EvalDuration),
			}
		}

		resp := GenerateResponse{
			Model:    oResp.Model,
			Provider: "ollama",
			Response: oResp.Response,
			Done:     oResp.Done,
			Usage:    lastUsage,
			Latency:  lastLatency,
		}
		if err := fn(resp); err != nil {
			callbackErr = err
			return err
		}
		return nil
	})

	// Graceful cancellation: if context was cancelled after receiving chunks,
	// emit a partial result so consumers can use what was generated.
	if streamErr != nil && ctx.Err() != nil && chunksReceived > 0 && !lastDone {
		partial := GenerateResponse{
			Model:    req.Model,
			Provider: "ollama",
			Response: accumulated.String(),
			Done:     true,
			Partial:  true,
			Usage:    lastUsage,
			Latency:  lastLatency,
		}
		// Best-effort delivery of partial result; ignore callback error.
		_ = fn(partial)
		return fmt.Errorf("provider: ollama: generate stream: %w", ctx.Err())
	}

	if streamErr != nil {
		if callbackErr != nil {
			return fmt.Errorf("provider: ollama: generate stream: %w", streamErr)
		}
		return fmt.Errorf("provider: ollama: generate stream: %w", streamErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Embed (with singleflight deduplication)
// ---------------------------------------------------------------------------

// Embed generates vector embeddings for the given input texts. Concurrent
// identical requests (same model and inputs) are deduplicated via singleflight
// to avoid redundant computation during batch RAG indexing.
func (p *OllamaProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("provider: ollama: embed: model name is required")
	}
	if len(req.Input) == 0 {
		return nil, fmt.Errorf("provider: ollama: embed: at least one input text is required")
	}

	// Build singleflight key from model + hash of all inputs.
	key := embedKey(req.Model, req.Input)

	result, err, _ := p.embedGroup.Do(key, func() (any, error) {
		embeddings := make([][]float64, len(req.Input))
		for i, text := range req.Input {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("provider: ollama: embed: %w", ctx.Err())
			}
			emb, embedErr := p.client.Embed(ctx, req.Model, text)
			if embedErr != nil {
				return nil, fmt.Errorf("provider: ollama: embed: %w", embedErr)
			}
			embeddings[i] = emb
		}
		return &EmbedResponse{
			Model:      req.Model,
			Provider:   "ollama",
			Embeddings: embeddings,
			Usage: Usage{
				PromptTokens: len(req.Input),
			},
		}, nil
	})

	if err != nil {
		return nil, err
	}
	return result.(*EmbedResponse), nil
}

// embedKey builds a singleflight dedup key from the model name and input texts.
// Inputs are joined with a null byte separator to avoid collisions, then hashed
// with SHA-256 for a fixed-size key component.
func embedKey(model string, inputs []string) string {
	h := sha256.Sum256([]byte(strings.Join(inputs, "\x00")))
	return model + ":" + hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Type mapping helpers
// ---------------------------------------------------------------------------

// toOllamaChatRequest converts a provider ChatRequest to an ollama ChatRequest.
func toOllamaChatRequest(req ChatRequest) ollama.ChatRequest {
	msgs := make([]ollama.ChatMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = ollama.ChatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolName:   m.ToolName,
			ToolCallID: m.ToolCallID,
			ToolCalls:  toOllamaToolCalls(m.ToolCalls),
		}
	}

	oReq := ollama.ChatRequest{
		Model:    req.Model,
		Messages: msgs,
		Stream:   req.Stream,
		Tools:    toOllamaTools(req.Tools),
	}
	if opts := toOllamaOptions(req.Options); opts != nil {
		oReq.Options = opts
	}
	return oReq
}

// toOllamaGenerateRequest converts a provider GenerateRequest to an ollama GenerateRequest.
func toOllamaGenerateRequest(req GenerateRequest) ollama.GenerateRequest {
	oReq := ollama.GenerateRequest{
		Model:  req.Model,
		Prompt: req.Prompt,
		System: req.System,
		Suffix: req.Suffix,
		Stream: req.Stream,
	}
	if opts := toOllamaOptions(req.Options); opts != nil {
		oReq.Options = opts
	}
	return oReq
}

// toOllamaOptions converts provider ModelOptions to ollama ModelOptions.
// Returns nil when all fields are zero-valued, matching ollama's omitempty behavior.
func toOllamaOptions(opts ModelOptions) *ollama.ModelOptions {
	o := &ollama.ModelOptions{
		NumPredict: opts.NumPredict,
		NumCtx:     opts.NumCtx,
		Stop:       opts.Stop,
	}
	if opts.Temperature != nil {
		o.Temperature = *opts.Temperature
	}
	if opts.TopP != nil {
		o.TopP = *opts.TopP
	}
	if opts.RepeatPenalty != nil {
		o.RepeatPenalty = *opts.RepeatPenalty
	}

	// Return nil if everything is zero to match omitempty.
	if o.Temperature == 0 && o.TopP == 0 && o.NumPredict == 0 &&
		o.NumCtx == 0 && len(o.Stop) == 0 && o.RepeatPenalty == 0 {
		return nil
	}
	return o
}

// toOllamaTools converts provider Tools to ollama Tools.
func toOllamaTools(tools []Tool) []ollama.Tool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]ollama.Tool, len(tools))
	for i, t := range tools {
		result[i] = ollama.Tool{
			Type: t.Type,
			Function: ollama.ToolFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		}
	}
	return result
}

// toOllamaToolCalls converts provider ToolCalls to ollama ToolCalls.
// The provider uses json.RawMessage for arguments while ollama uses map[string]any,
// so a JSON unmarshal is performed for each call.
func toOllamaToolCalls(calls []ToolCall) []ollama.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]ollama.ToolCall, len(calls))
	for i, c := range calls {
		var args map[string]any
		if len(c.Function.Arguments) > 0 {
			// Best-effort unmarshal; nil on failure.
			_ = json.Unmarshal(c.Function.Arguments, &args)
		}
		result[i] = ollama.ToolCall{
			ID:   c.ID,
			Type: c.Type,
			Function: ollama.ToolCallFunction{
				Index:     c.Function.Index,
				Name:      c.Function.Name,
				Arguments: args,
			},
		}
	}
	return result
}

// toProviderToolCalls converts ollama ToolCalls to provider ToolCalls.
// The ollama package uses map[string]any for arguments while the provider uses
// json.RawMessage, so a JSON marshal is performed for each call.
func toProviderToolCalls(calls []ollama.ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	result := make([]ToolCall, len(calls))
	for i, c := range calls {
		var args json.RawMessage
		if c.Function.Arguments != nil {
			// Best-effort marshal; nil on failure.
			args, _ = json.Marshal(c.Function.Arguments)
		}
		result[i] = ToolCall{
			ID:   c.ID,
			Type: c.Type,
			Function: ToolCallFunction{
				Index:     c.Function.Index,
				Name:      c.Function.Name,
				Arguments: args,
			},
		}
	}
	return result
}
