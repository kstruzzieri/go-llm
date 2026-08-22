package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticOfWrappedChain(t *testing.T) {
	base := fmt.Errorf("config: model %q: type is required", "agent")
	err := fmt.Errorf("outer: %w", diagWrap(CodeModelInvalid, SubjectRole, "agent", base))
	d, ok := DiagnosticOf(err)
	if !ok {
		t.Fatal("expected diagnostic")
	}
	if d.Code != CodeModelInvalid || d.SubjectKind != SubjectRole || d.Subject != "agent" {
		t.Fatalf("got %+v", d)
	}
}

func TestDiagnosticErrorTextUnchanged(t *testing.T) {
	base := fmt.Errorf("config: provider %q: base_url is required", "p")
	err := diagWrap(CodeProviderEndpointInvalid, SubjectProvider, "p", base)
	if err.Error() != base.Error() {
		t.Fatalf("wrapper altered message: %q", err.Error())
	}
	if !errors.Is(err, base) {
		t.Fatal("wrapper must unwrap to base")
	}
}

func TestDiagnosticOfSentinels(t *testing.T) {
	cases := []struct {
		err  error
		code ErrorCode
	}{
		{fmt.Errorf("x: %w", ErrRevisionConflict), CodeRevisionConflict},
		{fmt.Errorf("x: %w", ErrDurabilityUncertain), CodeDurabilityUncertain},
		{fmt.Errorf("x: %w", ErrConfigNotFound), CodeConfigNotFound},
	}
	for _, tc := range cases {
		d, ok := DiagnosticOf(tc.err)
		if !ok || d.Code != tc.code {
			t.Fatalf("%v: got %+v ok=%v", tc.err, d, ok)
		}
	}
	if _, ok := DiagnosticOf(errors.New("plain")); ok {
		t.Fatal("plain error must not classify")
	}
	if _, ok := DiagnosticOf(nil); ok {
		t.Fatal("nil must not classify")
	}
}

func TestSanitizeSubjectOrderAndBound(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"clean", "agent", "agent"},
		{"control", "a\x00b\u202ec", "a�b�c"}, // Cc then bidi Cf
		{"invalid-utf8", "a\xffb", "a�b"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := sanitizeSubject(tc.in); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
	// Growth-then-truncate: 62 ASCII + 1 control byte -> repair grows to 65
	// bytes (U+FFFD is 3 bytes), truncation walks back over the partial
	// U+FFFD to exactly the 62 ASCII bytes.
	if got := sanitizeSubject(strings.Repeat("a", 62) + "\x00"); got != strings.Repeat("a", 62) {
		t.Fatalf("growth case: got %q", got)
	}
	// 2-byte runes: 80 bytes in, the cut at 64 lands on a rune boundary.
	if got := sanitizeSubject(strings.Repeat("é", 40)); got != strings.Repeat("é", 32) {
		t.Fatalf("2-byte case: got %q", got)
	}
	// 4-byte runes: 68 bytes in, exactly 16 runes (64 bytes) survive.
	if got := sanitizeSubject(strings.Repeat("🜁", 17)); got != strings.Repeat("🜁", 16) {
		t.Fatalf("4-byte case: got %q", got)
	}
	// Max walk-back: 65 bytes in, the cut at 64 lands 3 bytes into the
	// final rune (which starts at byte 61), so truncation drops it whole.
	if got := sanitizeSubject("a" + strings.Repeat("🜁", 16)); got != "a"+strings.Repeat("🜁", 15) {
		t.Fatalf("max walk-back case: got %q", got)
	}
}

func TestDiagnosticOfPrecedence(t *testing.T) {
	inner := diagWrap(CodeModelInvalid, SubjectRole, "inner", errors.New("base"))
	outer := diagWrap(CodeProviderNotFound, SubjectProvider, "outer", inner)
	d, ok := DiagnosticOf(outer)
	if !ok || d.Code != CodeProviderNotFound || d.SubjectKind != SubjectProvider || d.Subject != "outer" {
		t.Fatalf("double wrap: got %+v ok=%v", d, ok)
	}
	// A diagError anywhere in the chain beats the sentinel fallback.
	mixed := diagWrap(CodeTargetExists, SubjectNone, "", fmt.Errorf("x: %w", ErrRevisionConflict))
	d, ok = DiagnosticOf(mixed)
	if !ok || d.Code != CodeTargetExists {
		t.Fatalf("mixed chain: got %+v ok=%v", d, ok)
	}
}

func TestDiagnosticSitesLoadAndSave(t *testing.T) {
	// discovery: GO_LLM_CONFIG set but empty
	t.Setenv("GO_LLM_CONFIG", "")
	_, err := DefaultDocument()
	assertDiag(t, err, CodeConfigDiscoveryInvalid, SubjectNone, "")

	// io: DefaultDocument read failure (env points at a missing file)
	t.Setenv("GO_LLM_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	_, err = DefaultDocument()
	assertDiag(t, err, CodeIO, SubjectNone, "")

	// parse failure
	_, err = NewDocumentFromBytes([]byte("{nope"), Origin{Source: OriginExplicit})
	assertDiag(t, err, CodeParseError, SubjectNone, "")

	// io: LoadDocument on a missing path
	missing := filepath.Join(t.TempDir(), "absent.json")
	_, err = LoadDocument(missing)
	assertDiag(t, err, CodeIO, SubjectNone, "")
	// Legacy Load is a Firn Phase-2 entry point too; it gets the same codes.
	_, err = Load(missing)
	assertDiag(t, err, CodeIO, SubjectNone, "")
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(bad)
	assertDiag(t, err, CodeParseError, SubjectNone, "")

	// target_exists: SaveNew onto an existing file
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "m.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	assertDiag(t, d.SaveNew(p), CodeTargetExists, SubjectNone, "")

	// revision_conflict via wrapped sentinel at source
	assertDiag(t, d.SaveReplace(p, "deadbeef"), CodeRevisionConflict, SubjectNone, "")
}

func assertDiag(t *testing.T, err error, code ErrorCode, kind SubjectKind, subject string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error with code %s, got nil", code)
	}
	d, ok := DiagnosticOf(err)
	if !ok || d.Code != code || d.SubjectKind != kind || d.Subject != subject {
		t.Fatalf("err=%v: got %+v ok=%v want {%s %s %q}", err, d, ok, code, kind, subject)
	}
}
