package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/completion"
	"github.com/kstruzzieri/go-llm/provider"
)

// generateToolCategory enumerates the error-category strings used by the
// FIM-via-Router helper. Centralised so callers can reason about
// categorisation without string matching.
type generateToolCategory string

const (
	generateToolConfig generateToolCategory = "config"
	generateToolRouter generateToolCategory = "router"
	generateToolOllama generateToolCategory = "ollama"
)

// routedGenerateError wraps a routing or execution error with a category.
// Callers extract the category via routedGenerateCategory; unwrapping yields
// the underlying error for normal error inspection (errors.Is / errors.As
// against the wrapped cause).
type routedGenerateError struct {
	category generateToolCategory
	err      error
}

func (e routedGenerateError) Error() string { return e.err.Error() }
func (e routedGenerateError) Unwrap() error { return e.err }

// routedGenerateCategory extracts the generateToolCategory from any error
// returned by the FIM-via-Router helper or fimGenerator. Errors not wrapping
// routedGenerateError are categorised as "ollama" (the most conservative —
// assume backend execution failed).
func routedGenerateCategory(err error) generateToolCategory {
	var re routedGenerateError
	if errors.As(err, &re) {
		return re.category
	}
	return generateToolOllama
}

// routedGenerate plans a FIM-shaped generate routing request and returns the
// executable RoutePlan. It does not execute the plan — callers branch on
// streaming vs non-streaming via plan.ExecuteGenerate or
// plan.ExecuteGenerateStream so request shaping stays shared while each
// branch can supply its required capability mask.
//
// FIM pinning policy: the resolved model is mandatory and PreferredChain is
// intentionally never set, so Router cannot substitute across FIM-family
// boundaries. Native prompt+suffix insert uses provider/model template
// semantics rather than client-side marker assembly, but compatibility is
// still model-family-specific; cross-family fallback therefore requires
// route-after-template or compatibility-class routing and is deferred to a
// future ticket.
//
// extraCaps lets callers layer transport-specific requirements onto the
// common FIM gate. Non-streaming calls pass 0; streaming calls pass
// provider.CapStream.
func (s *Server) routedGenerate(ctx context.Context, req completion.GenerateRequest, priority provider.Priority, extraCaps provider.Capability) (*provider.RoutePlan, error) {
	if req.Model == "" {
		return nil, routedGenerateError{
			category: generateToolConfig,
			err:      fmt.Errorf("fim: generator requires explicit model to prevent FIM-family token-template mismatch"),
		}
	}
	router := s.routerSnapshot()
	if router == nil {
		return nil, routedGenerateError{category: generateToolConfig, err: fmt.Errorf("router unavailable")}
	}
	requiredCaps := provider.CapGenerate | provider.CapInsert | extraCaps
	rr := provider.RoutingRequest{
		Model:        req.Model,
		UseCase:      "fim",
		RequiredCaps: requiredCaps,
		Prompt:       req.Prompt,
		Suffix:       req.Suffix,
		Options: provider.ModelOptions{
			// provider.Ptr ensures literal-zero temperatures (FIM uses 0.0
			// for deterministic completions in import blocks) round-trip as
			// a non-nil pointer rather than being lost as "unset" through
			// the *float64/omitempty encoding.
			Temperature: provider.Ptr(req.Temperature),
			NumPredict:  req.NumPredict,
			NumCtx:      req.NumCtx,
			Stop:        req.Stop,
		},
		ExpectedOutput: provider.DefaultExpectedOutput("fim"),
		Priority:       priority,
		// PreferredChain intentionally left empty — see FIM pinning policy.
	}
	plan, err := router.Route(ctx, rr)
	if err != nil {
		return nil, routedGenerateError{category: generateToolRouter, err: err}
	}
	return plan, nil
}

// resultFromGenerateResponse maps a *provider.GenerateResponse into the
// completion seam's GenerateResult, backfilling Model and Provider from
// RouteOutcome when the immediate response leaves them empty (e.g. when the
// Router answered via fallback and the provider response carried only the
// fallback's payload without re-stamping the top-level identity). When
// neither resp.Model nor resp.RouteOutcome carries the model identity,
// out.Model falls back to req.Model so the FIM-pin propagates consistently
// with resultFromAggregatedStream.
func resultFromGenerateResponse(resp *provider.GenerateResponse, req completion.GenerateRequest) completion.GenerateResult {
	out := completion.GenerateResult{
		Response: resp.Response,
		Tokens:   resp.Usage.CompletionTokens,
		Model:    resp.Model,
		Provider: resp.Provider,
	}
	if resp.RouteOutcome != nil {
		if out.Model == "" {
			out.Model = resp.RouteOutcome.ActualModel.Model
		}
		if out.Provider == "" {
			out.Provider = resp.RouteOutcome.ActualModel.Provider
		}
		out.Outcome = translateRouteOutcome(resp.RouteOutcome)
	}
	if out.Model == "" {
		out.Model = req.Model
	}
	return out
}

// resultFromAggregatedStream builds a GenerateResult after streaming
// completion. The full Response is the concatenation of all chunk Response
// strings; Tokens and Outcome are taken from the last chunk that surfaced
// route metadata. When no router-side metadata is available (ExecuteGenerate
// for a provider that doesn't populate RouteOutcome), Model falls back to the
// request's pinned model so callers see consistent provenance.
func resultFromAggregatedStream(full string, tokens int, outcome *provider.RouteOutcome, req completion.GenerateRequest) completion.GenerateResult {
	out := completion.GenerateResult{
		Response: full,
		Tokens:   tokens,
	}
	if outcome != nil {
		out.Model = outcome.ActualModel.Model
		out.Provider = outcome.ActualModel.Provider
		out.Outcome = translateRouteOutcome(outcome)
	} else {
		out.Model = req.Model
	}
	return out
}

// translateRouteOutcome maps provider.RouteOutcome to the consumer-owned
// completion.RouteOutcome shape.
//
// RouteHint prefers the provider-authored Reason ("breaker-open on candidate
// 1, fell through to qwen3:8b") because it is more diagnostic than a
// synthesized model string; falls back to ActualModel.String() when Reason
// is empty. PlannedModel is rendered via ModelKey.String() so downstream
// telemetry can compare it to GenerateResult.Model lexically.
//
// Score, FallbacksUsed, and WasSticky are copied verbatim — these are the
// load-bearing structured fields for #82 drift telemetry. Warm is not
// included: warmth is a routing input signal, not an outcome, and querying
// the warmth tracker at translation time produces a tautological answer
// (the act of routing makes the model warm).
func translateRouteOutcome(o *provider.RouteOutcome) *completion.RouteOutcome {
	if o == nil {
		return nil
	}
	hint := o.Reason
	if hint == "" {
		hint = o.ActualModel.String()
	}
	return &completion.RouteOutcome{
		RouteHint:     hint,
		PlannedModel:  o.PlannedModel.String(),
		Score:         o.Score,
		FallbacksUsed: o.FallbacksUsed,
		WasSticky:     o.WasSticky,
	}
}
