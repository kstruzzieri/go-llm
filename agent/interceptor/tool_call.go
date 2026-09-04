package interceptor

import (
	"bytes"
	"encoding/json"
	"io"
)

// toolCallTexts returns the raw argument text followed by every decoded JSON
// object key and string value in source order. Raw text remains first so the
// detectors retain syntax-level coverage as well as seeing what Unmarshal will
// present to a tool. Invalid JSON has no semantic projection.
func toolCallTexts(raw json.RawMessage) []string {
	texts := []string{string(raw)}
	if !json.Valid(raw) {
		return texts
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var semantic []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return append(texts, semantic...)
		}
		if err != nil {
			return texts
		}
		if s, ok := tok.(string); ok {
			semantic = append(semantic, s)
		}
	}
}
