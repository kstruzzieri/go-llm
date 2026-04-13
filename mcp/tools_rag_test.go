package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/rag"
)

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
