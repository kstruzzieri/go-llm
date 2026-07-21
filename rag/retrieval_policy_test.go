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
