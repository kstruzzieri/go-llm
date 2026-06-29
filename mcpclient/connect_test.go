package mcpclient

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestAdaptToolsSchemaSizeCapSkips(t *testing.T) {
	// A schema that marshals beyond maxSchemaBytes must be skipped+warned, not
	// registered (it would bloat the prompt every turn).
	big := map[string]any{"type": "object", "x": strings.Repeat("a", maxSchemaBytes+1)}
	tools, warns := adaptTools(&fakeCaller{}, "fs", []*gomcp.Tool{
		{Name: "ok", InputSchema: map[string]any{"type": "object"}},
		{Name: "huge", InputSchema: big},
	})
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1 (huge skipped)", len(tools))
	}
	if tools[0].Spec().Name != "mcp__fs__ok" {
		t.Fatalf("kept the wrong tool: %s", tools[0].Spec().Name)
	}
	if len(warns) != 1 {
		t.Fatalf("oversized schema must warn; got %d warns", len(warns))
	}
}

func TestAdaptToolsDescriptionTruncated(t *testing.T) {
	tools, warns := adaptTools(&fakeCaller{}, "fs", []*gomcp.Tool{
		{Name: "d", Description: strings.Repeat("x", maxDescBytes+50), InputSchema: map[string]any{"type": "object"}},
	})
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1 (description truncated, not skipped)", len(tools))
	}
	if got := len(tools[0].Spec().Description); got > maxDescBytes+len("...[truncated]") {
		t.Fatalf("description not truncated: %d bytes", got)
	}
	if len(warns) != 1 {
		t.Fatalf("truncation must warn; got %d warns", len(warns))
	}
}

func TestAdaptToolsDescriptionTruncationKeepsValidUTF8(t *testing.T) {
	// Multi-byte runes so a naive byte cut at maxDescBytes would split a rune.
	tools, _ := adaptTools(&fakeCaller{}, "fs", []*gomcp.Tool{
		{Name: "u", Description: strings.Repeat("世", maxDescBytes), InputSchema: map[string]any{"type": "object"}},
	})
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	if !utf8.ValidString(tools[0].Spec().Description) {
		t.Fatal("truncated description is not valid UTF-8 (cut split a rune)")
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
