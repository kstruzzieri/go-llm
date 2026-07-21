package rag

import (
	"context"
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

func composePolicyRequest(req RetrievalRequest) (composedPolicy, error) {
	callerScope, err := normalizeRetrievalScope(req.Scope)
	if err != nil {
		return composedPolicy{}, err
	}
	policyScope, err := normalizeRetrievalScope(req.Policy.Scope)
	if err != nil {
		return composedPolicy{}, err
	}
	collection, emptyScope := intersectCollection(callerScope.Collection, policyScope.Collection)
	tags, err := normalizeManagedTags(append(callerScope.Tags, policyScope.Tags...))
	if err != nil {
		return composedPolicy{}, err
	}
	req.Scope = RetrievalScope{Collection: collection, Tags: tags}
	req.Policy.Scope = policyScope
	result := composedPolicy{
		request:      req,
		limit:        minPositive(req.K, req.Policy.MaxResults),
		maxCost:      minPositive64(req.Policy.MaxCost),
		requireFresh: req.Policy.RequireFresh,
		emptyScope:   emptyScope,
	}
	result.request.K = result.limit
	return result, nil
}

func sourceCount(results []ScoredResult) int {
	sources := make(map[string]struct{}, len(results))
	for _, result := range results {
		sources[result.Chunk.Source] = struct{}{}
	}
	return len(sources)
}

// RetrieveRequest is the canonical scored retrieval surface.
func (r *Retriever) RetrieveRequest(ctx context.Context, req RetrievalRequest) (RetrievalResponse, error) {
	if !policyRequestPresent(req.Policy) {
		results, err := r.retrieveScoredBase(ctx, req)
		if err != nil {
			return RetrievalResponse{}, err
		}
		return RetrievalResponse{Results: results}, nil
	}

	req = cloneRetrievalRequest(req)
	failedOutcome := RetrievalPolicyOutcome{Applied: true, Disposition: RetrievalPolicyFailed, ReasonCode: "request_invalid"}
	if err := validatePolicyRequest(req.Policy); err != nil {
		return RetrievalResponse{Policy: failedOutcome}, fmt.Errorf("%w: invalid retrieval policy request", ErrPolicyDecisionInvalid)
	}
	policy, err := composePolicyRequest(req)
	if err != nil {
		return RetrievalResponse{Policy: failedOutcome}, fmt.Errorf("%w: invalid retrieval policy request", ErrPolicyDecisionInvalid)
	}
	if policy.emptyScope {
		return RetrievalResponse{Policy: RetrievalPolicyOutcome{
			Applied:         true,
			Disposition:     RetrievalPolicyAllowed,
			ReasonCode:      "default_allow",
			AuditLabelCount: len(req.Policy.AuditLabels),
		}}, nil
	}

	results, err := r.retrieveScoredBase(ctx, policy.request)
	if err != nil {
		return RetrievalResponse{}, err
	}
	if policy.limit > 0 && len(results) > policy.limit {
		results = results[:policy.limit]
	}
	count := sourceCount(results)
	return RetrievalResponse{
		Results: results,
		Policy: RetrievalPolicyOutcome{
			Applied:              true,
			Disposition:          RetrievalPolicyAllowed,
			ReasonCode:           "default_allow",
			CandidateCount:       len(results),
			CandidateSourceCount: count,
			ReturnedCount:        len(results),
			ReturnedSourceCount:  count,
			AuditLabelCount:      len(req.Policy.AuditLabels),
		},
	}, nil
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
