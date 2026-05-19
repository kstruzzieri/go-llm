// Package openaicompat — Provider impl. See doc.go for the package overview.
package openaicompat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
	"golang.org/x/sync/singleflight"
)

const defaultProviderName = "openai-compat"

// ---------------------------------------------------------------------------
// Provider construction + options
// ---------------------------------------------------------------------------

// Option configures a Provider.
type Option func(*Provider)

// WithProviderName overrides the registry identity returned by Name() and
// stamped on every response's Provider field. Default is "openai-compat";
// supply a config-key instance name (e.g. "vllm-workstation") when
// registering multiple OpenAI-compat instances. Mirrors the OllamaProvider
// option of the same name; behavior is parallel.
//
// Panics on empty input — an empty registry identity is a configuration
// bug that would otherwise produce a Provider whose Registry.Register
// fails later with a less-actionable error. Fail fast at construction
// instead of silently keeping the default.
func WithProviderName(name string) Option {
	if name == "" {
		panic("openaicompat: WithProviderName called with empty name; omit the option to use the default \"openai-compat\"")
	}
	return func(p *Provider) {
		p.name = name
	}
}

// WithThinkMode sets the ThinkMode used when constructing per-request
// ThinkParsers. Defaults to ThinkAuto (extract whenever tags appear).
func WithThinkMode(mode provider.ThinkMode) Option {
	return func(p *Provider) {
		p.thinkMode = mode
	}
}

// WithThinkTags overrides the default <think></think> delimiters.
func WithThinkTags(tags provider.ThinkTags) Option {
	return func(p *Provider) {
		p.thinkTags = tags
	}
}

// WithThinkBudget sets a budget constraint for thinking extraction.
func WithThinkBudget(budget *provider.ThinkBudget) Option {
	return func(p *Provider) {
		p.thinkBudget = budget
	}
}

// WithCapabilities overrides the default capability bitmask returned by
// Capabilities(). Use when the target server is known to lack a specific
// endpoint (e.g. a llama.cpp build without /v1/embeddings). Note: this
// affects the Provider-level Capabilities() report only; per-model
// carve-down should still be done via config.ModelConfig.Capabilities.
func WithCapabilities(caps provider.Capability) Option {
	return func(p *Provider) {
		p.caps = caps
	}
}

// Provider implements provider.Provider against an OpenAI-compatible server.
// It composes a Client for HTTP/SSE transport and shares the same
// ThinkParser / singleflight patterns as OllamaProvider so behavior is
// consistent across backend kinds.
type Provider struct {
	name        string
	client      *Client
	caps        provider.Capability
	thinkMode   provider.ThinkMode
	thinkTags   provider.ThinkTags
	thinkBudget *provider.ThinkBudget
	embedGroup  singleflight.Group
}

