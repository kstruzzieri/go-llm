package mcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/net/http2"
)

type countingReader struct {
	reader io.Reader
	read   int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"0.0.0.0:8080", false},
		{":8080", false},
		{"192.168.1.1:8080", false},
		{"10.0.0.1:443", false},
		{"example.com:80", false},
		// Malformed — not a host:port
		{"just-a-host", false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got := isLoopback(tt.addr)
			if got != tt.want {
				t.Errorf("isLoopback(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestStreamableHTTPHandlerRejectsCrossOrigin(t *testing.T) {
	srv := &Server{
		mcpServer: gomcp.NewServer(&gomcp.Implementation{Name: "test", Version: "0"}, nil),
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "https://malicious.example")

	rec := httptest.NewRecorder()
	streamableHTTPHandler(srv).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestLimitMCPHTTPBodyRejectsKnownOversizeBodyBeforeDownstream(t *testing.T) {
	const limit = int64(4)
	called := false
	handler := limitMCPHTTPBody(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), limit)
	req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", strings.NewReader("12345"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
	if called {
		t.Fatal("downstream handler was called")
	}
}

func TestLimitMCPHTTPBodyCapsUnknownLengthBodyForDownstream(t *testing.T) {
	const limit = int64(4)
	var (
		body []byte
		err  error
	)
	handler := limitMCPHTTPBody(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		body, err = io.ReadAll(req.Body)
	}), limit)
	req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", struct{ io.Reader }{strings.NewReader("12345")})
	if req.ContentLength != -1 {
		t.Fatalf("ContentLength = %d, want unknown", req.ContentLength)
	}
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if string(body) != "1234" {
		t.Fatalf("downstream body = %q, want capped body", body)
	}
	if err == nil {
		t.Fatal("downstream read error = nil, want body-too-large error")
	}
}

func TestLimitMCPH2CBodyCapsUpgradeBeforeH2CBuffersIt(t *testing.T) {
	const limit = int64(4)
	called := false
	handler := limitMCPH2CBody(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), &http2.Server{}, limit)
	body := &countingReader{reader: strings.NewReader(strings.Repeat("x", 100))}
	req := httptest.NewRequest(http.MethodPost, "http://localhost/mcp", struct{ io.Reader }{body})
	req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	req.Header.Set("Upgrade", "h2c")
	req.Header.Set("HTTP2-Settings", "AAMAAABkAAQAAP__")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("downstream handler was called after oversized h2c upgrade")
	}
	if body.read != int(limit)+1 {
		t.Fatalf("h2c upgrade read %d body bytes, want bounded probe of %d", body.read, limit+1)
	}
}

func TestLimitMCPH2CBodyCapsNonPOSTUpgradeBeforeH2CBuffersIt(t *testing.T) {
	const limit = int64(4)
	called := false
	handler := limitMCPH2CBody(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}), &http2.Server{}, limit)
	body := &countingReader{reader: strings.NewReader(strings.Repeat("x", 100))}
	req := httptest.NewRequest(http.MethodGet, "http://localhost/mcp", struct{ io.Reader }{body})
	req.Header.Set("Connection", "Upgrade, HTTP2-Settings")
	req.Header.Set("Upgrade", "h2c")
	req.Header.Set("HTTP2-Settings", "AAMAAABkAAQAAP__")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if called {
		t.Fatal("downstream handler was called after oversized h2c upgrade")
	}
	if body.read != int(limit)+1 {
		t.Fatalf("h2c upgrade read %d body bytes, want bounded probe of %d", body.read, limit+1)
	}
}

func TestLimitMCPHTTPBodyCapsGETRequestBodies(t *testing.T) {
	const limit = int64(1)
	var (
		body string
		err  error
	)
	handler := limitMCPHTTPBody(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		data, readErr := io.ReadAll(req.Body)
		body = string(data)
		err = readErr
	}), limit)
	req := httptest.NewRequest(http.MethodGet, "http://localhost/mcp", struct{ io.Reader }{strings.NewReader("SSE")})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if body != "S" || err == nil {
		t.Fatalf("GET body = %q, error = %v, want capped body and error", body, err)
	}
}

func TestLimitMCPH2CBodyCapsOrdinaryGETRequestBodies(t *testing.T) {
	const limit = int64(1)
	var (
		body string
		err  error
	)
	handler := limitMCPH2CBody(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		data, readErr := io.ReadAll(req.Body)
		body = string(data)
		err = readErr
	}), &http2.Server{}, limit)
	req := httptest.NewRequest(http.MethodGet, "http://localhost/mcp", struct{ io.Reader }{strings.NewReader("SSE")})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if body != "S" || err == nil {
		t.Fatalf("GET body = %q, error = %v, want capped body and error", body, err)
	}
}

func TestMCPHTTPBodyLimitCoversLargestRAGRequest(t *testing.T) {
	const envelopeHeadroom = int64(1 << 20)
	if maxMCPHTTPBodyBytes < int64(maxRAGIngestTextArgumentsBytes)+envelopeHeadroom {
		t.Fatalf("HTTP body limit = %d, want at least %d", maxMCPHTTPBodyBytes, int64(maxRAGIngestTextArgumentsBytes)+envelopeHeadroom)
	}
}
