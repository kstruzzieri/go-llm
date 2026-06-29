package mcpclient

import (
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFlattenContent(t *testing.T) {
	t.Run("text blocks joined", func(t *testing.T) {
		res := &gomcp.CallToolResult{Content: []gomcp.Content{
			&gomcp.TextContent{Text: "hello"},
			&gomcp.TextContent{Text: "world"},
		}}
		if got := flattenContent(res); got != "hello\nworld" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("non-text becomes a marker, never dropped", func(t *testing.T) {
		res := &gomcp.CallToolResult{Content: []gomcp.Content{
			&gomcp.TextContent{Text: "caption"},
			&gomcp.ImageContent{},
		}}
		got := flattenContent(res)
		if !strings.Contains(got, "caption") || !strings.Contains(got, "[non-text content") {
			t.Fatalf("expected text + marker, got %q", got)
		}
	})
	t.Run("structured content appended under marker", func(t *testing.T) {
		res := &gomcp.CallToolResult{
			Content:           []gomcp.Content{&gomcp.TextContent{Text: "t"}},
			StructuredContent: map[string]any{"k": "v"},
		}
		got := flattenContent(res)
		if !strings.Contains(got, "t") || !strings.Contains(got, "[structured]") || !strings.Contains(got, `"k":"v"`) {
			t.Fatalf("got %q", got)
		}
	})
}
