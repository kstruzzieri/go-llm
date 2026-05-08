// Generator seam for completion.
//
// The Generator interface is completion's narrow generation dependency:
// two-method (Generate + GenerateStream), returning generic provenance
// instead of importing provider response types into completion. Router-
// backed implementations are supplied by callers (see mcp.Server.fimGenerator);
// the legacy *ollama.Client path is preserved via the private
// generatorFromOllamaClient adapter used by NewProvider.
package completion

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/ollama"
)

// providerNameOllama is the canonical provider identifier surfaced in
// GenerateResult.Provider when the ollama adapter answers. Mirrors the
// string returned by provider.OllamaProvider.Name() (provider/ollama.go).
// Single source of truth within completion/ so the two adapter call sites
// stay in lockstep.
const providerNameOllama = "ollama"

// GenerateRequest is the generation request shape the completion package
// hands to the Generator seam. Suffix being non-empty implies FIM mode and
// requires the backing implementation to honour CapInsert at the provider
// level. NumCtx, NumPredict, Temperature, and Stop reflect the resolved
// budget computed by Provider.planRequest and must be passed through
// unchanged.
type GenerateRequest struct {
	Model       string
	Prompt      string
	Suffix      string
	Temperature float64
	NumPredict  int
	NumCtx      int
	Stop        []string
}

// GenerateResult is the non-streaming generation payload plus enough
// provenance for completion (and future #82 drift telemetry) to identify
// the model that actually answered. Top-level Model and Provider are the
// source of truth post-routing; Outcome carries supplementary route-side
// signals (planned model, score, fallback count, sticky-cache hit) when
// the backing implementation has them. The legacy ollama adapter leaves
// Outcome nil.
type GenerateResult struct {
	Response string
	Tokens   int
	Model    string        // model that actually answered (post-routing)
	Provider string        // provider that actually answered
	Outcome  *RouteOutcome // nil when no router-side metadata is available
}

// RouteOutcome carries router-side decision metadata for a generation
// response. Populated by Router-backed Generator implementations (see
// mcp.Server.fimGenerator); the legacy ollama adapter leaves
// GenerateResult.Outcome nil.
//
// RouteHint is human-readable provenance suitable for logs and telemetry
// (typically the underlying provider.RouteOutcome.Reason; falls back to
// ActualModel.String() if Reason is empty). PlannedModel records the
// router's first-choice candidate as a provider-qualified identity in
// "provider/model" form (e.g. "ollama/qwen3:8b"); it preserves the
// provider component so provider-level fallback is observable even when
// the model name happens to coincide across providers.
//
// The canonical drift detector for #82 is therefore the qualified
// comparison:
//
//	result.Outcome != nil && result.Outcome.PlannedModel != result.Provider+"/"+result.Model
//
// Comparing PlannedModel against the unqualified GenerateResult.Model
// alone would report false drift on every routed success because the
// formats differ. Score is the composite ranking score the resolved
// candidate received. FallbacksUsed is the number of chain fallbacks
// consumed before success (0 = first candidate answered). WasSticky is
// true if the resolved model came from the sticky-cache rather than
// fresh scoring.
type RouteOutcome struct {
	RouteHint     string
	PlannedModel  string
	Score         float64
	FallbacksUsed int
	WasSticky     bool
}

// GenerateChunk is a streaming chunk's payload. The completion package's
// CompleteStream owns stop-token suppression on top of these raw chunks.
//
// Done MUST be true on exactly the last chunk emitted by a successful
// GenerateStream call (matching ollama.GenerateResponse.Done semantics);
// callers rely on this to flush trailing stop-token-suppression buffers.
// Implementations that cannot guarantee a final Done=true chunk on
// success must return an error from GenerateStream rather than emit a
// stream that never terminates cleanly.
type GenerateChunk struct {
	Response string
	Done     bool
}

// Generator is the narrow generation dependency completion needs.
//
// Both methods MUST be implemented; a generator that does not support
// streaming should return an error from GenerateStream rather than
// silently degrading to Generate (callers may interpret a one-chunk
// stream as the final answer).
//
// Implementations MUST be safe for concurrent use across separate
// Generate / GenerateStream calls — the IDE invokes FIM completions on
// every keystroke and may have multiple in flight. Within a single
// GenerateStream call, however, fn MUST be invoked serially from a
// single goroutine and MUST NOT be called concurrently. Provider.
// CompleteStream relies on this serial-fn contract to maintain its
// stop-token suppression buffer without external synchronization; a
// Generator that fans chunks out across goroutines would break it.
//
// Empty-Prompt-AND-empty-Suffix requests are not rejected by the
// interface itself: ollama.Client.Generate already rejects such requests
// at the backend layer (the bundled ollamaGenerator surfaces the same
// rejection at the seam), and Provider.planRequest does not produce them
// for current FIM call sites. Implementations MAY add their own
// defensive validation; callers should treat such requests as invalid
// regardless.
//
// Suffix being non-empty implies FIM mode and requires the backing
// implementation to honour CapInsert at the provider level. The
// implementation MUST NOT substitute across FIM-family template
// compatibility boundaries within a single call: families differ in
// their native prompt+suffix marker tokens (qwen3-coder, codellama,
// codestral, etc. each use distinct templates), so cross-family
// fallback can produce malformed prompts and incorrect completions.
// The MCP server's s.fimGenerator pins the resolved model end-to-end
// to enforce this; other Generator implementations should follow the
// same policy or document the deviation.
//
// On success, GenerateResult.Response is the complete, non-stop-stripped
// backend payload; Provider.Complete owns stop-token stripping.
// GenerateStream returns the final aggregated GenerateResult (Response =
// full concatenation) for symmetry with Generate and to expose route
// metadata uniformly.
type Generator interface {
	Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
	GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateChunk) error) (GenerateResult, error)
}

