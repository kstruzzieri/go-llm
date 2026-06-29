package mcpclient

import (
	"context"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAdaptToolsSkipsAndCaps(t *testing.T) {
	remote := []*gomcp.Tool{
		{Name: "good", InputSchema: map[string]any{"type": "object"}},
		{Name: "bad name", InputSchema: map[string]any{"type": "object"}},
		{Name: "arr", InputSchema: []any{1}},
		{Name: "nilschema"},
	}
	tools, warns := adaptTools(&fakeCaller{}, "fs", remote)
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(tools))
	}
	if len(warns) != 2 {
		t.Fatalf("got %d warns, want 2", len(warns))
	}
}

func TestAdaptToolsPerServerCap(t *testing.T) {
	var remote []*gomcp.Tool
	for i := 0; i < maxToolsPerServer+5; i++ {
		remote = append(remote, &gomcp.Tool{Name: "t" + itoa(i), InputSchema: map[string]any{"type": "object"}})
	}
	tools, warns := adaptTools(&fakeCaller{}, "fs", remote)
	if len(tools) != maxToolsPerServer {
		t.Fatalf("got %d tools, want cap %d", len(tools), maxToolsPerServer)
	}
	if len(warns) != 1 {
		t.Fatalf("cap truncation must warn; got %d warns", len(warns))
	}
}

func TestConnectDuplicateAliasFatal(t *testing.T) {
	_, _, err := Connect(context.Background(), Implementation{Name: "golem"},
		[]Server{StdioServer("fs", []string{"x"}), StdioServer("fs", []string{"y"})})
	if err == nil {
		t.Fatal("duplicate alias must be a fatal error")
	}
}

func TestConnectInvalidAliasFatal(t *testing.T) {
	_, _, err := Connect(context.Background(), Implementation{Name: "golem"},
		[]Server{StdioServer("bad alias", []string{"x"})})
	if err == nil {
		t.Fatal("invalid alias must be a fatal error")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
