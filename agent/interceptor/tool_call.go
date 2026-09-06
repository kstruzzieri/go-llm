package interceptor

import (
	"bytes"
	"encoding/json"
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

// walkToolCall visits raw text first, then decoded keys and string values.
// Only a direct object string value carries its assignment key. Decoder tokens
// preserve duplicate members and source order without re-marshalling objects.
func walkToolCall(raw json.RawMessage, visit func(text, key string)) {
	visit(string(raw), "")
	if !json.Valid(raw) {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		object bool
		value  bool
		key    string
	}
	var stack []frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		if tok == json.Delim('}') || tok == json.Delim(']') {
			stack = stack[:len(stack)-1]
			continue
		}
		var key string
		if len(stack) > 0 {
			parent := &stack[len(stack)-1]
			if parent.object {
				if !parent.value {
					parent.key = tok.(string) // json.Valid guarantees an object key.
					parent.value = true
					visit(parent.key, "")
					continue
				}
				key = parent.key
				parent.key, parent.value = "", false
			}
		}
		switch tok := tok.(type) {
		case json.Delim:
			stack = append(stack, frame{object: tok == '{'})
		case string:
			visit(tok, key)
		}
	}
}
