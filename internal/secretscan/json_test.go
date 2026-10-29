package secretscan

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const jsonFixture = "FIXTURE-unknown-01"

// uEscape renders every byte of s as a JSON \u00XX escape so the result decodes
// to s but never contains s as a raw substring.
func uEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		_, _ = fmt.Fprintf(&b, `\u%04x`, s[i])
	}
	return b.String()
}

func TestMatchJSON(t *testing.T) {
	t.Parallel()
	esc := uEscape(jsonFixture)
	match := func(s string) bool { return strings.Contains(s, jsonFixture) }
	for _, tt := range []struct {
		name string
		doc  string
		want bool
		// decoded rows only match after JSON decoding, so a plain substring
		// check over the raw document must miss them.
		decoded bool
	}{
		{name: "raw plain", doc: `{"cmd":"echo ` + jsonFixture + `"}`, want: true},
		{name: "fully escaped value", doc: `{"cmd":"` + esc + `"}`, want: true, decoded: true},
		{name: "escaped object key", doc: `{"` + esc + `":"x"}`, want: true, decoded: true},
		{name: "nested array", doc: `{"a":[["x",{"b":["` + esc + `"]}]]}`, want: true, decoded: true},
		{name: "second duplicate member", doc: `{"a":"benign","a":"` + esc + `"}`, want: true, decoded: true},
		{name: "malformed raw contains", doc: `{"cmd":"` + jsonFixture + `"`, want: true},
		{name: "malformed escaped is raw only", doc: `{"cmd":"` + esc + `"`, want: false},
		{name: "split across two values", doc: `["FIXTURE-unk","nown-01"]`, want: false},
		{name: "case changed", doc: `{"cmd":"` + strings.ToLower(jsonFixture) + `"}`, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.decoded && strings.Contains(tt.doc, jsonFixture) {
				t.Fatalf("row is not decoded-only: raw document contains the fixture: %s", tt.doc)
			}
			if got := MatchJSON([]byte(tt.doc), match); got != tt.want {
				t.Fatalf("MatchJSON(%s) = %v, want %v", tt.doc, got, tt.want)
			}
		})
	}
}

func TestMatchJSONStopsAtFirstAccept(t *testing.T) {
	t.Parallel()
	var calls []string
	got := MatchJSON([]byte(`{"a":"hit","b":"later"}`), func(s string) bool {
		calls = append(calls, s)
		return s == "a"
	})
	if !got {
		t.Fatal("MatchJSON = false, want true")
	}
	// Raw text, then the key "a" (accepted); nothing after it is offered.
	if want := []string{`{"a":"hit","b":"later"}`, "a"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("matcher saw %q, want %q", calls, want)
	}
}

type visit struct{ Text, Key string }

func collect(walk func(raw []byte, visit func(text, key string)), doc string) []visit {
	var got []visit
	walk([]byte(doc), func(text, key string) { got = append(got, visit{text, key}) })
	return got
}

// TestWalkJSONVisitSequence pins the contract literally so the frozen reference
// below cannot drift from it unnoticed.
func TestWalkJSONVisitSequence(t *testing.T) {
	t.Parallel()
	doc := `{"k":"v","k":"w","n":{"m":"x"},"a":["y",{"z":"q"}],"e":1,"f":"g"}`
	want := []visit{
		{doc, ""},
		{"k", ""}, {"v", "k"},
		{"k", ""}, {"w", "k"},
		{"n", ""}, {"m", ""}, {"x", "m"},
		{"a", ""}, {"y", ""}, {"z", ""}, {"q", "z"},
		{"e", ""}, {"f", ""}, {"g", "f"},
	}
	if got := collect(WalkJSON, doc); !reflect.DeepEqual(got, want) {
		t.Fatalf("WalkJSON visits\n got %q\nwant %q", got, want)
	}
}

func TestWalkJSONMatchesLegacyOrder(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		`{"k":"v","k":"w","n":{"m":"x"},"a":["y",{"z":"q"}],"e":1}`,
		`[["a",["b"]],{"c":"d"},"e",null,true,12.50]`,
		`{"n":1,"s":"x"}`,
		`{"t":true,"u":null,"s":"x"}`,
		`{"` + uEscape("key") + `":"` + uEscape("val") + `","k2":{"k3":["v3",{"k4":"v4"}],"k3":"dup"}}`,
		`{"cmd":"unterminated"`,
		`"just a string"`,
		`42`,
		``,
	} {
		want := collect(legacyWalkToolCall, doc)
		if got := collect(WalkJSON, doc); !reflect.DeepEqual(got, want) {
			t.Errorf("WalkJSON(%q) visits\n got %q\nwant %q", doc, got, want)
		}
	}
}

