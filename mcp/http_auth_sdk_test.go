package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// refusalTimeout bounds a request the gate must answer 401 at once.
const refusalTimeout = 5 * time.Second

// bearerRoundTripper adds "Authorization: Bearer <token>" to every request, the
// way an SDK client configured with a token would. A nil next uses
// http.DefaultTransport.
type bearerRoundTripper struct {
	token string
	next  http.RoundTripper
}

func (b bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	next := b.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(req)
}

// withBearer returns a copy of c whose requests all carry token as a bearer.
func withBearer(c *http.Client, token string) *http.Client {
	return &http.Client{Timeout: c.Timeout, Transport: bearerRoundTripper{token: token, next: c.Transport}}
}

// jsonRPC returns a JSON-RPC request for method with the smallest params the
// SDK accepts: initialize's, an echo-extra call for tools/call, and none
// otherwise.
func jsonRPC(method string, id int) string {
	params := `{}`
	switch method {
	case "initialize":
		params = `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}`
	case "tools/call":
		params = `{"name":"echo-extra","arguments":{}}`
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
}

// rpcRequest builds a request to url with the headers the SDK's streamable
// handler requires for method: mcpHeaders on a POST, an event-stream Accept
// on a GET. Authorization and Mcp-Session-Id are set only when non-empty.
func rpcRequest(t *testing.T, method, url, body, auth, session string) *http.Request {
	t.Helper()

	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("NewRequest(%s %s) error = %v", method, url, err)
	}
	switch method {
	case http.MethodPost:
		mcpHeaders(req.Header)
	case http.MethodGet:
		req.Header.Set("Accept", "text/event-stream")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	return req
}

// sendAndRead sends req through c and returns the response with its whole body
// read and closed. c's timeout bounds the read.
func sendAndRead(t *testing.T, c *http.Client, req *http.Request) (*http.Response, string) {
	t.Helper()

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s error = %v", req.Method, req.URL.Path, err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("%s %s: reading response error = %v", req.Method, req.URL.Path, err)
	}
	return resp, string(body)
}

// initializeSession initializes a session over c with the listener token and
// returns its Mcp-Session-Id, which it requires to be non-empty.
func initializeSession(t *testing.T, c *http.Client, url string) string {
	t.Helper()

	resp, body := sendAndRead(t, c, rpcRequest(t, http.MethodPost, url, jsonRPC("initialize", 1), "Bearer "+listenerToken, ""))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize with bearer: status = %d, want %d; body %q", resp.StatusCode, http.StatusOK, body)
	}
	id := resp.Header.Get("Mcp-Session-Id")
	if id == "" {
		t.Fatal("initialize with bearer: Mcp-Session-Id is empty")
	}
	return id
}

// toolsListResponse is the part of a tools/list JSON-RPC response the tests
// check.
type toolsListResponse struct {
	Error  json.RawMessage `json:"error"`
	Result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	} `json:"result"`
}

// decodeToolsList decodes a tools/list response: plain JSON, or the first
// non-empty "data:" line of an SSE event when contentType is an event stream.
func decodeToolsList(t *testing.T, contentType, body string) toolsListResponse {
	t.Helper()

	payload := body
	if strings.HasPrefix(contentType, "text/event-stream") {
		payload = ""
		for _, line := range strings.Split(body, "\n") {
			if data, ok := strings.CutPrefix(line, "data:"); ok && strings.TrimSpace(data) != "" {
				payload = strings.TrimSpace(data)
				break
			}
		}
		if payload == "" {
			t.Fatalf("event stream has no data line: %q", body)
		}
	}
	var out toolsListResponse
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("decoding tools/list response error = %v; payload %q", err, payload)
	}
	return out
}

func TestSDKClientWithBearer(t *testing.T) {
	for _, lc := range listenerCases {
		t.Run(lc.name, func(t *testing.T) {
			ts := startMCPListener(t, newTestMCPServer(t, listenerToken), lc.useTLS)
			hc := withBearer(lc.client(t, ts), listenerToken)

			ctx, cancel := context.WithTimeout(t.Context(), listenerTimeout)
			defer cancel()
			client := gomcp.NewClient(&gomcp.Implementation{Name: "test-client", Version: "0"}, nil)
			session, err := client.Connect(ctx, &gomcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: hc}, nil)
			if err != nil {
				t.Fatalf("Connect() error = %v", err)
			}
			defer func() { _ = session.Close() }()

			tools, err := session.ListTools(ctx, nil)
			if err != nil {
				t.Fatalf("ListTools() error = %v", err)
			}
			if !slices.ContainsFunc(tools.Tools, func(tool *gomcp.Tool) bool { return tool.Name == "echo-extra" }) {
				t.Errorf("ListTools() = %d tools, none named echo-extra", len(tools.Tools))
			}

			res, err := session.CallTool(ctx, &gomcp.CallToolParams{Name: "echo-extra", Arguments: map[string]any{}})
			if err != nil {
				t.Fatalf("CallTool() error = %v", err)
			}
			if len(res.Content) != 1 {
				t.Fatalf("CallTool() content = %d items, want 1", len(res.Content))
			}
			text, ok := res.Content[0].(*gomcp.TextContent)
			if !ok {
				t.Fatalf("CallTool() content = %T, want *gomcp.TextContent", res.Content[0])
			}
			// The tool saw neither the credential nor an identity derived
			// from it.
			if want := `authorization="" tokeninfo=false`; text.Text != want {
				t.Errorf("echo-extra reported %q, want %q", text.Text, want)
			}
		})
	}
}

