package signing

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"unicode/utf8"
)

// Canonicalize returns canonical form v1 of the single JSON value in raw:
// object keys sorted (Go string order, recursively), compact, number
// literals verbatim, minimal string escaping with HTML escaping off. It
// rejects invalid UTF-8, unpaired surrogate escapes, duplicate object keys,
// trailing data, and empty input. See doc.go for the full rule list and the
// RFC 8785 divergences.
func Canonicalize(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("signing: canonicalize: empty input")
	}
	if !utf8.Valid(raw) {
		return nil, ErrInvalidUTF8
	}
	if err := rejectUnpairedSurrogateEscapes(raw); err != nil {
		return nil, err
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("signing: canonicalize: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrTrailingData
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("signing: canonicalize: %w", err)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte{'\n'}), nil
}

// rejectUnpairedSurrogateEscapes prevents encoding/json from replacing
// distinct invalid UTF-16 escape sequences with the same U+FFFD rune.
// General JSON escape syntax remains the decoder's responsibility.
func rejectUnpairedSurrogateEscapes(raw []byte) error {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '"' {
			continue
		}
	stringValue:
		for i++; i < len(raw); i++ {
			switch raw[i] {
			case '"':
				break stringValue
			case '\\':
				if i+1 >= len(raw) || raw[i+1] != 'u' {
					i++ // consume the escaped byte; malformed JSON fails later
					continue
				}
				unit, ok := parseHex4(raw, i+2)
				if !ok {
					i++ // malformed escape; leave the diagnostic to encoding/json
					continue
				}
				switch {
				case unit >= 0xd800 && unit <= 0xdbff:
					if i+12 > len(raw) || raw[i+6] != '\\' || raw[i+7] != 'u' {
						return ErrInvalidUnicodeEscape
					}
					low, ok := parseHex4(raw, i+8)
					if !ok || low < 0xdc00 || low > 0xdfff {
						return ErrInvalidUnicodeEscape
					}
					i += 11 // consume both six-byte escapes
				case unit >= 0xdc00 && unit <= 0xdfff:
					return ErrInvalidUnicodeEscape
				default:
					i += 5 // consume one six-byte escape
				}
			}
		}
	}
	return nil
}

func parseHex4(raw []byte, start int) (uint16, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	var value uint16
	for _, b := range raw[start : start+4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

// MarshalCanonical marshals v with encoding/json and canonicalizes the result.
// Marshal failures (NaN, functions, channels) are returned, never panicked.
func MarshalCanonical(v any) ([]byte, error) {
	if err := rejectInvalidUTF8Value(reflect.ValueOf(v), map[utf8Visit]bool{}); err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("signing: marshal canonical: %w", err)
	}
	return Canonicalize(b)
}

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

type utf8Visit struct {
	typ reflect.Type
	ptr uintptr
}

// rejectInvalidUTF8Value closes encoding/json's invalid-string replacement
// behavior for ordinary Go values. It deliberately walks all reachable
// fields instead of duplicating encoding/json's field-selection rules.
// Custom json.Marshaler and encoding.TextMarshaler implementations own their
// pre-coercion semantics; Canonicalize still validates their final JSON.
func rejectInvalidUTF8Value(v reflect.Value, visiting map[utf8Visit]bool) error {
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Interface {
		if v.IsNil() {
			return nil // encoding/json emits null
		}
		// encoding/json selects the marshaler from the STATIC interface type
		// before looking at the dynamic value: an interface type embedding
		// json.Marshaler routes to MarshalJSON, one embedding only
		// encoding.TextMarshaler routes to MarshalText (whose output is then
		// string-coerced), and any other interface type dispatches on the
		// dynamic value as a non-addressable operand. Review finding: unwrapping
		// first let a dynamic json.Marshaler mask a static MarshalText route.
		if v.Type().Implements(jsonMarshalerType) {
			return nil
		}
		if v.Type().Implements(textMarshalerType) && v.CanInterface() {
			return validTextOutput(v.Interface().(encoding.TextMarshaler))
		}
		return rejectInvalidUTF8Value(v.Elem(), visiting)
	}
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return nil
	}
	if handled, err := customMarshalerOutput(v); handled {
		return err
	}
	if v.Kind() == reflect.String {
		if !utf8.ValidString(v.String()) {
			return ErrInvalidUTF8
		}
		return nil
	}
	if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
		return nil // encoding/json intentionally base64-encodes arbitrary bytes
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Map || v.Kind() == reflect.Slice {
		if v.IsNil() {
			return nil
		}
		visit := utf8Visit{typ: v.Type(), ptr: v.Pointer()}
		if visiting[visit] {
			return nil // json.Marshal reports the cycle itself
		}
		visiting[visit] = true
		defer delete(visiting, visit)
	}
	switch v.Kind() {
	case reflect.Pointer:
		return rejectInvalidUTF8Value(v.Elem(), visiting)
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key()
			if key.Kind() == reflect.String {
				if !utf8.ValidString(key.String()) {
					return ErrInvalidUTF8
				}
			} else if err := textMarshalerKeyOutput(key); err != nil {
				return err
			}
			if err := rejectInvalidUTF8Value(iter.Value(), visiting); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := rejectInvalidUTF8Value(v.Index(i), visiting); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := rejectInvalidUTF8Value(v.Field(i), visiting); err != nil {
				return err
			}
		}
	}
	return nil
}

