package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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

type policyObserverFunc func(context.Context, RetrievalPolicyEvent) error

func (f policyObserverFunc) OnRetrievalPolicy(ctx context.Context, event RetrievalPolicyEvent) error {
	return f(ctx, event)
}

type panicPolicyObserver struct{}

func (o *panicPolicyObserver) OnRetrievalPolicy(context.Context, RetrievalPolicyEvent) error {
	if o == nil {
		panic("typed-nil observer invoked")
	}
	return nil
}

type retrievalPolicyMultiStore struct {
	retrieverMultiStore
	gotQueryContext QueryContext
}

func (s *retrievalPolicyMultiStore) SearchMulti(ctx context.Context, embedding []float64, query string, k int, qctx QueryContext) ([]ScoredResult, error) {
	s.gotQueryContext = qctx
	return s.retrieverMultiStore.SearchMulti(ctx, embedding, query, k, qctx)
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

func newPolicyRetriever(t *testing.T, results []ScoredResult, opts ...RetrieverOption) (*Retriever, *recordingEmbedder, *retrieverMultiStore) {
	t.Helper()
	embedder := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}
	store := &retrieverMultiStore{multiResults: results}
	r, err := NewRetrieverWithEmbedder(embedder, store, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return r, embedder, store
}

func TestRetrievalPolicyObserverAlonePreservesLegacyResults(t *testing.T) {
	observer := &policyObserverSpy{}
	want := []ScoredResult{policyScored("c1", "s1")}
	legacy, _, _ := newPolicyRetriever(t, want)
	observed, _, _ := newPolicyRetriever(t, want, WithRetrievalPolicyObserver(observer))
	legacyResponse, legacyErr := legacy.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
	observedResponse, observedErr := observed.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
	if legacyErr != nil || observedErr != nil || !reflect.DeepEqual(legacyResponse.Results, observedResponse.Results) {
		t.Fatalf("legacy=%#v/%v observed=%#v/%v", legacyResponse, legacyErr, observedResponse, observedErr)
	}
	legacyJSON, err := json.Marshal(legacyResponse.Results)
	if err != nil {
		t.Fatal(err)
	}
	observedJSON, err := json.Marshal(observedResponse.Results)
	if err != nil || !bytes.Equal(legacyJSON, observedJSON) {
		t.Fatalf("legacy JSON=%s observed JSON=%s error=%v", legacyJSON, observedJSON, err)
	}
	if observedResponse.Policy.Applied || len(observer.events) != 1 || observer.events[0].Outcome.ReasonCode != "default_allow" {
		t.Fatalf("response/events=%#v/%#v", observedResponse, observer.events)
	}
}

func TestRetrievalPolicyObserverEmitsOneTerminalEvent(t *testing.T) {
	cases := []struct {
		name        string
		disposition RetrievalPolicyDisposition
		reasonCode  string
		run         func(*policyObserverSpy) (RetrievalResponse, error)
	}{
		{
			name: "observer-only/default allow", disposition: RetrievalPolicyAllowed, reasonCode: "default_allow",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
			},
		},
		{
			name: "evaluator allow", disposition: RetrievalPolicyAllowed, reasonCode: "allowed",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				r, _, _ := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(allowPolicySpy()), WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q"})
			},
		},
		{
			name: "deny", disposition: RetrievalPolicyDenied, reasonCode: "denied",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				evaluator := allowPolicySpy()
				evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
					return RetrievalPolicyDecision{}, nil
				}
				r, _, _ := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator), WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q"})
			},
		},
		{
			name: "evaluator error", disposition: RetrievalPolicyFailed, reasonCode: "evaluator_failed",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				evaluator := allowPolicySpy()
				evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
					return RetrievalPolicyDecision{}, errors.New("private evaluator detail")
				}
				r, _, _ := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator), WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q"})
			},
		},
		{
			name: "invalid caller policy request", disposition: RetrievalPolicyFailed, reasonCode: "request_invalid",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				r, _, _ := newPolicyRetriever(t, nil, WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", Policy: RetrievalPolicyRequest{MaxResults: -1}})
			},
		},
		{
			name: "invalid decision", disposition: RetrievalPolicyFailed, reasonCode: "decision_invalid",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				evaluator := allowPolicySpy()
				evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
					return RetrievalPolicyDecision{Allow: true, MaxResults: -1}, nil
				}
				r, _, _ := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator), WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q"})
			},
		},
		{
			name: "retrieval error", disposition: RetrievalPolicyFailed, reasonCode: "retrieval_failed",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				r, _, store := newPolicyRetriever(t, nil, WithRetrievalPolicyObserver(observer))
				store.multiErr = errors.New("private retrieval detail")
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q"})
			},
		},
		{
			name: "unknown freshness", disposition: RetrievalPolicyFailed, reasonCode: "freshness_unknown",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", Policy: RetrievalPolicyRequest{RequireFresh: true}})
			},
		},
		{
			name: "filtered/redacted success", disposition: RetrievalPolicyAllowed, reasonCode: "allowed",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				redacted := "safe"
				evaluator := allowPolicySpy()
				evaluator.evaluateResults = func(context.Context, RetrievalRequest, []Chunk) ([]RetrievalResultDecision, error) {
					return []RetrievalResultDecision{{Keep: true, RedactedContent: &redacted}}, nil
				}
				r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyEvaluator(evaluator), WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
			},
		},
		{
			name: "result evaluator error", disposition: RetrievalPolicyFailed, reasonCode: "evaluator_failed",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				evaluator := allowPolicySpy()
				evaluator.evaluateResults = func(context.Context, RetrievalRequest, []Chunk) ([]RetrievalResultDecision, error) {
					return nil, errors.New("private result evaluator detail")
				}
				r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyEvaluator(evaluator), WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
			},
		},
		{
			name: "collection conflict", disposition: RetrievalPolicyAllowed, reasonCode: "default_allow",
			run: func(observer *policyObserverSpy) (RetrievalResponse, error) {
				r, _, _ := newPolicyRetriever(t, nil, WithRetrievalPolicyObserver(observer))
				return r.RetrieveRequest(context.Background(), RetrievalRequest{
					Query: "q", Scope: RetrievalScope{Collection: "caller"},
					Policy: RetrievalPolicyRequest{Scope: RetrievalScope{Collection: "policy"}},
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observer := &policyObserverSpy{}
			response, _ := tc.run(observer)
			if len(observer.events) != 1 {
				t.Fatalf("events = %#v, want exactly one", observer.events)
			}
			event := observer.events[0]
			if event.Outcome.Disposition != tc.disposition || event.Outcome.ReasonCode != tc.reasonCode {
				t.Fatalf("event = %#v, want %s/%s", event, tc.disposition, tc.reasonCode)
			}
			if !reflect.DeepEqual(event.Outcome, response.Policy) {
				t.Fatalf("event outcome = %#v, response outcome = %#v", event.Outcome, response.Policy)
			}
			typ := reflect.TypeOf(event)
			if typ.NumField() != 1 || typ.Field(0).Name != "Outcome" {
				t.Fatalf("event fields = %#v, want only Outcome", typ)
			}
		})
	}
}

func TestRetrievalPolicyObserverFailureFailsClosed(t *testing.T) {
	observerErr := errors.New("observer failed")
	observer := &policyObserverSpy{err: observerErr}
	r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyObserver(observer))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
	if !errors.Is(err, observerErr) || response.Results != nil || len(observer.events) != 1 {
		t.Fatalf("response/error/events = %#v/%v/%#v", response, err, observer.events)
	}
	if response.Policy.Applied || response.Policy.Disposition != RetrievalPolicyFailed || response.Policy.ReasonCode != "observer_failed" {
		t.Fatalf("policy outcome = %#v, want unapplied observer failure", response.Policy)
	}
}

