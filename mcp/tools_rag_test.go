package mcp

import (
	"context"
	"encoding/json"
	"errors"
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

func mcpTestRetriever(t *testing.T, store rag.VectorStore) *rag.Retriever {
	t.Helper()
	retriever, err := rag.NewRetrieverWithEmbedder(rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		return rag.EmbedResult{Embeddings: [][]float64{{1, 0}}}, nil
	}), store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}
	return retriever
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

func TestRAGDeleteToolReturnsJSONIdentity(t *testing.T) {
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
	if text := extractText(result); text != `{"deleted_source":"test.go"}` {
		t.Errorf("result = %q, want JSON identity echo", text)
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
