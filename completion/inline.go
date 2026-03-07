package completion

import (
	"context"
	"fmt"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

const (
	fimPrefix = "<|fim_prefix|>"
	fimSuffix = "<|fim_suffix|>"
	fimMiddle = "<|fim_middle|>"

	defaultMaxTokens   = 128
	defaultNumCtx      = 2048
	defaultTemperature = 0.2

	// fimTokenOverhead accounts for the 3 FIM special tokens in the context budget.
	fimTokenOverhead = 3
	// prefixBudgetPercent is the percentage of available context allocated to prefix.
	prefixBudgetPercent = 75
)

// FIMRequest represents a Fill-in-the-Middle completion request.
type FIMRequest struct {
	Prefix    string // code before cursor
	Suffix    string // code after cursor
	FilePath  string // for language detection
	MaxTokens int    // max tokens to generate (default: 128)
	Language  string // auto-detect from FilePath if empty
}

// FIMResponse contains the result of a Fill-in-the-Middle completion.
type FIMResponse struct {
	Completion string // generated code
	Tokens     int    // number of tokens generated
	LatencyMs  int64  // round-trip latency in milliseconds
}

// Provider generates inline completions using Ollama's /api/generate endpoint
// with Fill-in-the-Middle prompting.
type Provider struct {
	client *ollama.Client
	model  string
}

// NewProvider creates a completion Provider targeting the given Ollama model.
// The model should support FIM tokens (e.g. qwen2.5-coder).
func NewProvider(client *ollama.Client, model string) *Provider {
	return &Provider{
		client: client,
		model:  model,
	}
}

// Complete generates an inline completion synchronously.
func (p *Provider) Complete(ctx context.Context, req FIMRequest) (*FIMResponse, error) {
	if p.model == "" {
		return nil, fmt.Errorf("completion: model is required")
	}
	if req.Prefix == "" {
		return nil, fmt.Errorf("completion: prefix is required")
	}

	genReq := p.buildRequest(req)
	start := time.Now()

	resp, err := p.client.Generate(ctx, genReq)
	if err != nil {
		return nil, fmt.Errorf("completion: %w", err)
	}

	return &FIMResponse{
		Completion: resp.Response,
		Tokens:     resp.EvalCount,
		LatencyMs:  time.Since(start).Milliseconds(),
	}, nil
}

// CompleteStream generates a streaming inline completion, calling fn for each token.
// If fn returns an error, streaming stops and that error is returned.
func (p *Provider) CompleteStream(ctx context.Context, req FIMRequest, fn func(token string) error) error {
	if p.model == "" {
		return fmt.Errorf("completion: model is required")
	}
	if req.Prefix == "" {
		return fmt.Errorf("completion: prefix is required")
	}
	if fn == nil {
		return fmt.Errorf("completion: callback function is required")
	}

	genReq := p.buildRequest(req)

	return p.client.GenerateStream(ctx, genReq, func(resp ollama.GenerateResponse) error {
		if resp.Response != "" {
			return fn(resp.Response)
		}
		return nil
	})
}

// buildRequest constructs an Ollama GenerateRequest with FIM prompt format.
// It enforces the context window budget by truncating prefix and suffix to fit
// within num_ctx, reserving space for generation tokens and FIM special tokens.
func (p *Provider) buildRequest(req FIMRequest) ollama.GenerateRequest {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	// Budget the context window: total - generation - FIM overhead
	availableCtx := defaultNumCtx - maxTokens - fimTokenOverhead
	if availableCtx < 0 {
		availableCtx = 0
	}
	prefixBudget := availableCtx * prefixBudgetPercent / 100
	suffixBudget := availableCtx - prefixBudget

	prefix := TruncateToTokens(req.Prefix, prefixBudget)
	suffix := TruncateSuffixToTokens(req.Suffix, suffixBudget)

	prompt := fimPrefix + prefix + fimSuffix + suffix + fimMiddle

	return ollama.GenerateRequest{
		Model:  p.model,
		Prompt: prompt,
		Options: &ollama.ModelOptions{
			NumPredict:  maxTokens,
			NumCtx:      defaultNumCtx,
			Temperature: defaultTemperature,
		},
	}
}

