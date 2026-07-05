// Package openaicompat — Provider impl. See doc.go for the package overview.
package openaicompat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	// The structured reasoning_content field, when present, is authoritative —
	// the server has already separated reasoning from the answer, so it is
	// captured unconditionally (independent of think mode, mirroring how #158
	// captures reasoning tokens). Inline <think>-tag extraction remains the
	// fallback for servers that don't separate reasoning, and still cleans
	// Content either way.
	content := msg.Content
	thinking := msg.ReasoningContent
	if p.shouldExtractThinking(req) {
		var inline string
		content, inline = provider.ExtractThinking(msg.Content, p.effectiveThinkTags(req))
		if thinking == "" {
			thinking = inline
		}
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
			PromptTokens:            resp.Usage.PromptTokens,
			CompletionTokens:        resp.Usage.CompletionTokens,
			TotalTokens:             resp.Usage.TotalTokens,
			ReasoningTokens:         resp.Usage.reasoningTokens(),
			ReasoningTokensReported: resp.Usage.reasoningTokensReported(),
		},
	}, nil
}

// ---------------------------------------------------------------------------
// ChatStream (streaming with think-tag extraction)
// ---------------------------------------------------------------------------

// ChatStream sends a streaming /v1/chat/completions request and invokes fn
// for each chunk. Content is processed through a per-request ThinkParser to
// separate reasoning from content in real time.
//
// Tool-call emission policy: OpenAI streams tool-call arguments as
// fragments that are only meaningful once concatenated, so this provider
// accumulates them silently across deltas and surfaces the assembled
// ToolCalls slice only on the final Done chunk. Consumers building
// incremental tool-call UI should treat the Done chunk as the single
// authoritative source for tool-call payloads on a given stream.
//
// Cancellation policy mirrors OllamaProvider: if the context is cancelled
// after at least one chunk has been received, the parser is flushed and a
// final synthetic Done+Partial chunk is emitted (carrying whatever tool
// calls had been accumulated so far) before the context error is returned.
func (p *Provider) ChatStream(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
	if fn == nil {
		return fmt.Errorf("provider: openaicompat: chat stream: callback function is required")
	}

	body := toChatRequest(req, true)
	reader, err := p.client.postSSE(ctx, "/v1/chat/completions", body)
	if err != nil {
		return fmt.Errorf("provider: openaicompat: chat stream: %w", err)
	}
	defer func() { _ = reader.Close() }()

	var (
		lastModel      string
		lastToolCalls  []provider.ToolCall
		lastUsage      provider.Usage
		chunksReceived int
		callbackErr    error
		streamErr      error
		finished       bool
		seenUsage      bool
		toolCalls      streamToolCallAccumulator
	)

	mode := p.effectiveThinkMode(req)
	parser := provider.NewThinkParser(provider.ThinkParserConfig{
		Mode: mode,
		Tags: p.effectiveThinkTags(req),
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
	if mode == provider.ThinkToggle {
		parser.SetActive(thinkToggleActive(req.Options))
	}

	for {
		payload, readErr := reader.Next()
		if readErr != nil {
			if !errors.Is(readErr, errStreamDone) {
				streamErr = readErr
			}
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
			seenUsage = true
			lastUsage = provider.Usage{
				PromptTokens:            chunk.Usage.PromptTokens,
				CompletionTokens:        chunk.Usage.CompletionTokens,
				TotalTokens:             chunk.Usage.TotalTokens,
				ReasoningTokens:         chunk.Usage.reasoningTokens(),
				ReasoningTokensReported: chunk.Usage.reasoningTokensReported(),
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if finished {
			continue
		}
		choice := chunk.Choices[0]

		if len(choice.Delta.ToolCalls) > 0 {
			lastToolCalls = toolCalls.Add(choice.Delta.ToolCalls)
		}
		// Native reasoning text arrives on its own delta field, separate from
		// content and from the inline-tag parser. Emit it directly as a Thinking
		// delta, unconditionally — the server has already classified it. Unlike
		// the non-streaming path (native field takes precedence over inline
		// <think> tags), a stream can't reconcile the two mid-flight, so a
		// backend emitting both would surface both. Backends use one mechanism
		// or the other, so this asymmetry is harmless in practice.
		if choice.Delta.ReasoningContent != "" {
			if err := fn(provider.ChatResponse{Model: lastModel, Provider: p.name, Thinking: choice.Delta.ReasoningContent}); err != nil {
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
			continue
		}
	}

	if callbackErr != nil {
		return callbackErr
	}
	if ctx.Err() != nil {
		if chunksReceived > 0 && !finished {
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
		}
		return fmt.Errorf("provider: openaicompat: chat stream: %w", ctx.Err())
	}
	if streamErr != nil {
		return fmt.Errorf("provider: openaicompat: chat stream: %w", streamErr)
	}
	if finished && !seenUsage {
		return fmt.Errorf("provider: openaicompat: chat stream: ended before usage chunk")
	}
	if finished {
		model := lastModel
		if model == "" {
			model = req.Model
		}
		if err := fn(provider.ChatResponse{
			Model:     model,
			Provider:  p.name,
			ToolCalls: lastToolCalls,
			Done:      true,
			Usage:     lastUsage,
		}); err != nil {
			return err
		}
		return nil
	}
	if !finished {
		return fmt.Errorf("provider: openaicompat: chat stream: ended before final chunk")
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
	defer func() { _ = reader.Close() }()

	var (
		lastModel      string
		lastUsage      provider.Usage
		chunksReceived int
		callbackErr    error
		streamErr      error
		finished       bool
	)

	for {
		payload, readErr := reader.Next()
		if readErr != nil {
			if !errors.Is(readErr, errStreamDone) {
				streamErr = readErr
			}
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

	if ctx.Err() != nil {
		if chunksReceived > 0 && !finished {
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
		}
		return fmt.Errorf("provider: openaicompat: generate stream: %w", ctx.Err())
	}
	if streamErr != nil {
		return fmt.Errorf("provider: openaicompat: generate stream: %w", streamErr)
	}
	if !finished {
		return fmt.Errorf("provider: openaicompat: generate stream: ended before final chunk")
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

// embedKey builds a singleflight dedup key from model + length-prefixed
// inputs. Length prefixes keep distinct inputs with embedded NUL bytes
// from colliding.
//
// The "model:hexhash" layout is collision-free because hex.EncodeToString
// produces only [0-9a-f] — a model name containing a literal ':' cannot
// be confused with the separator+hash boundary. If this is ever changed
// to a non-hex encoding (e.g. base64 with ':' in its alphabet) the
// separator must be re-chosen.
func embedKey(model string, inputs []string) string {
	h := sha256.New()
	var lenBuf [8]byte
	for _, input := range inputs {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(input)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(input))
	}
	return model + ":" + hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// Thinking helpers
// ---------------------------------------------------------------------------

// shouldExtractThinking mirrors OllamaProvider's per-request decision.
func (p *Provider) shouldExtractThinking(req provider.ChatRequest) bool {
	switch p.effectiveThinkMode(req) {
	case provider.ThinkNone:
		return false
	case provider.ThinkToggle:
		return thinkToggleActive(req.Options)
	default:
		return true
	}
}

// thinkToggleActive reports whether a ThinkToggle parser should be active for
// the given options: explicit Think wins; a bare effort hint implies on.
// Mirrors provider/ollama.go; keep the pair in sync.
func thinkToggleActive(opts provider.ModelOptions) bool {
	if opts.Think != nil {
		return *opts.Think
	}
	return opts.ThinkEffort != ""
}

// effectiveThinkMode returns the parser mode for a chat request, honoring the
// per-request ParseThinkMode override before the provider-instance default.
// Mirrors provider/ollama.go; keep the pair in sync.
func (p *Provider) effectiveThinkMode(req provider.ChatRequest) provider.ThinkMode {
	if req.ParseThinkMode != nil {
		return *req.ParseThinkMode
	}
	return p.thinkMode
}

// effectiveThinkTags returns the parser tags for a chat request, honoring the
// per-request ParseThinkTags override before the provider-instance default.
// Mirrors provider/ollama.go; keep the pair in sync.
func (p *Provider) effectiveThinkTags(req provider.ChatRequest) provider.ThinkTags {
	if req.ParseThinkTags != nil {
		return *req.ParseThinkTags
	}
	return p.thinkTags
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
		Model:      req.Model,
		Messages:   msgs,
		Stream:     stream,
		Tools:      toWireTools(req.Tools),
		ToolChoice: req.ToolChoice,
	}
	if stream {
		r.StreamOptions = &chatStreamOptions{IncludeUsage: true}
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
	// Think controls (#220). Explicit Think=false wins: no effort is sent
	// and the template is asked not to think. A bare effort implies on.
	if opts.Think == nil && opts.ThinkEffort == "" {
		return // pre-#220 byte-identical request
	}
	active := thinkToggleActive(opts) // single source of think precedence
	r.ChatTemplateKwargs = map[string]any{"enable_thinking": active}
	if active && opts.ThinkEffort != "" {
		r.ReasoningEffort = opts.ThinkEffort
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
//
// Note: provider.ToolCallFunction.Index is intentionally NOT copied to
// chatToolCall.Index here. OpenAI's spec treats `index` as a response-only
// field used to correlate streaming deltas; sending it on an outbound
// request can be rejected by strict server-side schema validators
// (notably some self-hosted vLLM and llama.cpp setups). The asymmetry is
// intentional and parallels what an OpenAI-spec-conformant client must do —
// it is NOT a missing-feature gap relative to OllamaProvider, which
// preserves Function.Index outbound because Ollama's wire shape accepts
// it.
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
				Arguments: encodeToolCallArguments(c.Function.Arguments),
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
		index := i
		if c.Index != nil {
			index = *c.Index
		}
		out[i] = provider.ToolCall{
			ID:   c.ID,
			Type: firstNonEmpty(c.Type, "function"),
			Function: provider.ToolCallFunction{
				Index:     index,
				Name:      c.Function.Name,
				Arguments: normalizeToolCallArguments(c.Function.Arguments),
			},
		}
	}
	return out
}

type streamToolCallAccumulator struct {
	order        []int
	calls        map[int]*streamToolCall
	lastIndex    int
	hasLastIndex bool
}

type streamToolCall struct {
	id        string
	callType  string
	name      string
	arguments string
	argMode   fragmentMode
}

// fragmentMode locks the argument-fragment encoding for a single tool call
// after the first non-empty fragment arrives. Once locked, subsequent
// fragments are decoded with the same mode so a server that switches
// between JSON-string envelopes and raw partial JSON mid-stream cannot
// produce an unparseable concatenation.
type fragmentMode uint8

const (
	fragModeUnknown fragmentMode = iota
	fragModeEncoded              // fragment was a JSON-encoded string envelope
	fragModeRaw                  // fragment was raw partial JSON
)

// Add merges OpenAI streaming tool-call deltas, whose arguments commonly arrive
// as fragments keyed by tool_calls[].index.
//
// Index resolution precedence per delta:
//  1. delta.Index when set — the canonical OpenAI signal.
//  2. The most-recently-used index (lastIndex) when at least one tool call
//     has already been observed — covers servers that send Index on the
//     opening delta and omit it on subsequent argument-only fragments
//     (the common single-tool case).
//  3. Loop position — bootstrap fallback for the very first delta when no
//     index has been seen yet.
//
// The lastIndex fallback is wrong in the pathological multi-tool case where
// a server sends Index on the opening deltas but then emits arg-only deltas
// with neither Index nor a way to disambiguate which call they extend.
// OpenAI's reference servers do not produce that shape; the consequence of
// the heuristic in that case is "arguments attach to the most recently
// active call" — degraded but not silently corrupted across the wrong slot.
func (a *streamToolCallAccumulator) Add(deltas []chatToolCall) []provider.ToolCall {
	if a.calls == nil {
		a.calls = make(map[int]*streamToolCall, len(deltas))
	}
	for position, delta := range deltas {
		index := a.resolveIndex(delta, position)
		call := a.calls[index]
		if call == nil {
			call = &streamToolCall{}
			a.calls[index] = call
			a.order = append(a.order, index)
		}
		if delta.ID != "" {
			call.id = delta.ID
		}
		if delta.Type != "" {
			call.callType = delta.Type
		}
		if delta.Function.Name != "" {
			call.name = delta.Function.Name
		}
		fragment, mode := decodeToolCallArgumentFragment(delta.Function.Arguments, call.argMode)
		if call.argMode == fragModeUnknown {
			call.argMode = mode
		}
		call.arguments += fragment
		a.lastIndex = index
		a.hasLastIndex = true
	}
	return a.Snapshot()
}

// resolveIndex implements the index-precedence rules documented on Add.
func (a *streamToolCallAccumulator) resolveIndex(delta chatToolCall, position int) int {
	if delta.Index != nil {
		return *delta.Index
	}
	if a.hasLastIndex {
		return a.lastIndex
	}
	return position
}

func (a *streamToolCallAccumulator) Snapshot() []provider.ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	out := make([]provider.ToolCall, 0, len(a.order))
	for _, index := range a.order {
		call := a.calls[index]
		out = append(out, provider.ToolCall{
			ID:   call.id,
			Type: firstNonEmpty(call.callType, "function"),
			Function: provider.ToolCallFunction{
				Index:     index,
				Name:      call.name,
				Arguments: normalizeToolCallArguments(json.RawMessage(call.arguments)),
			},
		})
	}
	return out
}

// decodeToolCallArgumentFragment extracts the argument-string payload from a
// single streaming-delta fragment. OpenAI's streaming spec is loose: some
// servers send each fragment as a JSON-encoded string ("{\"q\""), others
// send raw partial JSON ({"q") that is only valid once concatenated.
//
// The locked argument forces consistent decoding once a call has seen its
// first non-empty fragment: a server cannot switch from encoded to raw or
// vice versa mid-stream without producing an unparseable concatenation.
// Returns the decoded fragment text plus the mode used (which the caller
// stores on the per-call accumulator state to lock subsequent fragments).
//
// fragModeUnknown locked input lets the function probe the first fragment;
// the locked return is then enforced on subsequent fragments by the caller.
func decodeToolCallArgumentFragment(raw json.RawMessage, locked fragmentMode) (string, fragmentMode) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", locked
	}
	switch locked {
	case fragModeEncoded:
		var s string
		if err := json.Unmarshal(trimmed, &s); err == nil {
			return s, fragModeEncoded
		}
		// Server violated its own mode — best-effort fall through as raw.
		return string(trimmed), fragModeEncoded
	case fragModeRaw:
		return string(trimmed), fragModeRaw
	}
	// fragModeUnknown — probe the first fragment to lock the mode.
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		return s, fragModeEncoded
	}
	return string(trimmed), fragModeRaw
}

// normalizeToolCallArguments unwraps OpenAI's JSON-string envelope around
// tool-call arguments and returns the inner JSON value, OR re-encodes a
// raw payload as the canonical string envelope when the input is already
// loose JSON. Empty / "null" inputs collapse to nil so downstream consumers
// see "no arguments" uniformly.
//
// Scalar JSON literals inside the string envelope (e.g. "42", "true",
// "null") are preserved as JSON-encoded strings rather than unwrapped to
// bare scalars — the inner string was the original argument value, and
// type-narrowing to a JSON number/bool would silently change the data
// shape the tool implementation receives. Object and array literals ARE
// unwrapped because that is the canonical tool-call shape providers
// actually consume.
func normalizeToolCallArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		if s == "" {
			return nil
		}
		// Only unwrap when the inner value is a JSON object or array — the
		// canonical tool-call shape. Bare scalars (numbers, bools, "null")
		// stay wrapped to preserve the originally-emitted type.
		if isJSONObjectOrArray(s) {
			return json.RawMessage(append([]byte(nil), s...))
		}
		return mustJSONRawString(s)
	}
	if json.Valid(trimmed) {
		return json.RawMessage(append([]byte(nil), trimmed...))
	}
	return mustJSONRawString(string(trimmed))
}

// isJSONObjectOrArray reports whether s begins with '{' or '[' after trim
// AND is well-formed JSON. Used by normalizeToolCallArguments to decide
// when an inner JSON-string-encoded value is safe to unwrap (object/array)
// vs must be preserved as a JSON-encoded string (scalar literals).
func isJSONObjectOrArray(s string) bool {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if trimmed == "" {
		return false
	}
	switch trimmed[0] {
	case '{', '[':
		return json.Valid([]byte(s))
	}
	return false
}

// encodeToolCallArguments produces the canonical OpenAI outbound shape for
// tool-call arguments: a JSON-encoded string of the inner payload. It
// accepts EITHER raw JSON (e.g. `{"q":"go"}` from upstream provider-side
// callers) OR an already-encoded JSON string (when round-tripping
// previously-received arguments back to the server) and emits the
// already-encoded form in both cases.
//
// Empty input AND the literal "null" both collapse to nil — the outbound
// wire then omits the arguments field entirely (json.RawMessage(nil) +
// omitempty). OpenAI does not distinguish "arguments: null" from
// arguments-omitted at the schema level today, so a caller passing the
// "null" literal to mean "no arguments" gets the same outbound shape as
// passing a nil RawMessage.
func encodeToolCallArguments(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		// Already JSON-string-encoded — pass through verbatim.
		return json.RawMessage(append([]byte(nil), trimmed...))
	}
	// Raw JSON — wrap as the OpenAI canonical string envelope.
	return mustJSONRawString(string(trimmed))
}

// mustJSONRawString JSON-encodes s as a string literal and returns it as
// a json.RawMessage. json.Marshal of a string is documented as infallible
// for valid UTF-8; the nil return on error is reachable only for invalid
// UTF-8 in tool-call arguments, which is already broken at the upstream
// model and would surface as missing arguments on the outbound wire — the
// least-bad failure mode.
func mustJSONRawString(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// firstNonEmpty returns the first non-empty string in values, or "" when
// all values are empty. Used to default tool-call Type to "function" when
// neither the response nor a delta carries the field — OpenAI's spec
// only defines "function" today, but storing whatever the server emits
// keeps the door open to future tool types without round-trip loss.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Compile-time assertion that *Provider satisfies provider.Provider.
var _ provider.Provider = (*Provider)(nil)
