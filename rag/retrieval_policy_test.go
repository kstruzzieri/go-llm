package rag

import (
	"context"
	"errors"
	"testing"
)

type policyEvaluatorSpy struct {
	evaluate        func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error)
	evaluateResults func(context.Context, RetrievalRequest, []Chunk) ([]RetrievalResultDecision, error)
}

func (s policyEvaluatorSpy) Evaluate(ctx context.Context, req RetrievalRequest) (RetrievalPolicyDecision, error) {
	return s.evaluate(ctx, req)
}

func (s policyEvaluatorSpy) EvaluateResults(ctx context.Context, req RetrievalRequest, chunks []Chunk) ([]RetrievalResultDecision, error) {
	return s.evaluateResults(ctx, req, chunks)
}

type policyObserverSpy struct {
	events []RetrievalPolicyEvent
	err    error
}

func (s *policyObserverSpy) OnRetrievalPolicy(_ context.Context, event RetrievalPolicyEvent) error {
	s.events = append(s.events, event)
	return s.err
}

func allowPolicySpy() policyEvaluatorSpy {
	return policyEvaluatorSpy{
		evaluate: func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
			return RetrievalPolicyDecision{Allow: true}, nil
		},
		evaluateResults: func(context.Context, RetrievalRequest, []Chunk) ([]RetrievalResultDecision, error) {
			return nil, nil
		},
	}
}

func TestRetrievalPolicyContractAndOptions(t *testing.T) {
	evaluator := allowPolicySpy()
	observer := &policyObserverSpy{}
	active := NewRetriever(nil, nil, WithRetrievalPolicyEvaluator(evaluator))
	if !active.PolicyActive() {
		t.Fatal("evaluator-backed retriever is not policy-active")
	}
	observed := NewRetriever(nil, nil, WithRetrievalPolicyObserver(observer))
	if observed.PolicyActive() {
		t.Fatal("observer-only retriever is policy-active")
	}
	var typedNil *policyEvaluatorSpy
	inactive := NewRetriever(nil, nil, WithRetrievalPolicyEvaluator(typedNil))
	if inactive.PolicyActive() {
		t.Fatal("typed-nil evaluator is policy-active")
	}

	redacted := "replacement"
	_ = RetrievalRequest{Policy: RetrievalPolicyRequest{MaxCost: 1}}
	_ = RetrievalPolicyDecision{Allow: true, MaxCost: 1}
	_ = RetrievalResultDecision{Keep: true, RedactedContent: &redacted}
	_ = RetrievalResponse{Policy: RetrievalPolicyOutcome{Disposition: RetrievalPolicyAllowed}}
	_ = RetrievalPolicyEvent{Outcome: RetrievalPolicyOutcome{ReasonCode: "allowed"}}
}

func TestRetrievalPolicyErrorsAreDistinct(t *testing.T) {
	errs := []error{ErrPolicyDenied, ErrPolicyEvaluatorFailed, ErrPolicyDecisionInvalid, ErrFreshnessUnknown}
	for i := range errs {
		for j := range errs {
			if i != j && errors.Is(errs[i], errs[j]) {
				t.Fatalf("error %d unexpectedly matches error %d", i, j)
			}
		}
	}
}
