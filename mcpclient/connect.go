package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/agent"
)

const (
	maxToolsPerServer = 128
	maxListPages      = 100
	// maxSchemaBytes bounds a remote tool's input schema. The schema comes from
	// an untrusted server and is sent to the model in the tool list on EVERY turn
	// (unlike tool results, which the runtime output-caps post-hoc), so an
	// oversized schema is a persistent prompt-bloat / cost vector. A tool whose
	// normalized schema exceeds this is skipped (a clipped JSON schema would be
	// invalid), so the cap is generous: any real tool schema is far smaller.
	maxSchemaBytes = 32 * 1024
	// maxDescBytes bounds a remote tool's description (also untrusted, also sent
	// every turn). Free text, so it is truncated rather than skipped.
	maxDescBytes = 8 * 1024
	// connectTimeout bounds the handshake + tools/list for one server so a stdio
	// command that spawns but never speaks MCP (or a hung HTTP endpoint) cannot
	// block startup forever. It scopes only setup: the SDK keeps the session's
	// background loop on its own context, so cancelling here does not close the
	// live session, which is later driven by per-call dispatch contexts.
	connectTimeout = 30 * time.Second
)

// lister is the minimal slice of *gomcp.ClientSession for paginated tools/list.
type lister interface {
	ListTools(ctx context.Context, params *gomcp.ListToolsParams) (*gomcp.ListToolsResult, error)
}

// listAllTools walks tools/list pagination, guarding against a runaway page
// count and a cursor that makes no progress. Errors and truncation are returned
// as warnings, never fatal.
func listAllTools(ctx context.Context, l lister, alias string) ([]*gomcp.Tool, []error) {
	var (
		out    []*gomcp.Tool
		warns  []error
		cursor string
	)
	for page := 0; ; page++ {
		if page >= maxListPages {
			warns = append(warns, fmt.Errorf("server %q: tools/list exceeded %d pages, truncating", alias, maxListPages))
			break
		}
		res, err := l.ListTools(ctx, &gomcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			warns = append(warns, fmt.Errorf("server %q: tools/list: %w", alias, err))
			break
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		if res.NextCursor == cursor {
			warns = append(warns, fmt.Errorf("server %q: tools/list cursor made no progress, stopping", alias))
			break
		}
		cursor = res.NextCursor
	}
	return out, warns
}

// Manager holds live client sessions and the tools adapted from them.
type Manager struct {
	sessions []*gomcp.ClientSession
	tools    []agent.Tool
}

// Tools returns the namespaced adapter tools in server-then-list order (the
// order each server returned its tools). The slice is a copy; callers may
// append freely.
func (m *Manager) Tools() []agent.Tool {
	out := make([]agent.Tool, len(m.tools))
	copy(out, m.tools)
	return out
}

// Close closes every client session (which terminates stdio subprocesses).
func (m *Manager) Close() error {
	var errs []error
	for _, s := range m.sessions {
		if err := s.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Connect dials each server, lists its tools (paginated), and adapts them.
// Fatal error: invalid or duplicate alias (config error). Everything else -- a
// server that fails to connect/list, a tool skipped for an invalid name/schema,
// a per-server cap truncation -- is a non-fatal warning, so one bad server never
// aborts startup.
func Connect(ctx context.Context, impl Implementation, servers []Server) (*Manager, []error, error) {
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		if !validAlias(s.Alias) {
			return nil, nil, fmt.Errorf("mcpclient: invalid server alias %q", s.Alias)
		}
		if seen[s.Alias] {
			return nil, nil, fmt.Errorf("mcpclient: duplicate server alias %q", s.Alias)
		}
		seen[s.Alias] = true
	}

	m := &Manager{}
	var warnings []error
	for _, s := range servers {
		session, tools, warns := connectOne(ctx, impl, s)
		warnings = append(warnings, warns...)
		if session == nil {
			continue
		}
		m.sessions = append(m.sessions, session)
		m.tools = append(m.tools, tools...)
	}
	return m, warnings, nil
}

func connectOne(ctx context.Context, impl Implementation, s Server) (*gomcp.ClientSession, []agent.Tool, []error) {
	tr, err := s.transport()
	if err != nil {
		return nil, nil, []error{fmt.Errorf("server %q: %w", s.Alias, err)}
	}
	return connectVia(ctx, impl, s.Alias, tr)
}

// connectVia performs the dial + handshake + tools/list + adapt for one server
// over an already-built transport. Split from connectOne so tests can drive it
// with an in-memory transport. Setup is time-bounded (see connectTimeout); the
// returned session outlives the bounded context.
func connectVia(ctx context.Context, impl Implementation, alias string, tr gomcp.Transport) (*gomcp.ClientSession, []agent.Tool, []error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	client := gomcp.NewClient(
		&gomcp.Implementation{Name: impl.Name, Version: impl.Version},
		// Empty Capabilities disables the roots capability (SDK #607 behavior) and
		// advertises no sampling/elicitation -- none are honored here.
		&gomcp.ClientOptions{Capabilities: &gomcp.ClientCapabilities{}},
	)
	session, err := client.Connect(ctx, tr, nil)
	if err != nil {
		return nil, nil, []error{fmt.Errorf("server %q: connect: %w", alias, err)}
	}
	remote, listWarns := listAllTools(ctx, session, alias)
	tools, adaptWarns := adaptTools(session, alias, remote)
	return session, tools, append(listWarns, adaptWarns...)
}

// adaptTools builds adapters for a server's tools, skipping (with a warning) any
// tool whose composed name is invalid or whose schema is not an object, and
// capping per-server tool count to guard the provider tool-count limit.
func adaptTools(caller toolCaller, alias string, remote []*gomcp.Tool) ([]agent.Tool, []error) {
	var (
		out   []agent.Tool
		warns []error
	)
	for _, rt := range remote {
		if len(out) >= maxToolsPerServer {
			warns = append(warns, fmt.Errorf("server %q: more than %d tools, truncating", alias, maxToolsPerServer))
			break
		}
		name, ok := composeName(alias, rt.Name)
		if !ok {
			warns = append(warns, fmt.Errorf("server %q: skipping tool %q (invalid or over-long composed name)", alias, rt.Name))
			continue
		}
		schema, err := normalizeSchema(rt.InputSchema)
		if err != nil {
			warns = append(warns, fmt.Errorf("server %q: skipping tool %q (%v)", alias, rt.Name, err))
			continue
		}
		if len(schema) > maxSchemaBytes {
			warns = append(warns, fmt.Errorf("server %q: skipping tool %q (schema %d bytes exceeds %d cap)", alias, rt.Name, len(schema), maxSchemaBytes))
			continue
		}
		desc := rt.Description
		if len(desc) > maxDescBytes {
			desc = desc[:maxDescBytes] + "...[truncated]"
			warns = append(warns, fmt.Errorf("server %q: tool %q description truncated to %d bytes", alias, rt.Name, maxDescBytes))
		}
		out = append(out, &toolAdapter{
			caller:       caller,
			remoteName:   rt.Name,
			prefixedName: name,
			description:  desc,
			schema:       schema,
			timeout:      defaultToolTimeout,
			outputCap:    defaultToolOutputCap,
		})
	}
	return out, warns
}
