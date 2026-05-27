package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	mcpClientName    = "llm-bench"
	mcpClientVersion = "0"
	// maxToolListPages caps pagination so a misbehaving MCP server can't
	// keep llm-bench in tools/list forever. 1000 pages × typical page
	// sizes is far above any plausible real tool catalog.
	maxToolListPages = 1000
)

// mcpToolSchemaSource implements toolSchemaSource against an external
// MCP server. Construct via newMCPToolSchemaSourceStdio or
// newMCPToolSchemaSourceHTTP — never instantiate the struct literal
// directly outside tests.
//
// Lifecycle: Snapshot opens a fresh ClientSession, paginates through
// tools/list, marshals each Tool into the minimal {name, description,
// inputSchema} JSON shape, then closes the session. There is no
// shared connection — the caller invokes Snapshot exactly once per
// capture run (see spec §4.2).
type mcpToolSchemaSource struct {
	transport  mcp.Transport
	clientName string
}

func newMCPToolSchemaSourceStdio(command string) (*mcpToolSchemaSource, error) {
	parts := strings.Fields(strings.TrimSpace(command))
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty mcp-stdio-command")
	}
	transport := &mcp.CommandTransport{Command: exec.Command(parts[0], parts[1:]...)}
	return &mcpToolSchemaSource{transport: transport, clientName: mcpClientName}, nil
}

func newMCPToolSchemaSourceHTTP(endpoint string) (*mcpToolSchemaSource, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("empty mcp-url")
	}
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint}
	return &mcpToolSchemaSource{transport: transport, clientName: mcpClientName}, nil
}

func (s *mcpToolSchemaSource) Snapshot(ctx context.Context) ([]json.RawMessage, error) {
	if s.transport == nil {
		return nil, fmt.Errorf("mcp snapshot: nil transport")
	}
	clientName := s.clientName
	if clientName == "" {
		clientName = mcpClientName
	}
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: mcpClientVersion}, nil)
	session, err := client.Connect(ctx, s.transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp snapshot: connect: %w", err)
	}
	defer func() { _ = session.Close() }()

	return paginateSnapshot(ctx, session)
}

// toolListPager abstracts the page-fetching surface so paginateSnapshot
// can be unit-tested without spinning up an MCP server.
// *mcp.ClientSession already satisfies this interface.
type toolListPager interface {
	ListTools(ctx context.Context, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error)
}

// paginateSnapshot walks tools/list with two safeguards against a
// misbehaving server: (1) a hard cap at maxToolListPages, and
// (2) detection of a repeated NextCursor (cycle).
func paginateSnapshot(ctx context.Context, pager toolListPager) ([]json.RawMessage, error) {
	var out []json.RawMessage
	var cursor string
	seenCursors := make(map[string]struct{})
	for page := 0; ; page++ {
		if page >= maxToolListPages {
			return nil, fmt.Errorf("mcp snapshot: tools/list exceeded %d pages", maxToolListPages)
		}
		params := &mcp.ListToolsParams{Cursor: cursor}
		res, err := pager.ListTools(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("mcp snapshot: tools/list: %w", err)
		}
		for _, tool := range res.Tools {
			raw, err := marshalMinimalTool(tool)
			if err != nil {
				return nil, fmt.Errorf("mcp snapshot: marshal tool %q: %w", toolNameForError(tool), err)
			}
			out = append(out, raw)
		}
		if res.NextCursor == "" {
			break
		}
		// A server that returns the same NextCursor twice is in a cycle.
		// We track non-empty cursors only; the initial "" is the
		// starting state, not a page identity.
		if _, dup := seenCursors[res.NextCursor]; dup {
			return nil, fmt.Errorf("mcp snapshot: tools/list cursor cycle on %q", res.NextCursor)
		}
		seenCursors[res.NextCursor] = struct{}{}
		cursor = res.NextCursor
	}
	return out, nil
}

// marshalMinimalTool projects a SDK Tool to the {name, description,
// inputSchema} subset persisted in trace.Tools. We avoid persisting
// the full SDK shape (Meta, Annotations, Icons, OutputSchema) because
// those fields aren't relevant to argument validation and would inflate
// trace files unnecessarily.
func marshalMinimalTool(tool *mcp.Tool) (json.RawMessage, error) {
	if tool == nil {
		return nil, fmt.Errorf("nil tool")
	}
	name := strings.TrimSpace(tool.Name)
	if name == "" {
		return nil, fmt.Errorf("missing name")
	}
	type minimal struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		InputSchema any    `json:"inputSchema"`
	}
	m := minimal{Name: name, Description: tool.Description, InputSchema: tool.InputSchema}
	if m.InputSchema == nil {
		m.InputSchema = map[string]any{"type": "object"}
	}
	return json.Marshal(m)
}

func toolNameForError(tool *mcp.Tool) string {
	if tool == nil {
		return "<nil>"
	}
	name := strings.TrimSpace(tool.Name)
	if name == "" {
		return "<empty>"
	}
	return name
}
