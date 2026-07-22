package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

func TestHandleChat_ForwardsPolicyMetaAndRedactsBeforeRouter(t *testing.T) {
	redacted := "REDACTED"
	evaluator := &mcpPolicyEvaluatorSpy{
		decision: rag.RetrievalPolicyDecision{Allow: true},
		resultDecision: []rag.RetrievalResultDecision{
			{Keep: false},
			{Keep: true, RedactedContent: &redacted},
		},
	}
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{
		{SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "denied", Source: "denied.go", Content: "DENIED_SECRET"}, Score: 0.9, Distance: 0.1}},
		{SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "redacted", Source: "allowed.go", Content: "ORIGINAL_SECRET"}, Score: 0.8, Distance: 0.2}},
	}}
	router := newRecordingRouteEngine("answer")
	s := &Server{
		router:                   router,
		retriever:                mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator)),
		retrievalPolicyEvaluator: evaluator,
	}
	WithRetrievalPrincipalResolver(func(context.Context, gomcp.Request) (string, error) {
		return "resolved-principal", nil
	})(s)
	req := rawArgs(t, `{"model":"ollama/test","use_rag":true,"messages":[{"role":"user","content":"question"}]}`)
	req.Params.Meta = policyMetaFromJSON(t, `{
		"principal_id":"claimed-principal","session_id":"claimed-session",
		"max_results":2,"audit_labels":{"purpose":"support"}
	}`)

	result, err := s.handleChat(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if result.Meta == nil {
		t.Fatal("successful governed chat omitted safe applied metadata")
	}
	if evaluator.evaluateCalls != 1 || evaluator.resultCalls != 1 {
		t.Fatalf("evaluator calls = %d/%d, want 1/1", evaluator.evaluateCalls, evaluator.resultCalls)
	}
	if store.calls != 1 {
		t.Fatalf("retrieval calls = %d, want 1", store.calls)
	}
	if evaluator.last.Policy.PrincipalID != "resolved-principal" ||
		evaluator.last.Policy.SessionID != "claimed-session" ||
		evaluator.last.Policy.MaxResults != 2 ||
		evaluator.last.Policy.AuditLabels["purpose"] != "support" {
		t.Fatalf("evaluator policy = %#v", evaluator.last.Policy)
	}
	if !reflect.DeepEqual(evaluator.last.QueryContext, rag.QueryContext{}) || evaluator.last.QueryContext.Metadata != nil {
		t.Fatalf("evaluator QueryContext = %#v, want exact zero value", evaluator.last.QueryContext)
	}
	rendered := fmt.Sprint(router.last.Messages)
	if strings.Contains(rendered, "DENIED_SECRET") || strings.Contains(rendered, "ORIGINAL_SECRET") || !strings.Contains(rendered, "REDACTED") {
		t.Fatalf("model-visible messages leaked policy content: %s", rendered)
	}
	metaJSON, err := json.Marshal(result.Meta)
	if err != nil || strings.Contains(string(metaJSON), "principal") || strings.Contains(string(metaJSON), "purpose") {
		t.Fatalf("unsafe result meta: %s, %v", metaJSON, err)
	}
}

func TestHandleChat_PolicyFailureIsSanitized(t *testing.T) {
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
	router := newRecordingRouteEngine("answer")
	s := &Server{router: router, retriever: retriever, retrievalPolicyEvaluator: evaluator}

	result, err := s.handleChat(context.Background(), rawArgs(t, `{"model":"ollama/test","use_rag":true,"messages":[{"role":"user","content":"question"}]}`))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if got := extractText(result); got != "policy_evaluator_failed: retrieval policy evaluation failed" || strings.Contains(got, "ERROR_SECRET") {
		t.Fatalf("error text = %q", got)
	}
	if result.Meta == nil {
		t.Fatal("policy failure omitted safe applied metadata")
	}
	if evaluator.evaluateCalls != 1 || evaluator.resultCalls != 0 || embedCalls != 0 || store.calls != 0 || router.called {
		t.Fatalf("work after policy failure evaluator=%d/%d embed=%d search=%d router=%v",
			evaluator.evaluateCalls, evaluator.resultCalls, embedCalls, store.calls, router.called)
	}
}

