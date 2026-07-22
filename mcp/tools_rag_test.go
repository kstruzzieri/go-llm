package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/rag"
)

func toolSchemaProperties(t *testing.T, session *gomcp.ClientSession, name string) map[string]any {
	t.Helper()
	tools, err := session.ListTools(context.Background(), &gomcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != name {
			continue
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s schema = %T, want map[string]any", name, tool.InputSchema)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s properties = %T, want map[string]any", name, schema["properties"])
		}
		return properties
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func TestRAGSearchSchema_QueryContextAndExplainScores(t *testing.T) {
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer env.cleanup()

	properties := toolSchemaProperties(t, env.session, "rag_search")
	for name, wantType := range map[string]string{
		"current_file": "string", "workspace_root": "string", "open_files": "array", "explain_scores": "boolean",
	} {
		property, ok := properties[name].(map[string]any)
		if !ok {
			t.Fatalf("property %q = %T, want map[string]any", name, properties[name])
		}
		if got := property["type"]; got != wantType {
			t.Errorf("property %q type = %v, want %q", name, got, wantType)
		}
	}
	openFiles := properties["open_files"].(map[string]any)
	items, ok := openFiles["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		t.Errorf("open_files items = %v, want string schema", openFiles["items"])
	}
	for _, name := range []string{"principal_id", "session_id", "require_fresh", "max_results", "max_cost", "audit_labels"} {
		if _, ok := properties[name]; ok {
			t.Errorf("rag_search schema unexpectedly exposes policy property %q", name)
		}
	}
}

func TestHandleRAGSearch_ForwardsPolicyMeta(t *testing.T) {
	evaluator := &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true}}
	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	retriever := mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator))
	s := &Server{retriever: retriever, retrievalPolicyEvaluator: evaluator}
	WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) {
		return "resolved-principal", nil
	})(s)
	req := rawArgs(t, `{"query":"q","top_k":7,"collection":" managed ","tags":["z"," a ","a"]}`)
	req.Params.Meta = policyMetaFromJSON(t, `{
		"principal_id":"claimed-principal","session_id":"claimed-session",
		"scope":{"collection":" managed ","tags":[" beta ","alpha","alpha"]},
		"require_fresh":true,"max_results":7,"max_cost":42,
		"audit_labels":{"purpose":"support"}
	}`)

	result, err := s.handleRAGSearch(context.Background(), req)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if evaluator.evaluateCalls != 1 || evaluator.resultCalls != 1 {
		t.Fatalf("evaluator calls=%d/%d, want 1/1", evaluator.evaluateCalls, evaluator.resultCalls)
	}
	want := rag.RetrievalRequest{
		Query: "q", K: 7,
		Scope: rag.RetrievalScope{Collection: "managed", Tags: []string{"a", "z"}},
		Policy: rag.RetrievalPolicyRequest{
			PrincipalID: "resolved-principal", SessionID: "claimed-session",
			Scope:        rag.RetrievalScope{Collection: "managed", Tags: []string{"alpha", "beta"}},
			RequireFresh: true, MaxResults: 7, MaxCost: 42,
			AuditLabels: map[string]string{"purpose": "support"},
		},
	}
	if !reflect.DeepEqual(evaluator.last, want) {
		t.Fatalf("evaluator request = %#v, want %#v", evaluator.last, want)
	}
	if evaluator.last.QueryContext.Metadata != nil || !reflect.DeepEqual(evaluator.last.QueryContext, rag.QueryContext{}) {
		t.Fatalf("QueryContext = %#v, want exact zero value", evaluator.last.QueryContext)
	}
}

