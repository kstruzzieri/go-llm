package mcpclient

import (
	"encoding/json"
	"errors"
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

func TestStdioPrepareBuildsCommandTransport(t *testing.T) {
	p, err := prepare(StdioServer("fs", []string{"/bin/echo", "hi"}).WithDir(t.TempDir()), "/ws", hostLaunchEnv())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := p.transport.(*gomcp.CommandTransport); !ok {
		t.Fatalf("want *CommandTransport, got %T", p.transport)
	}
}

func TestStdioEmptyCommandInvalid(t *testing.T) {
	if err := validateLaunchPolicy(StdioServer("fs", nil), unixEnvPolicy); err == nil {
		t.Fatal("empty command must error")
	}
}

func TestHTTPPrepareBuildsStreamableTransport(t *testing.T) {
	p, err := prepare(HTTPServer("api", "https://h/mcp"), "/ws", hostLaunchEnv())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	sc, ok := p.transport.(*gomcp.StreamableClientTransport)
	if !ok {
		t.Fatalf("want *StreamableClientTransport, got %T", p.transport)
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

	tr, _ := newHTTPTransport(mustEndpoint(t, "https://mcp.invalid"))
	client := tr.(*gomcp.StreamableClientTransport).HTTPClient
	req, err := http.NewRequest(http.MethodDelete, "https://mcp.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if redirected {
		t.Error("HTTP session DELETE followed a redirect")
	}
	if !errors.Is(err, errRedirectRefused) {
		t.Errorf("HTTP session DELETE error = %v, want errRedirectRefused", err)
	}
	if !deleteBody.closed {
		t.Error("HTTP session DELETE redirect body was not closed")
	}
}

// The go-sdk drops its session DELETE response unclosed (`_, err :=
// c.client.Do(req)`), so the transport itself must close that body.
func TestHTTPSessionDeleteClosesDroppedBody(t *testing.T) {
	deleteBody := &closeTrackingBody{Reader: strings.NewReader("bye")}
	originalClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: mcpRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := mcpHTTPResponse(req, http.StatusOK, "")
		resp.Body = deleteBody
		return resp, nil
	})}
	t.Cleanup(func() { http.DefaultClient = originalClient })

	tr, _ := newHTTPTransport(mustEndpoint(t, "https://mcp.invalid"))
	client := tr.(*gomcp.StreamableClientTransport).HTTPClient
	req, err := http.NewRequest(http.MethodDelete, "https://mcp.invalid/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); err != nil {
		t.Fatal(err)
	}
	if !deleteBody.closed {
		t.Error("HTTP session DELETE body was left open when the response was dropped")
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

func TestHTTPEmptyEndpointInvalid(t *testing.T) {
	if err := validateLaunchPolicy(HTTPServer("api", ""), unixEnvPolicy); err == nil {
		t.Fatal("empty endpoint must error")
	}
}