func TestRetrievalPolicyTypedNilObserverIsIgnored(t *testing.T) {
	var observer *panicPolicyObserver
	r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyObserver(observer))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
	if err != nil || len(response.Results) != 1 || response.Policy.Applied {
		t.Fatalf("response/error = %#v/%v", response, err)
	}
}

func TestRetrievalPolicyObserverErrorJoinsPrimaryError(t *testing.T) {
	observerErr := errors.New("observer failed")
	observer := &policyObserverSpy{err: observerErr}
	evaluator := allowPolicySpy()
	evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
		return RetrievalPolicyDecision{}, nil
	}
	r, _, _ := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator), WithRetrievalPolicyObserver(observer))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q"})
	if !errors.Is(err, ErrPolicyDenied) || !errors.Is(err, observerErr) || response.Results != nil || len(observer.events) != 1 {
		t.Fatalf("response/error/events = %#v/%v/%#v", response, err, observer.events)
	}
}

func TestRetrievalPolicyEventContainsOnlySafeOutcome(t *testing.T) {
	typ := reflect.TypeOf(RetrievalPolicyEvent{})
	if typ.NumField() != 1 || typ.Field(0).Name != "Outcome" {
		t.Fatalf("event fields = %#v, want only Outcome", typ)
	}
	observer := &policyObserverSpy{}
	result := policyScored("c1", "secret-source")
	result.Chunk.Content = "secret-content"
	r, _, _ := newPolicyRetriever(t, []ScoredResult{result}, WithRetrievalPolicyObserver(observer))
	_, err := r.RetrieveRequest(context.Background(), RetrievalRequest{
		Query: "secret-query", K: 1,
		Policy: RetrievalPolicyRequest{
			PrincipalID: "secret-principal", SessionID: "secret-session",
			AuditLabels: map[string]string{"secret-audit-label": "secret-audit-value"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	evaluator := allowPolicySpy()
	evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
		return RetrievalPolicyDecision{}, errors.New("secret-raw-error")
	}
	r, _, _ = newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator), WithRetrievalPolicyObserver(observer))
	_, _ = r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q"})
	payload, err := json.Marshal(observer.events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"secret-query", "secret-content", "secret-source", "secret-principal", "secret-session",
		"secret-audit-label", "secret-audit-value", "secret-raw-error",
	} {
		if bytes.Contains(payload, []byte(secret)) {
			t.Fatalf("event payload leaks %q: %s", secret, payload)
		}
	}
}

func TestRetrievalPolicyObserverReceivesValueOwnedOutcome(t *testing.T) {
	observer := policyObserverFunc(func(_ context.Context, event RetrievalPolicyEvent) error {
		event.Outcome.ReasonCode = "observer-mutated"
		event.Outcome.ReturnedCount = 999
		return nil
	})
	r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyObserver(observer))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
	if err != nil || len(response.Results) != 1 || response.Policy.ReasonCode != "default_allow" || response.Policy.ReturnedCount != 1 {
		t.Fatalf("response/error = %#v/%v", response, err)
	}
}

func TestRetrieveRequestDeniedBeforeEmbeddingOrSearch(t *testing.T) {
	evaluator := allowPolicySpy()
	evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
		return RetrievalPolicyDecision{}, nil
	}
	r, embedder, store := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyEvaluator(evaluator))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "secret", K: 1})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("error = %v", err)
	}
	if response.Results != nil || response.Policy.Disposition != RetrievalPolicyDenied || response.Policy.ReasonCode != "denied" {
		t.Fatalf("response = %#v", response)
	}
	if embedder.calls != 0 || store.searchMultiCalls != 0 {
		t.Fatalf("work after deny: embed=%d search=%d", embedder.calls, store.searchMultiCalls)
	}
}

func TestRetrieveRequestDenialPrecedesConstraintComposition(t *testing.T) {
	callerTags := make([]string, MaxManagedTags)
	for i := range callerTags {
		callerTags[i] = fmt.Sprintf("tag-%02d", i)
	}
	evaluateCalls := 0
	evaluator := allowPolicySpy()
	evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
		evaluateCalls++
		return RetrievalPolicyDecision{}, nil
	}
	r, embedder, store := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{
		Query:  "q",
		Scope:  RetrievalScope{Tags: callerTags},
		Policy: RetrievalPolicyRequest{Scope: RetrievalScope{Tags: []string{"request-only"}}},
	})
	if !errors.Is(err, ErrPolicyDenied) || response.Policy.ReasonCode != "denied" {
		t.Fatalf("response/error = %#v/%v", response, err)
	}
	if evaluateCalls != 1 || embedder.calls != 0 || store.searchMultiCalls != 0 {
		t.Fatalf("calls/work = %d/%d/%d", evaluateCalls, embedder.calls, store.searchMultiCalls)
	}
}

func TestRetrieveRequestEvaluatorFailureWrapsCause(t *testing.T) {
	cause := errors.New("trusted evaluator detail")
	evaluator := allowPolicySpy()
	evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
		return RetrievalPolicyDecision{}, cause
	}
	r, embedder, store := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q"})
	if !errors.Is(err, ErrPolicyEvaluatorFailed) || !errors.Is(err, cause) {
		t.Fatalf("error = %v", err)
	}
	if response.Policy.ReasonCode != "evaluator_failed" || embedder.calls != 0 || store.searchMultiCalls != 0 {
		t.Fatalf("response/work = %#v/%d/%d", response, embedder.calls, store.searchMultiCalls)
	}
}

func TestRetrieveRequestResultEvaluatorFailureWrapsCause(t *testing.T) {
	cause := errors.New("trusted result evaluator detail")
	evaluator := allowPolicySpy()
	evaluator.evaluateResults = func(context.Context, RetrievalRequest, []Chunk) ([]RetrievalResultDecision, error) {
		return nil, cause
	}
	r, embedder, store := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyEvaluator(evaluator))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
	if !errors.Is(err, ErrPolicyEvaluatorFailed) || !errors.Is(err, cause) {
		t.Fatalf("error = %v", err)
	}
	if response.Results != nil || response.Policy.ReasonCode != "evaluator_failed" || embedder.calls != 1 || store.searchMultiCalls != 1 {
		t.Fatalf("response/work = %#v/%d/%d", response, embedder.calls, store.searchMultiCalls)
	}
}

func TestRetrieveRequestNilResultDecisionsKeepAll(t *testing.T) {
	evaluator := allowPolicySpy()
	r, _, _ := newPolicyRetriever(t,
		[]ScoredResult{policyScored("c1", "s1"), policyScored("c2", "s2")},
		WithRetrievalPolicyEvaluator(evaluator),
	)
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 2})
	if err != nil || !slices.Equal(scoredIDs(response.Results), []string{"c1", "c2"}) {
		t.Fatalf("response=%#v error=%v", response, err)
	}
}