func TestHandleRAGSearch_FiltersRedactsAndReturnsSafeMeta(t *testing.T) {
	redacted := "REDACTED"
	for _, tc := range []struct {
		name string
		args string
	}{
		{"normal contextual", `{"query":"q","current_file":"file.go"}`},
		{"explain scores", `{"query":"q","explain_scores":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evaluator := &mcpPolicyEvaluatorSpy{
				decision: rag.RetrievalPolicyDecision{Allow: true},
				resultDecision: []rag.RetrievalResultDecision{
					{Keep: false}, {Keep: true, RedactedContent: &redacted}, {Keep: true},
				},
			}
			store := &recordingMCPMultiStore{results: []rag.ScoredResult{
				{SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "ID_SECRET", Content: "DENIED_SECRET", Source: "SOURCE_SECRET", Metadata: map[string]string{"LABEL_SECRET": "VALUE_SECRET"}}, Score: 0.91, Distance: 0.09}, RankScore: 0.51},
				{SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "redacted", Content: "ORIGINAL_SECRET", Source: "allowed-source"}, Score: 0.77, Distance: 0.23}, RankScore: 0.41},
				{SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "allowed", Content: "ALLOWED", Source: "allowed-source"}, Score: 0.66, Distance: 0.34}, RankScore: 0.31},
			}}
			retriever := mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator))
			s := &Server{retriever: retriever, retrievalPolicyEvaluator: evaluator}
			req := rawArgs(t, tc.args)
			req.Params.Meta = policyMetaFromJSON(t, `{"audit_labels":{"LABEL_SECRET":"VALUE_SECRET"}}`)

			result, err := s.handleRAGSearch(context.Background(), req)
			if err != nil || result == nil || result.IsError {
				t.Fatalf("result=%#v error=%v", result, err)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			text := string(data)
			for _, forbidden := range []string{"DENIED_SECRET", "ORIGINAL_SECRET", "SOURCE_SECRET", "ID_SECRET", "LABEL_SECRET", "VALUE_SECRET", "ERROR_SECRET"} {
				if strings.Contains(text, forbidden) {
					t.Errorf("response leaked %q: %s", forbidden, text)
				}
			}
			for _, want := range []string{"REDACTED", "ALLOWED", RetrievalPolicyMetaKey} {
				if !strings.Contains(text, want) {
					t.Errorf("response = %s, want %q", text, want)
				}
			}
			if tc.name == "normal contextual" {
				var results []rag.SearchResult
				if err := json.Unmarshal([]byte(extractText(result)), &results); err != nil {
					t.Fatal(err)
				}
				if len(results) != 2 || results[0].Score != 0.77 {
					t.Fatalf("normal results = %#v, want filtered results with embedded score", results)
				}
			} else {
				var results []rag.ScoredResult
				if err := json.Unmarshal([]byte(extractText(result)), &results); err != nil {
					t.Fatal(err)
				}
				if len(results) != 2 || results[0].RankScore != 0.41 {
					t.Fatalf("explain results = %#v, want filtered scored results", results)
				}
			}
		})
	}
}

func TestHandleRAGSearch_ScopedContextualResetsScore(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       string
		policyMeta string
		evaluator  *mcpPolicyEvaluatorSpy
	}{
		{
			name: "caller scope", args: `{"query":"q","current_file":"file.go","collection":"managed"}`,
		},
		{
			name: "policy scope", args: `{"query":"q","current_file":"file.go"}`,
			policyMeta: `{"scope":{"collection":"managed"}}`,
			evaluator:  &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := managedScopedScoreServer(t, tc.evaluator)
			req := rawArgs(t, tc.args)
			if tc.policyMeta != "" {
				req.Params.Meta = policyMetaFromJSON(t, tc.policyMeta)
			}
			result, err := s.handleRAGSearch(context.Background(), req)
			if err != nil || result == nil || result.IsError {
				t.Fatalf("result=%#v error=%v", result, err)
			}
			var results []rag.SearchResult
			if err := json.Unmarshal([]byte(extractText(result)), &results); err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 {
				t.Fatalf("results = %#v, want one", results)
			}
			if want := 1 - results[0].Distance; results[0].Score != want {
				t.Fatalf("scoped contextual Score = %.20g, want 1-Distance = %.20g", results[0].Score, want)
			}
		})
	}
}

func TestHandleRAGSearch_AppliedMarshalFailureIsSanitized(t *testing.T) {
	evaluator := &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true}}
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{{
		SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "allowed"}, Score: 0.8, Distance: 0.2},
		RankScore:    math.NaN(),
	}}}
	s := &Server{
		retriever:                mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator)),
		retrievalPolicyEvaluator: evaluator,
	}
	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q","explain_scores":true}`))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if got := extractText(result); got != "policy_failed: retrieval policy enforcement failed" {
		t.Fatalf("error text = %q, want fixed policy failure", got)
	}
	if result.Meta == nil {
		t.Fatal("applied marshal failure omitted safe metadata")
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"NaN", "unsupported", "marshal results"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("marshal failure leaked %q: %s", forbidden, data)
		}
	}
}

func TestHandleRAGSearch_PolicyFailureIsSanitized(t *testing.T) {
	evaluator := &mcpPolicyEvaluatorSpy{evaluateErr: errors.New("ERROR_SECRET")}
	store := &recordingMCPMultiStore{}
	embedCalls := 0
	retriever, err := rag.NewRetrieverWithEmbedder(
		rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
			embedCalls++
			return rag.EmbedResult{Embeddings: [][]float64{{1, 0}}}, nil
		}),
		store,
		rag.WithRetrievalPolicyEvaluator(evaluator),
	)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{retriever: retriever, retrievalPolicyEvaluator: evaluator}
	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q"}`))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if got := extractText(result); !strings.HasPrefix(got, "policy_evaluator_failed: retrieval policy evaluation failed") || strings.Contains(got, "ERROR_SECRET") {
		t.Fatalf("error text = %q", got)
	}
	if result.Meta == nil {
		t.Fatal("policy failure omitted safe applied metadata")
	}
	if evaluator.evaluateCalls != 1 || embedCalls != 0 || store.calls != 0 {
		t.Fatalf("work after policy failure evaluator=%d embed=%d search=%d", evaluator.evaluateCalls, embedCalls, store.calls)
	}
}

