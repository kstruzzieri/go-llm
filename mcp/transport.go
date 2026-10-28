package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const maxMCPHTTPBodyBytes int64 = 100 << 20

// ListenStdio starts the MCP server on stdin/stdout.
// It blocks until ctx is cancelled or an error occurs.
func (s *Server) ListenStdio(ctx context.Context) error {
	// Stdio is a trusted local single-client transport; remote HTTP is body-limited below.
	return s.mcpServer.Run(ctx, &gomcp.StdioTransport{})
}

// ListenHTTP starts the MCP server over HTTP/2.
// If TLS was configured via WithTLS, it uses full HTTP/2 with TLS.
// Otherwise it uses h2c (cleartext HTTP/2) and requires addr to be a
// loopback address.
//
// Without WithHTTPBearerToken the HTTP transport is unauthenticated. With it,
// every request that reaches the MCP handler, on either transport and
// including "OPTIONS *", must present the bearer token. TLS encrypts traffic
// but does not authenticate the caller. On h2c, the 101 Switching Protocols
// reply to an HTTP/1.1 upgrade is sent before the handler runs; the upgraded
// request and every later stream are then checked.
func (s *Server) ListenHTTP(ctx context.Context, addr string) error {
	if s.tlsCert == "" && !isLoopback(addr) {
		return fmt.Errorf("mcp: non-loopback address %q requires TLS (use WithTLS)", addr)
	}

	httpServer := s.newHTTPServer(addr, s.tlsCert != "")
	s.mu.Lock()
	s.httpServer = httpServer
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	var err error
	if s.tlsCert != "" {
		err = httpServer.ListenAndServeTLS(s.tlsCert, s.tlsKey)
	} else {
		err = httpServer.ListenAndServe()
	}
	// ErrServerClosed is the expected result of graceful shutdown, not an error.
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// newHTTPServer builds the http.Server ListenHTTP serves. Under TLS the MCP
// handler is served directly; on h2c it is wrapped in h2c and the outer body
// limiter. With a bearer token configured, Go's built-in "OPTIONS *" reply
// is disabled so that request reaches the gate as well.
func (s *Server) newHTTPServer(addr string, useTLS bool) *http.Server {
	handler := streamableHTTPHandler(s)
	if !useTLS {
		// h2c: HTTP/2 over cleartext (no TLS)
		handler = limitMCPH2CBody(handler, &http2.Server{}, maxMCPHTTPBodyBytes)
	}
	return &http.Server{
		Addr:                         addr,
		Handler:                      handler,
		DisableGeneralOptionsHandler: s.httpBearerDigest != nil,
	}
}

func streamableHTTPHandler(s *Server) http.Handler {
	handler := gomcp.NewStreamableHTTPHandler(
		func(r *http.Request) *gomcp.Server { return s.mcpServer },
		&gomcp.StreamableHTTPOptions{},
	)
	// The SDK's CrossOriginProtection option is deprecated in favour of
	// wrapping the handler; the protection itself is unchanged.
	protected := http.NewCrossOriginProtection().Handler(handler)
	if s.httpBearerDigest != nil {
		// The gate sits outside cross-origin protection and inside the body
		// limiter, so an oversized body keeps its own status.
		protected = requireHTTPBearer(protected, s.httpBearerDigest)
	}
	return limitMCPHTTPBody(protected, maxMCPHTTPBodyBytes)
}

func limitMCPH2CBody(next http.Handler, server *http2.Server, maxBytes int64) http.Handler {
	// h2c.NewHandler buffers an HTTP/1.1 upgrade body before invoking its child,
	// so the limiter must wrap h2c rather than sit only inside it.
	return limitMCPHTTPBody(h2c.NewHandler(next, server), maxBytes)
}

func limitMCPHTTPBody(next http.Handler, maxBytes int64) http.Handler {
	bounded := http.MaxBytesHandler(next, maxBytes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxBytes {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		bounded.ServeHTTP(w, r)
	})
}

// isLoopback reports whether the host part of addr is a loopback address.
// An empty host (e.g. ":8080") is NOT considered loopback because Go's
// net/http binds to 0.0.0.0 (all interfaces) in that case.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
