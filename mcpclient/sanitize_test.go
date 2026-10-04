package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCallFailureAllowlist(t *testing.T) {
	leak := &url.Error{Op: "Post", URL: "https://h/mcp?token=canary", Err: errors.New("dial canary")}
	rejected := &jsonrpc.Error{Code: -32005, Message: "rejected by transport"}
	// A non-lifecycle code, so only the case order keeps its message out.
	server := &jsonrpc.Error{Code: -32603, Message: "canary from server"}
	for _, tt := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrap: %w", errRedirectRefused), "mcp call failed: redirect refused"},
		{fmt.Errorf("%w: %w", rejected, &url.Error{Op: "Post", URL: "https://h/?token=canary", Err: errRedirectRefused}), "mcp call failed: redirect refused"},
		{errDestinationRefused, "mcp call failed: destination refused"},
		{context.Canceled, "mcp call failed: canceled"},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), "mcp call failed: timed out"},
		{fmt.Errorf("%w: %w", server, errRedirectRefused), "mcp call failed: redirect refused"},
		{fmt.Errorf("%w: %w", server, context.DeadlineExceeded), "mcp call failed: timed out"},
		// Chains can mix sentinels (go-sdk's call returns errors.Join(ctx.Err(), err)).
		{errors.Join(context.Canceled, errDestinationRefused, errRedirectRefused), "mcp call failed: redirect refused"},
		{errors.Join(context.DeadlineExceeded, context.Canceled, errDestinationRefused), "mcp call failed: destination refused"},
		{errors.Join(context.DeadlineExceeded, context.Canceled), "mcp call failed: canceled"},
		{fmt.Errorf("call: %w", &jsonrpc.Error{Code: -32602, Message: "invalid params: path"}), "mcp call failed: invalid params: path"},
		{fmt.Errorf("%w: %w", rejected, leak), "mcp call failed: transport error"},
		{&jsonrpc.Error{Code: -32001, Message: "canary from server"}, "mcp call failed: transport error"},
		{&jsonrpc.Error{Code: -32003, Message: "canary from server"}, "mcp call failed: transport error"},
		{&jsonrpc.Error{Code: -32004, Message: "canary from server"}, "mcp call failed: transport error"},
		{errors.New("https://h/mcp?token=canary"), "mcp call failed: transport error"},
	} {
		if got := callFailure(tt.err); got != tt.want {
			t.Errorf("callFailure(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestServerJSONRPCErrorsShowOnlyTheirMessage(t *testing.T) {
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "refuser"}, nil)
	for name, rpcErr := range map[string]*jsonrpc.Error{
		"deny":   {Code: -32602, Message: "server says no"},
		"reject": {Code: -32005, Message: "canary-rejected"},
	} {
		srv.AddTool(&gomcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			return nil, rpcErr
		})
	}
	httpServer := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil))
	t.Cleanup(httpServer.Close)
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{HTTPServer("fs", httpServer.URL+"/mcp?token=canary")}, ConnectOptions{Pins: testPins(t)})
	if err != nil || len(m.Tools()) != 2 {
		t.Fatalf("connect = (%v, %v)", err, w)
	}
	t.Cleanup(func() { _ = m.Close() })
	got := map[string]string{}
	for _, tool := range m.Tools() {
		res, err := tool.Invoke(context.Background(), json.RawMessage(`{}`))
		if err != nil || !res.IsError {
			t.Fatalf("%s = (%+v, %v), want an error result", tool.Spec().Name, res, err)
		}
		got[tool.Spec().Name] = res.Content
	}
	want := map[string]string{
		"mcp__fs__deny":   "mcp call failed: server says no",
		"mcp__fs__reject": "mcp call failed: transport error",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("server errors = %q, want %q", got, want)
	}
}

func TestManagerCloseRedactsEndpoint(t *testing.T) {
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "closer"}, nil)
	srv.AddTool(&gomcp.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{}, nil
	})
	httpServer := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil))
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{HTTPServer("fs", httpServer.URL+"/mcp?token=canary")}, ConnectOptions{Pins: testPins(t)})
	if err != nil || len(m.Tools()) != 1 {
		t.Fatalf("connect = (%v, %v)", err, w)
	}
	httpServer.Close() // the session DELETE now fails with a *url.Error naming the endpoint
	err = m.Close()
	if err == nil || err.Error() != "mcpclient: closing MCP sessions failed" || strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), "127.0.0.1") || !errors.As(err, new(*url.Error)) {
		t.Fatalf("close = %v, want the fixed text with the *url.Error still reachable", err)
	}
}

func TestConnectRefusesRedirectedInitialize(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	pins := testPins(t)
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{HTTPServer("fs", origin.URL+"/mcp?token=canary")}, ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	failure := admission(t, w)
	if failure.Reason != "redirect_refused" || targetHits.Load() != 0 || pinBytes(t, pins, "fs") != nil || strings.Contains(failure.Error(), "canary") {
		t.Fatalf("redirected initialize = (%q, %d target hits), want redirect_refused and no contact", failure.Reason, targetHits.Load())
	}
}