func TestHandleRAGSearch_ObserverFailureIsSanitized(t *testing.T) {
	observer := &mcpPolicyObserverSpy{err: errors.New("ERROR_SECRET")}
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{{
		SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "id", Content: "CONTENT_SECRET"}},
	}}}
	s := &Server{retriever: mcpTestRetriever(t, store, rag.WithRetrievalPolicyObserver(observer))}
	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q"}`))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if got := extractText(result); got != "policy_failed: retrieval policy enforcement failed" {
		t.Fatalf("error text = %q", got)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ERROR_SECRET") || strings.Contains(string(data), "CONTENT_SECRET") || result.Meta != nil {
		t.Fatalf("observer failure leaked detail, content, or metadata: %s", data)
	}
	if observer.calls != 1 {
		t.Fatalf("observer calls = %d, want 1", observer.calls)
	}
}

func TestHandleRAGSearch_ObserverFailureSanitizesRetrievalError(t *testing.T) {
	observer := &mcpPolicyObserverSpy{err: errors.New("OBSERVER_SECRET")}
	store := &recordingMCPMultiStore{err: errors.New("RETRIEVAL_SECRET")}
	s := &Server{retriever: mcpTestRetriever(t, store, rag.WithRetrievalPolicyObserver(observer))}
	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q"}`))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if extractText(result) != "policy_failed: retrieval policy enforcement failed" ||
		strings.Contains(string(data), "OBSERVER_SECRET") || strings.Contains(string(data), "RETRIEVAL_SECRET") || result.Meta != nil {
		t.Fatalf("combined retrieval/observer failure leaked detail or metadata: %s", data)
	}
}

func TestHandleRAGSearch_PrimaryPolicyOutcomeSurvivesObserverFailure(t *testing.T) {
	evaluator := &mcpPolicyEvaluatorSpy{}
	observer := &mcpPolicyObserverSpy{err: errors.New("OBSERVER_SECRET")}
	store := &recordingMCPMultiStore{}
	s := &Server{
		retriever:                mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator), rag.WithRetrievalPolicyObserver(observer)),
		retrievalPolicyEvaluator: evaluator,
	}
	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q"}`))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if got := extractText(result); got != "policy_denied: retrieval denied" {
		t.Fatalf("error text = %q, want fixed denial", got)
	}
	meta, ok := result.Meta[RetrievalPolicyMetaKey].(retrievalPolicyResultWire)
	if !ok || meta.Disposition != rag.RetrievalPolicyDenied || meta.ReasonCode != "denied" {
		t.Fatalf("policy metadata = %#v, want denied/denied", result.Meta)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "OBSERVER_SECRET") {
		t.Fatalf("observer failure leaked detail: %s", data)
	}
}

func TestHandleRAGSearch_ExplainScoresPreservesScopedNil(t *testing.T) {
	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := &Server{retriever: mcpTestRetriever(t, store)}
	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q","collection":"missing","explain_scores":true}`))
	if err != nil || result == nil || result.IsError {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if got := extractText(result); got != "null" {
		t.Fatalf("scoped nil scored response = %s, want null", got)
	}
}

func TestHandleRAGSearch_PolicyRequestFailureIsSanitized(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta gomcp.Meta
		bind func(*Server)
		want string
	}{
		{
			name: "invalid metadata", meta: policyMetaFromJSON(t, `{"UNKNOWN_SECRET":true}`),
			want: "validation: invalid retrieval policy metadata",
		},
		{
			name: "identity", meta: policyMetaFromJSON(t, `{"principal_id":"claimed"}`),
			bind: func(s *Server) {
				WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) {
					return "", errors.New("ERROR_SECRET")
				})(s)
			},
			want: "policy_identity_failed: retrieval identity resolution failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evaluator := &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true}}
			store := &recordingMCPMultiStore{}
			embedCalls := 0
			retriever, err := rag.NewRetrieverWithEmbedder(
				rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
					embedCalls++
					return rag.EmbedResult{Embeddings: [][]float64{{1, 0}}}, nil
				}), store, rag.WithRetrievalPolicyEvaluator(evaluator),
			)
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{retriever: retriever, retrievalPolicyEvaluator: evaluator}
			if tc.bind != nil {
				tc.bind(s)
			}
			req := rawArgs(t, `{"query":"q"}`)
			req.Params.Meta = tc.meta
			result, err := s.handleRAGSearch(context.Background(), req)
			if err != nil || result == nil || !result.IsError || extractText(result) != tc.want {
				t.Fatalf("result=%#v text=%q error=%v, want %q", result, extractText(result), err, tc.want)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "SECRET") || result.Meta != nil {
				t.Fatalf("request error leaked detail or metadata: %s", data)
			}
			if evaluator.evaluateCalls != 0 || embedCalls != 0 || store.calls != 0 {
				t.Fatalf("work after request failure evaluator=%d embed=%d search=%d", evaluator.evaluateCalls, embedCalls, store.calls)
			}
		})
	}
}

