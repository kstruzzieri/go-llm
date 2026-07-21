package rag

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"
)

const (
	maxRetrievalIdentityBytes = 4 << 10
	maxRetrievalAuditLabels   = 64
	maxRetrievalAuditBytes    = 256
)

// RetrievalRequest describes a retrieval operation and its policy request.
type RetrievalRequest struct {
	Query        string
	K            int
	Scope        RetrievalScope
	QueryContext QueryContext
	Policy       RetrievalPolicyRequest
}

// RetrievalPolicyRequest supplies optional constraints for a retrieval policy.
// Zero limits are unspecified.
type RetrievalPolicyRequest struct {
	PrincipalID  string
	SessionID    string
	Scope        RetrievalScope
	RequireFresh bool
	MaxResults   int
	MaxCost      int64
	AuditLabels  map[string]string
}

// RetrievalPolicyEvaluator decides whether a retrieval and its candidates are
// allowed. A zero decision denies because Allow must be explicit. Nil result
// decisions keep all candidates.
type RetrievalPolicyEvaluator interface {
	Evaluate(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error)
	EvaluateResults(context.Context, RetrievalRequest, []Chunk) ([]RetrievalResultDecision, error)
}

// RetrievalPolicyDecision is the evaluator's request-level decision. A zero
// decision denies because Allow must be explicit; zero limits are unspecified.
type RetrievalPolicyDecision struct {
	Allow        bool
	Scope        RetrievalScope
	RequireFresh bool
	MaxResults   int
	MaxCost      int64
}

// RetrievalResultDecision is an evaluator's decision for one candidate. Nil
// result decisions keep all candidates.
type RetrievalResultDecision struct {
	Keep            bool
	RedactedContent *string
}

// RetrievalPolicyDisposition classifies a policy outcome. Dispositions and
// reason codes are core-assigned.
type RetrievalPolicyDisposition string

const (
	// RetrievalPolicyAllowed indicates that retrieval was allowed by policy.
	RetrievalPolicyAllowed RetrievalPolicyDisposition = "allowed"
	// RetrievalPolicyDenied indicates that retrieval was denied by policy.
	RetrievalPolicyDenied RetrievalPolicyDisposition = "denied"
	// RetrievalPolicyFailed indicates that policy evaluation failed.
	RetrievalPolicyFailed RetrievalPolicyDisposition = "failed"
)

// RetrievalPolicyOutcome summarizes the core-assigned policy result.
// Dispositions and reason codes are core-assigned.
type RetrievalPolicyOutcome struct {
	Applied              bool
	Disposition          RetrievalPolicyDisposition
	ReasonCode           string
	CandidateCount       int
	CandidateSourceCount int
	ReturnedCount        int
	ReturnedSourceCount  int
	FilteredCount        int
	RedactedCount        int
	StaleDroppedCount    int
	AuditLabelCount      int
}

// RetrievalResponse contains scored retrieval results and their policy outcome.
type RetrievalResponse struct {
	Results []ScoredResult
	Policy  RetrievalPolicyOutcome
}

type composedPolicy struct {
	request      RetrievalRequest
	limit        int
	maxCost      int64
	requireFresh bool
	emptyScope   bool
}

func minPositive(values ...int) int {
	result := 0
	for _, value := range values {
		if value > 0 && (result == 0 || value < result) {
			result = value
		}
	}
	return result
}

func minPositive64(values ...int64) int64 {
	result := int64(0)
	for _, value := range values {
		if value > 0 && (result == 0 || value < result) {
			result = value
		}
	}
	return result
}

func policyRequestPresent(policy RetrievalPolicyRequest) bool {
	return policy.PrincipalID != "" || policy.SessionID != "" || !policy.Scope.empty() ||
		policy.RequireFresh || policy.MaxResults != 0 || policy.MaxCost != 0 || len(policy.AuditLabels) != 0
}

func cloneRetrievalRequest(req RetrievalRequest) RetrievalRequest {
	req.Scope.Tags = slices.Clone(req.Scope.Tags)
	req.QueryContext.OpenFiles = slices.Clone(req.QueryContext.OpenFiles)
	req.QueryContext.Metadata = maps.Clone(req.QueryContext.Metadata)
	req.Policy.Scope.Tags = slices.Clone(req.Policy.Scope.Tags)
	req.Policy.AuditLabels = maps.Clone(req.Policy.AuditLabels)
	return req
}

