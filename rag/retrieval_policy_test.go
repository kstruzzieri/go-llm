package rag

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

func newPolicyRetriever(t *testing.T, results []ScoredResult) (*Retriever, *recordingEmbedder, *retrieverMultiStore) {
	t.Helper()
	embedder := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}}
	store := &retrieverMultiStore{multiResults: results}
	r, err := NewRetrieverWithEmbedder(embedder, store)
	if err != nil {
		t.Fatal(err)
	}
	return r, embedder, store
}

func policyScored(id, source string) ScoredResult {
	return ScoredResult{
		SearchResult: SearchResult{Chunk: Chunk{ID: id, Source: source, Metadata: map[string]string{}}},
		Signals:      map[string]float64{},
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
