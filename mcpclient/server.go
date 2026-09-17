package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const httpSessionCloseTimeout = 5 * time.Second

var errHTTPSessionCloseTimeout = errors.New("mcpclient: HTTP session close timed out")

type httpSessionTransport struct {
	http.RoundTripper
}

func (t httpSessionTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodDelete {
		return t.RoundTripper.RoundTrip(req)
	}
	ctx, cancel := context.WithTimeoutCause(req.Context(), httpSessionCloseTimeout, errHTTPSessionCloseTimeout)
	defer cancel()
	resp, err := t.RoundTripper.RoundTrip(req.WithContext(ctx))
	if resp != nil {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		resp.Body = http.NoBody
	}
	if context.Cause(ctx) != errHTTPSessionCloseTimeout {
		return resp, err
	}
	return nil, errHTTPSessionCloseTimeout
}

// Implementation identifies this client to the server. Local mirror of the SDK
// type so cmd/golem never imports go-sdk.
type Implementation struct {
	Name    string
	Version string
}

type transportKind int

const (
	transportStdio transportKind = iota
	transportHTTP
)

// Server is one configured MCP server to attach. Build it via StdioServer or
// HTTPServer; the underlying SDK transport is constructed internally by Connect.
type Server struct {
	Alias    string
	kind     transportKind
	command  []string // stdio
	endpoint string   // http
	// tr, when non-nil, overrides the built transport. Test-only: lets the
	// concurrency tests drive Connect with gated in-memory transports, the same
	// way connectOne lets them drive a single dial.
	tr gomcp.Transport
}

// StdioServer attaches an MCP server run as a subprocess over stdin/stdout.
func StdioServer(alias string, command []string) Server {
	return Server{Alias: alias, kind: transportStdio, command: command}
}

// HTTPServer attaches an MCP server reachable over streamable HTTP.
func HTTPServer(alias, endpoint string) Server {
	return Server{Alias: alias, kind: transportHTTP, endpoint: endpoint}
}

// transport builds the SDK transport. The stdio subprocess is created with
// exec.Command (NOT CommandContext): its lifetime is bound to the session and
// ended by Manager.Close, not by the short-lived Connect context.
func (s Server) transport() (gomcp.Transport, error) {
	if s.tr != nil {
		return s.tr, nil
	}
	switch s.kind {
	case transportStdio:
		if len(s.command) == 0 {
			return nil, fmt.Errorf("mcpclient: stdio server %q has empty command", s.Alias)
		}
		return &gomcp.CommandTransport{Command: exec.Command(s.command[0], s.command[1:]...)}, nil
	case transportHTTP:
		if s.endpoint == "" {
			return nil, fmt.Errorf("mcpclient: http server %q has empty endpoint", s.Alias)
		}
		client := *http.DefaultClient
		if client.Transport == nil {
			client.Transport = http.DefaultTransport
		}
		client.Transport = httpSessionTransport{RoundTripper: client.Transport}
		checkRedirect := client.CheckRedirect
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && via[0].Method == http.MethodDelete {
				return http.ErrUseLastResponse
			}
			if checkRedirect != nil {
				return checkRedirect(req, via)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
		// DisableStandaloneSSE: MVP only needs request/response; no server-initiated
		// notifications, no standalone SSE stream, no auto-reconnect on that stream.
		return &gomcp.StreamableClientTransport{Endpoint: s.endpoint, HTTPClient: &client, DisableStandaloneSSE: true}, nil
	default:
		return nil, fmt.Errorf("mcpclient: server %q has unknown transport", s.Alias)
	}
}
