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

// ProbeChat and ProbeEmbedding are implemented in Task 8.
func (p *OpenAICompatProber) ProbeChat(ctx context.Context, model string, opts any) (*fingerprint.ChatMetrics, error) {
	return nil, fmt.Errorf("fingerprint: openaicompat ProbeChat not implemented")
}

func (p *OpenAICompatProber) ProbeEmbedding(ctx context.Context, model string) (*fingerprint.EmbeddingMetrics, error) {
	return nil, fmt.Errorf("fingerprint: openaicompat ProbeEmbedding not implemented")
}
