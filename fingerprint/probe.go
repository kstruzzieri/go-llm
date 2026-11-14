package fingerprint

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kstruzzieri/go-llm/internal/responsebound"
	"github.com/kstruzzieri/go-llm/ollama"
)

// OllamaProber queries an Ollama backend to detect model kind and
// collect performance metrics for building a fingerprint Profile.
type OllamaProber struct {
	client *ollama.Client

	// requestTimeout replaces the recipe's timeout in tests; zero uses the
	// recipe's.
	requestTimeout time.Duration
}

// NewProber creates an OllamaProber using the given Ollama client.
func NewProber(client *ollama.Client) *OllamaProber {
	return &OllamaProber{client: client}
}

// DetectKind determines the model's kind using a three-tier strategy:
//  1. Capabilities array from /api/show (Ollama 0.5.x+)
//  2. Family/template heuristics (older Ollama versions)
//  3. Live probe: try chat, then embedding
//
// Returns a KindDetection describing the result and how it was determined.
func (p *OllamaProber) DetectKind(ctx context.Context, model string) (*KindDetection, error) {
	info, err := p.client.ShowModel(ctx, model)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: detect kind %q: %w", model, err)
	}

	// Tier 1: explicit capabilities
	if len(info.Capabilities) > 0 {
		kind := InferKindFromCapabilities(info.Capabilities)
		if kind != ModelKindUnknown {
			return &KindDetection{
				Kind:         kind,
				Capabilities: info.Capabilities,
				Family:       info.Family,
				Source:       "capabilities",
			}, nil
		}
	}

	// Tier 2: family/template heuristic
	kind := InferKind(info.Family, info.Template)
	if kind != ModelKindUnknown {
		return &KindDetection{
			Kind:         kind,
			Capabilities: info.Capabilities,
			Family:       info.Family,
			Source:       "heuristic",
		}, nil
	}

	// Tier 3: live probe — try chat first, then embedding
	det, err := p.probeKind(ctx, model, info)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: detect kind %q: %w", model, err)
	}
	return det, nil
}

// probeKind attempts to classify an unknown model by making live API calls.
func (p *OllamaProber) probeKind(ctx context.Context, model string, info *ollama.ModelInfo) (*KindDetection, error) {
	// Try chat
	_, chatErr := p.client.Chat(ctx, ollama.ChatRequest{
		Model:    model,
		Messages: []ollama.ChatMessage{{Role: "user", Content: "hi"}},
		Options:  &ollama.ModelOptions{NumPredict: 1},
	})
	if chatErr == nil {
		return &KindDetection{
			Kind:         ModelKindChat,
			Capabilities: info.Capabilities,
			Family:       info.Family,
			Source:       "probe",
		}, nil
	}

	// Try embedding
	_, embedErr := p.client.Embed(ctx, model, "test")
	if embedErr == nil {
		return &KindDetection{
			Kind:         ModelKindEmbedding,
			Capabilities: info.Capabilities,
			Family:       info.Family,
			Source:       "probe",
		}, nil
	}

	// Both failed — return unknown rather than an error, since the model
	// exists but we can't classify it.
	return &KindDetection{
		Kind:         ModelKindUnknown,
		Capabilities: info.Capabilities,
		Family:       info.Family,
		Source:       "probe",
	}, nil
}

// Compile-time interface checks.
var (
	_ ModelProber      = (*OllamaProber)(nil)
	_ ToolCallProber   = (*OllamaProber)(nil)
	_ CapabilityProber = (*OllamaProber)(nil)
)

// ProbeCapabilities answers from /api/show metadata with one bounded request
// under the ollama-show recipe's timeout and byte limit: no generation, no
// model load. Every answer comes from OllamaCapabilityFacts with source
// provider. A requested capability the metadata does not answer is an
// inconclusive attempt, never an invented no. A failed request answers
// nothing.
func (p *OllamaProber) ProbeCapabilities(ctx context.Context, model string, req CapabilityProbeRequest) (CapabilityProbeResult, error) {
	recipe, err := RecipeForRequest(req, AdapterOllama)
	if err != nil {
		return CapabilityProbeResult{}, fmt.Errorf("fingerprint: ollama probe %q: %w", model, err)
	}
	reqCtx, cancel := context.WithTimeout(responsebound.WithLimit(ctx, recipe.MaxResponseBytes), cmp.Or(p.requestTimeout, recipe.RequestTimeout))
	defer cancel()
	result := CapabilityProbeResult{Method: recipe.Method, Requests: 1}
	info, err := p.client.ShowModel(reqCtx, model)
	if err != nil {
		return result, fmt.Errorf("fingerprint: ollama probe %q: %w", model, err)
	}
	facts := OllamaCapabilityFacts(info.Capabilities, info.Template)
	for _, capability := range recipe.Capabilities {
		answer := ProbeAnswer{Capability: capability, Source: EvidenceSourceProvider}
		if state, ok := facts[capability]; ok {
			answer.Kind, answer.State = EvidenceKindAnswer, state
		} else if slices.Contains(req.Capabilities, capability) {
			answer.Kind, answer.State, answer.TTL = EvidenceKindAttempt, CapProbeInconclusive, CapProbeInconclusiveTTL
		} else {
			continue
		}
		result.Answers = append(result.Answers, answer)
	}
	return result, nil
}