func TestRetrieveRequestFiltersAndRedactsPositionally(t *testing.T) {
	replacement := "REDACTED"
	evaluator := allowPolicySpy()
	evaluator.evaluateResults = func(_ context.Context, _ RetrievalRequest, chunks []Chunk) ([]RetrievalResultDecision, error) {
		if len(chunks) != 3 {
			t.Fatalf("evaluator candidates=%d, want capped 3", len(chunks))
		}
		chunks[0].Content = "evaluator mutation"
		chunks[1].Metadata["mutated"] = "yes"
		return []RetrievalResultDecision{
			{Keep: false},
			{Keep: true, RedactedContent: &replacement},
			{Keep: true},
		}, nil
	}
	redacted := ScoredResult{
		SearchResult: SearchResult{Chunk: Chunk{
			ID: "c2", Content: "secret", Source: "s1", StartLine: 2, EndLine: 4,
			Language: "go", Metadata: map[string]string{"owner": "docs"}, StableKey: "stable-c2",
		}, Score: 0.8, Distance: 0.2},
		RankScore: 0.7,
		Signals:   map[string]float64{"semantic": 0.8, "keyword": 0.6},
	}
	results := []ScoredResult{policyScored("c1", "s1"), redacted, policyScored("c3", "s2"), policyScored("c4", "s3")}
	r, _, _ := newPolicyRetriever(t, results, WithRetrievalPolicyEvaluator(evaluator))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(scoredIDs(response.Results), []string{"c2", "c3"}) || response.Results[0].Chunk.Content != replacement {
		t.Fatalf("results=%#v", response.Results)
	}
	wantRedacted := redacted
	wantRedacted.Chunk.Content = replacement
	if !reflect.DeepEqual(response.Results[0], wantRedacted) {
		t.Fatalf("redacted result=%#v, want only content changed from %#v", response.Results[0], redacted)
	}
	if response.Policy.CandidateCount != 3 || response.Policy.CandidateSourceCount != 2 ||
		response.Policy.FilteredCount != 1 || response.Policy.RedactedCount != 1 ||
		response.Policy.ReturnedCount != 2 || response.Policy.ReturnedSourceCount != 2 {
		t.Fatalf("outcome=%#v", response.Policy)
	}
	if _, ok := response.Results[0].Chunk.Metadata["mutated"]; ok {
		t.Fatalf("evaluator input aliased output: %#v", response.Results[0].Chunk.Metadata)
	}
}

func TestRetrieveRequestRejectsInvalidResultDecisionList(t *testing.T) {
	replacement := "must not be accepted"
	cases := []struct {
		name      string
		decisions []RetrievalResultDecision
	}{
		{name: "wrong length", decisions: []RetrievalResultDecision{}},
		{name: "dropped result has redaction", decisions: []RetrievalResultDecision{{Keep: false, RedactedContent: &replacement}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evaluator := allowPolicySpy()
			evaluator.evaluateResults = func(context.Context, RetrievalRequest, []Chunk) ([]RetrievalResultDecision, error) {
				return tc.decisions, nil
			}
			r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyEvaluator(evaluator))
			response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
			if !errors.Is(err, ErrPolicyDecisionInvalid) {
				t.Fatalf("error=%v", err)
			}
			if response.Results != nil || response.Policy.Disposition != RetrievalPolicyFailed || response.Policy.ReasonCode != "decision_invalid" {
				t.Fatalf("response=%#v", response)
			}
		})
	}
}

func TestRetrieveRequestPolicyResultsDoNotAliasStore(t *testing.T) {
	replacement := "REDACTED"
	results := []ScoredResult{{
		SearchResult: SearchResult{Chunk: Chunk{
			ID: "c1", Content: "original", Source: "source.go", StartLine: 10, EndLine: 20,
			Language: "go", Metadata: map[string]string{"owner": "store"}, StableKey: "stable-c1",
		}, Score: 0.9, Distance: 0.1},
		RankScore: 0.75,
		Signals:   map[string]float64{"semantic": 0.9, "structural": 0.4},
	}}
	wantOriginal := results[0]
	wantOriginal.Chunk.Metadata = maps.Clone(results[0].Chunk.Metadata)
	wantOriginal.Signals = maps.Clone(results[0].Signals)

	calls := 0
	var evaluatorChunks []Chunk
	evaluator := allowPolicySpy()
	evaluator.evaluateResults = func(_ context.Context, _ RetrievalRequest, chunks []Chunk) ([]RetrievalResultDecision, error) {
		calls++
		evaluatorChunks = chunks
		chunks[0].ID = "evaluator-id"
		chunks[0].Content = "evaluator-content"
		chunks[0].Source = "evaluator-source"
		chunks[0].StartLine = -1
		chunks[0].EndLine = -1
		chunks[0].Language = "evaluator-language"
		chunks[0].Metadata["owner"] = "evaluator"
		chunks[0].StableKey = "evaluator-stable-key"
		if calls == 1 {
			return []RetrievalResultDecision{{Keep: true, RedactedContent: &replacement}}, nil
		}
		return nil, nil
	}
	r, _, store := newPolicyRetriever(t, results, WithRetrievalPolicyEvaluator(evaluator))

	first, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
	if err != nil {
		t.Fatal(err)
	}
	wantRedacted := wantOriginal
	wantRedacted.Chunk.Metadata = maps.Clone(wantOriginal.Chunk.Metadata)
	wantRedacted.Signals = maps.Clone(wantOriginal.Signals)
	wantRedacted.Chunk.Content = replacement
	if len(first.Results) != 1 || !reflect.DeepEqual(first.Results[0], wantRedacted) {
		t.Fatalf("first=%#v, want=%#v", first.Results, wantRedacted)
	}
	evaluatorChunks[0].Metadata["after"] = "evaluation"
	if _, ok := first.Results[0].Chunk.Metadata["after"]; ok {
		t.Fatalf("output aliases evaluator input: %#v", first.Results[0])
	}
	first.Results[0] = policyScored("returned-mutation", "returned-source")
	first.Results[0].Chunk.Metadata["returned"] = "mutation"
	first.Results[0].Signals["returned"] = 1

	second, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Results) != 1 || !reflect.DeepEqual(second.Results[0], wantOriginal) {
		t.Fatalf("second=%#v, want=%#v", second.Results, wantOriginal)
	}
	second.Results[0].Chunk.Metadata["second"] = "mutation"
	second.Results[0].Signals["second"] = 1
	if len(store.multiResults) != 1 || !reflect.DeepEqual(store.multiResults[0], wantOriginal) {
		t.Fatalf("store mutated=%#v, want=%#v", store.multiResults, wantOriginal)
	}
}

func TestPolicyOwnedResultsCloneBeforeFreshnessStamp(t *testing.T) {
	stored := []ScoredResult{{
		SearchResult: SearchResult{Chunk: Chunk{
			ID: "c1", Content: "original", Source: "managed://document/source", StartLine: 10, EndLine: 20,
			Language: "go", Metadata: map[string]string{"managed_freshness": "original"}, StableKey: "stable-c1",
		}, Score: 0.9, Distance: 0.1},
		RankScore: 0.75,
		Signals:   map[string]float64{"semantic": 0.9},
	}}
	wantStored := cloneScoredResults(stored)
	owned := ownScoredResults(stored, true)

	stampManagedChunkStale(&owned[0].Chunk)
	owned[0].Chunk.Content = "policy mutation"
	owned[0].Signals["semantic"] = 0
	if got := owned[0].Chunk.Metadata["managed_freshness"]; got != string(DocumentFreshnessStale) {
		t.Fatalf("owned freshness=%q, want stale", got)
	}
	if !reflect.DeepEqual(stored, wantStored) {
		t.Fatalf("freshness mutated store result: got=%#v want=%#v", stored, wantStored)
	}
	if legacy := ownScoredResults(stored, false); &legacy[0] != &stored[0] {
		t.Fatal("legacy result backing slice was cloned")
	}
}