// legacyWalkToolCall is the walker body as it stood in agent/interceptor before
// it moved here, kept frozen as the behavioral reference.
func legacyWalkToolCall(raw []byte, visit func(text, key string)) {
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

func TestUnescapeJSONLenient(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in, want string
		valid    bool // in is a valid JSON string body: encoding/json must agree
	}{
		{in: "", want: "", valid: true},
		{in: "plain", want: "plain", valid: true},
		{in: `a\"b\\c\/d\be\ff\ng\rh\ti`, want: "a\"b\\c/d\be\ff\ng\rh\ti", valid: true},
		{in: `\u0041\u00e9\u00E9`, want: "A\u00e9\u00e9", valid: true},
		{in: `\ud83d\ude00`, want: "\U0001F600", valid: true},
		{in: `\ud83d`, want: "\ufffd", valid: true},
		{in: `\ude00x`, want: "\ufffdx", valid: true},
		{in: `\ud83d\u0041`, want: "\ufffdA", valid: true},
		{in: `\\u0041`, want: `\u0041`, valid: true},
		{in: `\u12`, want: `\u12`},
		{in: `\u+041`, want: `\u+041`},
		{in: `\u004G`, want: `\u004G`},
		{in: `\x\u0041`, want: `\xA`},
		{in: `end\`, want: `end\`},
	} {
		if got := unescapeJSONLenient(tt.in); got != tt.want {
			t.Errorf("unescapeJSONLenient(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if !tt.valid {
			continue
		}
		var std string
		if err := json.Unmarshal([]byte(`"`+tt.in+`"`), &std); err != nil {
			t.Fatalf("row %q is marked valid but json.Unmarshal fails: %v", tt.in, err)
		}
		if std != tt.want {
			t.Errorf("encoding/json decodes %q to %q, row wants %q", tt.in, std, tt.want)
		}
	}
}

func TestMatchToolArguments(t *testing.T) {
	t.Parallel()
	esc := uEscape(jsonFixture)
	double := strings.ReplaceAll(esc, `\`, `\\`)
	match := func(s string) bool { return strings.Contains(s, jsonFixture) }
	wrap := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, tt := range []struct {
		name string
		doc  string
		want bool
		// lenient rows match only through the lenient pass: MatchJSON must
		// miss them.
		lenient bool
	}{
		{name: "malformed escaped", doc: `{"cmd":"` + esc, want: true, lenient: true},
		{name: "wrapped malformed escaped", doc: wrap(`{"cmd":"` + esc), want: true, lenient: true},
		{name: "padded wrapped malformed escaped", doc: " \n" + wrap(`{"cmd":"`+esc) + "\t ", want: true, lenient: true},
		{name: "wrapped valid object escaped", doc: wrap(`{"cmd":"` + esc + `"}`), want: true, lenient: true},
		{name: "malformed partly escaped", doc: `{"cmd":"FIXTURE\u002dunknown\u002d01`, want: true, lenient: true},
		{name: "malformed raw contains", doc: `{"cmd":"` + jsonFixture, want: true},
		{name: "valid object escaped", doc: `{"cmd":"` + esc + `"}`, want: true},
		// One decoded level only, for valid structure and for the lenient pass.
		{name: "valid object double-escaped", doc: `{"cmd":"` + double + `"}`, want: false},
		{name: "malformed double-escaped", doc: `{"cmd":"` + double, want: false},
		{name: "wrapped malformed double-escaped", doc: wrap(`{"cmd":"` + double), want: false},
		{name: "malformed escape left as is", doc: `{"cmd":"FIXTURE\u00zdunknown-01`, want: false},
		{name: "number", doc: `42`, want: false},
		{name: "empty", doc: ``, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.lenient && MatchJSON([]byte(tt.doc), match) {
				t.Fatalf("row is not lenient-only: MatchJSON already matches %s", tt.doc)
			}
			if got := MatchToolArguments([]byte(tt.doc), match); got != tt.want {
				t.Fatalf("MatchToolArguments(%s) = %v, want %v", tt.doc, got, tt.want)
			}
		})
	}
	// A valid object gets no lenient pass, so fields stay unjoined even where
	// decoding its text whole would close a string around the credential.
	t.Run("valid object fields stay unjoined", func(t *testing.T) {
		t.Parallel()
		const key = `FIX"","b":"TURE`
		doc := `{"a":"FIX\u0022","b":"TURE"}`
		if MatchToolArguments([]byte(doc), func(s string) bool { return strings.Contains(s, key) }) {
			t.Fatalf("MatchToolArguments(%s) matched %q across two fields", doc, key)
		}
	})
}
