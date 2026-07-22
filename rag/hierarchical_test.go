package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

type hierarchicalDenseStore struct {
	results []SearchResult
	gotK    int
}

func (*hierarchicalDenseStore) Store(context.Context, []Chunk, [][]float64) error { return nil }
func (s *hierarchicalDenseStore) Search(_ context.Context, _ []float64, k int) ([]SearchResult, error) {
	s.gotK = k
	results := slices.Clone(s.results)
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	return results, nil
}
func (*hierarchicalDenseStore) DeleteBySource(context.Context, string) error { return nil }
func (*hierarchicalDenseStore) Stats(context.Context) (StoreStats, error)    { return StoreStats{}, nil }
func (*hierarchicalDenseStore) Close() error                                 { return nil }

type hierarchicalMultiStore struct {
	retrieverPlainStore
	results []ScoredResult
	gotK    int
}

func (s *hierarchicalMultiStore) SearchMulti(_ context.Context, _ []float64, _ string, k int, _ QueryContext) ([]ScoredResult, error) {
	s.gotK = k
	results := cloneScoredResults(s.results)
	if k > 0 && len(results) > k {
		results = results[:k]
	}
	return results, nil
}

func hierarchicalResult(id, source, content string, score float64, metadata map[string]string) ScoredResult {
	return ScoredResult{
		SearchResult: SearchResult{
			Chunk: Chunk{ID: id, Source: source, Content: content, StartLine: 1, EndLine: 1, Metadata: metadata},
			Score: score, Distance: 1 - score,
		},
		RankScore: score,
		Signals:   map[string]float64{"semantic": score},
	}
}

func newHierarchicalMultiRetriever(t *testing.T, results []ScoredResult, opts ...RetrieverOption) (*Retriever, *hierarchicalMultiStore) {
	t.Helper()
	store := &hierarchicalMultiStore{results: results}
	r, err := NewRetrieverWithEmbedder(
		&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}}},
		store,
		opts...,
	)
	if err != nil {
		t.Fatal(err)
	}
	return r, store
}

func hierarchicalRequest(k, candidates, depth, groups, tokens int) HierarchicalRetrievalRequest {
	return HierarchicalRetrievalRequest{
		Request:        RetrievalRequest{Query: "q", K: k, QueryContext: QueryContext{WorkspaceRoot: "/work"}},
		CandidateLimit: candidates,
		MaxDepth:       depth,
		MaxGroups:      groups,
		MaxTokens:      tokens,
		Timeout:        time.Second,
	}
}

func resultIDs(results []ScoredResult) []string {
	ids := make([]string, len(results))
	for i := range results {
		ids[i] = results[i].Chunk.ID
	}
	return ids
}

func skipCount(trace HierarchicalRetrievalTrace, reason string) int {
	for _, skip := range trace.Skipped {
		if skip.Reason == reason {
			return skip.Count
		}
	}
	return 0
}

func TestRetrieveHierarchicalValidatesPositiveBounds(t *testing.T) {
	valid := hierarchicalRequest(1, 2, 1, 1, 1)
	cases := map[string]func(*HierarchicalRetrievalRequest){
		"results":           func(req *HierarchicalRetrievalRequest) { req.Request.K = 0 },
		"overscan":          func(req *HierarchicalRetrievalRequest) { req.CandidateLimit = req.Request.K },
		"negative overscan": func(req *HierarchicalRetrievalRequest) { req.CandidateLimit = -1 },
		"depth":             func(req *HierarchicalRetrievalRequest) { req.MaxDepth = 0 },
		"groups":            func(req *HierarchicalRetrievalRequest) { req.MaxGroups = 0 },
		"tokens":            func(req *HierarchicalRetrievalRequest) { req.MaxTokens = 0 },
		"timeout":           func(req *HierarchicalRetrievalRequest) { req.Timeout = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := valid
			mutate(&req)
			embedder := &recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1}}}}
			r, err := NewRetrieverWithEmbedder(embedder, &hierarchicalDenseStore{})
			if err != nil {
				t.Fatal(err)
			}
			if response, err := r.RetrieveHierarchical(context.Background(), req); err == nil || !reflect.ValueOf(response).IsZero() {
				t.Fatalf("response=%#v error=%v, want zero response and validation error", response, err)
			}
			if embedder.calls != 0 {
				t.Fatalf("embed calls=%d, want validation before work", embedder.calls)
			}
		})
	}
}