// generatorFromOllamaClient adapts *ollama.Client to Generator. Private —
// used only by the legacy NewProvider compat shim. External consumers
// wiring router-aware generation switch to NewProviderWithGenerator and
// supply their own Generator.
//
// Returns nil when client is nil so the legacy nil-client behaviour of
// NewProvider is preserved (existing code rejects a nil client with an
// error from Complete/CompleteStream; the adapter preserves that path).
func generatorFromOllamaClient(c *ollama.Client) Generator {
	if c == nil {
		return nil
	}
	return &ollamaGenerator{client: c}
}

// ollamaGenerator wraps *ollama.Client to satisfy Generator. It is the
// only place in completion/ that imports ollama types; consumers wiring
// router-aware generation never touch this struct.
//
// Safe for concurrent use: *ollama.Client is a stateless HTTP wrapper
// (each Generate / GenerateStream call issues its own request), and this
// struct adds no mutable state on top of it.
type ollamaGenerator struct {
	client *ollama.Client
}

func (g *ollamaGenerator) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	if err := validateOllamaGenerateRequest(req); err != nil {
		return GenerateResult{}, err
	}
	resp, err := g.client.Generate(ctx, ollamaGenerateRequest(req))
	if err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{
		Response: resp.Response,
		Tokens:   resp.EvalCount,
		Model:    req.Model,
		Provider: providerNameOllama,
	}, nil
}

func (g *ollamaGenerator) GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateChunk) error) (GenerateResult, error) {
	if err := validateOllamaGenerateRequest(req); err != nil {
		return GenerateResult{}, err
	}
	if fn == nil {
		return GenerateResult{}, fmt.Errorf("completion: GenerateStream callback is required")
	}
	var (
		full   strings.Builder
		tokens int
	)
	err := g.client.GenerateStream(ctx, ollamaGenerateRequest(req), func(resp ollama.GenerateResponse) error {
		full.WriteString(resp.Response)
		if resp.EvalCount > 0 {
			tokens = resp.EvalCount
		}
		return fn(GenerateChunk{Response: resp.Response, Done: resp.Done})
	})
	if err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{
		Response: full.String(),
		Tokens:   tokens,
		Model:    req.Model,
		Provider: providerNameOllama,
	}, nil
}

// validateOllamaGenerateRequest enforces upfront defense-in-depth on the
// fields Provider.Generate / *ollama.Client.Generate would reject anyway.
// Catching at the seam yields a clearer error message tagged with
// "completion:" rather than "ollama: generate: ..." surfacing from the
// network layer, and matches the validation surface of the future
// Router-backed Generator (mcp.Server.fimGenerator) so callers can write
// uniform error handling regardless of which Generator is wired in.
func validateOllamaGenerateRequest(req GenerateRequest) error {
	if req.Model == "" {
		return fmt.Errorf("completion: model is required")
	}
	if req.Prompt == "" && req.Suffix == "" {
		return fmt.Errorf("completion: prompt or suffix is required")
	}
	return nil
}

// ollamaGenerateRequest converts the seam-shaped GenerateRequest into the
// ollama.GenerateRequest the underlying client expects. The Stop slice is
// copied (not aliased) so the in-flight goroutine cannot observe caller
// mutation; concurrent FIM workloads that reuse request structs from a
// pool would otherwise be exposed to data races on Stop.
func ollamaGenerateRequest(req GenerateRequest) ollama.GenerateRequest {
	var stop []string
	if len(req.Stop) > 0 {
		stop = append(make([]string, 0, len(req.Stop)), req.Stop...)
	}
	return ollama.GenerateRequest{
		Model:  req.Model,
		Prompt: req.Prompt,
		Suffix: req.Suffix,
		Options: &ollama.ModelOptions{
			NumPredict:  req.NumPredict,
			NumCtx:      req.NumCtx,
			Temperature: req.Temperature,
			Stop:        stop,
		},
	}
}
