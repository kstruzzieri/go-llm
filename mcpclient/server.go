package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	// tools is the host-selected subset of original server tool names.
	// toolsSet distinguishes an explicit empty selection (expose no tools)
	// from an omitted one (expose the whole admitted catalog).
	tools    []string
	toolsSet bool
	// env and dir are stdio launch options: host-approved environment
	// additions and the working directory ("" = process cwd at prepare).
	env []EnvVar
	dir string
	// tr, when non-nil, overrides the built transport. Test-only: lets the
	// concurrency tests drive Connect with gated in-memory transports, the same
	// way connectOne lets them drive a single dial.
	tr gomcp.Transport
}

// StdioServer attaches an MCP server run as a subprocess over stdin/stdout.
// The command slice is copied.
func StdioServer(alias string, command []string) Server {
	return Server{Alias: alias, kind: transportStdio, command: append([]string(nil), command...)}
}

// WithTools returns a copy that exposes only the named original server tools
// (not the mcp__alias__ form). With zero names it exposes no tools; never
// calling it exposes the whole admitted catalog. A later call replaces an
// earlier one, and names is copied. Selection never narrows catalog
// verification: pins, diffs and Inspect always cover every tool.
func (s Server) WithTools(names ...string) Server {
	s.tools = append([]string(nil), names...)
	s.toolsSet = true
	return s
}

// WithEnv returns a copy of a stdio server that forwards these additions on
// top of the baseline environment. A later call replaces an earlier one, and
// vars is copied.
func (s Server) WithEnv(vars ...EnvVar) Server {
	s.env = append([]EnvVar(nil), vars...)
	return s
}

// WithDir returns a copy of a stdio server that runs in dir, which must be
// absolute. Without it the server runs in the process working directory
// captured when Connect, Inspect or Approve prepares it.
func (s Server) WithDir(dir string) Server {
	s.dir = dir
	return s
}

// Format renders only the transport kind and alias, for every verb, so argv,
// endpoints and explicit environment values never reach logs through fmt.
func (s Server) Format(f fmt.State, _ rune) {
	kind := "stdio"
	if s.kind == transportHTTP {
		kind = "http"
	}
	_, _ = fmt.Fprintf(f, "%s:%s", kind, s.Alias)
}

// HTTPServer attaches an MCP server reachable over streamable HTTP.
func HTTPServer(alias, endpoint string) Server {
	return Server{Alias: alias, kind: transportHTTP, endpoint: endpoint}
}