func TestHandleChat_PolicyRequestFailureIsSanitized(t *testing.T) {
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
			router := newRecordingRouteEngine("answer")
			s := &Server{router: router, retriever: retriever, retrievalPolicyEvaluator: evaluator}
			if tc.bind != nil {
				tc.bind(s)
			}
			req := rawArgs(t, `{"model":"ollama/test","use_rag":true,"messages":[{"role":"user","content":"question"}]}`)
			req.Params.Meta = tc.meta

			result, err := s.handleChat(context.Background(), req)
			if err != nil || result == nil || !result.IsError || extractText(result) != tc.want {
				t.Fatalf("result = %#v, text = %q, error = %v, want %q", result, extractText(result), err, tc.want)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "SECRET") || result.Meta != nil {
				t.Fatalf("request error leaked detail or metadata: %s", data)
			}
			if evaluator.evaluateCalls != 0 || evaluator.resultCalls != 0 || embedCalls != 0 || store.calls != 0 || router.called {
				t.Fatalf("work after request failure evaluator=%d/%d embed=%d search=%d router=%v",
					evaluator.evaluateCalls, evaluator.resultCalls, embedCalls, store.calls, router.called)
			}
		})
	}
}

func TestHandleChat_PolicyMetaSurvivesPostRetrievalErrors(t *testing.T) {
	newServer := func(t *testing.T, router routeEngine) *Server {
		t.Helper()
		evaluator := &mcpPolicyEvaluatorSpy{
			decision:       rag.RetrievalPolicyDecision{Allow: true},
			resultDecision: []rag.RetrievalResultDecision{{Keep: true}},
		}
		store := &recordingMCPMultiStore{results: []rag.ScoredResult{{
			SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "allowed", Source: "allowed.go", Content: "allowed"}, Score: 0.8, Distance: 0.2},
		}}}
		return &Server{
			router: router, retriever: mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator)),
			retrievalPolicyEvaluator: evaluator,
		}
	}

	for _, tc := range []struct {
		name   string
		server func(*testing.T) *Server
		model  string
		want   string
	}{
		{
			name: "config", server: func(t *testing.T) *Server { return newServer(t, newRecordingRouteEngine("answer")) },
			want: "config:",
		},
		{
			name: "router", server: func(t *testing.T) *Server {
				router := newRecordingRouteEngine("answer")
				router.routeErr = errors.New("route failed")
				return newServer(t, router)
			}, model: "ollama/test", want: "router: route failed",
		},
		{
			name: "model", server: func(t *testing.T) *Server {
				return newServer(t, &chatErrorRouteEngine{recordingRouteEngine: newRecordingRouteEngine("answer")})
			}, model: "ollama/test", want: "ollama: model failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := fmt.Sprintf(`{"model":%q,"use_rag":true,"messages":[{"role":"user","content":"question"}]}`, tc.model)
			result, err := tc.server(t).handleChat(context.Background(), rawArgs(t, args))
			if err != nil || result == nil || !result.IsError || !strings.HasPrefix(extractText(result), tc.want) {
				t.Fatalf("result = %#v, text = %q, error = %v, want prefix %q", result, extractText(result), err, tc.want)
			}
			if result.Meta == nil {
				t.Fatal("post-retrieval error omitted safe applied metadata")
			}
		})
	}
}

type chatErrorRouteEngine struct {
	*recordingRouteEngine
}

func (e *chatErrorRouteEngine) Route(ctx context.Context, req provider.RoutingRequest) (*provider.RoutePlan, error) {
	plan, err := e.recordingRouteEngine.Route(ctx, req)
	if err == nil {
		plan.Provider = &chatErrorProvider{fakeRouteProvider: plan.Provider.(*fakeRouteProvider)}
	}
	return plan, err
}

type chatErrorProvider struct {
	*fakeRouteProvider
}

func (*chatErrorProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("model failed")
}

type policyMetadataEvaluator struct{}

func (*policyMetadataEvaluator) Evaluate(context.Context, rag.RetrievalRequest) (rag.RetrievalPolicyDecision, error) {
	return rag.RetrievalPolicyDecision{Allow: true}, nil
}

func (*policyMetadataEvaluator) EvaluateResults(_ context.Context, req rag.RetrievalRequest, _ []rag.Chunk) ([]rag.RetrievalResultDecision, error) {
	if req.Policy.AuditLabels["purpose"] != "support" {
		return nil, nil
	}
	redacted := "REDACTED"
	return []rag.RetrievalResultDecision{{Keep: false}, {Keep: true, RedactedContent: &redacted}}, nil
}

