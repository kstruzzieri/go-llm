package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/term"
)

type sizeOps struct {
	w   int
	err error
}

type plainConsoleOps struct{ sizeOps }

func (plainConsoleOps) supportsANSI(int) bool    { return false }
func (sizeOps) supportsANSI(int) bool            { return true }
func (sizeOps) IsTerminal(int) bool              { return true }
func (sizeOps) MakeRaw(int) (*term.State, error) { return nil, nil }
func (sizeOps) Restore(int, *term.State) error   { return nil }
func (o sizeOps) GetSize(int) (int, int, error)  { return o.w, 24, o.err }

func TestStyleForNonTerminal(t *testing.T) {
	if got := styleFor(&bytes.Buffer{}, false); got != (consoleStyle{false, 80}) {
		t.Fatalf("buffer: %+v", got)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()
	if got := styleFor(w, false); got != (consoleStyle{false, 80}) {
		t.Fatalf("pipe: %+v", got)
	}
}

func TestStyleForWidthFallback(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, tc := range []struct {
		name string
		ops  sizeOps
		want int
	}{
		{"error", sizeOps{0, errors.New("x")}, 80},
		{"zero", sizeOps{0, nil}, 80},
		{"narrow", sizeOps{20, nil}, 40},
		{"wide", sizeOps{120, nil}, 120},
	} {
		if got := styleForOps(tc.ops, f, true).width; got != tc.want {
			t.Errorf("%s: width %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestStyleForColor(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	if got := styleForOps(sizeOps{120, nil}, f, false); got != (consoleStyle{true, 120}) {
		t.Fatalf("color permitted: %+v", got)
	}
	if got := styleForOps(plainConsoleOps{sizeOps{120, nil}}, f, false); got != (consoleStyle{false, 120}) {
		t.Fatalf("console without ANSI processing: %+v", got)
	}
	if got := styleForOps(sizeOps{120, nil}, f, true); got != (consoleStyle{false, 120}) {
		t.Fatalf("noColor: %+v", got)
	}
}

func TestPaint(t *testing.T) {
	if got := (consoleStyle{false, 80}).paint(sgrBold, "x"); got != "x" {
		t.Fatalf("off: %q", got)
	}
	if got := (consoleStyle{true, 80}).paint(sgrBold, "x"); got != "\x1b[1mx\x1b[0m" {
		t.Fatalf("on: %q", got)
	}
}

func TestRule(t *testing.T) {
	s := consoleStyle{false, 80}
	got := s.rule("model servers", "where this session's model requests can go")
	if n := strings.Count(got, "\u2500"); n != 4+17 {
		t.Fatalf("dashes %d in %q", n, got)
	}
	if !strings.HasSuffix(got, strings.Repeat("\u2500", 17)) || strings.HasSuffix(got, strings.Repeat("\u2500", 18)) {
		t.Fatalf("fill: %q", got)
	}
	// Too narrow for the title: at least three dashes, and the subtitle on
	// its own line.
	if got, want := (consoleStyle{false, 10}).rule("model servers", "x"), "\u2500\u2500 model servers \u2500\u2500\u2500\nx"; got != want {
		t.Fatalf("narrow: %q, want %q", got, want)
	}
	// At the 40-column floor a subtitle that does not fit on the rule moves
	// under the title-only rule, which fills exactly the width.
	const index = "where golem index sends model requests" // 38 columns
	titleOnly40 := "\u2500\u2500 model servers " + strings.Repeat("\u2500", 23)
	if got, want := (consoleStyle{false, 40}).rule("model servers", index), titleOnly40+"\n"+index; got != want {
		t.Fatalf("width 40: %q, want %q", got, want)
	}
	// The session subtitle is 42 columns: wider than 40 on its own, so it is
	// dropped and the title-only rule stands alone.
	const session = "where this session's model requests can go"
	if got := (consoleStyle{false, 40}).rule("model servers", session); got != titleOnly40 {
		t.Fatalf("width 40: %q, want %q", got, titleOnly40)
	}
	// The session prefix is 63 columns: 63 + 3 = 66 is the narrowest width
	// that keeps the subtitle on the rule; at 65 it takes its own line.
	if got := (consoleStyle{false, 66}).rule("model servers", session); !strings.HasSuffix(got, "go \u2500\u2500\u2500") || strings.Contains(got, "\n") {
		t.Fatalf("width 66 moved the subtitle off the rule: %q", got)
	}
	if got, want := (consoleStyle{false, 65}).rule("model servers", session), "\u2500\u2500 model servers "+strings.Repeat("\u2500", 48)+"\n"+session; got != want {
		t.Fatalf("width 65: %q, want %q", got, want)
	}
	if got, want := s.rule("t", ""), "\u2500\u2500 t "+strings.Repeat("\u2500", 75); got != want {
		t.Fatalf("no subtitle: %q, want %q", got, want)
	}
	c := consoleStyle{true, 80}
	wantColor := "\x1b[2m\u2500\u2500 \x1b[0m\x1b[1mt\x1b[0m \x1b[2m\u2500\u2500 s " + strings.Repeat("\u2500", 70) + "\x1b[0m"
	if got := c.rule("t", "s"); got != wantColor {
		t.Fatalf("color: %q, want %q", got, wantColor)
	}
	// A subtitle on its own line is dim too.
	wantColor = "\x1b[2m\u2500\u2500 \x1b[0m\x1b[1mt\x1b[0m \x1b[2m" + strings.Repeat("\u2500", 35) + "\x1b[0m\n\x1b[2m" + index + "\x1b[0m"
	if got := (consoleStyle{true, 40}).rule("t", index); got != wantColor {
		t.Fatalf("color, subtitle below: %q, want %q", got, wantColor)
	}
}

func TestWrapList(t *testing.T) {
	got := wrapList([]string{"chat (fallback)", "summaries", "planning"}, "used for  ", "          ", 30)
	want := []string{"used for  chat (fallback),", "          summaries, planning"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
	items := []string{"aa", "bb"}
	if got := wrapList(items, "> ", "  ", 8); strings.Join(got, "|") != "> aa, bb" {
		t.Fatalf("exact width: %q", got)
	}
	if got := wrapList(items, "> ", "  ", 7); strings.Join(got, "|") != "> aa,|  bb" {
		t.Fatalf("one under: %q", got)
	}
	got = wrapList([]string{"a", strings.Repeat("x", 50), "b"}, "> ", "  ", 20)
	want = []string{"> a,", "  " + strings.Repeat("x", 50) + ",", "  b"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("long: got %q", got)
	}
}

func TestWrapWords(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		want  []string
	}{
		{"aa bb", 5, []string{"aa bb"}},                     // exactly the width: one line
		{"aa bb", 4, []string{"aa", "bb"}},                  // one under: breaks at the space
		{"a  b\tc", 80, []string{"a b c"}},                  // whitespace runs collapse
		{"a xxxxxxxx b", 4, []string{"a", "xxxxxxxx", "b"}}, // an over-long word stands alone, whole
		{"xxxxxxxx", 4, []string{"xxxxxxxx"}},
	} {
		if got := wrapWords(tc.text, tc.width); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("wrapWords(%q, %d) = %q, want %q", tc.text, tc.width, got, tc.want)
		}
	}
}

func TestDisplayFieldExact(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\nb", `a\nb`},
		{"a\tb", `a\tb`},
		{"a\u202eb", "a\\u202eb"},
		{"a\xffb", "a\ufffdb"},
		{"e\u0301\u0301\u0301", "e\u0301\u0301" + "\\u0301"},
		{"ab", "ab"},
	} {
		if got := displayField(tc.in); got != tc.want {
			t.Errorf("displayField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
