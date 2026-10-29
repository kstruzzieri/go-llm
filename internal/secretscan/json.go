package secretscan

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
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

// MatchToolArguments is MatchJSON plus one lenient pass for tool-call
// arguments that are not a JSON object or array. Invalid JSON, or a JSON
// string literal such as provider/openaicompat writes for arguments Go's
// decoder rejects, is matched again with its JSON string escapes decoded
// wherever they are well formed (the literal's decoded text, for a literal).
// So a credential written as \u or \/ escapes in unparseable arguments still
// matches. Valid objects and arrays get MatchJSON alone: one decoded level.
func MatchToolArguments(raw []byte, match func(string) bool) bool {
	if MatchJSON(raw, match) {
		return true
	}
	// JSON whitespace only: bytes.TrimSpace would also strip \v or U+00A0,
	// turning invalid padded text into a valid document that is never decoded.
	trimmed := bytes.Trim(raw, " \t\r\n")
	var text string
	switch {
	case len(trimmed) > 0 && trimmed[0] == '"' && json.Unmarshal(trimmed, &text) == nil:
	case !json.Valid(trimmed):
		text = string(raw)
	default:
		return false
	}
	return strings.Contains(text, `\`) && match(unescapeJSONLenient(text))
}

// jsonSimpleEscapes maps the byte after a backslash to what a JSON string
// escape decodes to, for every escape except \u.
var jsonSimpleEscapes = map[byte]byte{
	'"': '"', '\\': '\\', '/': '/', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t',
}

// unescapeJSONLenient decodes each well-formed JSON string escape in s, with
// \u surrogate pairs combined and a lone surrogate written as U+FFFD (as
// encoding/json does), and keeps every other byte, a malformed escape
// included, as it is. It decodes one level and never parses structure.
func unescapeJSONLenient(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '\\' || i+1 == len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		if c, ok := jsonSimpleEscapes[s[i+1]]; ok {
			b.WriteByte(c)
			i += 2
			continue
		}
		r, ok := jsonHex4(s, i)
		if !ok {
			b.WriteByte(s[i])
			i++
			continue
		}
		i += 6
		if utf16.IsSurrogate(r) {
			r2, ok := jsonHex4(s, i)
			if pair := utf16.DecodeRune(r, r2); ok && pair != utf8.RuneError {
				r = pair
				i += 6
			} else {
				r = utf8.RuneError
			}
		}
		b.WriteRune(r)
	}
	return b.String()
}

// jsonHex4 decodes the \uXXXX escape starting at s[i], if there is one.
func jsonHex4(s string, i int) (rune, bool) {
	if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
		return 0, false
	}
	v, err := strconv.ParseUint(s[i+2:i+6], 16, 16)
	return rune(v), err == nil
}