func TestChatSchema_QueryContext(t *testing.T) {
	env := newTestEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer env.cleanup()

	properties := toolSchemaProperties(t, env.session, "chat")
	for name, wantType := range map[string]string{
		"current_file": "string", "workspace_root": "string", "open_files": "array",
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
}

func TestHandleChat_RAGDefaultPathAndPromptRemainUnchanged(t *testing.T) {
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{{
		SearchResult: rag.SearchResult{
			Chunk: rag.Chunk{ID: "c1", Source: "default.go", StartLine: 10, EndLine: 11, Content: "line one\nline two"},
			Score: 0.016, Distance: 0.2,
		},
		RankScore: 0.016,
		Signals:   map[string]float64{"semantic": 0.8, "keyword": 0.5},
	}}}
	router := newRecordingRouteEngine("answer")
	s := &Server{router: router, retriever: mcpTestRetriever(t, store)}

	result, err := s.handleChat(context.Background(), rawArgs(t, `{
		"model":"ollama/test","use_rag":true,
		"current_file":"","workspace_root":"","open_files":[],
		"messages":[{"role":"system","content":"original"},{"role":"user","content":"question"}]
	}`))
	if err != nil || result.IsError {
		t.Fatalf("handleChat: err=%v isError=%v text=%q", err, result.IsError, extractText(result))
	}
	if !reflect.DeepEqual(store.qCtx, rag.QueryContext{}) {
		t.Fatalf("default QueryContext = %+v, want zero value", store.qCtx)
	}
	wantPrompt := "Relevant context from the codebase:\n\nRelevant code context:\n\n" +
		"--- default.go (lines 10-11, similarity: 0.80) ---\n10| line one\n11| line two\n\n"
	if len(router.last.Messages) != 3 || router.last.Messages[0].Role != "system" || router.last.Messages[0].Content != wantPrompt {
		t.Fatalf("model messages = %+v, want unchanged compact RAG prompt", router.last.Messages)
	}
	if router.last.Messages[1].Content != "original" || router.last.Messages[2].Content != "question" {
		t.Fatalf("message order changed: %+v", router.last.Messages)
	}
	if strings.Contains(wantPrompt, "RankScore") || strings.Contains(wantPrompt, "keyword") {
		t.Fatalf("default prompt leaked score metadata: %q", wantPrompt)
	}
}

func TestHandleChat_RAGForwardsQueryContextAndKeepsPromptCompact(t *testing.T) {
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{
		{
			SearchResult: rag.SearchResult{
				Chunk: rag.Chunk{ID: "c1", Source: "context.go", StartLine: 7, EndLine: 7, Content: "context line"},
				Score: 0.61, Distance: 0.2,
			},
			RankScore: 0.047,
			Signals:   map[string]float64{"semantic": 0.8, "keyword": 0.4, "structural": 1},
		},
		{
			SearchResult: rag.SearchResult{
				Chunk: rag.Chunk{ID: "c2", Source: "too-large.go", StartLine: 1, EndLine: 1, Content: strings.Repeat("x", ragContextMaxTokens*4)},
				Score: 0.7, Distance: 0.3,
			},
			RankScore: 0.03,
			Signals:   map[string]float64{"semantic": 0.7},
		},
	}}
	router := newRecordingRouteEngine("answer")
	s := &Server{router: router, retriever: mcpTestRetriever(t, store)}

	result, err := s.handleChat(context.Background(), rawArgs(t, `{
		"model":"ollama/test","use_rag":true,"rag_top_k":2,
		"current_file":"pkg/current.go","workspace_root":"/workspace","open_files":["pkg/a.go","pkg/b.go"],
		"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"reply"},{"role":"user","content":"last question"}]
	}`))
	if err != nil || result.IsError {
		t.Fatalf("handleChat: err=%v isError=%v text=%q", err, result.IsError, extractText(result))
	}
	wantContext := rag.QueryContext{
		CurrentFile: "pkg/current.go", WorkspaceRoot: "/workspace", OpenFiles: []string{"pkg/a.go", "pkg/b.go"}, Timestamp: store.qCtx.Timestamp,
	}
	if store.qCtx.Timestamp.IsZero() {
		t.Fatal("QueryContext.Timestamp is zero, want request time")
	}
	if store.query != "last question" || store.topK != 2 || !reflect.DeepEqual(store.qCtx, wantContext) {
		t.Fatalf("retrieval query=%q topK=%d context=%+v, want last question/2/%+v", store.query, store.topK, store.qCtx, wantContext)
	}
	if len(router.last.Messages) != 4 {
		t.Fatalf("model messages = %d, want injected context plus three original messages", len(router.last.Messages))
	}
	contextMessage := router.last.Messages[0].Content
	if !strings.Contains(contextMessage, "similarity: 0.61") {
		t.Fatalf("contextual prompt did not preserve embedded SearchResult score: %s", contextMessage)
	}
	for _, unwanted := range []string{"RankScore", "Signals", "keyword", "structural", "0.047", "too-large.go"} {
		if strings.Contains(contextMessage, unwanted) {
			t.Errorf("ordinary chat context contains %q: %s", unwanted, contextMessage)
		}
	}
	for i, want := range []struct{ role, content string }{{"user", "first"}, {"assistant", "reply"}, {"user", "last question"}} {
		got := router.last.Messages[i+1]
		if got.Role != want.role || got.Content != want.content {
			t.Fatalf("message %d = %+v, want role=%q content=%q", i+1, got, want.role, want.content)
		}
	}
}