func TestRetrieveRequestEvaluatorReceivesNormalizedClone(t *testing.T) {
	request := RetrievalRequest{
		Query: "q",
		K:     1,
		Scope: RetrievalScope{Collection: " docs ", Tags: []string{" caller ", "caller"}},
		QueryContext: QueryContext{
			OpenFiles: []string{"before.go"},
			Metadata:  map[string]string{"phase": "before"},
		},
		Policy: RetrievalPolicyRequest{
			Scope:       RetrievalScope{Collection: " other ", Tags: []string{" request "}},
			AuditLabels: map[string]string{"purpose": "support"},
		},
	}
	var evaluated, evaluatedResults RetrievalRequest
	evaluator := allowPolicySpy()
	evaluator.evaluate = func(_ context.Context, req RetrievalRequest) (RetrievalPolicyDecision, error) {
		evaluated = cloneRetrievalRequest(req)
		req.Scope.Tags[0] = "mutated"
		req.Policy.Scope.Tags[0] = "mutated"
		req.QueryContext.OpenFiles[0] = "mutated.go"
		req.QueryContext.Metadata["phase"] = "mutated"
		req.Policy.AuditLabels["purpose"] = "mutated"
		return RetrievalPolicyDecision{Allow: true}, nil
	}
	evaluator.evaluateResults = func(_ context.Context, req RetrievalRequest, _ []Chunk) ([]RetrievalResultDecision, error) {
		evaluatedResults = cloneRetrievalRequest(req)
		req.Scope.Tags[0] = "mutated-results"
		req.Policy.Scope.Tags[0] = "mutated-results"
		return nil, nil
	}
	r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")}, WithRetrievalPolicyEvaluator(evaluator))
	if _, err := r.RetrieveRequest(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	wantEvaluated := cloneRetrievalRequest(request)
	wantEvaluated.Scope = RetrievalScope{Collection: "docs", Tags: []string{"caller"}}
	wantEvaluated.Policy.Scope = RetrievalScope{Collection: "other", Tags: []string{"request"}}
	if !reflect.DeepEqual(evaluated, wantEvaluated) {
		t.Fatalf("Evaluate request = %#v, want %#v", evaluated, wantEvaluated)
	}
	if !slices.Equal(evaluatedResults.Scope.Tags, []string{"caller", "request"}) ||
		!slices.Equal(evaluatedResults.Policy.Scope.Tags, []string{"caller", "request"}) {
		t.Fatalf("EvaluateResults request = %#v", evaluatedResults)
	}
	if request.Scope.Collection != " docs " || !slices.Equal(request.Scope.Tags, []string{" caller ", "caller"}) ||
		request.Policy.Scope.Collection != " other " || !slices.Equal(request.Policy.Scope.Tags, []string{" request "}) ||
		!slices.Equal(request.QueryContext.OpenFiles, []string{"before.go"}) ||
		!maps.Equal(request.QueryContext.Metadata, map[string]string{"phase": "before"}) ||
		!maps.Equal(request.Policy.AuditLabels, map[string]string{"purpose": "support"}) {
		t.Fatalf("caller request mutated: %#v", request)
	}
}

func TestRetrieveRequestInvalidDecisionFailsClosed(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	cases := []struct {
		name     string
		decision RetrievalPolicyDecision
	}{
		{name: "negative max results", decision: RetrievalPolicyDecision{Allow: true, MaxResults: -1}},
		{name: "negative max cost", decision: RetrievalPolicyDecision{Allow: true, MaxCost: -1}},
		{name: "invalid collection", decision: RetrievalPolicyDecision{Allow: true, Scope: RetrievalScope{Collection: invalidUTF8}}},
		{name: "too many tags", decision: RetrievalPolicyDecision{Allow: true, Scope: RetrievalScope{Tags: slices.Repeat([]string{"tag"}, MaxManagedTags+1)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resultCalls := 0
			evaluator := allowPolicySpy()
			evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
				return tc.decision, nil
			}
			evaluator.evaluateResults = func(context.Context, RetrievalRequest, []Chunk) ([]RetrievalResultDecision, error) {
				resultCalls++
				return nil, nil
			}
			r, embedder, store := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator))
			response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q"})
			if !errors.Is(err, ErrPolicyDecisionInvalid) {
				t.Fatalf("error = %v", err)
			}
			if response.Policy.Disposition != RetrievalPolicyFailed || response.Policy.ReasonCode != "decision_invalid" {
				t.Fatalf("response = %#v", response)
			}
			if embedder.calls != 0 || store.searchMultiCalls != 0 || resultCalls != 0 {
				t.Fatalf("work after invalid decision: embed=%d search=%d results=%d", embedder.calls, store.searchMultiCalls, resultCalls)
			}
		})
	}
}

func TestRetrieveRequestComposesScopeLimitsCostAndFreshness(t *testing.T) {
	t.Run("tightens all constraints", func(t *testing.T) {
		ctx := context.Background()
		managed, _, sqliteStore := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
		for i := 1; i <= 3; i++ {
			document, err := managed.IngestText(ctx, fmt.Sprintf("source-%d.md", i), fmt.Sprintf("content-%d", i), DocumentOptions{
				Collection: "docs",
				Tags:       []string{"caller", "decision", "request"},
			})
			if err != nil {
				t.Fatal(err)
			}
			replaceManagedScopeChunk(t, sqliteStore, document, fmt.Sprintf("c%d", i), []float64{1 - float64(i-1)/10, float64(i-1) / 10})
		}
		var evaluated, evaluatedResults RetrievalRequest
		var resultChunks []Chunk
		evaluator := allowPolicySpy()
		evaluator.evaluate = func(_ context.Context, req RetrievalRequest) (RetrievalPolicyDecision, error) {
			evaluated = cloneRetrievalRequest(req)
			return RetrievalPolicyDecision{
				Allow:        true,
				Scope:        RetrievalScope{Collection: "docs", Tags: []string{"decision"}},
				RequireFresh: true,
				MaxResults:   2,
				MaxCost:      3,
			}, nil
		}
		evaluator.evaluateResults = func(_ context.Context, req RetrievalRequest, chunks []Chunk) ([]RetrievalResultDecision, error) {
			evaluatedResults = cloneRetrievalRequest(req)
			resultChunks = slices.Clone(chunks)
			chunks[0].Content = "mutated by evaluator"
			chunks[0].Metadata["mutated"] = "true"
			return nil, nil
		}
		r, err := NewRetrieverWithEmbedder(
			&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}},
			sqliteStore,
			WithVectorOnly(),
			WithRetrievalPolicyEvaluator(evaluator),
		)
		if err != nil {
			t.Fatal(err)
		}
		request := RetrievalRequest{
			Query: "q",
			K:     4,
			Scope: RetrievalScope{Collection: " docs ", Tags: []string{"caller"}},
			Policy: RetrievalPolicyRequest{
				PrincipalID: "p", SessionID: "s",
				Scope:       RetrievalScope{Collection: "docs", Tags: []string{"request"}},
				MaxResults:  3,
				MaxCost:     9,
				AuditLabels: map[string]string{"purpose": "support"},
			},
		}
		response, err := r.RetrieveRequest(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		wantEvaluated := cloneRetrievalRequest(request)
		wantEvaluated.Scope.Collection = "docs"
		if !reflect.DeepEqual(evaluated, wantEvaluated) {
			t.Fatalf("Evaluate request = %#v, want %#v", evaluated, wantEvaluated)
		}
		want := RetrievalRequest{
			Query: "q",
			K:     2,
			Scope: RetrievalScope{Collection: "docs", Tags: []string{"caller", "decision", "request"}},
			Policy: RetrievalPolicyRequest{
				PrincipalID: "p", SessionID: "s",
				Scope:        RetrievalScope{Collection: "docs", Tags: []string{"caller", "decision", "request"}},
				RequireFresh: true,
				MaxResults:   2,
				MaxCost:      3,
				AuditLabels:  map[string]string{"purpose": "support"},
			},
		}
		if !reflect.DeepEqual(evaluatedResults, want) {
			t.Fatalf("EvaluateResults request = %#v, want %#v", evaluatedResults, want)
		}
		if len(resultChunks) != 2 || len(response.Results) != 2 {
			t.Fatalf("candidates/returned = %d/%d", len(resultChunks), len(response.Results))
		}
		if response.Results[0].Chunk.Content == "mutated by evaluator" || response.Results[0].Chunk.Metadata["mutated"] != "" {
			t.Fatalf("evaluator mutated response results: %#v", response.Results[0])
		}
	})

	t.Run("larger decision ceiling does not widen", func(t *testing.T) {
		evaluator := allowPolicySpy()
		evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
			return RetrievalPolicyDecision{Allow: true, MaxResults: 9, MaxCost: 9}, nil
		}
		var effective RetrievalRequest
		evaluator.evaluateResults = func(_ context.Context, req RetrievalRequest, _ []Chunk) ([]RetrievalResultDecision, error) {
			effective = cloneRetrievalRequest(req)
			return nil, nil
		}
		r, _, store := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1"), policyScored("c2", "s2"), policyScored("c3", "s3")}, WithRetrievalPolicyEvaluator(evaluator))
		response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{
			Query: "q", K: 4,
			Policy: RetrievalPolicyRequest{MaxResults: 2, MaxCost: 3},
		})
		if err != nil {
			t.Fatal(err)
		}
		if effective.K != 2 || effective.Policy.MaxResults != 2 || effective.Policy.MaxCost != 3 || store.gotK != 2 || len(response.Results) != 2 {
			t.Fatalf("effective/search/results = %#v/%d/%d", effective, store.gotK, len(response.Results))
		}
	})
}

