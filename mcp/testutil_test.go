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
//
// The handler is wrapped to also serve default responses for the routing-
// related Ollama endpoints (/api/tags, /api/show) so that the Router's
// model registry can resolve test models without each test mock having
// to stub those routes.
func newTestEnv(t *testing.T, mockHandler http.Handler) testEnv {
	t.Helper()

	mock := httptest.NewServer(routingFallbackHandler(mockHandler))

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
		_ = s.Close()
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
		_ = s.Close()
		mock.Close()
		t.Fatalf("newTestEnv: client.Connect() error = %v", cErr)
	}

	return testEnv{
		server:  s,
		session: session,
		cleanup: func() {
			_ = session.Close()
			_ = serverSession.Close()
			_ = s.Close()
			mock.Close()
		},
	}
}

// routingFallbackHandler wraps a test mock so that /api/tags and /api/show
// always return a permissive default response. The Router's model registry
// queries these endpoints to populate its provider/model index; without
// stubs, every Router.Route call against an unstubbed test mock would fail
// with "no providers found for model …".
//
// The fallback responds for any path the inner mock does not handle (i.e.
// the inner handler returns 404). Tests that explicitly stub /api/tags or
// /api/show keep their behavior because the inner mock writes a non-404
// response first.
func routingFallbackHandler(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			rec := newCapturingWriter(w)
			inner.ServeHTTP(rec, r)
			if rec.status == http.StatusNotFound {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"models":[]}`))
				return
			}
			rec.flush()
		case "/api/show":
			rec := newCapturingWriter(w)
			inner.ServeHTTP(rec, r)
			if rec.status == http.StatusNotFound {
				w.Header().Set("Content-Type", "application/json")
				// Advertise all common capabilities so the Router does not
				// reject test models at the capability gate. "completion"
				// implies CapChat | CapGenerate | CapStream per parseCaps.
				_, _ = w.Write([]byte(`{"modelfile":"","parameters":"","template":"","capabilities":["completion","embedding","insert","tools"],"details":{}}`))
				return
			}
			rec.flush()
		default:
			inner.ServeHTTP(w, r)
		}
	})
}

// capturingWriter buffers ResponseWriter calls so we can detect a 404 and
// substitute a default response. If the inner handler did write a body, the
// buffered response is flushed; otherwise the caller writes the fallback.
type capturingWriter struct {
	w       http.ResponseWriter
	header  http.Header
	body    []byte
	status  int
	wrote   bool
	flushed bool
}

func newCapturingWriter(w http.ResponseWriter) *capturingWriter {
	return &capturingWriter{w: w, header: http.Header{}, status: http.StatusOK}
}

func (c *capturingWriter) Header() http.Header { return c.header }
func (c *capturingWriter) WriteHeader(statusCode int) {
	c.status = statusCode
}
func (c *capturingWriter) Write(p []byte) (int, error) {
	c.wrote = true
	c.body = append(c.body, p...)
	return len(p), nil
}
func (c *capturingWriter) flush() {
	if c.flushed {
		return
	}
	c.flushed = true
	for k, vs := range c.header {
		for _, v := range vs {
			c.w.Header().Add(k, v)
		}
	}
	c.w.WriteHeader(c.status)
	if len(c.body) > 0 {
		_, _ = c.w.Write(c.body)
	}
}