// customMarshalerOutput mirrors encoding/json's value dispatch (newTypeEncoder
// order): a json.Marshaler reachable through the address when v is
// addressable, then on v itself; then an encoding.TextMarshaler the same way.
// A json.Marshaler's bytes are validated later by Canonicalize, so it is only
// skipped here. A TextMarshaler's bytes pass through encoding/json's string
// encoder, which replaces invalid UTF-8 with U+FFFD before Canonicalize can
// see it, so its output is validated now. Internal fields of either kind are
// not walked: they may legitimately hold binary and are never serialized.
func customMarshalerOutput(v reflect.Value) (handled bool, err error) {
	if _, ok := asJSONMarshaler(v); ok {
		return true, nil
	}
	if tm, ok := asTextMarshaler(v); ok {
		return true, validTextOutput(tm)
	}
	return false, nil
}

// textMarshalerKeyOutput mirrors encoding/json's map-key dispatch, which
// ignores json.Marshaler and formats non-string keys through MarshalText on
// the key value. Review finding: a key type implementing both marshalers was
// skipped as a json.Marshaler while encoding/json used its MarshalText.
func textMarshalerKeyOutput(key reflect.Value) error {
	if !key.CanInterface() {
		return nil
	}
	tm, ok := key.Interface().(encoding.TextMarshaler)
	if !ok {
		return nil
	}
	return validTextOutput(tm)
}

// asJSONMarshaler prefers the addressable receiver, as encoding/json does, so
// a pointer-receiver MarshalJSON is honored exactly when json would use it.
func asJSONMarshaler(v reflect.Value) (json.Marshaler, bool) {
	if v.CanAddr() && v.Addr().CanInterface() {
		if m, ok := v.Addr().Interface().(json.Marshaler); ok {
			return m, true
		}
	}
	if v.CanInterface() {
		if m, ok := v.Interface().(json.Marshaler); ok {
			return m, true
		}
	}
	return nil, false
}

func asTextMarshaler(v reflect.Value) (encoding.TextMarshaler, bool) {
	if v.CanAddr() && v.Addr().CanInterface() {
		if m, ok := v.Addr().Interface().(encoding.TextMarshaler); ok {
			return m, true
		}
	}
	if v.CanInterface() {
		if m, ok := v.Interface().(encoding.TextMarshaler); ok {
			return m, true
		}
	}
	return nil, false
}

func validTextOutput(tm encoding.TextMarshaler) error {
	text, err := tm.MarshalText()
	if err != nil {
		return nil // json.Marshal reports the marshaler's error itself
	}
	if !utf8.Valid(text) {
		return ErrInvalidUTF8
	}
	return nil
}

// rejectDuplicateKeys walks the token stream and fails on the first object
// that repeats a key at any depth. encoding/json keeps the last duplicate
// silently; a signed record must not admit two spellings of one value.
func rejectDuplicateKeys(raw []byte) error {
	type object struct {
		keys      map[string]struct{}
		expectKey bool
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var stack []*object // nil entries are arrays
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("signing: canonicalize: %w", err)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				stack = append(stack, &object{keys: map[string]struct{}{}, expectKey: true})
			case '[':
				stack = append(stack, nil)
			default: // '}' or ']': the closed value belonged to the parent
				stack = stack[:len(stack)-1]
				if n := len(stack); n > 0 && stack[n-1] != nil {
					stack[n-1].expectKey = true
				}
			}
			continue
		}
		n := len(stack)
		if n == 0 || stack[n-1] == nil {
			continue // top-level scalar or array element
		}
		top := stack[n-1]
		if !top.expectKey {
			top.expectKey = true // scalar value consumed; a key comes next
			continue
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("signing: canonicalize: object key %v is not a string", tok)
		}
		if _, dup := top.keys[key]; dup {
			return fmt.Errorf("%w: %q", ErrDuplicateKey, key)
		}
		top.keys[key] = struct{}{}
		top.expectKey = false
	}
}
