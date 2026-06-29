package mcpclient

import (
	"encoding/json"
	"fmt"
	"strings"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// flattenContent renders an MCP tool result into a single string for
// agent.ToolResult.Content. Nothing is silently dropped: text blocks are
// concatenated, any non-text block becomes a typed marker, and StructuredContent
// (if present) is appended under a [structured] marker. We type-switch only on
// TextContent and let every other content variant fall to the marker branch, so
// the function does not depend on the field layout of image/audio/resource types.
func flattenContent(res *gomcp.CallToolResult) string {
	var b strings.Builder
	for i, c := range res.Content {
		if i > 0 {
			b.WriteByte('\n')
		}
		if tc, ok := c.(*gomcp.TextContent); ok {
			b.WriteString(tc.Text)
			continue
		}
		fmt.Fprintf(&b, "[non-text content: %T]", c)
	}
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("[structured] ")
			b.Write(raw)
		}
	}
	return b.String()
}