func TestRetrieveRequestConflictingCollectionsSkipSearch(t *testing.T) {
	resultCalls := 0
	resultChunks := -1
	evaluator := allowPolicySpy()
	evaluator.evaluateResults = func(_ context.Context, _ RetrievalRequest, chunks []Chunk) ([]RetrievalResultDecision, error) {
		resultCalls++
		resultChunks = len(chunks)
		return nil, nil
	}
	r, embedder, store := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{
		Query:  "q",
		Scope:  RetrievalScope{Collection: "caller"},
		Policy: RetrievalPolicyRequest{Scope: RetrievalScope{Collection: "request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resultCalls != 1 || resultChunks != 0 || embedder.calls != 0 || store.searchMultiCalls != 0 {
		t.Fatalf("calls/chunks/work = %d/%d/%d/%d", resultCalls, resultChunks, embedder.calls, store.searchMultiCalls)
	}
	if response.Results != nil || !response.Policy.Applied || response.Policy.Disposition != RetrievalPolicyAllowed ||
		response.Policy.ReasonCode != "allowed" || response.Policy.CandidateCount != 0 || response.Policy.ReturnedCount != 0 {
		t.Fatalf("response = %#v", response)
	}
}

func TestRetrieveRequestDecisionTagsDeduplicateBeforeUnionLimit(t *testing.T) {
	tags := make([]string, MaxManagedTags)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag-%02d", i)
	}
	evaluator := allowPolicySpy()
	evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
		return RetrievalPolicyDecision{Allow: true, Scope: RetrievalScope{Tags: []string{tags[0]}}}, nil
	}
	var effective RetrievalRequest
	evaluator.evaluateResults = func(_ context.Context, req RetrievalRequest, _ []Chunk) ([]RetrievalResultDecision, error) {
		effective = cloneRetrievalRequest(req)
		return nil, nil
	}
	r, _, _ := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{
		Query:  "q",
		Scope:  RetrievalScope{Collection: "caller", Tags: tags},
		Policy: RetrievalPolicyRequest{Scope: RetrievalScope{Collection: "request"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Policy.Disposition != RetrievalPolicyAllowed || !slices.Equal(effective.Scope.Tags, tags) {
		t.Fatalf("response/effective = %#v/%#v", response, effective)
	}
}

func TestLegacyRetrievalMethodsEnforceConfiguredEvaluator(t *testing.T) {
	cases := []struct {
		name string
		run  func(*Retriever) error
	}{
		{name: "Retrieve", run: func(r *Retriever) error { _, err := r.Retrieve(context.Background(), "q", 1); return err }},
		{name: "RetrieveScoped", run: func(r *Retriever) error {
			_, err := r.RetrieveScoped(context.Background(), "q", 1, RetrievalScope{})
			return err
		}},
		{name: "RetrieveScored", run: func(r *Retriever) error {
			_, err := r.RetrieveScored(context.Background(), "q", 1, QueryContext{})
			return err
		}},
		{name: "RetrieveScoredScoped", run: func(r *Retriever) error {
			_, err := r.RetrieveScoredScoped(context.Background(), "q", 1, RetrievalScope{}, QueryContext{})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evaluateCalls := 0
			evaluator := allowPolicySpy()
			evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
				evaluateCalls++
				return RetrievalPolicyDecision{}, nil
			}
			r, embedder, store := newPolicyRetriever(t, nil, WithRetrievalPolicyEvaluator(evaluator))
			if err := tc.run(r); !errors.Is(err, ErrPolicyDenied) {
				t.Fatalf("error = %v", err)
			}
			if evaluateCalls != 1 || embedder.calls != 0 || store.searchMultiCalls != 0 {
				t.Fatalf("calls/work = %d/%d/%d", evaluateCalls, embedder.calls, store.searchMultiCalls)
			}
		})
	}
}

func policyScored(id, source string) ScoredResult {
	return ScoredResult{
		SearchResult: SearchResult{Chunk: Chunk{ID: id, Source: source, Metadata: map[string]string{}}},
		Signals:      map[string]float64{},
	}
}

func TestRetrieveRequestRequireFreshKeepsRegistryVerifiedText(t *testing.T) {
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	document, err := managed.IngestText(context.Background(), "runbook", "trusted text", DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRetrieverWithEmbedder(
		&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 1}}, VectorSpaceID: "test/v1"}},
		store,
		WithVectorOnly(),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 5, Policy: RetrievalPolicyRequest{RequireFresh: true}})
	if err != nil || len(response.Results) != 1 || response.Results[0].Chunk.Source != document.source {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	if response.Policy.StaleDroppedCount != 0 {
		t.Fatalf("outcome=%#v", response.Policy)
	}
}

func TestRetrieveRequestRequireFreshRejectsUnknown(t *testing.T) {
	result := policyScored("c1", "custom")
	result.Chunk.Metadata["managed_freshness"] = string(DocumentFreshnessFresh)
	r, _, _ := newPolicyRetriever(t, []ScoredResult{result})
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", Policy: RetrievalPolicyRequest{RequireFresh: true}})
	if !errors.Is(err, ErrFreshnessUnknown) {
		t.Fatalf("error=%v", err)
	}
	if response.Results != nil || response.Policy.ReasonCode != "freshness_unknown" {
		t.Fatalf("response=%#v", response)
	}
}

