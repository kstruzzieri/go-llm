// Package probers contains ModelProber implementations that must bridge the
// fingerprint abstraction to backends living in the provider layer.
//
// fingerprint is a low-level package: provider depends on it (see
// provider/model_registry.go), so fingerprint cannot import provider or
// provider/openaicompat without creating an import cycle. The OllamaProber
// avoids this by reaching down to the provider-free ollama client and so lives
// in fingerprint itself. The OpenAICompatProber instead drives the
// provider/openaicompat adapter, which sits above fingerprint; it therefore
// lives here, where importing both layers is allowed.
package probers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// OpenAICompatProber probes an OpenAI-compatible backend (llama.cpp via
// llama-server, vLLM, LM Studio) to detect model kind and collect performance
// metrics. Unlike OllamaProber it has no /api/show metadata to inspect; when
// model-config capabilities are available they should be passed as a hint so
// the profiler can benchmark every declared capability.
type OpenAICompatProber struct {
	prov         *openaicompat.Provider
	capabilities []string
}

// OpenAICompatProberOption configures an OpenAICompatProber.
type OpenAICompatProberOption func(*OpenAICompatProber)

// WithOpenAICompatCapabilities provides authoritative model-config
// capabilities for a model. These are returned from DetectKind with
// Source "capabilities", allowing Profiler.selectProbes to run both chat and
// embedding probes when a model declares both capabilities.
func WithOpenAICompatCapabilities(caps []string) OpenAICompatProberOption {
	return func(p *OpenAICompatProber) {
		p.capabilities = append([]string(nil), caps...)
	}
}

