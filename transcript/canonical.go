// Package transcript persists MCP chat calls as replayable benchmark traces.
// It appends an immutable raw call record first (the durable source of truth)
// and then projects a best-effort canonical conversation. The canonical
// conversations table is a strict superset of the columns cmd/llm-bench's
// capture reader consumes, so captured traces need no schema changes.
package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/kstruzzieri/go-llm/conversation"
)

// canonicalMsg is the comparison/hashing form of a conversation.Message. Every
// field is a string so the struct is comparable with ==; ToolCalls holds the
// canonical JSON of the message's tool_calls ("" when absent).
type canonicalMsg struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCalls  string `json:"tool_calls"`
	ToolName   string `json:"tool_name"`
	ToolCallID string `json:"tool_call_id"`
}

// canonicalToolCallsJSON re-marshals a tool_calls payload with stable key
// ordering so whitespace/key-order differences never produce false divergences
// or fork ids. Empty, omitted, null, and empty-array payloads all canonicalize
// to "" (absence). A payload that is not valid JSON is returned verbatim so a
// malformed value never silently collides with a different one.
func canonicalToolCallsJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	if v == nil {
		return ""
	}
	if arr, ok := v.([]any); ok && len(arr) == 0 {
		return ""
	}
	b, err := json.Marshal(v) // encoding/json emits map keys in sorted order
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// canonicalMessage converts a conversation.Message into its comparison form.
func canonicalMessage(m conversation.Message) canonicalMsg {
	return canonicalMsg{
		Role:       m.Role,
		Content:    m.Content,
		ToolCalls:  canonicalToolCallsJSON(m.ToolCalls),
		ToolName:   m.ToolName,
		ToolCallID: m.ToolCallID,
	}
}

// sha256Hex returns the lowercase hex SHA-256 of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
