package mcpclient

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Text string `json:"text" jsonschema:"the text to echo"`
}

func TestEndToEndInMemory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Real SDK server exposing one tool.
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "test-srv", Version: "0.0.1"}, nil)
	gomcp.AddTool(srv, &gomcp.Tool{Name: "echo", Description: "echo the input"},
		func(_ context.Context, _ *gomcp.CallToolRequest, in echoIn) (*gomcp.CallToolResult, any, error) {
			return &gomcp.CallToolResult{Content: []gomcp.Content{&gomcp.TextContent{Text: "echo: " + in.Text}}}, nil, nil
		})

	serverTr, clientTr := gomcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverTr) }()

	// Drive the real mcpclient connect path: handshake + time-bounded setup +
	// paginated list + adapt, over the in-memory transport.
	session, tools, warns := connectVia(ctx, Implementation{Name: "golem", Version: "test"}, "fs", clientTr)
	if session == nil {
		t.Fatalf("connectVia returned nil session; warns=%v", warns)
	}
	defer func() { _ = session.Close() }()
	if len(tools) != 1 || len(warns) != 0 {
		t.Fatalf("adapted %d tools, %d warns: %v", len(tools), len(warns), warns)
	}

	tl := tools[0]
	if tl.Spec().Name != "mcp__fs__echo" {
		t.Fatalf("name %q", tl.Spec().Name)
	}
	// Invoke AFTER connectVia returned: its bounded setup context is now cancelled,
	// so a successful call proves the session outlives the connect timeout.
	out, err := tl.Invoke(ctx, json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.IsError || out.Content != "echo: hi" {
		t.Fatalf("got %+v", out)
	}
}