// NewOpenAICompatProber creates a prober backed by an openai-compat Provider.
func NewOpenAICompatProber(prov *openaicompat.Provider, opts ...OpenAICompatProberOption) *OpenAICompatProber {
	p := &OpenAICompatProber{prov: prov}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Compile-time check.
var _ fingerprint.ModelProber = (*OpenAICompatProber)(nil)

// DetectKind classifies the model using configured capabilities when present,
// otherwise by live probe: chat first, then embedding.
func (p *OpenAICompatProber) DetectKind(ctx context.Context, model string) (*fingerprint.KindDetection, error) {
	if err := p.ensureModelExists(ctx, model); err != nil {
		return nil, err
	}

	if len(p.capabilities) > 0 {
		caps := append([]string(nil), p.capabilities...)
		return &fingerprint.KindDetection{
			Kind:         fingerprint.InferKindFromCapabilities(caps),
			Capabilities: caps,
			Source:       "capabilities",
		}, nil
	}

	if _, err := p.prov.Chat(ctx, provider.ChatRequest{
		Model:    model,
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
		Options:  provider.ModelOptions{NumPredict: 1},
	}); err == nil {
		return &fingerprint.KindDetection{Kind: fingerprint.ModelKindChat, Source: "probe"}, nil
	}

	if _, err := p.prov.Embed(ctx, provider.EmbedRequest{
		Model: model,
		Input: []string{"test"},
	}); err == nil {
		return &fingerprint.KindDetection{Kind: fingerprint.ModelKindEmbedding, Source: "probe"}, nil
	}

	// Model exists but we cannot classify it — return unknown, not an error.
	return &fingerprint.KindDetection{Kind: fingerprint.ModelKindUnknown, Source: "probe"}, nil
}

func (p *OpenAICompatProber) ensureModelExists(ctx context.Context, model string) error {
	if p == nil || p.prov == nil {
		return fmt.Errorf("fingerprint: openaicompat detect kind %q: provider is required", model)
	}
	models, err := p.prov.Models(ctx)
	if err != nil {
		return fmt.Errorf("fingerprint: openaicompat detect kind %q: list models: %w", model, err)
	}
	for _, m := range models {
		if m.Name == model {
			return nil
		}
	}
	return fmt.Errorf("fingerprint: openaicompat detect kind %q: model not listed by /v1/models", model)
}

// ProbeChat sends a minimal chat request and derives throughput from the
// reported completion-token count over measured wall-clock. opts is ignored
// (kept for ModelProber parity); openai-compat exposes no server-side timing
// breakdown, so PromptLatency/ColdStartLatency are left zero.
func (p *OpenAICompatProber) ProbeChat(ctx context.Context, model string, opts any) (*fingerprint.ChatMetrics, error) {
	start := time.Now()
	resp, err := p.prov.Chat(ctx, provider.ChatRequest{
		Model:    model,
		Messages: []provider.ChatMessage{{Role: "user", Content: "Say hello."}},
		Options:  provider.ModelOptions{NumPredict: 16},
	})
	if err != nil {
		return nil, fmt.Errorf("fingerprint: openaicompat probe chat %q: %w", model, err)
	}
	elapsed := time.Since(start)

	metrics := &fingerprint.ChatMetrics{}
	if resp.Usage.CompletionTokens > 0 && elapsed > 0 {
		metrics.TokensPerSecond = float64(resp.Usage.CompletionTokens) / elapsed.Seconds()
	}
	return metrics, nil
}

// ProbeEmbedding sends a minimal embedding request and captures dimension and
// latency.
func (p *OpenAICompatProber) ProbeEmbedding(ctx context.Context, model string) (*fingerprint.EmbeddingMetrics, error) {
	start := time.Now()
	resp, err := p.prov.Embed(ctx, provider.EmbedRequest{
		Model: model,
		Input: []string{"The quick brown fox jumps over the lazy dog."},
	})
	if err != nil {
		return nil, fmt.Errorf("fingerprint: openaicompat probe embedding %q: %w", model, err)
	}
	// An empty embedding response is a probe failure, not a zero-dimension
	// model: surface it instead of reporting Dim 0, which the profiler would
	// silently treat as "not tested".
	if len(resp.Embeddings) == 0 || len(resp.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("fingerprint: openaicompat probe embedding %q: backend returned no embedding vector", model)
	}
	return &fingerprint.EmbeddingMetrics{Latency: time.Since(start), Dim: len(resp.Embeddings[0])}, nil
}

// Compile-time check: the prober actively probes tool-call support.
var _ fingerprint.ToolCallProber = (*OpenAICompatProber)(nil)

// toolProbeTimeout bounds one tool-call probe request. It must absorb a
// llama-swap model load (seconds to tens of seconds for large models),
// unlike the #218 port-scan probe's 800ms budget.
const toolProbeTimeout = 30 * time.Second

// probeToolSchema is the minimal no-arg function used to elicit a call.
var probeToolSchema = provider.Tool{
	Type: "function",
	Function: provider.ToolFunction{
		Name:        "get_time",
		Description: "Get the current time.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	},
}

// ProbeToolCall actively determines tool-call support with at most two
// chat-completions requests (spec section 3):
//
//	attempt 1: tools + tool_choice "required" (deterministic elicitation)
//	attempt 2 (only after attempt-1 400/422): tools without tool_choice,
//	           prompt-engineered — some servers reject tool_choice but
//	           accept tools.
//
// Verdicts: 200 with tool_calls => yes; 200 without => inconclusive
// (short TTL: the model ignored the request, which varies by template);
// 400/422 on attempt 2 => no (the tools array itself is rejected).
// Auth/endpoint/rate-limit statuses and transport failures return a
// non-nil error — diagnostic only, never persisted.
func (p *OpenAICompatProber) ProbeToolCall(ctx context.Context, model string) (fingerprint.CapProbeOutcome, error) {
	if p == nil || p.prov == nil {
		return fingerprint.CapProbeOutcome{}, fmt.Errorf("fingerprint: openaicompat tool probe %q: provider is required", model)
	}
	ctx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()

	outcome, retry, err := p.toolProbeAttempt(ctx, model, true)
	if err != nil || !retry {
		return outcome, err
	}
	// Attempt 1 was rejected with 400/422: the server may object to
	// tool_choice rather than tools. Re-try without tool_choice; a second
	// 400/422 means the tools array itself is unsupported => hard no.
	outcome, retry, err = p.toolProbeAttempt(ctx, model, false)
	if err != nil {
		return outcome, err
	}
	if retry {
		return fingerprint.CapProbeOutcome{
			State:  fingerprint.CapProbeNo,
			Detail: "server rejected tools request (400/422 on both attempts)",
		}, nil
	}
	return outcome, nil
}

// toolProbeAttempt runs one probe request. retry=true signals a 400/422
// rejection that the caller may escalate past (attempt 1) or convert to a
// hard no (attempt 2).
func (p *OpenAICompatProber) toolProbeAttempt(ctx context.Context, model string, forceToolChoice bool) (fingerprint.CapProbeOutcome, bool, error) {
	temp := 0.0
	req := provider.ChatRequest{
		Model: model,
		Messages: []provider.ChatMessage{
			{Role: "user", Content: "Call the get_time tool to get the current time."},
		},
		Tools: []provider.Tool{probeToolSchema},
		Options: provider.ModelOptions{
			NumPredict:  128, // tool-call JSON needs room; truncation reads as a false negative
			Temperature: &temp,
		},
	}
	if forceToolChoice {
		req.ToolChoice = "required"
	}

	resp, err := p.prov.Chat(ctx, req)
	if err != nil {
		var hs interface{ HTTPStatusCode() int }
		if errors.As(err, &hs) {
			if code := hs.HTTPStatusCode(); code == 400 || code == 422 {
				return fingerprint.CapProbeOutcome{}, true, nil
			}
			// 401/403/404/405/429, 5xx, anything else with a status:
			// says nothing about the model. Diagnostic, not persisted.
			return fingerprint.CapProbeOutcome{}, false, fmt.Errorf("fingerprint: openaicompat tool probe %q: %w", model, err)
		}
		// Transport-level failure (network, timeout, cancel): transient.
		return fingerprint.CapProbeOutcome{}, false, fmt.Errorf("fingerprint: openaicompat tool probe %q: %w", model, err)
	}
	if len(resp.ToolCalls) > 0 {
		return fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes, Detail: "model produced a tool call"}, false, nil
	}
	return fingerprint.CapProbeOutcome{
		State:  fingerprint.CapProbeInconclusive,
		TTL:    fingerprint.CapProbeInconclusiveTTL,
		Detail: "200 response without tool_calls",
	}, false, nil
}
