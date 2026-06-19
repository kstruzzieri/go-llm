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

// ProbeChat sends a minimal chat request and derives throughput from the
// reported completion-token count over measured wall-clock. opts is ignored
// (kept for ModelProber parity); openai-compat exposes no server-side timing
// breakdown, so PromptLatency/ColdStartLatency are left zero.
func (p *OpenAICompatProber) ProbeChat(ctx context.Context, model string, opts any) (*fingerprint.ChatMetrics, error) {
	start := time.Now()
	resp, err := p.prov.Chat(ctx, provider.ChatRequest{
		Model:    model,
		Messages: []provider.ChatMessage{{Role: "user", Content: "Say hello."}},
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
