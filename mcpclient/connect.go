package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/promptfence"
)

const truncatedDescriptionSuffix = "...[truncated]"

func normalizeDescription(description string) (string, bool) {
	description = strings.ToValidUTF8(description, "\ufffd")
	description = promptfence.FlattenLine(description)
	description = strings.ReplaceAll(description, "<<<", " ")
	description = strings.ReplaceAll(description, ">>>", " ")
	if len(description) <= maxDescBytes {
		return description, false
	}
	cut := maxDescBytes - len(truncatedDescriptionSuffix)
	for !utf8.RuneStart(description[cut]) {
		cut--
	}
	return description[:cut] + truncatedDescriptionSuffix, true
}

const (
	maxToolsPerServer = 128
	maxListPages      = 100
	// maxSchemaBytes bounds a remote tool's input schema. The schema comes from
	// an untrusted server and is sent to the model in the tool list on EVERY turn
	// (unlike tool results, which the runtime output-caps post-hoc), so an
	// oversized schema is a persistent prompt-bloat / cost vector. A tool whose
	// normalized schema exceeds this rejects its catalog (a clipped schema is
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
	// maxConcurrentConnects bounds concurrent dials in Connect. Dials fork/exec
	// a subprocess per stdio server (unlike tool calls, which ride established
	// connections at parallelToolCallLimit=8), so the bound also caps the
	// startup process-spawn burst.
	maxConcurrentConnects = 4
)

// lister is the minimal slice of *gomcp.ClientSession for paginated tools/list.
type lister interface {
	ListTools(ctx context.Context, params *gomcp.ListToolsParams) (*gomcp.ListToolsResult, error)
}