func TestRetrieveRequestRequireFreshDropsKnownStale(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	path := filepath.Join(t.TempDir(), "runbook.md")
	if err := os.WriteFile(path, []byte("indexed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := managed.IngestFile(ctx, path, DocumentOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRetrieverWithEmbedder(
		&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 1}}, VectorSpaceID: "test/v1"}},
		store,
		WithVectorOnly(),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := r.RetrieveRequest(ctx, RetrievalRequest{Query: "q", K: 5, Policy: RetrievalPolicyRequest{RequireFresh: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 || response.Policy.CandidateCount != 1 || response.Policy.StaleDroppedCount != 1 || response.Policy.ReturnedCount != 0 {
		t.Fatalf("response=%#v", response)
	}
}

func TestRetrieveRequestCapsOverReturnedCandidatesBeforeFreshness(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	paths := []string{filepath.Join(t.TempDir(), "first.md"), filepath.Join(t.TempDir(), "outside.md")}
	results := make([]ScoredResult, 0, len(paths))
	for _, path := range paths {
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
		document, err := managed.IngestFile(ctx, path, DocumentOptions{})
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, ScoredResult{SearchResult: SearchResult{Chunk: requireManagedChunks(t, store, document.source)[0].Chunk}})
	}

	var reads []string
	r := &Retriever{store: store, readManagedFile: func(_ context.Context, path string) ([]byte, error) {
		reads = append(reads, path)
		return os.ReadFile(path)
	}}
	got, freshness, err := r.prepareUnscopedScoredResults(ctx, results, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(freshness) != 1 || !slices.Equal(reads, paths[:1]) {
		t.Fatalf("results/freshness/reads = %d/%d/%v, want 1/1/%v", len(got), len(freshness), reads, paths[:1])
	}
}

func TestRetrieveRequestRequireFreshIgnoresForgedMetadata(t *testing.T) {
	result := policyScored("c1", "custom")
	result.Chunk.Metadata["managed_freshness"] = string(DocumentFreshnessFresh)
	r, _, _ := newPolicyRetriever(t, []ScoredResult{result})
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", Policy: RetrievalPolicyRequest{RequireFresh: true}})
	if !errors.Is(err, ErrFreshnessUnknown) || response.Results != nil || response.Policy.ReasonCode != "freshness_unknown" {
		t.Fatalf("response=%#v error=%v", response, err)
	}
}

func TestRetrieveRequestFreshnessDoesNotMutateStoredMetadata(t *testing.T) {
	result := policyScored("c1", "custom")
	result.Chunk.Metadata["managed_freshness"] = "original"
	stored := result.Chunk.Metadata
	r, _, store := newPolicyRetriever(t, []ScoredResult{result})
	for i := 0; i < 2; i++ {
		response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", Policy: RetrievalPolicyRequest{RequireFresh: true}})
		if !errors.Is(err, ErrFreshnessUnknown) || response.Results != nil {
			t.Fatalf("retrieve %d: response=%#v error=%v", i+1, response, err)
		}
		if got := store.multiResults[0].Chunk.Metadata["managed_freshness"]; got != "original" {
			t.Fatalf("retrieve %d mutated stored freshness to %q", i+1, got)
		}
		stored["map_identity"] = "preserved"
		if got := store.multiResults[0].Chunk.Metadata["map_identity"]; got != "preserved" {
			t.Fatalf("retrieve %d replaced the stored metadata map", i+1)
		}
		delete(stored, "map_identity")
	}
}

func scoredIDs(results []ScoredResult) []string {
	ids := make([]string, len(results))
	for i := range results {
		ids[i] = results[i].Chunk.ID
	}
	return ids
}

func TestRetrieveRequestDefaultPolicyConstraints(t *testing.T) {
	cases := []struct {
		name    string
		request RetrievalRequest
		wantIDs []string
	}{
		{name: "max results", request: RetrievalRequest{Query: "q", K: 5, Policy: RetrievalPolicyRequest{MaxResults: 1}}, wantIDs: []string{"c1"}},
		{name: "request k wins", request: RetrievalRequest{Query: "q", K: 1, Policy: RetrievalPolicyRequest{MaxResults: 5}}, wantIDs: []string{"c1"}},
		{name: "zero remains unbounded", request: RetrievalRequest{Query: "q", K: 0, Policy: RetrievalPolicyRequest{MaxCost: 7}}, wantIDs: []string{"c1", "c2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _, _ := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1"), policyScored("c2", "s2")})
			response, err := r.RetrieveRequest(context.Background(), tc.request)
			if err != nil {
				t.Fatal(err)
			}
			if got := scoredIDs(response.Results); !slices.Equal(got, tc.wantIDs) {
				t.Fatalf("ids = %v, want %v", got, tc.wantIDs)
			}
			if !response.Policy.Applied || response.Policy.ReasonCode != "default_allow" {
				t.Fatalf("outcome = %#v", response.Policy)
			}
		})
	}
}