// NewProvider creates a Provider backed by the given Client. Options
// configure registry identity, thinking, and capability advertisement.
//
// Default capabilities are CapChat | CapGenerate | CapStream | CapEmbed |
// CapToolCall. CapInsert is intentionally NOT advertised by default because
// OpenAI-compat servers vary on FIM support and the spec offers no probe
// equivalent to Ollama's template inspection; opt in via WithCapabilities
// when the target server is known to support it.
func NewProvider(client *Client, opts ...Option) *Provider {
	p := &Provider{
		name:      defaultProviderName,
		client:    client,
		caps:      provider.CapChat | provider.CapGenerate | provider.CapStream | provider.CapEmbed | provider.CapToolCall,
		thinkMode: provider.ThinkAuto,
		thinkTags: provider.DefaultThinkTags(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// ---------------------------------------------------------------------------
// Identity / Capability / Health
// ---------------------------------------------------------------------------

// Name returns the registry identity for this Provider instance. Defaults
// to "openai-compat" unless WithProviderName overrode it; the value is
// also stamped on every response's Provider field.
func (p *Provider) Name() string {
	return p.name
}

// Capabilities returns the bitmask of features this Provider advertises.
// See NewProvider for the default set and the rationale for omitting
// CapInsert by default.
func (p *Provider) Capabilities() provider.Capability {
	return p.caps
}

// Health probes the server with a /v1/models request. A 2xx response is
// the same liveness signal Ollama's IsAvailable uses for /api/tags —
// minimal, no special endpoint required, and works against every
// OpenAI-compat implementation since /v1/models is the spec's only
// mandatory GET.
func (p *Provider) Health(ctx context.Context) error {
	var resp modelsResponse
	if err := p.client.getJSON(ctx, "/v1/models", &resp); err != nil {
		return fmt.Errorf("provider: openaicompat: health: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Models discovery
// ---------------------------------------------------------------------------

// Models returns the list of models advertised by /v1/models. OpenAI-compat
// servers expose only the ID reliably; Family, ParameterSize, QuantLevel,
// Template, Capabilities, ContextWindow, and Digest are NOT part of the
// /v1/models spec and will be empty/zero. Consumers needing those fields
// should populate them via models.json (catalog merge happens upstream in
// ModelRegistry).
func (p *Provider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	var resp modelsResponse
	if err := p.client.getJSON(ctx, "/v1/models", &resp); err != nil {
		return nil, fmt.Errorf("provider: openaicompat: list models: %w", err)
	}
	out := make([]provider.ModelInfo, len(resp.Data))
	for i, m := range resp.Data {
		out[i] = provider.ModelInfo{Name: m.ID}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Chat (non-streaming)
// ---------------------------------------------------------------------------

// Chat sends a non-streaming /v1/chat/completions request. Content is
// processed through ExtractThinking to separate inline reasoning from
// the final answer, matching the OllamaProvider contract.
func (p *Provider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	body := toChatRequest(req, false)
	var resp chatResponse
	if err := p.client.postJSON(ctx, "/v1/chat/completions", body, &resp); err != nil {
		return nil, fmt.Errorf("provider: openaicompat: chat: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("provider: openaicompat: chat: response had no choices")
	}
	msg := resp.Choices[0].Message

	content := msg.Content
	thinking := ""
	if p.shouldExtractThinking(req.Options) {
		content, thinking = provider.ExtractThinking(msg.Content, p.thinkTags)
	}

	model := resp.Model
	if model == "" {
		model = req.Model
	}

	return &provider.ChatResponse{
		Model:     model,
		Provider:  p.name,
		Content:   content,
		Thinking:  thinking,
		ToolCalls: toProviderToolCalls(msg.ToolCalls),
		Done:      true,
		Usage: provider.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// ChatStream (streaming with think-tag extraction)
// ---------------------------------------------------------------------------

// ChatStream sends a streaming /v1/chat/completions request and invokes fn
// for each chunk. Content is processed through a per-request ThinkParser to
// separate reasoning from content in real time. Tool-call deltas are
// emitted as standalone chunks alongside content/thinking.
//
// Cancellation policy mirrors OllamaProvider: if the context is cancelled
// after at least one chunk has been received, the parser is flushed and a
// final synthetic Done+Partial chunk is emitted before the context error
// is returned.
func (p *Provider) ChatStream(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
	if fn == nil {
		return fmt.Errorf("provider: openaicompat: chat stream: callback function is required")
	}

	body := toChatRequest(req, true)
	reader, err := p.client.postSSE(ctx, "/v1/chat/completions", body)
	if err != nil {
		return fmt.Errorf("provider: openaicompat: chat stream: %w", err)
	}
	defer reader.Close()

	var (
		lastModel      string
		lastToolCalls  []provider.ToolCall
		lastUsage      provider.Usage
		chunksReceived int
		callbackErr    error
		finished       bool
	)

	parser := provider.NewThinkParser(provider.ThinkParserConfig{
		Mode: p.thinkMode,
		Tags: p.thinkTags,
		OnThinking: func(s string) error {
			err := fn(provider.ChatResponse{Model: lastModel, Provider: p.name, Thinking: s})
			if err != nil {
				callbackErr = err
			}
			return err
		},
		OnContent: func(s string) error {
			err := fn(provider.ChatResponse{Model: lastModel, Provider: p.name, Content: s})
			if err != nil {
				callbackErr = err
			}
			return err
		},
		Budget: p.thinkBudget,
	})
	if p.thinkMode == provider.ThinkToggle {
		parser.SetActive(thinkToggleActive(req.Options))
	}

	for {
		payload, readErr := reader.Next()
		if readErr != nil {
			break
		}

		var chunk chatChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			// Skip malformed chunks rather than aborting the stream — servers
			// occasionally inject keepalive comments or oddly-shaped frames.
			continue
		}
		chunksReceived++
		if chunk.Model != "" {
			lastModel = chunk.Model
		}
		if chunk.Usage != nil {
			lastUsage = provider.Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		if len(choice.Delta.ToolCalls) > 0 {
			lastToolCalls = toProviderToolCalls(choice.Delta.ToolCalls)
			if err := fn(provider.ChatResponse{
				Model:     lastModel,
				Provider:  p.name,
				ToolCalls: lastToolCalls,
			}); err != nil {
				callbackErr = err
				break
			}
		}
		if choice.Delta.Content != "" {
			if err := parser.Process(choice.Delta.Content); err != nil {
				callbackErr = err
				break
			}
		}
		if choice.FinishReason != nil {
			finished = true
			if err := parser.Flush(); err != nil {
				callbackErr = err
				break
			}
			if err := fn(provider.ChatResponse{
				Model:     lastModel,
				Provider:  p.name,
				ToolCalls: lastToolCalls,
				Done:      true,
				Usage:     lastUsage,
			}); err != nil {
				callbackErr = err
			}
			break
		}
	}

	if callbackErr != nil {
		return callbackErr
	}

	if ctx.Err() != nil && chunksReceived > 0 && !finished {
		_ = parser.Flush()
		model := lastModel
		if model == "" {
			model = req.Model
		}
		_ = fn(provider.ChatResponse{
			Model:     model,
			Provider:  p.name,
			ToolCalls: lastToolCalls,
			Done:      true,
			Partial:   true,
			Usage:     lastUsage,
		})
		return fmt.Errorf("provider: openaicompat: chat stream: %w", ctx.Err())
	}

	return nil
}

// ---------------------------------------------------------------------------
// Generate (non-streaming, /v1/completions, FIM via suffix)
// ---------------------------------------------------------------------------

// Generate sends a non-streaming /v1/completions request. When req.Suffix
// is set, the underlying server is expected to run native FIM (vLLM and
// llama.cpp --api both support this); when empty, behaves as plain
// left-to-right generation.
//
// System prompts are NOT supported by /v1/completions in OpenAI's spec; if
// req.System is set, it is prepended to req.Prompt with a newline boundary
// so callers can keep using the provider-agnostic ChatRequest shape without
// losing the directive. This is best-effort and may not match every
// server's instruction-tuning expectations.
func (p *Provider) Generate(ctx context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
	body := toCompletionRequest(req, false)
	var resp completionResponse
	if err := p.client.postJSON(ctx, "/v1/completions", body, &resp); err != nil {
		return nil, fmt.Errorf("provider: openaicompat: generate: %w", err)
	}

	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("provider: openaicompat: generate: response had no choices")
	}
	text := resp.Choices[0].Text

	model := resp.Model
	if model == "" {
		model = req.Model
	}

	return &provider.GenerateResponse{
		Model:    model,
		Provider: p.name,
		Response: text,
		Done:     true,
		Usage: provider.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// GenerateStream (streaming /v1/completions)
// ---------------------------------------------------------------------------

// GenerateStream sends a streaming /v1/completions request. Each chunk's
// delta text is forwarded to fn. Cancellation policy matches ChatStream:
// emit a final Done+Partial chunk if cancelled mid-stream.
func (p *Provider) GenerateStream(ctx context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
	if fn == nil {
		return fmt.Errorf("provider: openaicompat: generate stream: callback function is required")
	}

	body := toCompletionRequest(req, true)
	reader, err := p.client.postSSE(ctx, "/v1/completions", body)
	if err != nil {
		return fmt.Errorf("provider: openaicompat: generate stream: %w", err)
	}
	defer reader.Close()

	var (
		lastModel      string
		lastUsage      provider.Usage
		chunksReceived int
		callbackErr    error
		finished       bool
	)

	for {
		payload, readErr := reader.Next()
		if readErr != nil {
			break
		}

		var chunk completionChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		chunksReceived++
		if chunk.Model != "" {
			lastModel = chunk.Model
		}
		if chunk.Usage != nil {
			lastUsage = provider.Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				TotalTokens:      chunk.Usage.TotalTokens,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		isFinal := choice.FinishReason != nil

		resp := provider.GenerateResponse{
			Model:    lastModel,
			Provider: p.name,
			Response: choice.Text,
			Done:     isFinal,
			Usage:    lastUsage,
		}
		if err := fn(resp); err != nil {
			callbackErr = err
			break
		}
		if isFinal {
			finished = true
			break
		}
	}

	if callbackErr != nil {
		return callbackErr
	}

	if ctx.Err() != nil && chunksReceived > 0 && !finished {
		model := lastModel
		if model == "" {
			model = req.Model
		}
		_ = fn(provider.GenerateResponse{
			Model:    model,
			Provider: p.name,
			Done:     true,
			Partial:  true,
			Usage:    lastUsage,
		})
		return fmt.Errorf("provider: openaicompat: generate stream: %w", ctx.Err())
	}

	return nil
}

// ---------------------------------------------------------------------------
// Embed (with singleflight deduplication)
// ---------------------------------------------------------------------------

// Embed generates vector embeddings via /v1/embeddings. Concurrent identical
// requests (same model + inputs) are deduplicated via singleflight to avoid
// redundant compute during batch RAG indexing, matching OllamaProvider's
// behavior.
//
// Returns are reordered by index defensively — OpenAI's spec says the data
// array order is unspecified, only that each entry's Index field maps back
// to the request Input position. Most servers preserve order in practice,
// but the reorder makes the contract explicit.
func (p *Provider) Embed(ctx context.Context, req provider.EmbedRequest) (*provider.EmbedResponse, error) {
	if req.Model == "" {
		return nil, fmt.Errorf("provider: openaicompat: embed: model name is required")
	}
	if len(req.Input) == 0 {
		return nil, fmt.Errorf("provider: openaicompat: embed: at least one input text is required")
	}

	key := embedKey(req.Model, req.Input)
	sharedCtx := context.WithoutCancel(ctx)
	ch := p.embedGroup.DoChan(key, func() (any, error) {
		body := embedRequest{Model: req.Model, Input: req.Input}
		var resp embedResponse
		if err := p.client.postJSON(sharedCtx, "/v1/embeddings", body, &resp); err != nil {
			return nil, fmt.Errorf("provider: openaicompat: embed: %w", err)
		}
		if len(resp.Data) != len(req.Input) {
			return nil, fmt.Errorf("provider: openaicompat: embed: server returned %d embeddings for %d inputs", len(resp.Data), len(req.Input))
		}
		ordered := make([][]float64, len(req.Input))
		for _, doc := range resp.Data {
			if doc.Index < 0 || doc.Index >= len(ordered) {
				return nil, fmt.Errorf("provider: openaicompat: embed: server returned out-of-range index %d", doc.Index)
			}
			ordered[doc.Index] = doc.Embedding
		}
		for i, emb := range ordered {
			if emb == nil {
				return nil, fmt.Errorf("provider: openaicompat: embed: server skipped index %d", i)
			}
		}
		return &provider.EmbedResponse{
			Model:      req.Model,
			Provider:   p.name,
			Embeddings: ordered,
			Usage: provider.Usage{
				PromptTokens: resp.Usage.PromptTokens,
				TotalTokens:  resp.Usage.TotalTokens,
			},
		}, nil
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		// Defensive copy: singleflight shares the result pointer.
		shared := res.Val.(*provider.EmbedResponse)
		copied := *shared
		copied.Embeddings = make([][]float64, len(shared.Embeddings))
		for i, emb := range shared.Embeddings {
			copied.Embeddings[i] = make([]float64, len(emb))
			copy(copied.Embeddings[i], emb)
		}
		return &copied, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("provider: openaicompat: embed: %w", ctx.Err())
	}
}

// embedKey builds a singleflight dedup key from model + sha256 of joined
// inputs. Identical to the OllamaProvider helper; duplicated here so the
// sub-package has no upward dependency on unexported symbols in provider/.
func embedKey(model string, inputs []string) string {
	h := sha256.Sum256([]byte(strings.Join(inputs, "\x00")))
	return model + ":" + hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// Thinking helpers
// ---------------------------------------------------------------------------

// shouldExtractThinking mirrors OllamaProvider's per-request decision.
func (p *Provider) shouldExtractThinking(opts provider.ModelOptions) bool {
	switch p.thinkMode {
	case provider.ThinkNone:
		return false
	case provider.ThinkToggle:
		return thinkToggleActive(opts)
	default:
		return true
	}
}

// thinkToggleActive reports whether ThinkToggle mode is active for opts.
func thinkToggleActive(opts provider.ModelOptions) bool {
	return opts.Think != nil && *opts.Think
}

// ---------------------------------------------------------------------------
// Provider -> OpenAI request conversion
// ---------------------------------------------------------------------------

// toChatRequest converts a provider ChatRequest to the OpenAI wire shape.
// stream is passed explicitly because the request body's Stream field is
// the source of truth for the HTTP path (postJSON vs postSSE).
func toChatRequest(req provider.ChatRequest, stream bool) chatRequest {
	msgs := make([]chatMessage, len(req.Messages))
	for i, m := range req.Messages {
		msgs[i] = chatMessage{
			Role:       m.Role,
			Content:    m.Content,
			Name:       m.ToolName,
			ToolCallID: m.ToolCallID,
			ToolCalls:  toWireToolCalls(m.ToolCalls),
		}
	}
	r := chatRequest{
		Model:    req.Model,
		Messages: msgs,
		Stream:   stream,
		Tools:    toWireTools(req.Tools),
	}
	applyOptionsChat(&r, req.Options)
	return r
}

// toCompletionRequest converts a provider GenerateRequest to the OpenAI
// /v1/completions wire shape. System is prepended to Prompt when set
// because /v1/completions has no system field.
func toCompletionRequest(req provider.GenerateRequest, stream bool) completionRequest {
	prompt := req.Prompt
	if req.System != "" {
		prompt = req.System + "\n" + prompt
	}
	r := completionRequest{
		Model:  req.Model,
		Prompt: prompt,
		Suffix: req.Suffix,
		Stream: stream,
	}
	applyOptionsCompletion(&r, req.Options)
	return r
}

// applyOptionsChat copies relevant ModelOptions onto a chatRequest. Pointer
// fields preserve "unset vs zero" semantics — OpenAI treats temperature=0
// differently from "not specified".
func applyOptionsChat(r *chatRequest, opts provider.ModelOptions) {
	r.Temperature = opts.Temperature
	r.TopP = opts.TopP
	r.MaxTokens = opts.NumPredict
	if len(opts.Stop) > 0 {
		r.Stop = opts.Stop
	}
}

// applyOptionsCompletion copies relevant ModelOptions onto a completionRequest.
func applyOptionsCompletion(r *completionRequest, opts provider.ModelOptions) {
	r.Temperature = opts.Temperature
	r.TopP = opts.TopP
	r.MaxTokens = opts.NumPredict
	if len(opts.Stop) > 0 {
		r.Stop = opts.Stop
	}
}

// toWireTools converts provider tools to the OpenAI wire shape.
func toWireTools(tools []provider.Tool) []chatTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]chatTool, len(tools))
	for i, t := range tools {
		out[i] = chatTool{
			Type: t.Type,
			Function: chatFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			},
		}
	}
	return out
}

// toWireToolCalls converts provider tool calls to the OpenAI wire shape.
func toWireToolCalls(calls []provider.ToolCall) []chatToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]chatToolCall, len(calls))
	for i, c := range calls {
		out[i] = chatToolCall{
			ID:   c.ID,
			Type: c.Type,
			Function: chatToolCallFunction{
				Name:      c.Function.Name,
				Arguments: c.Function.Arguments,
			},
		}
	}
	return out
}

// toProviderToolCalls converts OpenAI wire tool calls to provider shape.
func toProviderToolCalls(calls []chatToolCall) []provider.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]provider.ToolCall, len(calls))
	for i, c := range calls {
		out[i] = provider.ToolCall{
			ID:   c.ID,
			Type: c.Type,
			Function: provider.ToolCallFunction{
				Name:      c.Function.Name,
				Arguments: c.Function.Arguments,
			},
		}
	}
	return out
}

// Compile-time assertion that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)