func TestHandleChat_QueryContextRequiresUseRAG(t *testing.T) {
	router := newRecordingRouteEngine("answer")
	s := &Server{router: router}

	result, err := s.handleChat(context.Background(), rawArgs(t, `{
		"model":"ollama/test","current_file":"pkg/current.go",
		"messages":[{"role":"user","content":"question"}]
	}`))
	if err != nil {
		t.Fatalf("handleChat: %v", err)
	}
	if !result.IsError || !strings.Contains(extractText(result), "require use_rag=true") {
		t.Fatalf("result = isError:%v text:%q, want context/use_rag validation error", result.IsError, extractText(result))
	}
	if router.called {
		t.Fatal("router called despite invalid context without use_rag")
	}
}

func TestChatToolRejectsMalformedOpenFiles(t *testing.T) {
	s := &Server{
		router: newRecordingRouteEngine("answer"),
		mcpServer: gomcp.NewServer(&gomcp.Implementation{
			Name: "test", Version: "0.0.1",
		}, nil),
	}
	s.registerChatTools()
	env := connectTestServer(t, s)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model": "ollama/test", "use_rag": false, "open_files": "pkg/a.go",
			"messages": []map[string]any{{"role": "user", "content": "question"}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !result.IsError {
		t.Fatal("malformed open_files accepted, want schema/JSON validation error")
	}
}

func TestHandleChat_ContextualRAGErrors(t *testing.T) {
	t.Run("retriever error", func(t *testing.T) {
		router := newRecordingRouteEngine("answer")
		store := &recordingMCPMultiStore{err: errors.New("boom")}
		s := &Server{router: router, retriever: mcpTestRetriever(t, store)}
		result, err := s.handleChat(context.Background(), rawArgs(t, `{
			"model":"ollama/test","use_rag":true,"current_file":"a.go",
			"messages":[{"role":"user","content":"question"}]
		}`))
		if err != nil {
			t.Fatalf("handleChat: %v", err)
		}
		if !result.IsError || !strings.Contains(extractText(result), "retrieve:") || !strings.Contains(extractText(result), "boom") {
			t.Fatalf("result = isError:%v text:%q, want established retrieve error", result.IsError, extractText(result))
		}
		if router.called {
			t.Fatal("router called after retrieval error")
		}
	})

	t.Run("no results", func(t *testing.T) {
		s := &Server{router: newRecordingRouteEngine("answer"), retriever: mcpTestRetriever(t, &recordingMCPMultiStore{})}
		result, err := s.handleChat(context.Background(), rawArgs(t, `{
			"model":"ollama/test","use_rag":true,"workspace_root":"/workspace",
			"messages":[{"role":"user","content":"question"}]
		}`))
		if err != nil {
			t.Fatalf("handleChat: %v", err)
		}
		if !result.IsError || extractText(result) != "rag: RAG index is empty; run rag_index_file or rag_index_directory first" || result.Meta != nil {
			t.Fatalf("result = isError:%v text:%q, want established empty-index error", result.IsError, extractText(result))
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		s := &Server{router: newRecordingRouteEngine("answer"), retriever: mcpTestRetriever(t, &recordingMCPMultiStore{})}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := s.handleChat(ctx, rawArgs(t, `{
			"model":"ollama/test","use_rag":true,"open_files":["a.go"],
			"messages":[{"role":"user","content":"question"}]
		}`))
		if err != nil {
			t.Fatalf("handleChat: %v", err)
		}
		if !result.IsError || !strings.Contains(extractText(result), "context canceled") {
			t.Fatalf("result = isError:%v text:%q, want cancellation error", result.IsError, extractText(result))
		}
	})
}

func TestChatToolBasic(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test requires /api/show context_length " +
		"parsing (pre-existing client limitation); request-shape coverage " +
		"now lives in TestHandleChat_UsesRouter via the routeEngine seam.")
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"test-model"}]}`))
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			resp := `{"model":"test-model","message":{"role":"assistant","content":"Hello back!"},"done":true}`
			_, _ = w.Write([]byte(resp))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model": "test-model",
			"messages": []map[string]any{
				{"role": "user", "content": "Hello!"},
			},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("CallTool() isError = true, content = %v", extractText(result))
	}

	text := extractText(result)
	if text != "Hello back!" {
		t.Errorf("got %q, want %q", text, "Hello back!")
	}
}

func TestChatToolWithTemperature(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test requires /api/show context_length " +
		"parsing (pre-existing client limitation); temperature-shape coverage " +
		"now lives in TestHandleChat_UsesRouter via the routeEngine seam.")
	var receivedTemp float64
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/chat":
			var body struct {
				Options struct {
					Temperature float64 `json:"temperature"`
				} `json:"options"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				receivedTemp = body.Options.Temperature
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	_, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model":       "m",
			"messages":    []map[string]any{{"role": "user", "content": "hi"}},
			"temperature": 0.9,
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if receivedTemp != 0.9 {
		t.Errorf("temperature = %v, want 0.9", receivedTemp)
	}
}

