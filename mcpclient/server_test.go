package mcpclient

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpRoundTripFunc func(*http.Request) (*http.Response, error)

func (f mcpRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error {
	b.closed = true
	return nil
}

func TestStdioTransport(t *testing.T) {
	tr, err := StdioServer("fs", []string{"echo", "hi"}).transport()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := tr.(*gomcp.CommandTransport); !ok {
		t.Fatalf("want *CommandTransport, got %T", tr)
	}
}

func TestStdioEmptyCommand(t *testing.T) {
	if _, err := StdioServer("fs", nil).transport(); err == nil {
		t.Fatal("empty command must error")
	}
}

func TestHTTPTransport(t *testing.T) {
	tr, err := HTTPServer("api", "https://h/mcp").transport()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	sc, ok := tr.(*gomcp.StreamableClientTransport)
	if !ok {
		t.Fatalf("want *StreamableClientTransport, got %T", tr)
	}
	if !sc.DisableStandaloneSSE {
		t.Fatal("MVP must disable the standalone SSE stream (request/response only)")
	}
	if sc.Endpoint != "https://h/mcp" {
		t.Fatalf("endpoint %q", sc.Endpoint)
	}
}

func TestHTTPRejectedSessionDeleteIsBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		deleteTimeout := make(chan time.Duration, 1)
		postDeadline := make(chan time.Time, 3)
		transport := mcpRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodDelete {
				deadline, ok := req.Context().Deadline()
				remaining := time.Duration(0)
				if ok {
					remaining = time.Until(deadline)
				}
				deleteTimeout <- remaining
				if ok {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return mcpHTTPResponse(req, http.StatusNoContent, ""), nil
			}
			if deadline, ok := req.Context().Deadline(); ok {
				postDeadline <- deadline
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
			}
			body := `{"jsonrpc":"2.0","id":` + string(call.ID) + `,"result":` + result + `}`
			return mcpHTTPResponse(req, http.StatusOK, body), nil
		})
		originalClient := http.DefaultClient
		http.DefaultClient = &http.Client{Transport: transport}
		t.Cleanup(func() { http.DefaultClient = originalClient })

		pins := testPins(t)
		mgr, warnings, err := Connect(t.Context(), Implementation{Name: "test"}, []Server{HTTPServer("fs", "https://mcp.invalid")}, ConnectOptions{Pins: pins, RequirePinned: true})
		if err != nil {
			t.Fatalf("Connect(strict missing pin) error = %v, want nil", err)
		}
		t.Cleanup(func() { _ = mgr.Close() })
		failure := admission(t, warnings)
		if failure.Reason != "pin_missing" {
			t.Errorf("Connect(strict missing pin) reason = %q, want pin_missing", failure.Reason)
		}
		if len(mgr.Tools()) != 0 || pinBytes(t, pins, "fs") != nil {
			t.Errorf("Connect(strict missing pin) = (%d tools, pin %t), want (0, false)", len(mgr.Tools()), pinBytes(t, pins, "fs") != nil)
		}
		if timeout := <-deleteTimeout; timeout <= 0 || timeout > 5*time.Second {
			t.Errorf("Connect(strict missing pin) HTTP DELETE timeout = %s, want (0s, 5s]", timeout)
		}
		for range 3 {
			if remaining := time.Until(<-postDeadline); remaining <= 20*time.Second {
				t.Errorf("Connect setup POST deadline remaining = %s, want more than 20s", remaining)
			}
		}
	})
}

func TestHTTPSessionDeleteDoesNotRedirectAndClosesBody(t *testing.T) {
	deleteBody := &closeTrackingBody{Reader: strings.NewReader("redirect")}
	redirected := false
	originalClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: mcpRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			resp := mcpHTTPResponse(req, http.StatusFound, "")
			resp.Header.Set("Location", "/redirected")
			resp.Body = deleteBody
			return resp, nil
		}
		redirected = true
		return mcpHTTPResponse(req, http.StatusOK, ""), nil
	})}
	t.Cleanup(func() { http.DefaultClient = originalClient })

	tr, err := HTTPServer("fs", "https://mcp.invalid").transport()
	if err != nil {
		t.Fatal(err)
	}
	client := tr.(*gomcp.StreamableClientTransport).HTTPClient
	req, err := http.NewRequest(http.MethodDelete, "https://mcp.invalid", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if redirected {
		t.Error("HTTP session DELETE followed a redirect")
	}
	if resp.StatusCode != http.StatusFound {
		t.Errorf("HTTP session DELETE status = %d, want %d", resp.StatusCode, http.StatusFound)
	}
	if !deleteBody.closed || resp.Body != http.NoBody {
		t.Errorf("HTTP session DELETE body = (closed %t, %T), want (true, http.NoBody)", deleteBody.closed, resp.Body)
	}
}

func mcpHTTPResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func TestHTTPEmptyEndpoint(t *testing.T) {
	if _, err := HTTPServer("api", "").transport(); err == nil {
		t.Fatal("empty endpoint must error")
	}
}