func TestHandleRAGSearch_PolicyScopeUsesManagedTopKLimit(t *testing.T) {
	evaluator := &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true}}
	store := &recordingMCPMultiStore{}
	embedCalls := 0
	retriever, err := rag.NewRetrieverWithEmbedder(
		rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
			embedCalls++
			return rag.EmbedResult{Embeddings: [][]float64{{1, 0}}}, nil
		}), store, rag.WithRetrievalPolicyEvaluator(evaluator),
	)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{retriever: retriever, retrievalPolicyEvaluator: evaluator}
	managed := rawArgs(t, `{"query":"q","top_k":101}`)
	managed.Params.Meta = policyMetaFromJSON(t, `{"scope":{"collection":"managed"}}`)
	result, err := s.handleRAGSearch(context.Background(), managed)
	if err != nil || result == nil || !result.IsError || !strings.Contains(extractText(result), "top_k") {
		t.Fatalf("managed result=%#v error=%v", result, err)
	}
	if evaluator.evaluateCalls != 0 || embedCalls != 0 || store.calls != 0 {
		t.Fatalf("work after invalid managed top_k evaluator=%d embed=%d search=%d", evaluator.evaluateCalls, embedCalls, store.calls)
	}

	result, err = s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q","top_k":101}`))
	if err != nil || result == nil || result.IsError {
		t.Fatalf("legacy result=%#v error=%v", result, err)
	}
	if evaluator.evaluateCalls != 1 || embedCalls != 1 || store.calls != 1 || store.topK != 101 {
		t.Fatalf("legacy work evaluator=%d embed=%d search=%d top_k=%d", evaluator.evaluateCalls, embedCalls, store.calls, store.topK)
	}
}

func TestRAGRetrievalSchemasPreserveLegacyUnboundedTopK(t *testing.T) {
	s := &Server{mcpServer: gomcp.NewServer(&gomcp.Implementation{Name: "test", Version: "0.0.1"}, nil)}
	s.registerRAGTools()
	env := connectTestServer(t, s)
	defer env.cleanup()

	for _, tool := range []string{"rag_search", "rag_answer"} {
		t.Run(tool, func(t *testing.T) {
			topK := toolSchemaProperties(t, env.session, tool)["top_k"].(map[string]any)
			if maximum, ok := topK["maximum"]; ok {
				t.Fatalf("top_k maximum = %v, want no global cap", maximum)
			}
		})
	}
}

func TestRAGSearchRejectsTopKOverMaximumOnlyWhenScoped(t *testing.T) {
	t.Run("scoped", func(t *testing.T) {
		store := &recordingMCPMultiStore{}
		embedCalls := 0
		retriever, err := rag.NewRetrieverWithEmbedder(rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
			embedCalls++
			return rag.EmbedResult{Embeddings: [][]float64{{1, 0}}}, nil
		}), store)
		if err != nil {
			t.Fatal(err)
		}
		result, err := (&Server{retriever: retriever}).handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q","top_k":101,"collection":"managed"}`))
		if err != nil || result == nil || !result.IsError || !strings.Contains(extractText(result), "top_k") {
			t.Fatalf("result=%#v error=%v, want scoped top_k validation", result, err)
		}
		if embedCalls != 0 || store.calls != 0 {
			t.Fatalf("work after invalid top_k: embeds=%d searches=%d", embedCalls, store.calls)
		}
	})

	t.Run("legacy unscoped search", func(t *testing.T) {
		store := &recordingMCPMultiStore{}
		result, err := (&Server{retriever: mcpTestRetriever(t, store)}).handleRAGSearch(
			context.Background(), rawArgs(t, `{"query":"q","top_k":101}`),
		)
		if err != nil || result == nil || result.IsError {
			t.Fatalf("result=%#v error=%v, want success", result, err)
		}
		if store.calls != 1 || store.topK != 101 {
			t.Fatalf("search calls=%d top_k=%d, want 1/101", store.calls, store.topK)
		}
	})

	for name, scope := range map[string]string{
		"whitespace collection": `"collection":"   "`,
		"whitespace tags":       `"tags":[" "]`,
	} {
		t.Run("legacy unscoped search/"+name, func(t *testing.T) {
			store := &recordingMCPMultiStore{}
			result, err := (&Server{retriever: mcpTestRetriever(t, store)}).handleRAGSearch(
				context.Background(), rawArgs(t, `{"query":"q","top_k":101,`+scope+`}`),
			)
			if err != nil || result == nil || result.IsError {
				t.Fatalf("result=%#v error=%v, want legacy unscoped success", result, err)
			}
			if store.calls != 1 || store.topK != 101 {
				t.Fatalf("search calls=%d top_k=%d, want 1/101", store.calls, store.topK)
			}
		})
	}

	t.Run("legacy answer", func(t *testing.T) {
		store := &answerStore{}
		engine := &queuedRouteEngine{}
		s := answerEnvWithStore(t, engine, store)
		result, err := s.handleRAGAnswer(context.Background(), rawArgs(t, `{"question":"q","top_k":101}`))
		if err != nil || result == nil || result.IsError {
			t.Fatalf("result=%#v error=%v, want success", result, err)
		}
		if store.searchCalls != 1 || store.topK != 101 || engine.calls != 0 {
			t.Fatalf("answer work: searches=%d top_k=%d model=%d, want 1/101/0", store.searchCalls, store.topK, engine.calls)
		}
	})
}