func validatePolicyRequest(policy RetrievalPolicyRequest) error {
	if policy.MaxResults < 0 || policy.MaxCost < 0 {
		return ErrPolicyDecisionInvalid
	}
	for _, value := range []string{policy.PrincipalID, policy.SessionID} {
		if !utf8.ValidString(value) || len(value) > maxRetrievalIdentityBytes {
			return ErrPolicyDecisionInvalid
		}
	}
	if len(policy.AuditLabels) > maxRetrievalAuditLabels {
		return ErrPolicyDecisionInvalid
	}
	for key, value := range policy.AuditLabels {
		if !utf8.ValidString(key) || !utf8.ValidString(value) || len(key) > maxRetrievalAuditBytes || len(value) > maxRetrievalAuditBytes {
			return ErrPolicyDecisionInvalid
		}
	}
	return nil
}

func normalizePolicyRequest(req RetrievalRequest) (RetrievalRequest, error) {
	req = cloneRetrievalRequest(req)
	if err := validatePolicyRequest(req.Policy); err != nil {
		return RetrievalRequest{}, err
	}
	var err error
	req.Scope, err = normalizeRetrievalScope(req.Scope)
	if err != nil {
		return RetrievalRequest{}, err
	}
	req.Policy.Scope, err = normalizeRetrievalScope(req.Policy.Scope)
	if err != nil {
		return RetrievalRequest{}, err
	}
	return req, nil
}

func intersectCollection(values ...string) (string, bool) {
	result := ""
	for _, value := range values {
		if value == "" {
			continue
		}
		if result != "" && result != value {
			return "", true
		}
		result = value
	}
	return result, false
}

func composeNormalizedPolicy(req RetrievalRequest, decision RetrievalPolicyDecision) (composedPolicy, error) {
	if decision.MaxResults < 0 || decision.MaxCost < 0 {
		return composedPolicy{}, ErrPolicyDecisionInvalid
	}
	decisionScope, err := normalizeRetrievalScope(decision.Scope)
	if err != nil {
		return composedPolicy{}, err
	}
	collection, emptyScope := intersectCollection(req.Scope.Collection, req.Policy.Scope.Collection, decisionScope.Collection)
	tags := slices.Clone(req.Scope.Tags)
	for _, tag := range req.Policy.Scope.Tags {
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	for _, tag := range decisionScope.Tags {
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
		}
	}
	tags, err = normalizeManagedTags(tags)
	if err != nil {
		return composedPolicy{}, err
	}
	req = cloneRetrievalRequest(req)
	req.Scope = RetrievalScope{Collection: collection, Tags: tags}
	req.Policy.Scope = req.Scope
	req.Policy.RequireFresh = req.Policy.RequireFresh || decision.RequireFresh
	req.Policy.MaxResults = minPositive(req.K, req.Policy.MaxResults, decision.MaxResults)
	req.K = req.Policy.MaxResults
	req.Policy.MaxCost = minPositive64(req.Policy.MaxCost, decision.MaxCost)
	result := composedPolicy{
		request:      req,
		limit:        req.Policy.MaxResults,
		maxCost:      req.Policy.MaxCost,
		requireFresh: req.Policy.RequireFresh,
		emptyScope:   emptyScope,
	}
	return result, nil
}

func composePolicyRequest(req RetrievalRequest) (composedPolicy, error) {
	normalized, err := normalizePolicyRequest(req)
	if err != nil {
		return composedPolicy{}, err
	}
	return composeNormalizedPolicy(normalized, RetrievalPolicyDecision{Allow: true})
}

func cloneChunk(chunk Chunk) Chunk {
	chunk.Metadata = maps.Clone(chunk.Metadata)
	return chunk
}

func cloneScoredResults(results []ScoredResult) []ScoredResult {
	if results == nil {
		return nil
	}
	cloned := make([]ScoredResult, len(results))
	for i, result := range results {
		cloned[i] = result
		cloned[i].Chunk = cloneChunk(result.Chunk)
		cloned[i].Signals = maps.Clone(result.Signals)
	}
	return cloned
}

func ownScoredResults(results []ScoredResult, owned bool) []ScoredResult {
	if owned {
		return cloneScoredResults(results)
	}
	return results
}

func cloneChunks(results []ScoredResult) []Chunk {
	chunks := make([]Chunk, len(results))
	for i := range results {
		chunks[i] = cloneChunk(results[i].Chunk)
	}
	return chunks
}

