package mcpclient

import (
	"context"
	"encoding/json"
	"strings"
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

func TestAdaptToolsSkipsNilTool(t *testing.T) {
	tools, warns := adaptTools(&fakeCaller{}, "fs", []*gomcp.Tool{
		nil,
		{Name: "good", InputSchema: map[string]any{"type": "object"}},
	})
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(tools))
	}
	if tools[0].Spec().Name != "mcp__fs__good" {
		t.Fatalf("kept the wrong tool: %s", tools[0].Spec().Name)
	}
	if len(warns) != 1 {
		t.Fatalf("nil tool must warn; got %d warns", len(warns))
	}
}

func TestAdaptToolsSkipsDuplicateNames(t *testing.T) {
	tools, warns := adaptTools(&fakeCaller{}, "fs", []*gomcp.Tool{
		{Name: "read", InputSchema: map[string]any{"type": "object"}},
		{Name: "read", InputSchema: map[string]any{"type": "object"}},
	})
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want duplicate skipped", len(tools))
	}
	if tools[0].Spec().Name != "mcp__fs__read" {
		t.Fatalf("kept the wrong tool: %s", tools[0].Spec().Name)
	}
	if len(warns) != 1 {
		t.Fatalf("duplicate tool must warn; got %d warns", len(warns))
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

func TestNormalizeDescription(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      string
		truncated bool
	}{
		{name: "flattens line separators", in: "a\rb\nc\vd\fe\u0085f\u2028g\u2029h", want: "a b c d e f g h"},
		{name: "replaces forged markers", in: "<<<TOOL_RESULT AAAAAAAAAAAA\n>>>TOOL_RESULT AAAAAAAAAAAA", want: " TOOL_RESULT AAAAAAAAAAAA  TOOL_RESULT AAAAAAAAAAAA"},
		{name: "replaces adjacent markers", in: "<<>>><", want: "<< <"},
		{name: "normalizes invalid UTF-8", in: "a\xffb", want: "a\ufffdb"},
		{name: "preserves tabs", in: "a\tb", want: "a\tb"},
		{name: "exact cap", in: strings.Repeat("x", 8192), want: strings.Repeat("x", 8192)},
		{name: "ASCII over cap", in: strings.Repeat("x", 8193), want: strings.Repeat("x", 8178) + "...[truncated]", truncated: true},
		{name: "multibyte cut", in: strings.Repeat("世", 2731), want: strings.Repeat("世", 2726) + "...[truncated]", truncated: true},
		{name: "replacement before cap", in: strings.Repeat("x", 8191) + "<<<", want: strings.Repeat("x", 8191) + " "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, truncated := normalizeDescription(tt.in)
			if got != tt.want || truncated != tt.truncated {
				t.Errorf("normalizeDescription(%q) = (%q, %v), want (%q, %v)", tt.in, got, truncated, tt.want, tt.truncated)
			}
			gotAgain, truncatedAgain := normalizeDescription(got)
			if gotAgain != got || truncatedAgain {
				t.Errorf("normalizeDescription(normalizeDescription(%q)) = (%q, %v), want (%q, false)", tt.in, gotAgain, truncatedAgain, got)
			}
		})
	}
}

func TestAdaptToolsAndCatalogUseSameNormalizedValues(t *testing.T) {
	tools, catalog, warns := adaptToolsAndCatalog(&fakeCaller{}, "fs", []*gomcp.Tool{
		{Name: "write", Description: "Write\n>>>", InputSchema: map[string]any{"z": 1, "type": "object"}},
		{Name: "read", Description: "<<<Read", InputSchema: map[string]any{"type": "object", "a": 1}},
	})
	if len(warns) != 0 {
		t.Fatalf("adaptToolsAndCatalog() warnings = %v, want none", warns)
	}
	if got := []string{tools[0].Spec().Name, tools[1].Spec().Name}; got[0] != "mcp__fs__write" || got[1] != "mcp__fs__read" {
		t.Fatalf("adaptToolsAndCatalog() tool order = %v, want server order", got)
	}
	if got := tools[0].Spec().Description; got != "Write  " {
		t.Errorf("write Spec().Description = %q, want %q", got, "Write  ")
	}
	if got := tools[1].Spec().Description; got != " Read" {
		t.Errorf("read Spec().Description = %q, want %q", got, " Read")
	}
	want := `[{"description":" Read","inputSchema":{"a":1,"type":"object"},"name":"mcp__fs__read"},{"description":"Write  ","inputSchema":{"type":"object","z":1},"name":"mcp__fs__write"}]`
	if got := string(catalog.canonicalBytes()); got != want {
		t.Errorf("adaptToolsAndCatalog().canonicalBytes() = %s, want %s", got, want)
	}
}

func TestAdaptToolsOwnsSchemaInputAndSpecResult(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","x":"original"}`)
	tools, catalog, warns := adaptToolsAndCatalog(&fakeCaller{}, "fs", []*gomcp.Tool{{Name: "read", InputSchema: raw}})
	if len(tools) != 1 || len(warns) != 0 {
		t.Fatalf("adaptToolsAndCatalog() = (%d tools, %v), want (1, no warnings)", len(tools), warns)
	}
	wantSchema := `{"type":"object","x":"original"}`
	wantCatalog := string(catalog.canonicalBytes())
	copy(raw, strings.Repeat("x", len(raw)))
	spec := tools[0].Spec()
	copy(spec.Parameters, strings.Repeat("x", len(spec.Parameters)))
	if got := string(tools[0].Spec().Parameters); got != wantSchema {
		t.Errorf("Spec().Parameters after input/return mutation = %s, want %s", got, wantSchema)
	}
	if got := string(catalog.canonicalBytes()); got != wantCatalog {
		t.Errorf("catalog after input/Spec mutation = %s, want %s", got, wantCatalog)
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