func TestRAGRetrievalHandlersAcceptTopKMaximumAndDefaultNonPositive(t *testing.T) {
	for _, topK := range []struct {
		input, want int
	}{{maxRAGTopK, maxRAGTopK}, {0, 5}, {-1, 5}} {
		t.Run(fmt.Sprintf("search/%d", topK.input), func(t *testing.T) {
			store := &recordingMCPMultiStore{}
			result, err := (&Server{retriever: mcpTestRetriever(t, store)}).handleRAGSearch(
				context.Background(), rawArgs(t, fmt.Sprintf(`{"query":"q","top_k":%d}`, topK.input)),
			)
			if err != nil || result == nil || result.IsError {
				t.Fatalf("result=%#v error=%v, want success", result, err)
			}
			if store.calls != 1 || store.topK != topK.want {
				t.Fatalf("search calls=%d top_k=%d, want 1/%d", store.calls, store.topK, topK.want)
			}
		})

		t.Run(fmt.Sprintf("answer/%d", topK.input), func(t *testing.T) {
			store := &answerStore{}
			engine := &queuedRouteEngine{}
			s := answerEnvWithStore(t, engine, store)
			result, err := s.handleRAGAnswer(
				context.Background(), rawArgs(t, fmt.Sprintf(`{"question":"q","top_k":%d}`, topK.input)),
			)
			if err != nil || result == nil || result.IsError {
				t.Fatalf("result=%#v error=%v, want success", result, err)
			}
			if store.searchCalls != 1 || store.topK != topK.want || engine.calls != 0 {
				t.Fatalf("answer work: searches=%d top_k=%d model=%d, want 1/%d/0", store.searchCalls, store.topK, engine.calls, topK.want)
			}
		})
	}
}