// listAllTools accepts only a complete, bounded listing.
func listAllTools(ctx context.Context, l lister, alias string) ([]*gomcp.Tool, []error) {
	var out []*gomcp.Tool
	cursor := ""
	seen := make(map[string]bool)
	reject := func(err error) ([]*gomcp.Tool, []error) {
		return nil, []error{admissionFailure(alias, "incomplete_catalog", err)}
	}
	for page := 0; page < maxListPages; page++ {
		if err := ctx.Err(); err != nil {
			return reject(err)
		}
		res, err := l.ListTools(ctx, &gomcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return reject(err)
		}
		if res == nil {
			return reject(errors.New("nil tools/list result"))
		}
		if len(out)+len(res.Tools) > maxToolsPerServer {
			return reject(errors.New("tool count exceeds limit"))
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		if seen[res.NextCursor] {
			return reject(errors.New("repeated tools/list cursor"))
		}
		seen[res.NextCursor] = true
		cursor = res.NextCursor
	}
	return reject(errors.New("tools/list exceeded page limit"))
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

// ConnectOptions selects the workspace trust store and first-contact policy.
// Pins is required; RequirePinned forbids automatic first pin creation.
type ConnectOptions struct {
	Pins          *PinStore
	RequirePinned bool
}

// Connect admits complete catalogs before publishing tools. Per-alias failures
// are *AdmissionError values in warnings; configuration errors are fatal.
// Sessions, tools and notices retain config order despite bounded parallel dials.
func Connect(ctx context.Context, impl Implementation, servers []Server, opts ConnectOptions) (*Manager, []error, error) {
	return connectWithHooks(ctx, impl, servers, opts, nil)
}

// connectHooks carries test-only observation callbacks; nil in production.
// launched fires on the caller's goroutine immediately before a server's dial
// is dispatched; published fires after that server's result is recorded.
type connectHooks struct {
	launched  func(i int)
	published func(i int)
}

func connectWithHooks(ctx context.Context, impl Implementation, servers []Server, opts ConnectOptions, h *connectHooks) (*Manager, []error, error) {
	if err := validateTrustConfig(servers, opts.Pins); err != nil {
		return nil, nil, err
	}

	// Dial concurrently (bounded), but keep aggregate state deterministic:
	// each worker writes only its own results slot, and g.Wait() supplies the
	// happens-before edge, so the serial aggregation below reproduces the
	// serial implementation's sessions/tools/warnings byte-for-byte for the
	// same per-server outcomes regardless of completion order.
	results := make([]connectResult, len(servers))
	var g errgroup.Group
	g.SetLimit(maxConcurrentConnects)
	for i, s := range servers {
		if h != nil && h.launched != nil {
			h.launched(i)
		}
		g.Go(func() error {
			session, tools, warns := connectOne(ctx, impl, s, opts)
			results[i] = connectResult{session: session, tools: tools, warns: warns}
			if h != nil && h.published != nil {
				h.published(i)
			}
			return nil // failures are per-server warnings, never group errors
		})
	}
	_ = g.Wait()

	m := &Manager{}
	var warnings []error
	for i, r := range results {
		if r.session != nil {
			if err := ctx.Err(); err != nil {
				warnings = append(warnings, admissionFailure(servers[i].Alias, "canceled", errors.Join(err, r.session.Close())))
				continue
			}
		}
		warnings = append(warnings, r.warns...)
		if r.session == nil {
			continue
		}
		m.sessions = append(m.sessions, r.session)
		m.tools = append(m.tools, r.tools...)
	}
	return m, warnings, nil
}

// connectResult is one server's dial outcome, slotted by config index.
type connectResult struct {
	session *gomcp.ClientSession
	tools   []agent.Tool
	warns   []error
}

func connectOne(ctx context.Context, impl Implementation, s Server, opts ConnectOptions) (*gomcp.ClientSession, []agent.Tool, []error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	session, remote, catalog, notices, err := discover(ctx, impl, s)
	if err != nil {
		return nil, nil, []error{err}
	}
	prior, created, err := opts.Pins.admit(ctx, s.Alias, catalog, opts.RequirePinned)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		closeErr := session.Close()
		failure := admissionFailure(s.Alias, "pin_unavailable", errors.Join(err, closeErr))
		failure.PinnedDigest, failure.CandidateDigest = prior.digest(), catalog.digest()
		failure.Diff = diffCatalogs(prior, catalog)
		return nil, nil, []error{failure}
	}
	if created {
		notices = append(notices, fmt.Errorf("server %q: first pin %s; tools: %s; use explicit alias= values for stable pins", s.Alias, catalog.digest(), strings.Join(catalogNames(catalog), ", ")))
	}
	return session, adapters(session, s.Alias, remote, catalog), notices
}

// discover owns the candidate session until a complete catalog is returned.
// The caller closes successful candidates or transfers ownership to Manager.
func discover(ctx context.Context, impl Implementation, s Server) (*gomcp.ClientSession, []*gomcp.Tool, toolCatalog, []error, error) {
	tr, err := s.transport()
	if err != nil {
		return nil, nil, toolCatalog{}, nil, admissionFailure(s.Alias, "unavailable", err)
	}
	// Empty capabilities disables roots, sampling and elicitation.
	client := gomcp.NewClient(&gomcp.Implementation{Name: impl.Name, Version: impl.Version}, &gomcp.ClientOptions{Capabilities: &gomcp.ClientCapabilities{}})
	session, err := client.Connect(ctx, tr, nil)
	if err != nil {
		return nil, nil, toolCatalog{}, nil, admissionFailure(s.Alias, "unavailable", err)
	}
	remote, failures := listAllTools(ctx, session, s.Alias)
	var catalog toolCatalog
	var notices []error
	if len(failures) > 0 {
		err = failures[0]
	} else {
		catalog, notices, err = validateCatalog(s.Alias, remote)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, nil, toolCatalog{}, nil, admissionFailure(s.Alias, "invalid_catalog", errors.Join(err, session.Close()))
	}
	return session, remote, catalog, notices, nil
}

func validateCatalog(alias string, remote []*gomcp.Tool) (toolCatalog, []error, error) {
	reject := func() (toolCatalog, []error, error) {
		return toolCatalog{}, nil, admissionFailure(alias, "invalid_catalog", nil)
	}
	if !validAlias(alias) || len(remote) > maxToolsPerServer {
		return reject()
	}
	entries := make([]catalogEntry, 0, len(remote))
	seen := make(map[string]bool, len(remote))
	var notices []error
	for _, rt := range remote {
		if rt == nil {
			return reject()
		}
		name, ok := composeName(alias, rt.Name)
		if !ok || seen[name] {
			return reject()
		}
		schema, err := normalizeSchema(rt.InputSchema)
		if err != nil || len(schema) > maxSchemaBytes {
			return reject()
		}
		desc, truncated := normalizeDescription(rt.Description)
		if truncated {
			notices = append(notices, fmt.Errorf("server %q: tool %q description truncated to %d bytes", alias, rt.Name, len(desc)))
		}
		entries = append(entries, catalogEntry{Name: name, Description: desc, InputSchema: schema})
		seen[name] = true
	}
	catalog, err := newToolCatalog(entries)
	if err != nil {
		return reject()
	}
	return catalog, notices, nil
}

func adapters(caller toolCaller, alias string, remote []*gomcp.Tool, catalog toolCatalog) []agent.Tool {
	entries := make(map[string]catalogEntry, len(catalog.entries))
	for _, entry := range catalog.entries {
		entries[entry.Name] = entry
	}
	out := make([]agent.Tool, 0, len(remote))
	for _, rt := range remote {
		name, _ := composeName(alias, rt.Name)
		entry := entries[name]
		out = append(out, &toolAdapter{caller: caller, remoteName: rt.Name, prefixedName: name, description: entry.Description, schema: entry.InputSchema, timeout: defaultToolTimeout, outputCap: defaultToolOutputCap})
	}
	return out
}