func TestRetrieveRequestRejectsInvalidPolicyInputBeforeWork(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	tooManyLabels := make(map[string]string, maxRetrievalAuditLabels+1)
	for i := 0; i <= maxRetrievalAuditLabels; i++ {
		tooManyLabels[string(rune(i+1))] = "value"
	}
	cases := []struct {
		name   string
		policy RetrievalPolicyRequest
	}{
		{name: "negative max results", policy: RetrievalPolicyRequest{MaxResults: -1}},
		{name: "negative max cost", policy: RetrievalPolicyRequest{MaxCost: -1}},
		{name: "invalid principal utf8", policy: RetrievalPolicyRequest{PrincipalID: invalidUTF8}},
		{name: "invalid session utf8", policy: RetrievalPolicyRequest{SessionID: invalidUTF8}},
		{name: "principal too long", policy: RetrievalPolicyRequest{PrincipalID: strings.Repeat("p", maxRetrievalIdentityBytes+1)}},
		{name: "session too long", policy: RetrievalPolicyRequest{SessionID: strings.Repeat("s", maxRetrievalIdentityBytes+1)}},
		{name: "too many audit labels", policy: RetrievalPolicyRequest{AuditLabels: tooManyLabels}},
		{name: "invalid audit key utf8", policy: RetrievalPolicyRequest{AuditLabels: map[string]string{invalidUTF8: "value"}}},
		{name: "invalid audit value utf8", policy: RetrievalPolicyRequest{AuditLabels: map[string]string{"key": invalidUTF8}}},
		{name: "audit key too long", policy: RetrievalPolicyRequest{AuditLabels: map[string]string{strings.Repeat("k", maxRetrievalAuditBytes+1): "value"}}},
		{name: "audit value too long", policy: RetrievalPolicyRequest{AuditLabels: map[string]string{"key": strings.Repeat("v", maxRetrievalAuditBytes+1)}}},
		{name: "collection too long", policy: RetrievalPolicyRequest{Scope: RetrievalScope{Collection: strings.Repeat("c", MaxManagedMetadataBytes+1)}}},
		{name: "too many tags", policy: RetrievalPolicyRequest{Scope: RetrievalScope{Tags: slices.Repeat([]string{"tag"}, MaxManagedTags+1)}}},
		{name: "tag too long", policy: RetrievalPolicyRequest{Scope: RetrievalScope{Tags: []string{strings.Repeat("t", MaxManagedTagBytes+1)}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, embedder, store := newPolicyRetriever(t, []ScoredResult{policyScored("c1", "s1")})
			response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", Policy: tc.policy})
			if !errors.Is(err, ErrPolicyDecisionInvalid) {
				t.Fatalf("error = %v, want ErrPolicyDecisionInvalid", err)
			}
			if response.Results != nil {
				t.Fatalf("results = %#v, want nil", response.Results)
			}
			if response.Policy.ReasonCode != "request_invalid" {
				t.Fatalf("outcome = %#v, want request_invalid", response.Policy)
			}
			if embedder.calls != 0 || store.searchMultiCalls != 0 {
				t.Fatalf("work: embeds=%d searches=%d, want 0/0", embedder.calls, store.searchMultiCalls)
			}
		})
	}
}

func TestRetrieveRequestPolicyMetadataIsCloned(t *testing.T) {
	request := RetrievalRequest{
		Query: "q",
		QueryContext: QueryContext{
			OpenFiles: []string{"before.go"},
			Metadata:  map[string]string{"phase": "before"},
		},
		Scope: RetrievalScope{Tags: []string{" "}},
		Policy: RetrievalPolicyRequest{
			Scope:       RetrievalScope{Tags: []string{"\t"}},
			AuditLabels: map[string]string{"label": "before"},
		},
	}
	wantOpenFiles := slices.Clone(request.QueryContext.OpenFiles)
	wantMetadata := maps.Clone(request.QueryContext.Metadata)
	store := &retrievalPolicyMultiStore{retrieverMultiStore: retrieverMultiStore{multiResults: []ScoredResult{policyScored("c1", "s1")}}}
	embedder := &recordingEmbedder{
		result: EmbedResult{Embeddings: [][]float64{{1, 0}}},
		beforeFunc: func(context.Context) {
			request.QueryContext.OpenFiles[0] = "after.go"
			request.QueryContext.Metadata["phase"] = "after"
			request.Scope.Tags[0] = "blocked"
			request.Policy.Scope.Tags[0] = "blocked"
			delete(request.Policy.AuditLabels, "label")
			request.Policy.AuditLabels["after-a"] = "one"
			request.Policy.AuditLabels["after-b"] = "two"
		},
	}
	r, err := NewRetrieverWithEmbedder(embedder, store)
	if err != nil {
		t.Fatal(err)
	}
	response, err := r.RetrieveRequest(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := scoredIDs(response.Results); !slices.Equal(got, []string{"c1"}) {
		t.Fatalf("ids = %v, want [c1]", got)
	}
	if !slices.Equal(store.gotQueryContext.OpenFiles, wantOpenFiles) || !maps.Equal(store.gotQueryContext.Metadata, wantMetadata) {
		t.Fatalf("query context = %#v, want open=%v metadata=%v", store.gotQueryContext, wantOpenFiles, wantMetadata)
	}
	if response.Policy.AuditLabelCount != 1 {
		t.Fatalf("audit label count = %d, want 1", response.Policy.AuditLabelCount)
	}
	if !slices.Equal(request.QueryContext.OpenFiles, []string{"after.go"}) ||
		!maps.Equal(request.QueryContext.Metadata, map[string]string{"phase": "after"}) ||
		!slices.Equal(request.Scope.Tags, []string{"blocked"}) ||
		!slices.Equal(request.Policy.Scope.Tags, []string{"blocked"}) ||
		!maps.Equal(request.Policy.AuditLabels, map[string]string{"after-a": "one", "after-b": "two"}) {
		t.Fatalf("caller collections changed unexpectedly: %#v", request)
	}
}

func TestComposePolicyRequestDeduplicatesTagsBeforeUnionLimit(t *testing.T) {
	callerTags := make([]string, MaxManagedTags)
	for i := range callerTags {
		callerTags[i] = fmt.Sprintf("tag-%02d", i)
	}

	policy, err := composePolicyRequest(RetrievalRequest{
		Scope:  RetrievalScope{Tags: callerTags},
		Policy: RetrievalPolicyRequest{Scope: RetrievalScope{Tags: []string{callerTags[0]}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := policy.request.Scope.Tags; !slices.Equal(got, callerTags) {
		t.Fatalf("composed tags = %v, want %v", got, callerTags)
	}
}

func TestRetrieveRequestPolicyScopeComposesWithCallerScope(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	documents := []struct {
		name       string
		id         string
		collection string
		tags       []string
		embedding  []float64
	}{
		{name: "match.md", id: "match", collection: "ops", tags: []string{"alpha", "beta"}, embedding: []float64{1, 0}},
		{name: "caller-only.md", id: "caller-only", collection: "ops", tags: []string{"alpha"}, embedding: []float64{0.9, 0.1}},
		{name: "policy-only.md", id: "policy-only", collection: "ops", tags: []string{"beta"}, embedding: []float64{0.8, 0.2}},
		{name: "wrong-collection.md", id: "wrong-collection", collection: "other", tags: []string{"alpha", "beta"}, embedding: []float64{0.7, 0.3}},
	}
	for _, item := range documents {
		document, err := managed.IngestText(ctx, item.name, item.id, DocumentOptions{Collection: item.collection, Tags: item.tags})
		if err != nil {
			t.Fatal(err)
		}
		replaceManagedScopeChunk(t, store, document, item.id, item.embedding)
	}
	embedder := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}}
	r, err := NewRetrieverWithEmbedder(embedder, store, WithVectorOnly())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("matching collections and tag union", func(t *testing.T) {
		response, err := r.RetrieveRequest(ctx, RetrievalRequest{
			Query: "q",
			K:     10,
			Scope: RetrievalScope{Collection: " ops ", Tags: []string{"alpha"}},
			Policy: RetrievalPolicyRequest{
				Scope: RetrievalScope{Collection: "ops", Tags: []string{"beta"}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := scoredIDs(response.Results); !slices.Equal(got, []string{"match"}) {
			t.Fatalf("ids = %v, want [match]", got)
		}
	})

	t.Run("conflicting collections are empty", func(t *testing.T) {
		beforeEmbeds := embedder.calls
		response, err := r.RetrieveRequest(ctx, RetrievalRequest{
			Query:  "q",
			K:      10,
			Scope:  RetrievalScope{Collection: "ops"},
			Policy: RetrievalPolicyRequest{Scope: RetrievalScope{Collection: "other"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if response.Results != nil {
			t.Fatalf("results = %#v, want nil", response.Results)
		}
		if embedder.calls != beforeEmbeds {
			t.Fatalf("embed calls = %d, want %d", embedder.calls, beforeEmbeds)
		}
	})
}

func TestRetrieveRequestCountsCandidatesAndSources(t *testing.T) {
	r, _, _ := newPolicyRetriever(t, []ScoredResult{
		policyScored("c1", "s1"),
		policyScored("c2", "s1"),
		policyScored("c3", "s2"),
	})
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{
		Query: "q",
		K:     5,
		Policy: RetrievalPolicyRequest{
			MaxResults:  3,
			AuditLabels: map[string]string{"team": "secret-alpha", "ticket": "secret-beta"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome := response.Policy
	if outcome.CandidateCount != 3 || outcome.CandidateSourceCount != 2 ||
		outcome.ReturnedCount != 3 || outcome.ReturnedSourceCount != 2 || outcome.AuditLabelCount != 2 {
		t.Fatalf("outcome = %#v", outcome)
	}
	if text := fmt.Sprintf("%#v", outcome); strings.Contains(text, "secret-alpha") || strings.Contains(text, "secret-beta") {
		t.Fatalf("outcome leaks audit values: %s", text)
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
	cleared := NewRetriever(nil, nil, WithRetrievalPolicyEvaluator(evaluator), WithRetrievalPolicyEvaluator(typedNil))
	if cleared.PolicyActive() {
		t.Fatal("typed-nil evaluator did not clear earlier policy")
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

func TestRetrieveRequestLegacyFastPath(t *testing.T) {
	store := &retrieverMultiStore{multiResults: []ScoredResult{{
		SearchResult: SearchResult{Chunk: Chunk{ID: "c1", Content: "legacy"}, Score: 0.0163, Distance: 0.1},
		RankScore:    0.7,
		Signals:      map[string]float64{"semantic": 0.9},
	}}}
	embedder := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}
	retriever, err := NewRetrieverWithEmbedder(embedder, store)
	if err != nil {
		t.Fatal(err)
	}
	qctx := QueryContext{CurrentFile: "main.go"}
	response, err := retriever.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 3, QueryContext: qctx})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 1 || response.Results[0].Score != 0.0163 || response.Results[0].RankScore != 0.7 {
		t.Fatalf("canonical results = %#v", response.Results)
	}
	if response.Policy != (RetrievalPolicyOutcome{}) {
		t.Fatalf("legacy policy outcome = %#v, want zero", response.Policy)
	}
	if store.gotK != 3 || store.gotQuery != "q" {
		t.Fatalf("search got query=%q k=%d", store.gotQuery, store.gotK)
	}
}

func TestRetrieverLegacyMethodsUseCanonicalResults(t *testing.T) {
	newRetriever := func(t *testing.T, results []ScoredResult) (*Retriever, *retrievalPolicyMultiStore) {
		t.Helper()
		store := &retrievalPolicyMultiStore{retrieverMultiStore: retrieverMultiStore{multiResults: results}}
		r, err := NewRetrieverWithEmbedder(
			&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}},
			store,
		)
		if err != nil {
			t.Fatal(err)
		}
		return r, store
	}
	want := []ScoredResult{{
		SearchResult: SearchResult{Chunk: Chunk{ID: "c1"}, Score: 0.0163, Distance: 0.1},
		RankScore:    0.7,
		Signals:      map[string]float64{"semantic": 0.9},
	}}

	t.Run("Retrieve", func(t *testing.T) {
		r, _ := newRetriever(t, want)
		got, err := r.Retrieve(context.Background(), "q", 1)
		if err != nil || len(got) != 1 || got[0].Score != 0.9 {
			t.Fatalf("Retrieve = %#v, %v", got, err)
		}

		dense := &retrieverPlainStore{searchResults: []SearchResult{want[0].SearchResult}}
		r, err = NewRetrieverWithEmbedder(
			&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}},
			dense,
			WithVectorOnly(),
		)
		if err != nil {
			t.Fatal(err)
		}
		got, err = r.Retrieve(context.Background(), "q", 1)
		if err != nil || len(got) != 1 || got[0].Score != 0.9 {
			t.Fatalf("dense Retrieve = %#v, %v", got, err)
		}

		for _, results := range [][]ScoredResult{nil, {}} {
			r, _ := newRetriever(t, results)
			empty, err := r.Retrieve(context.Background(), "q", 1)
			if err != nil || empty != nil {
				t.Fatalf("empty Retrieve = %#v, %v; want nil, nil", empty, err)
			}
		}
	})

	t.Run("RetrieveScoped", func(t *testing.T) {
		r, _ := newRetriever(t, want)
		got, err := r.RetrieveScoped(context.Background(), "q", 1, RetrievalScope{})
		if err != nil || len(got) != 1 || got[0].Score != 0.9 {
			t.Fatalf("RetrieveScoped empty scope = %#v, %v", got, err)
		}
		for _, results := range [][]ScoredResult{nil, {}} {
			r, _ := newRetriever(t, results)
			empty, err := r.RetrieveScoped(context.Background(), "q", 1, RetrievalScope{})
			if err != nil || empty != nil {
				t.Fatalf("empty RetrieveScoped = %#v, %v; want nil, nil", empty, err)
			}
		}

		ctx := context.Background()
		managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
		outside, err := managed.IngestText(ctx, "outside.md", "outside", DocumentOptions{Collection: "other", Tags: []string{"alpha"}})
		if err != nil {
			t.Fatal(err)
		}
		match, err := managed.IngestText(ctx, "match.md", "match", DocumentOptions{Collection: "ops", Tags: []string{"alpha"}})
		if err != nil {
			t.Fatal(err)
		}
		replaceManagedScopeChunk(t, store, outside, "outside", []float64{1, 0})
		replaceManagedScopeChunk(t, store, match, "match", []float64{0.9, 0.1})
		r, err = NewRetrieverWithEmbedder(
			&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}},
			store,
			WithVectorOnly(),
		)
		if err != nil {
			t.Fatal(err)
		}
		got, err = r.RetrieveScoped(ctx, "q", 1, RetrievalScope{Collection: "ops", Tags: []string{"alpha"}})
		if err != nil || len(got) != 1 || got[0].Chunk.ID != "match" || got[0].Score != 1-got[0].Distance {
			t.Fatalf("mutable RetrieveScoped = %#v, %v", got, err)
		}

		readOnly, matchSource := newReadOnlyManagedScopeStore(t)
		r, err = NewRetrieverWithEmbedder(
			&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}},
			readOnly,
			WithVectorOnly(),
		)
		if err != nil {
			t.Fatal(err)
		}
		got, err = r.RetrieveScoped(ctx, "alpha", 1, RetrievalScope{Collection: "ops", Tags: []string{"alpha"}})
		if err != nil || len(got) != 1 || got[0].Chunk.Source != matchSource || got[0].Score != 1-got[0].Distance {
			t.Fatalf("immutable RetrieveScoped = %#v, %v; want source %q", got, err, matchSource)
		}
	})

	t.Run("RetrieveScored", func(t *testing.T) {
		r, store := newRetriever(t, want)
		qctx := QueryContext{CurrentFile: "main.go"}
		got, err := r.RetrieveScored(context.Background(), "q", 1, qctx)
		if err != nil || len(got) != 1 || got[0].Score != 0.0163 || got[0].RankScore != 0.7 || got[0].Signals["semantic"] != 0.9 {
			t.Fatalf("RetrieveScored = %#v, %v", got, err)
		}
		if store.gotQueryContext.CurrentFile != qctx.CurrentFile {
			t.Fatalf("RetrieveScored QueryContext = %#v, want %#v", store.gotQueryContext, qctx)
		}
	})

	t.Run("RetrieveScoredScoped", func(t *testing.T) {
		r, store := newRetriever(t, want)
		qctx := QueryContext{CurrentFile: "main.go"}
		got, err := r.RetrieveScoredScoped(context.Background(), "q", 1, RetrievalScope{}, qctx)
		if err != nil || len(got) != 1 || got[0].Score != 0.0163 || got[0].RankScore != 0.7 || got[0].Signals["semantic"] != 0.9 {
			t.Fatalf("RetrieveScoredScoped empty scope = %#v, %v", got, err)
		}
		if store.gotQueryContext.CurrentFile != qctx.CurrentFile {
			t.Fatalf("RetrieveScoredScoped QueryContext = %#v, want %#v", store.gotQueryContext, qctx)
		}

		ctx := context.Background()
		managed, _, sqliteStore := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
		outside, err := managed.IngestText(ctx, "outside.md", "alpha", DocumentOptions{Collection: "other", Tags: []string{"alpha"}})
		if err != nil {
			t.Fatal(err)
		}
		match, err := managed.IngestText(ctx, "match.md", "alpha", DocumentOptions{Collection: "ops", Tags: []string{"alpha"}})
		if err != nil {
			t.Fatal(err)
		}
		replaceManagedScopeChunk(t, sqliteStore, outside, "outside", []float64{1, 0})
		matchChunk := replaceManagedScopeChunk(t, sqliteStore, match, "match", []float64{0.9, 0.1})
		r, err = NewRetrieverWithEmbedder(
			&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}},
			sqliteStore,
		)
		if err != nil {
			t.Fatal(err)
		}
		got, err = r.RetrieveScoredScoped(ctx, "alpha", 1, RetrievalScope{Collection: "ops", Tags: []string{"alpha"}}, QueryContext{CurrentFile: matchChunk.Source})
		if err != nil || len(got) != 1 || got[0].Chunk.ID != "match" || got[0].RankScore == 0 || got[0].Signals["structural"] == 0 {
			t.Fatalf("mutable RetrieveScoredScoped = %#v, %v", got, err)
		}

		readOnly, matchSource := newReadOnlyManagedScopeStore(t)
		r, err = NewRetrieverWithEmbedder(
			&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}},
			readOnly,
		)
		if err != nil {
			t.Fatal(err)
		}
		got, err = r.RetrieveScoredScoped(ctx, "alpha", 1, RetrievalScope{Collection: "ops", Tags: []string{"alpha"}}, QueryContext{CurrentFile: matchSource})
		if err != nil || len(got) != 1 || got[0].Chunk.Source != matchSource || got[0].RankScore == 0 || got[0].Signals["structural"] == 0 {
			t.Fatalf("immutable RetrieveScoredScoped = %#v, %v; want source %q", got, err, matchSource)
		}
	})
}