// ProbeToolCall determines tool support passively from /api/show model
// metadata — no generation request, no model load — through the same
// OllamaCapabilityFacts mapping as ProbeCapabilities, so "Tools" and
// "tool_call" read yes. A present capabilities array is authoritative in both
// directions; a missing array (older Ollama) is inconclusive, never a hard no.
func (p *OllamaProber) ProbeToolCall(ctx context.Context, model string) (CapProbeOutcome, error) {
	info, err := p.client.ShowModel(ctx, model)
	if err != nil {
		return CapProbeOutcome{}, fmt.Errorf("fingerprint: ollama tool probe %q: %w", model, err)
	}
	if info.Capabilities == nil {
		return CapProbeOutcome{
			State:  CapProbeInconclusive,
			TTL:    CapProbeInconclusiveTTL,
			Detail: "ollama /api/show has no capabilities array",
		}, nil
	}
	if OllamaCapabilityFacts(info.Capabilities, info.Template)["tool_call"] == CapProbeYes {
		return CapProbeOutcome{State: CapProbeYes, Detail: "ollama capabilities lists tools"}, nil
	}
	return CapProbeOutcome{State: CapProbeNo, Detail: "ollama capabilities array lacks tools"}, nil
}

// ProbeChat sends a minimal chat request and extracts performance metrics
// from Ollama's timing fields. The opts parameter accepts *ollama.ModelOptions
// or nil for defaults.
func (p *OllamaProber) ProbeChat(ctx context.Context, model string, opts interface{}) (*ChatMetrics, error) {
	var modelOpts *ollama.ModelOptions
	if opts != nil {
		var ok bool
		modelOpts, ok = opts.(*ollama.ModelOptions)
		if !ok {
			return nil, fmt.Errorf("fingerprint: probe chat %q: opts must be *ollama.ModelOptions, got %T", model, opts)
		}
	}
	if modelOpts == nil {
		modelOpts = &ollama.ModelOptions{NumPredict: 16}
	}

	start := time.Now()
	resp, err := p.client.Chat(ctx, ollama.ChatRequest{
		Model:    model,
		Messages: []ollama.ChatMessage{{Role: "user", Content: "Say hello."}},
		Options:  modelOpts,
	})
	if err != nil {
		return nil, fmt.Errorf("fingerprint: probe chat %q: %w", model, err)
	}

	metrics := &ChatMetrics{}

	// Token throughput from eval_count / eval_duration
	if resp.EvalDuration > 0 && resp.EvalCount > 0 {
		evalSecs := float64(resp.EvalDuration) / 1e9
		metrics.TokensPerSecond = float64(resp.EvalCount) / evalSecs
	}

	// Prompt eval latency
	if resp.PromptEvalDuration > 0 {
		metrics.PromptLatency = time.Duration(resp.PromptEvalDuration)
	}

	// Cold start: if load_duration is significant (>100ms), the model
	// was loaded from disk for this request.
	if resp.LoadDuration > 100_000_000 { // 100ms in nanoseconds
		metrics.ColdStartLatency = time.Since(start)
	}

	return metrics, nil
}

// ProbeEmbedding sends a minimal embedding request and captures dimension
// and latency metrics.
func (p *OllamaProber) ProbeEmbedding(ctx context.Context, model string) (*EmbeddingMetrics, error) {
	start := time.Now()
	vec, err := p.client.Embed(ctx, model, "The quick brown fox jumps over the lazy dog.")
	if err != nil {
		var apiErr *ollama.APIError
		if errors.As(err, &apiErr) {
			return nil, fmt.Errorf("fingerprint: probe embedding %q: %w", model, apiErr)
		}
		return nil, fmt.Errorf("fingerprint: probe embedding %q: %w", model, err)
	}

	return &EmbeddingMetrics{
		Latency: time.Since(start),
		Dim:     len(vec),
	}, nil
}