func TestRAGSearchSchemaAndHandlerScope(t *testing.T) {
	s := newManagedRAGTestServer(t)
	env := connectTestServer(t, s)
	defer env.cleanup()
	properties := toolSchemaProperties(t, env.session, "rag_search")
	for _, name := range []string{"collection", "tags"} {
		property, ok := properties[name].(map[string]any)
		if !ok {
			t.Fatalf("property %q = %T, want map", name, properties[name])
		}
		if name == "collection" && property["maxLength"] != float64(rag.MaxManagedMetadataBytes) {
			t.Fatalf("collection maxLength = %v", property["maxLength"])
		}
	}
	tags := properties["tags"].(map[string]any)
	if tags["maxItems"] != float64(rag.MaxManagedTags) || tags["items"].(map[string]any)["maxLength"] != float64(rag.MaxManagedTagBytes) {
		t.Fatalf("tags schema = %#v", tags)
	}

	if _, err := s.managedSources.IngestText(context.Background(), "ops.md", "ops", rag.DocumentOptions{Collection: "ops", Tags: []string{"alpha", "beta"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.managedSources.IngestText(context.Background(), "other.md", "other", rag.DocumentOptions{Collection: "other", Tags: []string{"alpha", "beta"}}); err != nil {
		t.Fatal(err)
	}
	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q","collection":"ops","tags":["alpha","beta"],"explain_scores":true}`))
	if err != nil || result.IsError {
		t.Fatalf("scoped search error=%v result=%s", err, extractText(result))
	}
	var scored []rag.ScoredResult
	if err := json.Unmarshal([]byte(extractText(result)), &scored); err != nil {
		t.Fatal(err)
	}
	if len(scored) != 1 || scored[0].Chunk.Metadata["managed_collection"] != "ops" {
		t.Fatalf("scoped scored response = %#v", scored)
	}
	result, err = s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q","tags":["`+strings.Repeat("x", rag.MaxManagedTagBytes+1)+`"]}`))
	if err != nil || !result.IsError {
		t.Fatalf("oversize scope result=%#v error=%v, want validation error", result, err)
	}
}

func TestHandleRAGSearchRejectsRawInvalidUTF8BeforeRetrieval(t *testing.T) {
	req := &gomcp.CallToolRequest{Params: &gomcp.CallToolParamsRaw{Arguments: json.RawMessage([]byte{'{', '"', 'q', 'u', 'e', 'r', 'y', '"', ':', '"', 0xff, '"', '}'})}}
	result, err := (&Server{}).handleRAGSearch(context.Background(), req)
	if err != nil || !result.IsError || !strings.Contains(extractText(result), "valid UTF-8") {
		t.Fatalf("result=%#v error=%v, want raw UTF-8 validation failure", result, err)
	}
}

type recordingMCPMultiStore struct {
	stubVectorStore
	results []rag.ScoredResult
	err     error
	query   string
	topK    int
	qCtx    rag.QueryContext
	calls   int
}

func (s *recordingMCPMultiStore) SearchMulti(ctx context.Context, _ []float64, query string, topK int, qCtx rag.QueryContext) ([]rag.ScoredResult, error) {
	s.calls++
	s.query, s.topK, s.qCtx = query, topK, qCtx
	if s.err != nil {
		return nil, s.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.results, nil
}

type recordingMCPDenseStore struct {
	stubVectorStore
	results []rag.SearchResult
}

func (s *recordingMCPDenseStore) Search(ctx context.Context, _ []float64, _ int) ([]rag.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.results, nil
}

func mcpTestRetriever(t *testing.T, store rag.VectorStore, opts ...rag.RetrieverOption) *rag.Retriever {
	t.Helper()
	retriever, err := rag.NewRetrieverWithEmbedder(rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		return rag.EmbedResult{Embeddings: [][]float64{{1, 0}}}, nil
	}), store, opts...)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}
	return retriever
}

func managedScopedScoreServer(t *testing.T, evaluator rag.RetrievalPolicyEvaluator) *Server {
	t.Helper()
	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	embedder := rag.EmbedderFunc(func(_ context.Context, model string, inputs []string) (rag.EmbedResult, error) {
		embeddings := make([][]float64, len(inputs))
		for i, input := range inputs {
			embeddings[i] = []float64{1, 0}
			if input == "q" {
				embeddings[i] = []float64{1e-17, 1}
			}
		}
		return rag.EmbedResult{Embeddings: embeddings, Model: model, Provider: "test", VectorSpaceID: "test/v1"}, nil
	})
	indexer, err := rag.NewIndexerWithEmbedder(embedder, store, rag.WithEmbeddingModel("test"))
	if err != nil {
		t.Fatal(err)
	}
	opts := []rag.RetrieverOption{rag.WithRetrieverModel("test")}
	if evaluator != nil {
		opts = append(opts, rag.WithRetrievalPolicyEvaluator(evaluator))
	}
	retriever, err := rag.NewRetrieverWithEmbedder(embedder, store, opts...)
	if err != nil {
		t.Fatal(err)
	}
	managed, err := rag.NewManagedSources(indexer, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := managed.IngestText(context.Background(), "doc.md", "document", rag.DocumentOptions{Collection: "managed"}); err != nil {
		t.Fatal(err)
	}
	return &Server{store: store, retriever: retriever, retrievalPolicyEvaluator: evaluator}
}

func TestHandleRAGSearch_DefaultResponseRemainsSearchResultJSON(t *testing.T) {
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{{
		SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "c1"}, Score: 0.016, Distance: 0.1},
		RankScore:    0.016,
		Signals:      map[string]float64{"semantic": 0.9, "keyword": 0.5},
	}}}
	s := &Server{retriever: mcpTestRetriever(t, store)}

	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"question","top_k":3}`))
	if err != nil {
		t.Fatalf("handleRAGSearch: %v", err)
	}
	want, _ := json.Marshal([]rag.SearchResult{{Chunk: rag.Chunk{ID: "c1"}, Score: 0.9, Distance: 0.1}})
	if got := extractText(result); got != string(want) {
		t.Fatalf("default response = %s, want byte-compatible SearchResult JSON %s", got, want)
	}
	if result.Meta != nil {
		t.Fatalf("legacy metadata = %#v, want nil", result.Meta)
	}
	if store.calls != 1 || store.query != "question" || store.topK != 3 {
		t.Fatalf("SearchMulti calls=%d query=%q topK=%d, want 1/question/3", store.calls, store.query, store.topK)
	}
	if !reflect.DeepEqual(store.qCtx, rag.QueryContext{}) {
		t.Fatalf("default QueryContext = %+v, want zero value", store.qCtx)
	}
}

func TestHandleRAGSearch_ForwardsQueryContextWithoutChangingResponseContract(t *testing.T) {
	scored := rag.ScoredResult{
		SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "contextual"}, Score: 0.75, Distance: 0.25},
		RankScore:    0.05,
		Signals:      map[string]float64{"semantic": 0.75, "structural": 1},
	}
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{scored}}
	s := &Server{retriever: mcpTestRetriever(t, store)}

	before := time.Now()
	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{
		"query":"question","current_file":"pkg/current.go","workspace_root":"/workspace",
		"open_files":["pkg/a.go","pkg/b.go"]
	}`))
	after := time.Now()
	if err != nil {
		t.Fatalf("handleRAGSearch: %v", err)
	}
	if store.qCtx.Timestamp.Before(before) || store.qCtx.Timestamp.After(after) {
		t.Fatalf("QueryContext.Timestamp = %v, want request time in [%v, %v]", store.qCtx.Timestamp, before, after)
	}
	wantContext := rag.QueryContext{
		CurrentFile: "pkg/current.go", WorkspaceRoot: "/workspace", OpenFiles: []string{"pkg/a.go", "pkg/b.go"}, Timestamp: store.qCtx.Timestamp,
	}
	if !reflect.DeepEqual(store.qCtx, wantContext) {
		t.Fatalf("QueryContext = %+v, want %+v", store.qCtx, wantContext)
	}
	want, _ := json.Marshal([]rag.SearchResult{scored.SearchResult})
	if got := extractText(result); got != string(want) {
		t.Fatalf("contextual response = %s, want flattened SearchResult JSON %s", got, want)
	}
}

