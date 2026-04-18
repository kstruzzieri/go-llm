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
)

// FIMRequest represents a Fill-in-the-Middle completion request.
type FIMRequest struct {
	Prefix    string // code before cursor
	Suffix    string // code after cursor
	FilePath  string // for language detection
	MaxTokens int    // max tokens to generate (0 means adaptive)
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

// plannedRequest carries the fully-resolved plan for a single FIM round-trip.
type plannedRequest struct {
	genReq   ollama.GenerateRequest
	analysis CursorAnalysis
	budget   ComputedBudget
	language string
	numCtx   int
	maxTok   int
}

// planRequest runs cursor analysis, computes the adaptive budget, truncates
// prefix/suffix, assembles the FIM prompt, and returns everything needed to
// call Ollama and construct the response.
func (p *Provider) planRequest(req FIMRequest) plannedRequest {
	lang := resolveLanguage(req.Language, req.FilePath)
	analysis := AnalyzeCursor(req.Prefix, req.Suffix, lang)
	budget := ComputeBudget(analysis, lang, p.cfg.QualityTier, p.cfg.FIM,
		p.cfg.ContextWindow, req.Prefix, req.Suffix)

	maxTokens := budget.MaxTokens
	if req.MaxTokens > 0 {
		maxTokens = req.MaxTokens
	}

	numCtx := p.effectiveNumCtx(EstimateTokens(req.Prefix) + EstimateTokens(req.Suffix))

	overhead := p.cfg.FIM.TokenOverhead()
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
	prefixBudget := availableCtx * budget.PrefixBudgetPct / 100
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

	genReq := ollama.GenerateRequest{
		Model:  p.model,
		Prompt: prompt,
		Options: &ollama.ModelOptions{
			NumPredict:  maxTokens,
			NumCtx:      numCtx,
			Temperature: budget.Temperature,
			Stop:        budget.StopTokens,
		},
	}

	return plannedRequest{
		genReq:   genReq,
		analysis: analysis,
		budget:   budget,
		language: lang,
		numCtx:   numCtx,
		maxTok:   maxTokens,
	}
}

// buildTrace constructs a BudgetTrace from a planned request.
func buildTrace(pr plannedRequest) *BudgetTrace {
	return &BudgetTrace{
		DetectedContext:    pr.analysis.Context,
		CompletionShape:    pr.analysis.Shape,
		DetectorConfidence: pr.analysis.Confidence,
		DecisionReason:     pr.analysis.Reason,
		Language:           pr.language,
		MaxTokens:          pr.maxTok,
		NumCtx:             pr.numCtx,
		PrefixBudgetPct:    pr.budget.PrefixBudgetPct,
		Temperature:        pr.budget.Temperature,
		ShapeMult:          shapeMultiplier[pr.analysis.Shape],
		StopTokenCount:     len(pr.budget.StopTokens),
	}
}

// Complete generates an inline completion synchronously.
func (p *Provider) Complete(ctx context.Context, req FIMRequest) (*FIMResponse, error) {
	if p.client == nil {
		return nil, fmt.Errorf("completion: client is required")
	}
	if p.model == "" {
		return nil, fmt.Errorf("completion: model is required")
	}

	pr := p.planRequest(req)
	start := time.Now()

	resp, err := p.client.Generate(ctx, pr.genReq)
	if err != nil {
		return nil, fmt.Errorf("completion: %w", err)
	}

	completion := stripStopTokens(resp.Response, pr.budget.StopTokens)

	result := &FIMResponse{
		Completion:      completion,
		Tokens:          resp.EvalCount,
		LatencyMs:       time.Since(start).Milliseconds(),
		CursorContext:   pr.analysis.Context,
		CompletionShape: pr.analysis.Shape,
	}
	if req.Trace {
		result.BudgetTrace = buildTrace(pr)
	}
	return result, nil
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

	pr := p.planRequest(req)

	return p.client.GenerateStream(ctx, pr.genReq, func(resp ollama.GenerateResponse) error {
		if resp.Response != "" {
			return fn(resp.Response)
		}
		return nil
	})
}
