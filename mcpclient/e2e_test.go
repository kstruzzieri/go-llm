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

	// Real adapter client over the in-memory transport.
	client := gomcp.NewClient(&gomcp.Implementation{Name: "golem", Version: "test"},
		&gomcp.ClientOptions{Capabilities: &gomcp.ClientCapabilities{}})
	session, err := client.Connect(ctx, clientTr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	remote, warns := listAllTools(ctx, session, "fs")
	if len(remote) != 1 || len(warns) != 0 {
		t.Fatalf("listed %d tools, %d warns", len(remote), len(warns))
	}
	tools, warns := adaptTools(session, "fs", remote)
	if len(tools) != 1 || len(warns) != 0 {
		t.Fatalf("adapted %d tools, %d warns", len(tools), len(warns))
	}

	tl := tools[0]
	if tl.Spec().Name != "mcp__fs__echo" {
		t.Fatalf("name %q", tl.Spec().Name)
	}
	out, err := tl.Invoke(ctx, json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if out.IsError || out.Content != "echo: hi" {
		t.Fatalf("got %+v", out)
	}
}