func TestHandleRAGSearch_DropsEmptyOpenFilesBeforeForwarding(t *testing.T) {
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{{
		SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "c1"}, Score: 0.75, Distance: 0.25},
		RankScore:    0.05,
	}}}
	s := &Server{retriever: mcpTestRetriever(t, store)}

	if _, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{
		"query":"question","current_file":"pkg/current.go","open_files":["","pkg/a.go",""]
	}`)); err != nil {
		t.Fatalf("handleRAGSearch: %v", err)
	}
	if !reflect.DeepEqual(store.qCtx.OpenFiles, []string{"pkg/a.go"}) {
		t.Fatalf("forwarded OpenFiles = %#v, want empty entries dropped to [pkg/a.go]", store.qCtx.OpenFiles)
	}
}

func TestHandleRAGSearch_ExplainScoresPreservesRankAndSignals(t *testing.T) {
	scored := rag.ScoredResult{
		SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "c1"}, Score: 0.8, Distance: 0.2},
		RankScore:    0.047,
		Signals:      map[string]float64{"semantic": 0.8, "keyword": 0.4, "temporal": 0.3, "structural": 1},
	}
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{scored}}
	s := &Server{retriever: mcpTestRetriever(t, store)}

	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"question","explain_scores":true}`))
	if err != nil {
		t.Fatalf("handleRAGSearch: %v", err)
	}
	var got []rag.ScoredResult
	if err := json.Unmarshal([]byte(extractText(result)), &got); err != nil {
		t.Fatalf("unmarshal scored response: %v", err)
	}
	if !reflect.DeepEqual(got, []rag.ScoredResult{scored}) {
		t.Fatalf("scored response = %+v, want %+v", got, []rag.ScoredResult{scored})
	}
	if result.Meta != nil {
		t.Fatalf("legacy metadata = %#v, want nil", result.Meta)
	}
}

func TestHandleRAGSearch_ExplainScoresDenseFallback(t *testing.T) {
	store := &recordingMCPDenseStore{results: []rag.SearchResult{{
		Chunk: rag.Chunk{ID: "dense"}, Score: 0.7, Distance: 0.3,
	}}}
	s := &Server{retriever: mcpTestRetriever(t, store)}

	result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"question","explain_scores":true}`))
	if err != nil {
		t.Fatalf("handleRAGSearch: %v", err)
	}
	var got []rag.ScoredResult
	if err := json.Unmarshal([]byte(extractText(result)), &got); err != nil {
		t.Fatalf("unmarshal scored response: %v", err)
	}
	if len(got) != 1 || got[0].RankScore != 0.7 || !reflect.DeepEqual(got[0].Signals, map[string]float64{"semantic": 0.7}) {
		t.Fatalf("dense scored response = %+v, want RankScore=Score and semantic-only signal", got)
	}
}

func TestHandleRAGSearch_PreservesLegacyEmptyJSON(t *testing.T) {
	t.Run("dense", func(t *testing.T) {
		store := &recordingMCPDenseStore{results: []rag.SearchResult{}}
		s := &Server{store: store, retriever: mcpTestRetriever(t, store)}
		result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q"}`))
		if err != nil || result == nil || result.IsError || extractText(result) != "[]" {
			t.Fatalf("result=%#v text=%q error=%v, want []", result, extractText(result), err)
		}
	})

	t.Run("hybrid", func(t *testing.T) {
		store := &recordingMCPMultiStore{results: []rag.ScoredResult{}}
		s := &Server{store: store, retriever: mcpTestRetriever(t, store)}
		result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q"}`))
		if err != nil || result == nil || result.IsError || extractText(result) != "null" {
			t.Fatalf("result=%#v text=%q error=%v, want null", result, extractText(result), err)
		}
	})

	t.Run("contextual hybrid", func(t *testing.T) {
		store := &recordingMCPMultiStore{results: []rag.ScoredResult{}}
		s := &Server{store: store, retriever: mcpTestRetriever(t, store)}
		result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q","current_file":"file.go"}`))
		if err != nil || result == nil || result.IsError || extractText(result) != "[]" {
			t.Fatalf("result=%#v text=%q error=%v, want []", result, extractText(result), err)
		}
	})
}

func TestHandleRAGSearch_PreservesScopedFilteredEmptyJSON(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       string
		policyMeta string
		evaluator  *mcpPolicyEvaluatorSpy
	}{
		{name: "caller scope", args: `{"query":"q","collection":"missing"}`},
		{
			name: "policy scope", args: `{"query":"q"}`,
			policyMeta: `{"scope":{"collection":"missing"}}`,
			evaluator:  &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true}},
		},
		{
			name:      "evaluator scope",
			args:      `{"query":"q"}`,
			evaluator: &mcpPolicyEvaluatorSpy{decision: rag.RetrievalPolicyDecision{Allow: true, Scope: rag.RetrievalScope{Collection: "missing"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := managedScopedScoreServer(t, tc.evaluator)
			req := rawArgs(t, tc.args)
			if tc.policyMeta != "" {
				req.Params.Meta = policyMetaFromJSON(t, tc.policyMeta)
			}
			result, err := s.handleRAGSearch(context.Background(), req)
			if err != nil || result == nil || result.IsError || extractText(result) != "[]" {
				t.Fatalf("result=%#v text=%q error=%v, want []", result, extractText(result), err)
			}
		})
	}
}

