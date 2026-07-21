package rag

import "context"

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
