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

func TestSDKTransportSchemaNumberSemantics(t *testing.T) {
	tests := []struct {
		name       string
		schema     string
		wantSchema string
		wantDigest string
	}{
		{name: "integer", schema: `{"type":"object","x-number":1}`, wantSchema: `{"type":"object","x-number":1}`, wantDigest: "sha256:fa68f79f14ad5bafddd912e30ebed7dc53c208ba420d8465fad50f1ddb8b927d"},
		{name: "decimal", schema: `{"type":"object","x-number":1.0}`, wantSchema: `{"type":"object","x-number":1}`, wantDigest: "sha256:fa68f79f14ad5bafddd912e30ebed7dc53c208ba420d8465fad50f1ddb8b927d"},
		{name: "exponent", schema: `{"type":"object","x-number":1e0}`, wantSchema: `{"type":"object","x-number":1}`, wantDigest: "sha256:fa68f79f14ad5bafddd912e30ebed7dc53c208ba420d8465fad50f1ddb8b927d"},
		{name: "large integer", schema: `{"type":"object","x-number":9007199254740992}`, wantSchema: `{"type":"object","x-number":9007199254740992}`, wantDigest: "sha256:5a6c5d40a3bf6a3ae48fc1dffbf0b167fdc685b4b4c559957a639240a0e763dd"},
		{name: "rounded adjacent integer", schema: `{"type":"object","x-number":9007199254740993}`, wantSchema: `{"type":"object","x-number":9007199254740992}`, wantDigest: "sha256:5a6c5d40a3bf6a3ae48fc1dffbf0b167fdc685b4b4c559957a639240a0e763dd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema, catalog := catalogThroughSDK(t, tt.schema)
			if schema != tt.wantSchema {
				t.Errorf("SDK decoded schema for %s = %s, want %s", tt.schema, schema, tt.wantSchema)
			}
			if got := catalog.digest(); got != tt.wantDigest {
				t.Errorf("catalog digest for %s = %s, want %s", tt.schema, got, tt.wantDigest)
			}
		})
	}
	_, different := catalogThroughSDK(t, `{"type":"object","x-number":2}`)
	if got, same := different.digest(), "sha256:fa68f79f14ad5bafddd912e30ebed7dc53c208ba420d8465fad50f1ddb8b927d"; got == same {
		t.Errorf("catalog digest for decoded 2 = %s, want different from decoded 1", got)
	}
}

func TestSDKTransportDuplicateKeyAndUnicodeSemantics(t *testing.T) {
	tests := []struct {
		name       string
		schema     string
		wantSchema string
		wantDigest string
	}{
		{name: "last duplicate wins", schema: `{"type":"object","x":"first","x":"last"}`, wantSchema: `{"type":"object","x":"last"}`, wantDigest: "sha256:548bbe165084a5f28aa0e6f9c5de79f2183fa11f9bbf7727c2f539082c97946a"},
		{name: "malformed Unicode replaced", schema: `{"type":"object","x":"\ud800"}`, wantSchema: `{"type":"object","x":"�"}`, wantDigest: "sha256:e5f9134c579d4c1279bffaa4107017a9aa789b228d3826e21df695dcbd6ece8e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema, catalog := catalogThroughSDK(t, tt.schema)
			if schema != tt.wantSchema {
				t.Errorf("SDK decoded schema for %s = %s, want %s", tt.schema, schema, tt.wantSchema)
			}
			if got := catalog.digest(); got != tt.wantDigest {
				t.Errorf("catalog digest for %s = %s, want %s", tt.schema, got, tt.wantDigest)
			}
		})
	}
}

func catalogThroughSDK(t *testing.T, schema string) (string, toolCatalog) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "schema-test", Version: "0.0.1"}, nil)
	srv.AddTool(&gomcp.Tool{
		Name:        "read",
		Description: "Number",
		InputSchema: json.RawMessage(schema),
	}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		return &gomcp.CallToolResult{}, nil
	})
	serverTr, clientTr := gomcp.NewInMemoryTransports()
	go func() { _ = srv.Run(ctx, serverTr) }()
	client := gomcp.NewClient(&gomcp.Implementation{Name: "schema-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTr, nil)
	if err != nil {
		t.Fatalf("client.Connect(%s): %v", schema, err)
	}
	t.Cleanup(func() { _ = session.Close() })
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools(%s): %v", schema, err)
	}
	tools, catalog, warns := adaptToolsAndCatalog(session, "fs", result.Tools)
	if len(tools) != 1 || len(warns) != 0 {
		t.Fatalf("adaptToolsAndCatalog(%s) = (%d tools, %v), want (1, no warnings)", schema, len(tools), warns)
	}
	return string(tools[0].Spec().Parameters), catalog
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