func TestHandleRAGSearch_ContextualErrorAndCancellation(t *testing.T) {
	t.Run("retriever error", func(t *testing.T) {
		store := &recordingMCPMultiStore{err: errors.New("boom")}
		s := &Server{retriever: mcpTestRetriever(t, store)}
		result, err := s.handleRAGSearch(context.Background(), rawArgs(t, `{"query":"q","current_file":"a.go"}`))
		if err != nil {
			t.Fatalf("handleRAGSearch: %v", err)
		}
		if !result.IsError || !strings.Contains(extractText(result), "search:") || !strings.Contains(extractText(result), "boom") {
			t.Fatalf("result = isError:%v text:%q, want established search tool error", result.IsError, extractText(result))
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		store := &recordingMCPMultiStore{}
		s := &Server{retriever: mcpTestRetriever(t, store)}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := s.handleRAGSearch(ctx, rawArgs(t, `{"query":"q","explain_scores":true}`))
		if err != nil {
			t.Fatalf("handleRAGSearch: %v", err)
		}
		if !result.IsError || !strings.Contains(extractText(result), "context canceled") {
			t.Fatalf("result = isError:%v text:%q, want cancellation error", result.IsError, extractText(result))
		}
	})
}

func TestRAGStatsToolDisabled(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock) // RAG disabled by default in newTestEnv
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "rag_stats",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when RAG disabled")
	}
	if text := extractText(result); !strings.Contains(text, "RAG is disabled") {
		t.Errorf("error = %q, want to contain %q", text, "RAG is disabled")
	}
}

func TestRAGStatsToolEnabled(t *testing.T) {
	// Create a server with a stub vector store to test the stats path.
	s := &Server{
		client:   nil,
		store:    stubVectorStore{},
		resolved: make(map[string]config.ResolvedModel),
		mcpServer: gomcp.NewServer(&gomcp.Implementation{
			Name: "test", Version: "0.0.1",
		}, nil),
	}
	s.registerRAGTools()

	env := connectTestServer(t, s)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "rag_stats",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}

	text := extractText(result)
	var stats rag.StoreStats
	if err := json.Unmarshal([]byte(text), &stats); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
}

func TestRAGSearchToolDisabled(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "rag_search",
		Arguments: map[string]any{
			"query": "test",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when RAG disabled")
	}
}

func TestRAGDeleteToolDisabled(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "rag_delete",
		Arguments: map[string]any{
			"source": "test.go",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when RAG disabled")
	}
}

func TestRAGDeleteToolPreservesLegacySuccessText(t *testing.T) {
	s := &Server{
		store: stubVectorStore{},
		mcpServer: gomcp.NewServer(&gomcp.Implementation{
			Name: "test", Version: "0.0.1",
		}, nil),
	}
	s.registerRAGTools()

	env := connectTestServer(t, s)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "rag_delete",
		Arguments: map[string]any{
			"source": "test.go",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", extractText(result))
	}
	if text := extractText(result); text != "deleted source test.go" {
		t.Fatalf("result = %q, want legacy success text", text)
	}
}

func TestRAGDeleteToolEmptySource(t *testing.T) {
	s := &Server{
		store: stubVectorStore{},
		mcpServer: gomcp.NewServer(&gomcp.Implementation{
			Name: "test", Version: "0.0.1",
		}, nil),
	}
	s.registerRAGTools()

	env := connectTestServer(t, s)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "rag_delete",
		Arguments: map[string]any{
			"source": "",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty source")
	}
	if text := extractText(result); !strings.Contains(text, "source must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "source must not be empty")
	}
}

func TestRAGIndexFileToolDisabled(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "rag_index_file",
		Arguments: map[string]any{
			"path": "/tmp/test.go",
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when RAG disabled")
	}
}

// connectTestServer connects an MCP client to an already-constructed Server
// for cases where we need a non-standard server configuration (e.g. RAG enabled).
func connectTestServer(t *testing.T, s *Server) testEnv {
	t.Helper()

	ctx := context.Background()
	serverTransport, clientTransport := gomcp.NewInMemoryTransports()

	serverSession, sErr := s.mcpServer.Connect(ctx, serverTransport, nil)
	if sErr != nil {
		t.Fatalf("server.Connect() error = %v", sErr)
	}

	client := gomcp.NewClient(&gomcp.Implementation{
		Name: "test-client", Version: "0.0.1",
	}, nil)
	session, cErr := client.Connect(ctx, clientTransport, nil)
	if cErr != nil {
		t.Fatalf("client.Connect() error = %v", cErr)
	}

	return testEnv{
		server:  s,
		session: session,
		cleanup: func() {
			_ = session.Close()
			_ = serverSession.Close()
		},
	}
}
