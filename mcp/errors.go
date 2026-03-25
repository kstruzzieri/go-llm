package mcp

import (
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolError returns a CallToolResult with isError set to true.
// The message is formatted with category prefix for consistent error reporting.
func toolError(category, msg string, args ...any) *gomcp.CallToolResult {
	text := fmt.Sprintf("%s: %s", category, fmt.Sprintf(msg, args...))
	return &gomcp.CallToolResult{
		Content: []gomcp.Content{
			&gomcp.TextContent{Text: text},
		},
		IsError: true,
	}
}

// toolResult returns a successful CallToolResult with text content.
func toolResult(text string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		Content: []gomcp.Content{
			&gomcp.TextContent{Text: text},
		},
	}
}
