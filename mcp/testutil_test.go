package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testEnv bundles a test MCP server, client session, and cleanup function.
type testEnv struct {
	server  *Server
	session *gomcp.ClientSession
	cleanup func()
}

// newTestEnv creates a testEnv with a mock Ollama HTTP server.
// The mockHandler handles Ollama API requests during the test.
// Call cleanup (or defer env.cleanup()) when done.
func newTestEnv(t *testing.T, mockHandler http.Handler) testEnv {
	t.Helper()

	mock := httptest.NewServer(mockHandler)

	ctx := context.Background()
	s, err := NewServer(ctx,
		WithOllamaURL(mock.URL),
		WithRAGDisabled(),
	)
	if err != nil {
		mock.Close()
		t.Fatalf("newTestEnv: NewServer() error = %v", err)
	}

	// Create in-memory transports for server and client.
	serverTransport, clientTransport := gomcp.NewInMemoryTransports()

	// Connect the MCP server to its transport.
	serverSession, sErr := s.mcpServer.Connect(ctx, serverTransport, nil)
	if sErr != nil {
		s.Close()
		mock.Close()
		t.Fatalf("newTestEnv: server.Connect() error = %v", sErr)
	}

	// Create an MCP client and connect via the paired transport.
	client := gomcp.NewClient(&gomcp.Implementation{
		Name:    "test-client",
		Version: "0.0.1",
	}, nil)

	session, cErr := client.Connect(ctx, clientTransport, nil)
	if cErr != nil {
		s.Close()
		mock.Close()
		t.Fatalf("newTestEnv: client.Connect() error = %v", cErr)
	}

	return testEnv{
		server:  s,
		session: session,
		cleanup: func() {
			session.Close()
			serverSession.Close()
			s.Close()
			mock.Close()
		},
	}
}