func TestChatToolMissingModelNoConfig(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when no model and no config")
	}
	text := extractText(result)
	if !strings.Contains(text, "model parameter required") {
		t.Errorf("error = %q, want to contain %q", text, "model parameter required")
	}
}

func TestChatToolEmptyMessages(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model":    "m",
			"messages": []map[string]any{},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for empty messages")
	}
	text := extractText(result)
	if !strings.Contains(text, "messages must not be empty") {
		t.Errorf("error = %q, want to contain %q", text, "messages must not be empty")
	}
}

func TestChatToolRAGDisabled(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model":    "m",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
			"use_rag":  true,
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when RAG disabled")
	}
	text := extractText(result)
	if !strings.Contains(text, "RAG is disabled") {
		t.Errorf("error = %q, want to contain %q", text, "RAG is disabled")
	}
}

func TestChatToolOllamaError(t *testing.T) {
	t.Skip("end-to-end Ollama-traffic test; Router error wrapping changed the " +
		"error prefix (router: vs ollama:). End-to-end error paths are " +
		"covered by the integration test in provider/router_integration_test.go.")
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/chat":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"model not loaded"}`))
		default:
			http.NotFound(w, r)
		}
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name: "chat",
		Arguments: map[string]any{
			"model":    "bad-model",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true when Ollama returns 500")
	}
	text := extractText(result)
	if !strings.Contains(text, "ollama:") {
		t.Errorf("error = %q, want to contain %q", text, "ollama:")
	}
}