func applyResultDecisions(results []ScoredResult, decisions []RetrievalResultDecision) ([]ScoredResult, int, int, error) {
	if decisions == nil {
		return cloneScoredResults(results), 0, 0, nil
	}
	if len(decisions) != len(results) {
		return nil, 0, 0, ErrPolicyDecisionInvalid
	}
	kept := make([]ScoredResult, 0, len(results))
	filtered, redacted := 0, 0
	for i, decision := range decisions {
		if !decision.Keep {
			if decision.RedactedContent != nil {
				return nil, 0, 0, ErrPolicyDecisionInvalid
			}
			filtered++
			continue
		}
		result := cloneScoredResults(results[i : i+1])[0]
		if decision.RedactedContent != nil {
			result.Chunk.Content = *decision.RedactedContent
			redacted++
		}
		kept = append(kept, result)
	}
	return kept, filtered, redacted, nil
}

func sourceCount(results []ScoredResult) int {
	sources := make(map[string]struct{}, len(results))
	for _, result := range results {
		sources[result.Chunk.Source] = struct{}{}
	}
	return len(sources)
}

func enforceRequiredFreshness(results []ScoredResult, freshness []retrievalFreshness) ([]ScoredResult, int, error) {
	kept := make([]ScoredResult, 0, len(results))
	stale := 0
	for i, result := range results {
		if i >= len(freshness) || !freshness[i].known {
			return nil, stale, ErrFreshnessUnknown
		}
		if freshness[i].value == DocumentFreshnessStale {
			stale++
			continue
		}
		if freshness[i].value != DocumentFreshnessFresh {
			return nil, stale, ErrFreshnessUnknown
		}
		kept = append(kept, result)
	}
	return kept, stale, nil
}

