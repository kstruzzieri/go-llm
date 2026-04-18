package completion

import (
	"context"
	"fmt"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

const (
	// fimCtxCeiling is the default upper bound on num_ctx for FIM requests.
	// Small enough to keep KV-cache warm for typical IDE completions.
	fimCtxCeiling = 8192
	// fimCtxCeilingLarge is used when input tokens already exceed fimCtxCeiling,
	// so the request can fit but remains capped to avoid blowing out memory.
	fimCtxCeilingLarge = 16384

	defaultMaxTokens   = 128
	defaultTemperature = 0.2

	// prefixBudgetPercent is the fraction of available context allocated to
	// prefix when cfg.FIM.PrefixBudgetPct is not set. Task 9 replaces this
	// with a per-family value from ComputeBudget.
	prefixBudgetPercent = 75
)

// FIMRequest represents a Fill-in-the-Middle completion request.
type FIMRequest struct {
	Prefix    string // code before cursor
	Suffix    string // code after cursor
	FilePath  string // for language detection
	MaxTokens int    // max tokens to generate (default: adaptive)
	Language  string // auto-detect from FilePath if empty
	Trace     bool   // when true, FIMResponse.BudgetTrace is populated
}

// FIMResponse contains the result of a Fill-in-the-Middle completion.
type FIMResponse struct {
	Completion      string          // generated code with stop tokens stripped
	Tokens          int             // number of tokens generated
	LatencyMs       int64           // round-trip latency in milliseconds
	CursorContext   CursorContext   // detected cursor context
	CompletionShape CompletionShape // inferred completion shape
	BudgetTrace     *BudgetTrace    // populated when FIMRequest.Trace is true
}

// Provider performs FIM (Fill-in-the-Middle) completions against an Ollama backend.
type Provider struct {
	client *ollama.Client
	model  string
	cfg    ProviderConfig
}

// NewProvider creates a completion Provider for the given model.
// Returns an error if cfg is invalid or cfg.FIM is nil.
func NewProvider(client *ollama.Client, model string, cfg ProviderConfig) (*Provider, error) {
	if cfg.FIM == nil {
		return nil, fmt.Errorf("completion: FIM config is required")
	}
	if err := cfg.FIM.Validate(); err != nil {
		return nil, fmt.Errorf("completion: %w", err)
	}
	if cfg.ContextWindow <= 0 {
		return nil, fmt.Errorf("completion: context window must be positive, got %d", cfg.ContextWindow)
	}
	return &Provider{
		client: client,
		model:  model,
		cfg:    cfg,
	}, nil
}

// effectiveNumCtx returns the num_ctx to use for a request given the input
// token count. Caps at fimCtxCeiling for typical requests, escalating to
// fimCtxCeilingLarge when input already exceeds the small ceiling. Never
// exceeds the model's configured context window.
func (p *Provider) effectiveNumCtx(inputTokens int) int {
	ceiling := fimCtxCeiling
	if inputTokens > fimCtxCeiling {
		ceiling = fimCtxCeilingLarge
	}
	if p.cfg.ContextWindow < ceiling {
		return p.cfg.ContextWindow
	}
	return ceiling
}

// Complete generates an inline completion synchronously.
func (p *Provider) Complete(ctx context.Context, req FIMRequest) (*FIMResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("completion: client is required")
	}
	if p.model == "" {
		return nil, fmt.Errorf("completion: model is required")
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
	if p.client == nil {
		return fmt.Errorf("completion: client is required")
	}
	if p.model == "" {
		return fmt.Errorf("completion: model is required")
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
// Uses the model's FIM tokens from cfg. Task 9 will replace this with full
// ComputeBudget integration; for now it preserves the existing non-adaptive
// budget behavior while switching to catalog-driven FIM tokens.
func (p *Provider) buildRequest(req FIMRequest) ollama.GenerateRequest {
	overhead := p.cfg.FIM.TokenOverhead()

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	numCtx := p.effectiveNumCtx(EstimateTokens(req.Prefix) + EstimateTokens(req.Suffix))

	maxLimit := numCtx - overhead - 1
	if maxLimit < 1 {
		maxLimit = 1
	}
	if maxTokens > maxLimit {
		maxTokens = maxLimit
	}

	availableCtx := numCtx - maxTokens - overhead
	if availableCtx < 0 {
		availableCtx = 0
	}

	familyPct := p.cfg.FIM.PrefixBudgetPct
	if familyPct == 0 {
		familyPct = prefixBudgetPercent
	}
	prefixBudget := availableCtx * familyPct / 100
	suffixBudget := availableCtx - prefixBudget

	prefixTokens := EstimateTokens(req.Prefix)
	suffixTokens := EstimateTokens(req.Suffix)
	if prefixTokens < prefixBudget {
		suffixBudget += prefixBudget - prefixTokens
		prefixBudget = prefixTokens
	} else if suffixTokens < suffixBudget {
		prefixBudget += suffixBudget - suffixTokens
		suffixBudget = suffixTokens
	}

	prefix := TruncateToTokens(req.Prefix, prefixBudget)
	suffix := TruncateSuffixToTokens(req.Suffix, suffixBudget)

	prompt := assembleFIMPrompt(p.cfg.FIM, prefix, suffix)

	return ollama.GenerateRequest{
		Model:  p.model,
		Prompt: prompt,
		Options: &ollama.ModelOptions{
			NumPredict:  maxTokens,
			NumCtx:      numCtx,
			Temperature: defaultTemperature,
		},
	}
}