func TestRetrieveHierarchicalCodeGroupFirstChangesFinalists(t *testing.T) {
	results := []ScoredResult{
		hierarchicalResult("a1", "/work/pkg/a/one.go", "best", 1, map[string]string{"symbol_path": "One"}),
		hierarchicalResult("b1", "/work/pkg/b/one.go", "global second", .9, map[string]string{"symbol_path": "One"}),
		hierarchicalResult("b2", "/work/pkg/b/two.go", "global third", .8, nil),
		hierarchicalResult("a2", "/work/pkg/a/two.go", "selected group second", .2, nil),
	}
	r, store := newHierarchicalMultiRetriever(t, results)
	response, err := r.RetrieveHierarchical(context.Background(), hierarchicalRequest(2, 4, 2, 1, 100))
	if err != nil {
		t.Fatal(err)
	}
	if got := resultIDs(response.Results); !slices.Equal(got, []string{"a1", "a2"}) {
		t.Fatalf("result ids=%v, want true group-first finalists [a1 a2]", got)
	}
	if store.gotK != 4 {
		t.Fatalf("SearchMulti k=%d, want candidate limit 4", store.gotK)
	}
	if response.Trace.SearchMode != "multi" || response.Trace.Budget.CandidateLimit != 4 ||
		response.Trace.Budget.MaxResults != 2 || response.Trace.Budget.DepthReached != 2 ||
		response.Trace.Budget.InspectedCandidates != 4 || response.Trace.Budget.ReturnedResults != 2 {
		t.Fatalf("trace budget/mode=%#v", response.Trace)
	}
	if skipCount(response.Trace, "group_limit") != 2 {
		t.Fatalf("skips=%#v, want two chunks excluded by group limit", response.Trace.Skipped)
	}
	if len(response.Trace.SelectedPaths) != 1 || response.Trace.SelectedPaths[0][len(response.Trace.SelectedPaths[0])-1] != "directory:pkg/a" {
		t.Fatalf("selected paths=%v, want pkg/a drill-down", response.Trace.SelectedPaths)
	}
	if len(response.Trace.FinalChunks) != 2 || response.Trace.FinalChunks[0].Result.Chunk.ID != "a1" {
		t.Fatalf("final chunk trace=%#v", response.Trace.FinalChunks)
	}
}

func TestRetrieveHierarchicalDepthResultGroupAndTokenBounds(t *testing.T) {
	results := []ScoredResult{
		hierarchicalResult("empty", "/work/a/empty.go", "", 1, nil),
		hierarchicalResult("four", "/work/a/four.go", "1234", .9, nil),
		hierarchicalResult("five", "/work/a/five.go", "12345", .8, nil),
		hierarchicalResult("other", "/work/b/other.go", "x", .7, nil),
	}
	r, _ := newHierarchicalMultiRetriever(t, results)

	depthOne := hierarchicalRequest(3, 4, 1, 1, 10)
	response, err := r.RetrieveHierarchical(context.Background(), depthOne)
	if err != nil {
		t.Fatal(err)
	}
	if got := resultIDs(response.Results); !slices.Equal(got, []string{"empty", "four", "five"}) {
		t.Fatalf("depth-one result ids=%v, want global ranked prefix", got)
	}
	if response.Trace.Budget.DepthReached != 1 || skipCount(response.Trace, "result_limit") != 1 {
		t.Fatalf("depth/result bounds trace=%#v", response.Trace)
	}

	tokenBound := hierarchicalRequest(3, 4, 2, 2, 1)
	response, err = r.RetrieveHierarchical(context.Background(), tokenBound)
	if err != nil {
		t.Fatal(err)
	}
	if got := resultIDs(response.Results); !slices.Equal(got, []string{"empty", "four"}) {
		t.Fatalf("token-bound ids=%v, want empty+four-byte prefix", got)
	}
	if response.Trace.Budget.ReturnedTokens != 1 || skipCount(response.Trace, "token_limit") != 1 ||
		skipCount(response.Trace, "result_limit") != 1 {
		t.Fatalf("token/result skips=%#v budget=%#v", response.Trace.Skipped, response.Trace.Budget)
	}
}