// RetrieveRequest is the canonical scored retrieval surface.
func (r *Retriever) RetrieveRequest(ctx context.Context, req RetrievalRequest) (RetrievalResponse, error) {
	policyRequested := policyRequestPresent(req.Policy)
	if !policyRequested && r.policyEvaluator == nil && r.policyObserver == nil {
		results, _, err := r.retrieveScoredBase(ctx, req, false)
		if err != nil {
			return RetrievalResponse{}, err
		}
		return RetrievalResponse{Results: results}, nil
	}

	policyApplied := policyRequested || r.policyEvaluator != nil
	failedOutcome := RetrievalPolicyOutcome{Applied: policyApplied, Disposition: RetrievalPolicyFailed, ReasonCode: "request_invalid"}
	normalized, err := normalizePolicyRequest(req)
	if err != nil {
		return r.finalizePolicy(ctx, RetrievalResponse{Policy: failedOutcome}, fmt.Errorf("%w: invalid retrieval policy request", ErrPolicyDecisionInvalid))
	}
	decision := RetrievalPolicyDecision{Allow: true}
	if r.policyEvaluator != nil {
		decision, err = r.policyEvaluator.Evaluate(ctx, cloneRetrievalRequest(normalized))
		if err != nil {
			failedOutcome.ReasonCode = "evaluator_failed"
			return r.finalizePolicy(ctx, RetrievalResponse{Policy: failedOutcome}, fmt.Errorf("%w: %w", ErrPolicyEvaluatorFailed, err))
		}
		if !decision.Allow {
			return r.finalizePolicy(ctx, RetrievalResponse{Policy: RetrievalPolicyOutcome{
				Applied:         true,
				Disposition:     RetrievalPolicyDenied,
				ReasonCode:      "denied",
				AuditLabelCount: len(req.Policy.AuditLabels),
			}}, ErrPolicyDenied)
		}
	}
	policy, err := composeNormalizedPolicy(normalized, RetrievalPolicyDecision{Allow: true})
	if err != nil {
		return r.finalizePolicy(ctx, RetrievalResponse{Policy: failedOutcome}, fmt.Errorf("%w: invalid retrieval policy request", ErrPolicyDecisionInvalid))
	}
	if r.policyEvaluator != nil {
		policy, err = composeNormalizedPolicy(normalized, decision)
		if err != nil {
			failedOutcome.ReasonCode = "decision_invalid"
			return r.finalizePolicy(ctx, RetrievalResponse{Policy: failedOutcome}, fmt.Errorf("%w: invalid retrieval policy decision", ErrPolicyDecisionInvalid))
		}
	}

	var results []ScoredResult
	var freshness []retrievalFreshness
	if !policy.emptyScope {
		results, freshness, err = r.retrieveScoredBase(ctx, policy.request, true)
		if err != nil {
			failedOutcome.ReasonCode = "retrieval_failed"
			failedOutcome.AuditLabelCount = len(normalized.Policy.AuditLabels)
			return r.finalizePolicy(ctx, RetrievalResponse{Policy: failedOutcome}, err)
		}
		if policy.limit > 0 && len(results) > policy.limit {
			results = results[:policy.limit]
			freshness = freshness[:policy.limit]
		}
	}
	candidateCount := len(results)
	candidateSourceCount := sourceCount(results)
	staleDroppedCount := 0
	if policy.requireFresh {
		results, staleDroppedCount, err = enforceRequiredFreshness(results, freshness)
		if err != nil {
			failedOutcome.ReasonCode = "freshness_unknown"
			failedOutcome.CandidateCount = candidateCount
			failedOutcome.CandidateSourceCount = candidateSourceCount
			failedOutcome.StaleDroppedCount = staleDroppedCount
			failedOutcome.AuditLabelCount = len(normalized.Policy.AuditLabels)
			return r.finalizePolicy(ctx, RetrievalResponse{Policy: failedOutcome}, err)
		}
	}
	filteredCount, redactedCount := 0, 0
	if r.policyEvaluator != nil {
		decisions, evaluateErr := r.policyEvaluator.EvaluateResults(ctx, cloneRetrievalRequest(policy.request), cloneChunks(results))
		if evaluateErr != nil {
			failedOutcome.ReasonCode = "evaluator_failed"
			failedOutcome.CandidateCount = candidateCount
			failedOutcome.CandidateSourceCount = candidateSourceCount
			failedOutcome.StaleDroppedCount = staleDroppedCount
			failedOutcome.AuditLabelCount = len(normalized.Policy.AuditLabels)
			return r.finalizePolicy(ctx, RetrievalResponse{Policy: failedOutcome}, fmt.Errorf("%w: %w", ErrPolicyEvaluatorFailed, evaluateErr))
		}
		results, filteredCount, redactedCount, err = applyResultDecisions(results, decisions)
		if err != nil {
			failedOutcome.ReasonCode = "decision_invalid"
			failedOutcome.CandidateCount = candidateCount
			failedOutcome.CandidateSourceCount = candidateSourceCount
			failedOutcome.StaleDroppedCount = staleDroppedCount
			failedOutcome.AuditLabelCount = len(normalized.Policy.AuditLabels)
			return r.finalizePolicy(ctx, RetrievalResponse{Policy: failedOutcome}, fmt.Errorf("%w: invalid retrieval result decision", ErrPolicyDecisionInvalid))
		}
	}
	if policy.limit > 0 && len(results) > policy.limit {
		results = results[:policy.limit]
	}
	count := sourceCount(results)
	reasonCode := "default_allow"
	if r.policyEvaluator != nil {
		reasonCode = "allowed"
	}
	return r.finalizePolicy(ctx, RetrievalResponse{
		Results: results,
		Policy: RetrievalPolicyOutcome{
			Applied:              policyApplied,
			Disposition:          RetrievalPolicyAllowed,
			ReasonCode:           reasonCode,
			CandidateCount:       candidateCount,
			CandidateSourceCount: candidateSourceCount,
			ReturnedCount:        len(results),
			ReturnedSourceCount:  count,
			FilteredCount:        filteredCount,
			RedactedCount:        redactedCount,
			StaleDroppedCount:    staleDroppedCount,
			AuditLabelCount:      len(normalized.Policy.AuditLabels),
		},
	}, nil)
}

func (r *Retriever) finalizePolicy(ctx context.Context, response RetrievalResponse, primary error) (RetrievalResponse, error) {
	if r.policyObserver == nil {
		return response, primary
	}
	observerErr := r.policyObserver.OnRetrievalPolicy(ctx, RetrievalPolicyEvent{Outcome: response.Policy})
	if observerErr == nil {
		return response, primary
	}
	response.Results = nil
	response.Policy.Disposition = RetrievalPolicyFailed
	response.Policy.ReasonCode = "observer_failed"
	if primary != nil {
		return response, errors.Join(primary, observerErr)
	}
	return response, observerErr
}

// RetrievalPolicyObserver receives synchronous, consumer-owned policy
// callbacks.
type RetrievalPolicyObserver interface {
	OnRetrievalPolicy(context.Context, RetrievalPolicyEvent) error
}

// RetrievalPolicyEvent carries a core-assigned policy outcome to an observer.
// Observer callbacks are synchronous and consumer-owned.
type RetrievalPolicyEvent struct {
	Outcome RetrievalPolicyOutcome
}