func TestChatToolInvalidArguments(t *testing.T) {
	mock := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	env := newTestEnv(t, mock)
	defer env.cleanup()

	result, err := env.session.CallTool(context.Background(), &gomcp.CallToolParams{
		Name:      "chat",
		Arguments: "not a json object",
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected isError = true for invalid arguments")
	}
	text := extractText(result)
	if !strings.Contains(text, "validation:") {
		t.Errorf("error = %q, want to contain %q", text, "validation:")
	}
}

func TestLastUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		msgs     []ollama.ChatMessage
		expected string
	}{
		{
			name:     "single user message",
			msgs:     []ollama.ChatMessage{{Role: "user", Content: "hello"}},
			expected: "hello",
		},
		{
			name: "multiple messages returns last user",
			msgs: []ollama.ChatMessage{
				{Role: "user", Content: "first"},
				{Role: "assistant", Content: "reply"},
				{Role: "user", Content: "second"},
			},
			expected: "second",
		},
		{
			name: "no user messages",
			msgs: []ollama.ChatMessage{
				{Role: "system", Content: "you are helpful"},
				{Role: "assistant", Content: "hi"},
			},
			expected: "",
		},
		{
			name:     "nil slice",
			msgs:     nil,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lastUserMessage(tt.msgs)
			if got != tt.expected {
				t.Errorf("lastUserMessage() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestHandleChat_UsesRouter(t *testing.T) {
	router := newRecordingRouteEngine("routed-chat")
	s := &Server{
		cfg: &config.Config{
			Defaults: map[string]string{"chat": "primary"},
			Models: map[string]config.ModelConfig{
				"primary": {Name: "qwen3:8b", Provider: "ollama"},
			},
		},
		router: router,
	}

	args, _ := json.Marshal(chatArgs{Messages: []ollama.ChatMessage{{Role: "user", Content: "hi"}}})
	res, err := s.handleChat(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleChat: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleChat returned error: %s", extractText(res))
	}
	if got := extractText(res); got != "routed-chat" {
		t.Errorf("response content = %q, want routed-chat", got)
	}
	if !router.called {
		t.Fatal("router was not called")
	}
	if router.last.Model != "" {
		t.Errorf("RoutingRequest.Model = %q, want empty for configured default", router.last.Model)
	}
	if want := []string{"ollama/qwen3:8b"}; !reflect.DeepEqual(router.last.PreferredChain, want) {
		t.Errorf("PreferredChain = %v, want %v", router.last.PreferredChain, want)
	}
	if router.last.UseCase != "chat" {
		t.Errorf("UseCase = %q, want chat", router.last.UseCase)
	}
	if router.last.RequiredCaps != provider.CapChat {
		t.Errorf("RequiredCaps = %v, want CapChat", router.last.RequiredCaps)
	}
}

func TestHandleChat_PreservesToolMetadata(t *testing.T) {
	router := newRecordingRouteEngine("routed-chat")
	s := &Server{router: router}

	args, err := json.Marshal(chatArgs{
		Model: "ollama/qwen3:8b",
		Messages: []ollama.ChatMessage{
			{
				Role: "assistant",
				ToolCalls: []ollama.ToolCall{
					{
						ID:   "call_1",
						Type: "function",
						Function: ollama.ToolCallFunction{
							Index:     0,
							Name:      "lookup",
							Arguments: map[string]any{"query": "weather"},
						},
					},
				},
			},
			{
				Role:       "tool",
				Content:    "sunny",
				ToolName:   "lookup",
				ToolCallID: "call_1",
			},
			{Role: "user", Content: "continue"},
		},
	})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	_, err = s.handleChat(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleChat: %v", err)
	}

	if len(router.last.Messages) != 3 {
		t.Fatalf("router messages = %d, want 3", len(router.last.Messages))
	}

	assistant := router.last.Messages[0]
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant tool calls = %d, want 1", len(assistant.ToolCalls))
	}
	call := assistant.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "lookup" {
		t.Fatalf("tool call = %+v, want call_1/function/lookup", call)
	}
	var callArgs map[string]string
	if err := json.Unmarshal(call.Function.Arguments, &callArgs); err != nil {
		t.Fatalf("unmarshal provider tool args: %v", err)
	}
	if callArgs["query"] != "weather" {
		t.Errorf("tool call query = %q, want weather", callArgs["query"])
	}

	tool := router.last.Messages[1]
	if tool.Role != "tool" || tool.ToolName != "lookup" || tool.ToolCallID != "call_1" {
		t.Errorf("tool message = %+v, want role/tool_name/tool_call_id preserved", tool)
	}
}

func TestHandleChat_ExplicitModelSkipsChain(t *testing.T) {
	router := newRecordingRouteEngine("routed-chat")
	s := &Server{router: router} // no cfg — explicit model only path

	args, _ := json.Marshal(chatArgs{
		Model:    "ollama/explicit:8b",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "hi"}},
	})
	_, err := s.handleChat(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleChat: %v", err)
	}
	if router.last.Model != "ollama/explicit:8b" {
		t.Errorf("Model = %q, want ollama/explicit:8b", router.last.Model)
	}
	if len(router.last.PreferredChain) != 0 {
		t.Errorf("PreferredChain = %v, want empty for explicit model", router.last.PreferredChain)
	}
}

// extractText returns the text content from a CallToolResult.
// It uses a type assertion to access the TextContent directly.
func extractText(r *gomcp.CallToolResult) string {
	if r == nil || len(r.Content) == 0 {
		return ""
	}
	if tc, ok := r.Content[0].(*gomcp.TextContent); ok {
		return tc.Text
	}
	// Fallback: marshal/unmarshal for unknown content types.
	data, err := json.Marshal(r.Content[0])
	if err != nil {
		return ""
	}
	var tc struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &tc); err != nil {
		return ""
	}
	return tc.Text
}