func TestToolCallAndCloseRedirectsRefused(t *testing.T) {
	var targetHits, redirectDeletes atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	t.Cleanup(target.Close)
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "redirector"}, nil)
	srv.AddTool(&gomcp.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{}, nil
	})
	handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		if strings.Contains(string(body), `"tools/call"`) || r.Method == http.MethodDelete {
			if r.Method == http.MethodDelete {
				redirectDeletes.Add(1)
			}
			http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(origin.Close)
	m, w, err := Connect(context.Background(), Implementation{Name: "test"}, []Server{HTTPServer("fs", origin.URL+"/mcp?token=canary")}, ConnectOptions{Pins: testPins(t)})
	if err != nil || len(m.Tools()) != 1 {
		t.Fatalf("connect = (%v, %v)", err, w)
	}
	res, err := m.Tools()[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil || !res.IsError || res.Content != "mcp call failed: redirect refused" {
		t.Fatalf("redirected call = (%+v, %v), want the fixed redirect text", res, err)
	}
	_ = m.Close()
	if targetHits.Load() != 0 || redirectDeletes.Load() == 0 {
		t.Fatalf("target hits = %d (want 0); redirected DELETEs = %d (want >= 1)", targetHits.Load(), redirectDeletes.Load())
	}
}

// TestInspectApproveSurviveRedirectedClose: Connect admits a server whose
// session-close DELETE answers 3xx, so Inspect and Approve must not fail on
// that refusal either, or the alias could never be re-approved. The redirect
// is still refused: its target is never contacted.
func TestInspectApproveSurviveRedirectedClose(t *testing.T) {
	var targetHits, redirectDeletes atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	t.Cleanup(target.Close)
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "redirector"}, nil)
	srv.AddTool(&gomcp.Tool{Name: "read", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{}, nil
	})
	handler := gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			redirectDeletes.Add(1)
			http.Redirect(w, r, target.URL+"/mcp", http.StatusTemporaryRedirect)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(origin.Close)
	pins := testPins(t)
	server := HTTPServer("fs", origin.URL+"/mcp")
	inspected, err := Inspect(context.Background(), Implementation{Name: "test"}, server, pins)
	if err != nil {
		t.Fatalf("inspect = %v, want success despite the redirected close", err)
	}
	if got := fmt.Sprint(inspected.Diff.Added); got != "[mcp__fs__read]" || pinBytes(t, pins, "fs") != nil {
		t.Fatalf("inspect catalog added = %s, want [mcp__fs__read] and no pin", got)
	}
	if _, err := Approve(context.Background(), Implementation{Name: "test"}, server, pins, approvalFor(inspected)); err != nil {
		t.Fatalf("approve = %v, want success despite the redirected close", err)
	}
	if pin := pinBytes(t, pins, "fs"); !strings.Contains(string(pin), `"version":2`) {
		t.Fatalf("approve wrote %q, want a version 2 pin", pin)
	}
	if targetHits.Load() != 0 || redirectDeletes.Load() < 2 {
		t.Fatalf("target hits = %d (want 0); redirected DELETEs = %d (want >= 2)", targetHits.Load(), redirectDeletes.Load())
	}
}

func TestSSEResumeRedirectIsRefusedAndSanitized(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var elsewhere, resumes atomic.Int32
		transport := mcpRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/mcp" {
				elsewhere.Add(1)
				return mcpHTTPResponse(req, http.StatusOK, ""), nil
			}
			switch req.Method {
			case http.MethodDelete:
				return mcpHTTPResponse(req, http.StatusNoContent, ""), nil
			case http.MethodGet: // the SDK's Last-Event-ID resume
				if req.Header.Get("Last-Event-ID") == "1" {
					resumes.Add(1)
				}
				resp := mcpHTTPResponse(req, http.StatusTemporaryRedirect, "")
				resp.Header.Set("Location", "/elsewhere")
				return resp, nil
			}
			defer func() { _ = req.Body.Close() }()
			var call struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
				return nil, err
			}
			if len(call.ID) == 0 {
				return mcpHTTPResponse(req, http.StatusAccepted, ""), nil
			}
			result := `{}`
			switch call.Method {
			case "initialize":
				result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"test","version":"1"}}`
			case "tools/list":
				result = `{"tools":[{"name":"read","inputSchema":{"type":"object"}}]}`
			case "tools/call":
				// A priming event with an id and no response: the SDK resumes
				// with a GET carrying Last-Event-ID.
				resp := mcpHTTPResponse(req, http.StatusOK, "id: 1\n\n")
				resp.Header.Set("Content-Type", "text/event-stream")
				return resp, nil
			}
			return mcpHTTPResponse(req, http.StatusOK, `{"jsonrpc":"2.0","id":`+string(call.ID)+`,"result":`+result+`}`), nil
		})
		originalClient := http.DefaultClient
		http.DefaultClient = &http.Client{Transport: transport}
		t.Cleanup(func() { http.DefaultClient = originalClient })
		m, w, err := Connect(t.Context(), Implementation{Name: "test"}, []Server{HTTPServer("fs", "https://mcp.invalid/mcp?token=canary")}, ConnectOptions{Pins: testPins(t)})
		if err != nil || len(m.Tools()) != 1 {
			t.Fatalf("connect = (%v, %v)", err, w)
		}
		t.Cleanup(func() { _ = m.Close() })
		res, err := m.Tools()[0].Invoke(t.Context(), json.RawMessage(`{}`))
		// The SDK flattens this path's cause with %v (spec F9), so the specific
		// refusal is unrecoverable; the fixed text is the pinned behavior.
		if err != nil || !res.IsError || res.Content != "mcp call failed: transport error" || elsewhere.Load() != 0 {
			t.Fatalf("resume = (%+v, %v, %d elsewhere), want the fixed transport text and no redirect contact", res, err, elsewhere.Load())
		}
		// Without this, a stream that never resumes would pass the same checks.
		if resumes.Load() == 0 {
			t.Fatal("the SDK never sent a Last-Event-ID: 1 resume; the fixture did not exercise resumption")
		}
	})
}
