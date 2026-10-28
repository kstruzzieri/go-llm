package secretscan

import (
	"bytes"
	"encoding/json"
)

// WalkJSON visits raw text first, then decoded keys and string values.
// Only a direct object string value carries its assignment key. Decoder tokens
// preserve duplicate members and source order without re-marshalling objects.
// Invalid JSON has no semantic projection, so only its raw text is visited.
func WalkJSON(raw []byte, visit func(text, key string)) {
	visit(string(raw), "")
	walkDecoded(raw, visit)
}

// walkDecoded makes WalkJSON's visits after the raw text: the decoded keys and
// string values, none for invalid JSON.
func walkDecoded(raw []byte, visit func(text, key string)) {
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

// MatchJSON reports whether match accepts the raw JSON text or any decoded
// object key or string value. Fields are never joined, so a value split across
// two strings does not match. match is not called again after it first accepts.
func MatchJSON(raw []byte, match func(string) bool) bool {
	// The raw text is the first visit; accepting it skips validation and the
	// decode.
	if match(string(raw)) {
		return true
	}
	found := false
	walkDecoded(raw, func(text, _ string) {
		if !found && match(text) {
			found = true
		}
	})
	return found
}