func TestSDKRequestsWithoutBearerRejected(t *testing.T) {
	methods := []string{"initialize", "tools/list", "resources/list", "prompts/list", "tools/call"}

	for _, lc := range listenerCases {
		t.Run(lc.name, func(t *testing.T) {
			srv := newTestMCPServer(t, listenerToken)
			dispatched := countMCPRequests(srv)
			ts := startMCPListener(t, srv, lc.useTLS)
			c := lc.client(t, ts)
			url := ts.URL + "/mcp"

			// A live session ID makes the unauthenticated requests below
			// reachable: without the gate they would run, tools/call
			// included, instead of failing on an uninitialized session.
			session := initializeSession(t, c, url)
			baseline := dispatched.Load()

			for _, withSession := range []bool{false, true} {
				for i, method := range methods {
					id := ""
					if withSession {
						id = session
					}
					name := fmt.Sprintf("%s/session=%t", method, withSession)
					resp, body := sendAndRead(t, c, rpcRequest(t, http.MethodPost, url, jsonRPC(method, i+2), "", id))

					if resp.StatusCode != http.StatusUnauthorized {
						t.Errorf("%s: status = %d, want %d; body %q", name, resp.StatusCode, http.StatusUnauthorized, body)
					}
					if body != "unauthorized\n" {
						t.Errorf("%s: body = %q, want %q", name, body, "unauthorized\n")
					}
					text := body
					for k, vs := range resp.Header {
						text += "\n" + k + ": " + strings.Join(vs, "\n")
					}
					assertNoBearerEcho(t, text, listenerToken)
				}
			}
			if got := dispatched.Load(); got != baseline {
				t.Errorf("SDK dispatched %d requests for unauthenticated calls, want 0", got-baseline)
			}

			// The SDK client without a token cannot even connect.
			ctx, cancel := context.WithTimeout(t.Context(), listenerTimeout)
			defer cancel()
			client := gomcp.NewClient(&gomcp.Implementation{Name: "test-client", Version: "0"}, nil)
			if s, err := client.Connect(ctx, &gomcp.StreamableClientTransport{Endpoint: url, HTTPClient: c}, nil); err == nil {
				_ = s.Close()
				t.Error("Connect() without a bearer error = nil, want a failure")
			}
		})
	}
}

func TestBearerRequiredOnSessionReuse(t *testing.T) {
	for _, lc := range listenerCases {
		t.Run(lc.name, func(t *testing.T) {
			ts := startMCPListener(t, newTestMCPServer(t, listenerToken), lc.useTLS)
			c := lc.client(t, ts)
			url := ts.URL + "/mcp"
			bearer := "Bearer " + listenerToken
			session := initializeSession(t, c, url)

			for _, tt := range []struct {
				method, body string
			}{
				{http.MethodPost, jsonRPC("tools/list", 2)},
				{http.MethodGet, ""},
				{http.MethodDelete, ""},
			} {
				// A refusal is immediate. Without the gate the GET would open
				// a stream that never ends, so bound it well below
				// listenerTimeout.
				ctx, cancel := context.WithTimeout(t.Context(), refusalTimeout)
				req := rpcRequest(t, tt.method, url, tt.body, "", session).WithContext(ctx)
				resp, body := sendAndRead(t, c, req)
				cancel()
				if resp.StatusCode != http.StatusUnauthorized {
					t.Errorf("%s with session, no bearer: status = %d, want %d; body %q", tt.method, resp.StatusCode, http.StatusUnauthorized, body)
				}
			}

			// A 200 is not enough: the SDK reports a JSON-RPC error inside
			// one (go-sdk mcp/server.go handle), so decode the result.
			resp, body := sendAndRead(t, c, rpcRequest(t, http.MethodPost, url, jsonRPC("tools/list", 3), bearer, session))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("tools/list with bearer and session: status = %d, want %d; body %q", resp.StatusCode, http.StatusOK, body)
			}
			list := decodeToolsList(t, resp.Header.Get("Content-Type"), body)
			if len(list.Error) != 0 {
				t.Errorf("tools/list returned a JSON-RPC error: %s", list.Error)
			}
			var names []string
			for _, tool := range list.Result.Tools {
				names = append(names, tool.Name)
			}
			if !slices.Contains(names, "echo-extra") {
				t.Errorf("tools/list result tools = %v, want echo-extra among them", names)
			}

			resp, body = sendAndRead(t, c, rpcRequest(t, http.MethodDelete, url, "", bearer, session))
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				t.Errorf("DELETE with bearer and session: status = %d, want 2xx; body %q", resp.StatusCode, body)
			}
		})
	}
}
