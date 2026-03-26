package mcp

import (
	"fmt"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolError returns a CallToolResult with isError set to true.
// The message is formatted with category prefix for consistent error reporting.
func toolError(category string, msg string, args ...any) *gomcp.CallToolResult {
	var detail string
	if len(args) > 0 {
		detail = fmt.Sprintf(msg, args...)
	} else {
		detail = msg
	}
	text := category + ": " + detail
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