func TestRetrieveHierarchicalDeterministicForPermutedTies(t *testing.T) {
	a := hierarchicalResult("a", "/work/a/a.go", "a", .5, nil)
	b := hierarchicalResult("b", "/work/b/b.go", "b", .5, nil)
	r, store := newHierarchicalMultiRetriever(t, []ScoredResult{b, a})
	req := hierarchicalRequest(1, 2, 2, 1, 10)
	first, err := r.RetrieveHierarchical(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	store.results = []ScoredResult{a, b}
	second, err := r.RetrieveHierarchical(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || !slices.Equal(resultIDs(first.Results), []string{"a"}) {
		t.Fatalf("permuted tie results differ:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestRetrieveHierarchicalDeterministicForNonFiniteRankScores(t *testing.T) {
	nonFinite := hierarchicalResult("nan", "/work/a.go", "nan", math.NaN(), nil)
	finite := hierarchicalResult("finite", "/work/b.go", "finite", .5, nil)
	r, store := newHierarchicalMultiRetriever(t, []ScoredResult{nonFinite, finite})
	req := hierarchicalRequest(1, 2, 1, 1, 10)
	first, err := r.RetrieveHierarchical(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	store.results = []ScoredResult{finite, nonFinite}
	second, err := r.RetrieveHierarchical(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || !slices.Equal(resultIDs(first.Results), []string{"finite"}) {
		t.Fatalf("non-finite score ordering differs:\nfirst=%#v\nsecond=%#v", first, second)
	}
}

func TestRetrieveHierarchicalDenseFallback(t *testing.T) {
	store := &hierarchicalDenseStore{results: []SearchResult{
		{Chunk: Chunk{ID: "b", Source: "/work/b.go", Content: "b", Metadata: map[string]string{}}, Score: .8, Distance: .2},
		{Chunk: Chunk{ID: "a", Source: "/work/a.go", Content: "a", Metadata: map[string]string{}}, Score: .9, Distance: .1},
	}}
	r, err := NewRetrieverWithEmbedder(&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1}}}}, store)
	if err != nil {
		t.Fatal(err)
	}
	response, err := r.RetrieveHierarchical(context.Background(), hierarchicalRequest(1, 2, 3, 2, 10))
	if err != nil {
		t.Fatal(err)
	}
	if response.Trace.SearchMode != "dense" || store.gotK != 2 || !slices.Equal(resultIDs(response.Results), []string{"a"}) {
		t.Fatalf("response=%#v search k=%d", response, store.gotK)
	}
	if got := response.Results[0].Signals["semantic"]; got != .9 {
		t.Fatalf("dense semantic signal=%v, want .9", got)
	}
}

func TestRetrieveHierarchicalCandidateLimitBoundsOverReturningStore(t *testing.T) {
	results := []ScoredResult{
		hierarchicalResult("one", "/work/one.go", "one", 1, nil),
		hierarchicalResult("two", "/work/two.go", "two", .9, nil),
		hierarchicalResult("three", "/work/three.go", "three", .8, nil),
	}
	// retrieverMultiStore deliberately returns its whole slice regardless of k.
	r, _, store := newPolicyRetriever(t, results)
	response, err := r.RetrieveHierarchical(context.Background(), hierarchicalRequest(1, 2, 1, 1, 100))
	if err != nil {
		t.Fatal(err)
	}
	if store.gotK != 2 || response.Policy.CandidateCount != 2 || response.Trace.Budget.InspectedCandidates != 2 {
		t.Fatalf("search K=%d policy candidates=%d inspected=%d, want bounded at 2",
			store.gotK, response.Policy.CandidateCount, response.Trace.Budget.InspectedCandidates)
	}
}

func TestRetrieveHierarchicalCancellationAndTimeoutIdentity(t *testing.T) {
	blocking := EmbedderFunc(func(ctx context.Context, _ string, _ []string) (EmbedResult, error) {
		<-ctx.Done()
		return EmbedResult{}, ctx.Err()
	})
	r, err := NewRetrieverWithEmbedder(blocking, &hierarchicalDenseStore{})
	if err != nil {
		t.Fatal(err)
	}

	parent, cancel := context.WithCancelCause(context.Background())
	parentErr := errors.New("caller stopped")
	cancel(parentErr)
	response, err := r.RetrieveHierarchical(parent, hierarchicalRequest(1, 2, 1, 1, 1))
	if !errors.Is(err, context.Canceled) || !errors.Is(err, parentErr) || !reflect.ValueOf(response).IsZero() {
		t.Fatalf("parent cancellation response=%#v error=%v", response, err)
	}

	req := hierarchicalRequest(1, 2, 1, 1, 1)
	req.Timeout = time.Millisecond
	response, err = r.RetrieveHierarchical(context.Background(), req)
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.ValueOf(response).IsZero() {
		t.Fatalf("configured timeout response=%#v error=%v", response, err)
	}
}

func TestRetrieveHierarchicalCancellationJoinsContextAndCustomCause(t *testing.T) {
	downstreamErr := errors.New("embedder cleanup failed")
	r, err := NewRetrieverWithEmbedder(EmbedderFunc(func(context.Context, string, []string) (EmbedResult, error) {
		return EmbedResult{}, downstreamErr
	}), &hierarchicalDenseStore{})
	if err != nil {
		t.Fatal(err)
	}

	parent, cancel := context.WithCancelCause(context.Background())
	parentErr := errors.New("caller stopped")
	cancel(parentErr)
	response, err := r.RetrieveHierarchical(parent, hierarchicalRequest(1, 2, 1, 1, 1))
	if !errors.Is(err, downstreamErr) || !errors.Is(err, context.Canceled) || !errors.Is(err, parentErr) || !reflect.ValueOf(response).IsZero() {
		t.Fatalf("response=%#v error=%v, want downstream, canceled, and custom-cause identities", response, err)
	}
}

func TestRetrieveHierarchicalResponseAndTraceOwnResults(t *testing.T) {
	stored := hierarchicalResult("a", "/work/a.go", "original", .9, map[string]string{"symbol_path": "A"})
	r, store := newHierarchicalMultiRetriever(t, []ScoredResult{stored, hierarchicalResult("b", "/work/b.go", "b", .8, nil)})
	response, err := r.RetrieveHierarchical(context.Background(), hierarchicalRequest(1, 2, 4, 2, 100))
	if err != nil {
		t.Fatal(err)
	}
	response.Results[0].Chunk.Content = "response mutation"
	response.Results[0].Chunk.Metadata["symbol_path"] = "response mutation"
	response.Results[0].Signals["semantic"] = 0
	if got := response.Trace.FinalChunks[0].Result; got.Chunk.Content != "original" || got.Chunk.Metadata["symbol_path"] != "A" || got.Signals["semantic"] != .9 {
		t.Fatalf("trace aliases response: %#v", got)
	}
	response.Trace.FinalChunks[0].Result.Chunk.Content = "trace mutation"
	response.Trace.FinalChunks[0].Result.Signals["semantic"] = -1
	if store.results[0].Chunk.Content != "original" || store.results[0].Signals["semantic"] != .9 {
		t.Fatalf("store aliases returned data: %#v", store.results[0])
	}
	if len(response.Trace.Groups) != 0 && len(response.Trace.SelectedPaths) != 0 {
		before := fmt.Sprint(response.Trace.SelectedPaths)
		response.Trace.Groups[0].Path[0] = "mutated"
		if fmt.Sprint(response.Trace.SelectedPaths) != before {
			t.Fatal("group and selected paths alias")
		}
	}
	if strings.Contains(fmt.Sprint(store.results), "mutation") {
		t.Fatalf("store mutated: %#v", store.results)
	}
}

func TestRetrieveHierarchicalPolicyRunsBeforeSelectionAndObserverRunsLast(t *testing.T) {
	results := []ScoredResult{
		hierarchicalResult("secret", "/work/private/secret.go", "secret identity", 1, nil),
		hierarchicalResult("a1", "/work/a/one.go", "one", .9, nil),
		hierarchicalResult("a2", "/work/a/two.go", "sensitive", .8, nil),
		hierarchicalResult("b1", "/work/b/one.go", "lower", .7, nil),
	}
	var evaluated, evaluatedResults RetrievalRequest
	var evaluatedChunks []Chunk
	evaluator := allowPolicySpy()
	evaluator.evaluate = func(_ context.Context, req RetrievalRequest) (RetrievalPolicyDecision, error) {
		evaluated = cloneRetrievalRequest(req)
		return RetrievalPolicyDecision{Allow: true}, nil
	}
	evaluator.evaluateResults = func(_ context.Context, req RetrievalRequest, chunks []Chunk) ([]RetrievalResultDecision, error) {
		evaluatedResults = cloneRetrievalRequest(req)
		evaluatedChunks = slices.Clone(chunks)
		redacted := "safe"
		return []RetrievalResultDecision{
			{Keep: false},
			{Keep: true},
			{Keep: true, RedactedContent: &redacted},
			{Keep: true},
		}, nil
	}
	observer := &policyObserverSpy{}
	r, store := newHierarchicalMultiRetriever(t, results,
		WithRetrievalPolicyEvaluator(evaluator), WithRetrievalPolicyObserver(observer))
	req := hierarchicalRequest(3, 4, 1, 1, 100)
	req.Request.Policy = RetrievalPolicyRequest{PrincipalID: "p", MaxResults: 2, MaxCost: 7}
	response, err := r.RetrieveHierarchical(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if evaluated.K != 3 || evaluated.Policy.MaxCost != 7 || evaluatedResults.K != 2 || evaluatedResults.Policy.MaxCost != 7 || len(evaluatedChunks) != 4 || store.gotK != 4 {
		t.Fatalf("policy/search inputs: Evaluate K=%d EvaluateResults K=%d candidates=%d search K=%d",
			evaluated.K, evaluatedResults.K, len(evaluatedChunks), store.gotK)
	}
	if got := resultIDs(response.Results); !slices.Equal(got, []string{"a1", "a2"}) || response.Results[1].Chunk.Content != "safe" {
		t.Fatalf("filtered/redacted results=%#v", response.Results)
	}
	if strings.Contains(fmt.Sprintf("%#v", response.Trace), "secret") {
		t.Fatalf("policy-filtered identity leaked into trace: %#v", response.Trace)
	}
	if skipCount(response.Trace, "policy_filtered") != 1 || response.Policy.CandidateCount != 4 ||
		response.Policy.FilteredCount != 1 || response.Policy.RedactedCount != 1 ||
		response.Policy.ReturnedCount != 2 || response.Trace.Budget.MaxResults != 2 {
		t.Fatalf("policy outcome/trace=%#v / %#v", response.Policy, response.Trace)
	}
	if len(observer.events) != 1 || !reflect.DeepEqual(observer.events[0].Outcome, response.Policy) ||
		!reflect.DeepEqual(response.Trace.Policy, response.Policy) {
		t.Fatalf("observer/trace policy mismatch: events=%#v response=%#v trace=%#v", observer.events, response.Policy, response.Trace.Policy)
	}
}

func TestRetrieveHierarchicalPolicyDenialReturnsSafeOutcomeTrace(t *testing.T) {
	evaluator := allowPolicySpy()
	evaluator.evaluate = func(context.Context, RetrievalRequest) (RetrievalPolicyDecision, error) {
		return RetrievalPolicyDecision{Allow: false}, nil
	}
	r, _ := newHierarchicalMultiRetriever(t,
		[]ScoredResult{hierarchicalResult("secret", "/work/secret.go", "secret", .9, nil)},
		WithRetrievalPolicyEvaluator(evaluator),
	)

	response, err := r.RetrieveHierarchical(context.Background(), hierarchicalRequest(1, 2, 1, 1, 10))
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("error=%v, want policy denied", err)
	}
	if response.Results != nil || len(response.Trace.Groups) != 0 || len(response.Trace.SelectedPaths) != 0 || len(response.Trace.FinalChunks) != 0 {
		t.Fatalf("denied response leaked result identities: %#v", response)
	}
	if response.Policy.Disposition != RetrievalPolicyDenied || response.Policy.ReasonCode != "denied" ||
		!reflect.DeepEqual(response.Trace.Policy, response.Policy) {
		t.Fatalf("policy outcome response=%#v trace=%#v", response.Policy, response.Trace.Policy)
	}
}

func TestRetrieveHierarchicalErrorClearsPartialTrace(t *testing.T) {
	observerErr := errors.New("audit unavailable")
	r, _ := newHierarchicalMultiRetriever(t,
		[]ScoredResult{hierarchicalResult("secret", "/work/secret.go", "secret", .9, nil), hierarchicalResult("other", "/work/other.go", "other", .8, nil)},
		WithRetrievalPolicyObserver(&policyObserverSpy{err: observerErr}),
	)
	response, err := r.RetrieveHierarchical(context.Background(), hierarchicalRequest(1, 2, 1, 1, 10))
	if !errors.Is(err, observerErr) {
		t.Fatalf("error=%v, want observer error", err)
	}
	if response.Results != nil || len(response.Trace.Groups) != 0 || len(response.Trace.SelectedPaths) != 0 || len(response.Trace.FinalChunks) != 0 {
		t.Fatalf("error response leaked result identities: %#v", response)
	}
	if response.Policy.Disposition != RetrievalPolicyFailed || response.Policy.ReasonCode != "observer_failed" ||
		!reflect.DeepEqual(response.Trace.Policy, response.Policy) {
		t.Fatalf("policy outcome response=%#v trace=%#v", response.Policy, response.Trace.Policy)
	}
}

func TestRetrieveHierarchicalCancellationDuringObserverReturnsFinalizedResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var observed RetrievalPolicyOutcome
	r, _ := newHierarchicalMultiRetriever(t,
		[]ScoredResult{hierarchicalResult("one", "/work/one.go", "one", .9, nil), hierarchicalResult("two", "/work/two.go", "two", .8, nil)},
		WithRetrievalPolicyObserver(policyObserverFunc(func(_ context.Context, event RetrievalPolicyEvent) error {
			observed = event.Outcome
			cancel()
			return nil
		})),
	)
	response, err := r.RetrieveHierarchical(ctx, hierarchicalRequest(1, 2, 1, 1, 10))
	if err != nil || !slices.Equal(resultIDs(response.Results), []string{"one"}) {
		t.Fatalf("response=%#v error=%v, want finalized success", response, err)
	}
	if !reflect.DeepEqual(observed, response.Policy) || !reflect.DeepEqual(response.Trace.Policy, response.Policy) {
		t.Fatalf("observer=%#v response=%#v trace=%#v", observed, response.Policy, response.Trace.Policy)
	}
}

func TestRetrieveHierarchicalTimeoutDuringObserver(t *testing.T) {
	r, _ := newHierarchicalMultiRetriever(t,
		[]ScoredResult{hierarchicalResult("one", "/work/one.go", "one", .9, nil), hierarchicalResult("two", "/work/two.go", "two", .8, nil)},
		WithRetrievalPolicyObserver(policyObserverFunc(func(ctx context.Context, _ RetrievalPolicyEvent) error {
			<-ctx.Done()
			return ctx.Err()
		})),
	)
	req := hierarchicalRequest(1, 2, 1, 1, 10)
	req.Timeout = time.Millisecond
	response, err := r.RetrieveHierarchical(context.Background(), req)
	if !errors.Is(err, context.DeadlineExceeded) || !reflect.ValueOf(response).IsZero() {
		t.Fatalf("response=%#v error=%v, want deadline and zero response", response, err)
	}
}

func TestRetrieveRequestFlatKeepsSharedSearchAndFinalLimit(t *testing.T) {
	results := []ScoredResult{
		hierarchicalResult("one", "one.go", "one", .9, nil),
		hierarchicalResult("two", "two.go", "two", .8, nil),
		hierarchicalResult("three", "three.go", "three", .7, nil),
	}
	seenCandidates := 0
	evaluator := allowPolicySpy()
	evaluator.evaluateResults = func(_ context.Context, req RetrievalRequest, chunks []Chunk) ([]RetrievalResultDecision, error) {
		if req.K != 2 {
			t.Fatalf("flat evaluator K=%d, want 2", req.K)
		}
		seenCandidates = len(chunks)
		return nil, nil
	}
	r, store := newHierarchicalMultiRetriever(t, results, WithRetrievalPolicyEvaluator(evaluator))
	response, err := r.RetrieveRequest(context.Background(), RetrievalRequest{Query: "q", K: 2})
	if err != nil {
		t.Fatal(err)
	}
	if store.gotK != 2 || seenCandidates != 2 || !slices.Equal(resultIDs(response.Results), []string{"one", "two"}) {
		t.Fatalf("flat search K=%d candidates=%d results=%v", store.gotK, seenCandidates, resultIDs(response.Results))
	}
}

func TestRetrieveHierarchicalCodeUsesOnlySymbolAsFourthLevel(t *testing.T) {
	results := []ScoredResult{
		hierarchicalResult("symbol", "/work/pkg/code.go", "symbol", .9, map[string]string{"symbol_path": "Type.Method", "section_path": "Ignored"}),
		hierarchicalResult("section", "/work/pkg/text.go", "section", .8, map[string]string{"section_path": "Not code hierarchy"}),
	}
	r, _ := newHierarchicalMultiRetriever(t, results)
	response, err := r.RetrieveHierarchical(context.Background(), hierarchicalRequest(2, 3, 4, 3, 100))
	if err != nil {
		t.Fatal(err)
	}
	symbols, sections := 0, 0
	for _, group := range response.Trace.Groups {
		switch group.Kind {
		case "symbol":
			symbols++
		case "section":
			sections++
		}
	}
	if symbols != 1 || sections != 0 || response.Trace.Budget.DepthReached != 4 {
		t.Fatalf("groups=%#v depth=%d", response.Trace.Groups, response.Trace.Budget.DepthReached)
	}
}

func TestRetrieveHierarchicalTrustedManagedGroupsAndFreshness(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	opsA, err := managed.IngestText(ctx, "ops-a.md", "ops a", DocumentOptions{Title: "Ops A", Collection: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := managed.IngestText(ctx, "other.md", "other", DocumentOptions{Title: "Other", Collection: "other"})
	if err != nil {
		t.Fatal(err)
	}
	opsC, err := managed.IngestText(ctx, "ops-c.md", "ops c", DocumentOptions{Title: "Ops C", Collection: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	opsAChunk := replaceManagedScopeChunk(t, store, opsA, "ops-a", []float64{1, 0})
	replaceManagedScopeChunk(t, store, other, "other", []float64{.9, .1})
	replaceManagedScopeChunk(t, store, opsC, "ops-c", []float64{.2, .8})
	opsAChunk = cloneChunk(opsAChunk)
	opsAChunk.Metadata["section_path"] = "Runbook > Restart"
	opsAChunk.Metadata["symbol_path"] = "MustNotBeUsed"
	if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, opsA.source, []Chunk{opsAChunk}, [][]float64{{1, 0}}, opsA.SourceSignature, opsA.VectorSpaceID); err != nil {
		t.Fatal(err)
	}

	r, err := NewRetrieverWithEmbedder(
		&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}},
		store, WithVectorOnly(),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := r.RetrieveHierarchical(ctx, hierarchicalRequest(2, 3, 1, 1, 100))
	if err != nil {
		t.Fatal(err)
	}
	if got := resultIDs(response.Results); !slices.Equal(got, []string{"ops-a", "ops-c"}) {
		t.Fatalf("managed group-first ids=%v, want ops collection finalists", got)
	}
	if len(response.Trace.Groups) < 2 || response.Trace.Groups[0].Kind != "collection" || response.Trace.Groups[0].Name != "ops" {
		t.Fatalf("managed collection groups=%#v", response.Trace.Groups)
	}
	for _, final := range response.Trace.FinalChunks {
		if !final.FreshnessKnown || final.Freshness != DocumentFreshnessFresh {
			t.Fatalf("trusted managed freshness=%#v", final)
		}
	}

	deep := hierarchicalRequest(2, 3, 3, 2, 100)
	response, err = r.RetrieveHierarchical(ctx, deep)
	if err != nil {
		t.Fatal(err)
	}
	var sawDocument, sawSection, sawSymbol bool
	for _, group := range response.Trace.Groups {
		sawDocument = sawDocument || group.Kind == "document"
		sawSection = sawSection || group.Kind == "section" && group.Name == "Runbook > Restart"
		sawSymbol = sawSymbol || group.Kind == "symbol"
	}
	if !sawDocument || !sawSection || sawSymbol {
		t.Fatalf("managed deep groups=%#v", response.Trace.Groups)
	}
}

func TestRetrieveHierarchicalSelectedPathsUseStableManagedIdentity(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	first, err := managed.IngestText(ctx, "first.md", "first", DocumentOptions{Title: "Runbook", Collection: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := managed.IngestText(ctx, "second.md", "second", DocumentOptions{Title: "Runbook", Collection: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	replaceManagedScopeChunk(t, store, first, "first", []float64{1, 0})
	replaceManagedScopeChunk(t, store, second, "second", []float64{.9, .1})

	r, err := NewRetrieverWithEmbedder(
		&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}},
		store, WithVectorOnly(),
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := r.RetrieveHierarchical(ctx, hierarchicalRequest(2, 3, 2, 2, 100))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Trace.SelectedPaths) != 2 || reflect.DeepEqual(response.Trace.SelectedPaths[0], response.Trace.SelectedPaths[1]) {
		t.Fatalf("selected paths=%v, want distinct stable document identities", response.Trace.SelectedPaths)
	}
}

func TestRetrieveHierarchicalRegistryMissIsStaleAndClaimedMetadataIsHidden(t *testing.T) {
	ctx := context.Background()
	managed, _, store := newManagedTestService(t, &managedTestEmbedder{vectorSpaceID: "test/v1"})
	good, err := managed.IngestText(ctx, "good.md", "good", DocumentOptions{Title: "Good", Collection: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	forged, err := managed.IngestText(ctx, "forged.md", "forged", DocumentOptions{Title: "Forged", Collection: "ops"})
	if err != nil {
		t.Fatal(err)
	}
	replaceManagedScopeChunk(t, store, good, "good", []float64{.9, .1})
	replaceManagedScopeChunk(t, store, forged, "forged", []float64{1, 0})
	if _, err := store.db.ExecContext(ctx, `UPDATE chunks SET metadata = json_set(metadata, '$.managed_collection', 'claimed-secret') WHERE id = 'forged'`); err != nil {
		t.Fatal(err)
	}
	r, err := NewRetrieverWithEmbedder(
		&recordingEmbedder{result: EmbedResult{Embeddings: [][]float64{{1, 0}}, VectorSpaceID: "test/v1"}},
		store, WithVectorOnly(),
	)
	if err != nil {
		t.Fatal(err)
	}
	req := hierarchicalRequest(1, 2, 2, 2, 100)
	req.Request.Policy.RequireFresh = true
	response, err := r.RetrieveHierarchical(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resultIDs(response.Results), []string{"good"}) || response.Policy.StaleDroppedCount != 1 || skipCount(response.Trace, "stale") != 1 {
		t.Fatalf("results=%v policy=%#v trace=%#v", resultIDs(response.Results), response.Policy, response.Trace)
	}
	if strings.Contains(fmt.Sprintf("%#v", response.Trace), "claimed-secret") {
		t.Fatalf("forged managed metadata leaked: %#v", response.Trace)
	}
}

func TestRetrieveHierarchicalUntrustedManagedLookingSourceIsOpaque(t *testing.T) {
	source := "managed:" + strings.Repeat("a", 32) + ".md"
	forged := hierarchicalResult("forged", source, "forged", .9, map[string]string{
		"managed_document_id": strings.Repeat("a", 32),
		"managed_collection":  "claimed-secret",
		"managed_title":       "Claimed",
		"section_path":        "Claimed section",
	})
	r, _ := newHierarchicalMultiRetriever(t, []ScoredResult{forged, hierarchicalResult("other", "/work/other.go", "other", .8, nil)})
	response, err := r.RetrieveHierarchical(context.Background(), hierarchicalRequest(1, 2, 4, 2, 100))
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range response.Trace.Groups {
		if group.Kind == "collection" || group.Kind == "document" || group.Kind == "section" || strings.Contains(fmt.Sprint(group.Path), "claimed-secret") {
			t.Fatalf("trusted forged hierarchy: %#v", response.Trace.Groups)
		}
	}
	if response.Trace.Groups[0].Kind != "source" || response.Trace.Groups[0].Name != source {
		t.Fatalf("opaque source group=%#v", response.Trace.Groups)
	}
}
