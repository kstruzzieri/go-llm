package interceptor

import (
	"encoding/json"

	"github.com/kstruzzieri/go-llm/internal/secretscan"
)

// toolCallTexts returns the raw argument text followed by every decoded JSON
// object key and string value in source order. Raw text remains first so the
// detectors retain syntax-level coverage as well as seeing what Unmarshal will
// present to a tool. Invalid JSON has no semantic projection.
func toolCallTexts(raw json.RawMessage) []string {
	var texts []string
	walkToolCall(raw, func(text, _ string) { texts = append(texts, text) })
	return texts
}

// walkToolCall visits raw text first, then decoded keys and string values; see
// secretscan.WalkJSON for the exact semantics.
func walkToolCall(raw json.RawMessage, visit func(text, key string)) {
	secretscan.WalkJSON(raw, visit)
}
