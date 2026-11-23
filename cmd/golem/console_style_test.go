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
	defer r.Close()
	defer w.Close()
	if got := styleFor(w, false); got != (consoleStyle{false, 80}) {
		t.Fatalf("pipe: %+v", got)
	}
}

func TestStyleForWidthFallback(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
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
	defer f.Close()
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	if got := styleForOps(sizeOps{120, nil}, f, false); got != (consoleStyle{true, 120}) {
		t.Fatalf("color permitted: %+v", got)
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
	if got := (consoleStyle{false, 10}).rule("model servers", "x"); !strings.HasSuffix(got, " \u2500\u2500\u2500") {
		t.Fatalf("narrow: %q", got)
	}
	if got, want := s.rule("t", ""), "\u2500\u2500 t "+strings.Repeat("\u2500", 75); got != want {
		t.Fatalf("no subtitle: %q, want %q", got, want)
	}
	c := consoleStyle{true, 80}
	wantColor := "\x1b[2m\u2500\u2500 \x1b[0m\x1b[1mt\x1b[0m \x1b[2m\u2500\u2500 s " + strings.Repeat("\u2500", 70) + "\x1b[0m"
	if got := c.rule("t", "s"); got != wantColor {
		t.Fatalf("color: %q, want %q", got, wantColor)
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
