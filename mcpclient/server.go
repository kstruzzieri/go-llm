package mcpclient

import (
	"fmt"
	"os/exec"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	// way the connectVia split lets them drive a single dial.
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
		// DisableStandaloneSSE: MVP only needs request/response; no server-initiated
		// notifications, no standalone SSE stream, no auto-reconnect on that stream.
		return &gomcp.StreamableClientTransport{Endpoint: s.endpoint, DisableStandaloneSSE: true}, nil
	default:
		return nil, fmt.Errorf("mcpclient: server %q has unknown transport", s.Alias)
	}
}
