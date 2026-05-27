package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newInMemoryMCPServer is a tiny fixture MCP server with two declared
// tools. It exposes only the client-side transport so the test can
// dial it as if it were a real server.
func newInMemoryMCPServer(t *testing.T, pageSize int) mcp.Transport {
	t.Helper()
	var opts *mcp.ServerOptions
	if pageSize > 0 {
		opts = &mcp.ServerOptions{PageSize: pageSize}
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "bench-fixture", Version: "0.0.1"}, opts)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_code",
		Description: "search files",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string"}},
			"required":   []string{"query"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_file",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	_, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	return clientTransport
}

func TestMCPToolSchemaSourceSnapshotMarshalsMinimalTools(t *testing.T) {
	src := &mcpToolSchemaSource{
		transport:  newInMemoryMCPServer(t, 0),
		clientName: "llm-bench-test",
	}
	got, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d; want 2", len(got))
	}
	names := make(map[string]bool, len(got))
	for _, raw := range got {
		var fields struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("unmarshal tool: %v", err)
		}
		names[fields.Name] = true
		if fields.Name == "" {
			t.Fatalf("snapshot tool missing name: %s", string(raw))
		}
		if len(fields.InputSchema) == 0 {
			t.Fatalf("snapshot tool %q missing inputSchema", fields.Name)
		}
	}
	if !names["search_code"] || !names["read_file"] {
		t.Fatalf("expected both tools; got %v", names)
	}
}

func TestMCPToolSchemaSourceSnapshotPaginates(t *testing.T) {
	// PageSize=1 forces the SDK server to split the two fixture tools
	// across two tools/list pages. This is the load-bearing test for the
	// cursor loop in Snapshot.
	src := &mcpToolSchemaSource{transport: newInMemoryMCPServer(t, 1)}
	got, _ := src.Snapshot(context.Background())
	if len(got) != 2 {
		t.Fatalf("len(got)=%d; want 2 tools across paginated pages", len(got))
	}
}

func TestMCPToolSchemaSourceSnapshotRequiresTransport(t *testing.T) {
	src := &mcpToolSchemaSource{}
	if _, err := src.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("want missing-transport error; got %v", err)
	}
}

func TestMarshalMinimalToolRejectsNilTool(t *testing.T) {
	if _, err := marshalMinimalTool(nil); err == nil || !strings.Contains(err.Error(), "nil tool") {
		t.Fatalf("want nil-tool error; got %v", err)
	}
}

func TestMarshalMinimalToolRejectsMissingName(t *testing.T) {
	if _, err := marshalMinimalTool(&mcp.Tool{Name: " \t "}); err == nil || !strings.Contains(err.Error(), "missing name") {
		t.Fatalf("want missing-name error; got %v", err)
	}
}
