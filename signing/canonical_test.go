package signing

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"testing"
)

func TestCanonicalizeGolden(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"struct-order to sorted", `{"z":"z","a":9007199254740993,"f":1e+21,"p":null,"b":"AQI=","t":null}`,
			`{"a":9007199254740993,"b":"AQI=","f":1e+21,"p":null,"t":null,"z":"z"}`},
		{"numbers verbatim", `{"n":9007199254740993,"x":1.0,"y":1e2,"z":-0.0,"w":1E+2}`,
			`{"n":9007199254740993,"w":1E+2,"x":1.0,"y":1e2,"z":-0.0}`},
		{"number outside float64 range", `{"n":1e1000}`, `{"n":1e1000}`},
		{"unicode and html chars", "{\"s\":\"<a>&é😀\u2028\"}",
			`{"s":"<a>&é😀\u2028"}`},
		{"escaped html decodes to literal", `{"s":"\u003ca\u003e\u0026"}`, `{"s":"<a>&"}`},
		{"whitespace stripped", " {\"a\" : 1 ,\n \"b\": [ 1 , 2 ] } \n", `{"a":1,"b":[1,2]}`},
		{"nested sort", `[1,[2,{"b":{"d":1,"c":2},"a":[]}]]`, `[1,[2,{"a":[],"b":{"c":2,"d":1}}]]`},
		{"key order is utf-8 byte order", `{"｡":1,"𐀀":2,"b":3,"B":4,"aa":5,"a":6}`,
			`{"B":4,"a":6,"aa":5,"b":3,"｡":1,"𐀀":2}`},
		{"null", `null`, `null`},
		{"empty string", `""`, `""`},
		{"empty array", `[]`, `[]`},
		{"empty object", `{}`, `{}`},
		{"bools", `{"t":true,"f":false}`, `{"f":false,"t":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonicalize([]byte(tc.in))
			if err != nil {
				t.Fatalf("Canonicalize: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
			again, err := Canonicalize(got)
			if err != nil {
				t.Fatalf("re-canonicalize: %v", err)
			}
			if string(again) != tc.want {
				t.Fatalf("not idempotent: %s", again)
			}
		})
	}
}

func TestCanonicalizeRejectsInvalidUTF8(t *testing.T) {
	a := []byte("{\"s\":\"\xff\xfe\"}")
	b := []byte("{\"s\":\"\xfe\xff\"}")
	for _, in := range [][]byte{a, b} {
		if _, err := Canonicalize(in); !errors.Is(err, ErrInvalidUTF8) {
			t.Fatalf("Canonicalize(%q) err = %v, want ErrInvalidUTF8", in, err)
		}
	}
}

func TestCanonicalizeRejectsUnpairedSurrogateEscapes(t *testing.T) {
	for _, in := range []string{
		`{"s":"\ud800"}`,
		`{"s":"\udbff\u0061"}`,
		`{"s":"\udc00"}`,
		`{"s":"\udfff"}`,
	} {
		if _, err := Canonicalize([]byte(in)); !errors.Is(err, ErrInvalidUnicodeEscape) {
			t.Errorf("Canonicalize(%s) err = %v, want ErrInvalidUnicodeEscape", in, err)
		}
	}
}

func TestCanonicalizeAcceptsPairedSurrogatesAndEscapedLiteral(t *testing.T) {
	got, err := Canonicalize([]byte(`{"s":"\ud83d\ude00","literal":"\\ud800"}`))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"literal":"\\ud800","s":"😀"}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCanonicalizeRejectsDuplicateKeys(t *testing.T) {
	cases := []string{
		`{"a":1,"a":2}`,
		`{"o":{"a":1,"a":2}}`,
		`[{"a":1,"a":2}]`,
		`{"a":{"x":1},"a":2}`,
		`{"a":[1,2],"b":3,"a":4}`,
		`{"a":1,"\u0061":2}`,
	}
	for _, in := range cases {
		if _, err := Canonicalize([]byte(in)); !errors.Is(err, ErrDuplicateKey) {
			t.Errorf("Canonicalize(%s) err = %v, want ErrDuplicateKey", in, err)
		}
	}
	if _, err := Canonicalize([]byte(`{"a":{"a":1},"b":{"a":2}}`)); err != nil {
		t.Fatalf("same key in sibling objects must be allowed: %v", err)
	}
}

func TestCanonicalizeRejectsTrailingData(t *testing.T) {
	if _, err := Canonicalize([]byte(`{"a":1} {"b":2}`)); !errors.Is(err, ErrTrailingData) {
		t.Fatalf("err = %v, want ErrTrailingData", err)
	}
	if _, err := Canonicalize([]byte(`{"a":1} x`)); err == nil {
		t.Fatal("trailing garbage accepted")
	}
	if _, err := Canonicalize([]byte(`{"a":1}]`)); err == nil {
		t.Fatal("trailing bracket accepted")
	}
}

func TestCanonicalizeRejectsMalformed(t *testing.T) {
	for _, in := range []string{``, `   `, "\n", `{"a":`, `nul`, `{"a" 1}`, `{1:2}`} {
		if _, err := Canonicalize([]byte(in)); err == nil {
			t.Errorf("Canonicalize(%q) accepted malformed input", in)
		}
	}
}

func TestMarshalCanonicalGolden(t *testing.T) {
	type rec struct {
		Z string         `json:"z"`
		A int64          `json:"a"`
		F float64        `json:"f"`
		M map[string]int `json:"m"`
		P *string        `json:"p"`
		B []byte         `json:"b"`
		T []string       `json:"t"`
	}
	got, err := MarshalCanonical(rec{Z: "z", A: 9007199254740993, F: 1e21, M: map[string]int{"y": 2, "x": 1}, B: []byte{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"a":9007199254740993,"b":"AQI=","f":1e+21,"m":{"x":1,"y":2},"p":null,"t":null,"z":"z"}`
	if string(got) != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	got, err = MarshalCanonical(map[string]string{"s": "<a>&"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"s":"<a>&"}` {
		t.Fatalf("html escaping leaked through: %s", got)
	}
	got, err = MarshalCanonical(json.RawMessage(`{"b":1,"a":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":2,"b":1}` {
		t.Fatalf("raw message not sorted: %s", got)
	}
}

func TestMarshalCanonicalErrors(t *testing.T) {
	if _, err := MarshalCanonical(math.NaN()); err == nil {
		t.Fatal("NaN accepted")
	}
	if _, err := MarshalCanonical(func() {}); err == nil {
		t.Fatal("func accepted")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	if _, err := MarshalCanonical(cycle); err == nil {
		t.Fatal("cyclic value accepted")
	}
}

func TestMarshalCanonicalRejectsInvalidUTF8BeforeReplacement(t *testing.T) {
	bad := string([]byte{0xff, 0xfe})
	type hidden struct{ S string }
	values := []any{
		struct {
			S string `json:"s"`
		}{S: bad},
		map[string]string{bad: "map key"},
		[]string{"nested", bad},
		struct{ hidden }{hidden{S: bad}},
	}
	for _, value := range values {
		if _, err := MarshalCanonical(value); !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("MarshalCanonical(%T) err = %v, want ErrInvalidUTF8", value, err)
		}
	}
	if got, err := MarshalCanonical([]byte{0xff}); err != nil || string(got) != `"/w=="` {
		t.Fatalf("byte slice = %s, %v; want base64 JSON string", got, err)
	}
}

func FuzzCanonicalize(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`null`),
		[]byte(`{"b":1,"a":[true,{"x":"é"}]}`),
		[]byte(`{"a":1,"a":2}`),
		[]byte(`{"s":"\ud800"}`),
		{'"', 0xff, '"'},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		got, err := Canonicalize(raw)
		if err != nil {
			return
		}
		again, err := Canonicalize(got)
		if err != nil {
			t.Fatalf("canonical output rejected: %v\n%s", err, got)
		}
		if !bytes.Equal(again, got) {
			t.Fatalf("not idempotent:\nfirst  %s\nsecond %s", got, again)
		}
	})
}
