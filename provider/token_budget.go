// token_budget.go validates whether a routing request fits within a candidate
// model's context window. It produces a BudgetResult with a decision (OK,
// Truncate, Reject), headroom metrics, and a human-readable reason. The
// HeadroomScore feeds into candidate scoring: 1.0 at <50% utilization,
// linear drop to 0.0 at 100%.
package provider

import "fmt"

// ---------------------------------------------------------------------------
// BudgetDecision
// ---------------------------------------------------------------------------

// BudgetDecision indicates whether a routing request fits within the
// candidate model's context budget.
type BudgetDecision int

const (
	// BudgetOK means the request fits within the model's context budget.
	BudgetOK BudgetDecision = iota
	// BudgetTruncate means the request can fit if the input is trimmed.
	BudgetTruncate
	// BudgetReject means the request cannot fit in the model's context.
	BudgetReject
)

// String returns the human-readable name of the budget decision.
func (d BudgetDecision) String() string {
	switch d {
	case BudgetOK:
		return "ok"
	case BudgetTruncate:
		return "truncate"
	case BudgetReject:
		return "reject"
	default:
		return "unknown"
	}
}

// ---------------------------------------------------------------------------
// BudgetResult
// ---------------------------------------------------------------------------

// BudgetResult holds the outcome of a token budget validation. It includes
// the decision, the token counts, headroom metrics, and a human-readable
// reason explaining the decision.
type BudgetResult struct {
	// Decision is the routing outcome: OK, Truncate, or Reject.
	Decision BudgetDecision
	// RequestTokens is the estimated input token count for the request.
	RequestTokens int
	// BudgetTokens is the available input budget (context window minus
	// reserved output tokens).
	BudgetTokens int
	// HeadroomPct is the fraction of budget remaining (0.0–1.0).
	HeadroomPct float64
	// HeadroomScore is a quality-weighted score: 1.0 at <50% utilization,
	// linear drop to 0.0 at 100% utilization.
	HeadroomScore float64
	// Reason is a human-readable explanation of the decision.
	Reason string
}

// ---------------------------------------------------------------------------
// TokenBudgetValidator
// ---------------------------------------------------------------------------

// TokenBudgetValidator checks whether a RoutingRequest fits within a
// ModelProfile's effective context window. It reserves output tokens per
// use case and computes headroom metrics for candidate scoring.
type TokenBudgetValidator struct {
	outputDefaults map[string]int
}

// BudgetOption configures a TokenBudgetValidator.
type BudgetOption func(*TokenBudgetValidator)

// WithOutputDefault sets a custom default output token reservation for a
// specific use case. This overrides the built-in default from
// DefaultExpectedOutput.
func WithOutputDefault(useCase string, tokens int) BudgetOption {
	return func(v *TokenBudgetValidator) {
		v.outputDefaults[useCase] = tokens
	}
}

// NewTokenBudgetValidator creates a TokenBudgetValidator with the given
// options. Without options the validator uses DefaultExpectedOutput for
// output token reservations.
func NewTokenBudgetValidator(opts ...BudgetOption) *TokenBudgetValidator {
	v := &TokenBudgetValidator{
		outputDefaults: make(map[string]int),
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// expectedOutput returns the output token reservation for the given use case.
// It checks, in order: the request's ExpectedOutput field, custom defaults
// set via WithOutputDefault, and finally the package-level DefaultExpectedOutput.
func (v *TokenBudgetValidator) expectedOutput(req RoutingRequest) int {
	if req.ExpectedOutput > 0 {
		return req.ExpectedOutput
	}
	if tokens, ok := v.outputDefaults[req.UseCase]; ok {
		return tokens
	}
	return DefaultExpectedOutput(req.UseCase)
}

// Validate checks whether req fits within profile's effective context window
// and returns a BudgetResult with the decision and headroom metrics.
func (v *TokenBudgetValidator) Validate(req RoutingRequest, profile *ModelProfile) BudgetResult {
	inputTokens := estimateRoutingInputTokens(req)
	expectedOut := v.expectedOutput(req)
	effectiveCtx := profile.EffectiveContextWindow(req.UseCase)

	// Guard: a zero or negative effective context means the profile is
	// misconfigured or uninitialized. Reject with a clear reason.
	if effectiveCtx <= 0 {
		return BudgetResult{
			Decision:      BudgetReject,
			RequestTokens: inputTokens,
			BudgetTokens:  0,
			Reason:        "model has no context window configured",
		}
	}

	// Budget is the context window minus the reserved output tokens, clamped to 0.
	budget := effectiveCtx - expectedOut
	if budget < 0 {
		budget = 0
	}

	// If there is no budget at all, reject immediately.
	if budget == 0 {
		return BudgetResult{
			Decision:      BudgetReject,
			RequestTokens: inputTokens,
			BudgetTokens:  0,
			Reason:        "output budget exceeds context window",
		}
	}

	// Compute utilization and headroom.
	utilization := float64(inputTokens) / float64(budget)

	headroomPct := 1.0 - utilization
	if headroomPct < 0 {
		headroomPct = 0
	}

	headroomScore := computeHeadroomScore(utilization)

	// Decide.
	var decision BudgetDecision
	var reason string

	switch {
	case inputTokens <= budget:
		decision = BudgetOK
		reason = fmt.Sprintf("input %d tokens fits within %d budget (%.0f%% utilization)",
			inputTokens, budget, utilization*100)
	case float64(inputTokens) < float64(budget)*1.5:
		decision = BudgetTruncate
		reason = fmt.Sprintf("input %d tokens exceeds %d budget but can be truncated (%.0f%% utilization)",
			inputTokens, budget, utilization*100)
	default:
		decision = BudgetReject
		reason = fmt.Sprintf("input %d tokens far exceeds %d budget (%.0f%% utilization)",
			inputTokens, budget, utilization*100)
	}

	return BudgetResult{
		Decision:      decision,
		RequestTokens: inputTokens,
		BudgetTokens:  budget,
		HeadroomPct:   headroomPct,
		HeadroomScore: headroomScore,
		Reason:        reason,
	}
}

// computeHeadroomScore maps utilization to a quality score:
//   - utilization <= 0.5 → 1.0
//   - utilization >= 1.0 → 0.0
//   - between → linear: 2.0 * (1.0 - utilization)
func computeHeadroomScore(utilization float64) float64 {
	switch {
	case utilization <= 0.5:
		return 1.0
	case utilization >= 1.0:
		return 0.0
	default:
		return 2.0 * (1.0 - utilization)
	}
}
