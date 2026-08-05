package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// termHarness drives a real x/term.Terminal through a keyFilter so the tests
// assert against upstream's actual parser rather than a model of it.
type termHarness struct{ r io.Reader }

func (h termHarness) Read(p []byte) (int, error)  { return h.r.Read(p) }
func (h termHarness) Write(p []byte) (int, error) { return len(p), nil }

// segment is one line the Terminal produced together with the provenance the
// filter reported for it.
type segment struct {
	text    string
	inPaste bool
}

// runTerminal drives in through a keyFilter into a real Terminal, popping the
// segment flag after each returned line exactly as composition will. A missing
// flag is a hard failure: a line that finds none would silently adopt the next
// segment's provenance.
func runTerminal(t *testing.T, in string) (segs []segment, err error) {
	t.Helper()
	return runTerminalReader(t, strings.NewReader(in))
}

func runTerminalReader(t *testing.T, r io.Reader) (segs []segment, err error) {
	t.Helper()
	f := newKeyFilter(r, maxGoalBytes)
	tm := term.NewTerminal(termHarness{f}, "> ")
	tm.SetBracketedPasteMode(true)

	type result struct {
		segs []segment
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var got []segment
		var last error
		for i := 0; i < 200; i++ {
			line, rerr := tm.ReadLine()
			if rerr != nil && !errors.Is(rerr, term.ErrPasteIndicator) {
				last = rerr
				break
			}
			inPaste, ok := f.PopSegmentFlag()
			if !ok {
				last = errors.New("line returned with no segment flag")
				break
			}
			got = append(got, segment{line, inPaste})
		}
		done <- result{got, last}
	}()

	select {
	case res := <-done:
		return res.segs, res.err
	case <-time.After(5 * time.Second):
		t.Fatalf("no progress within the timeout")
		return nil, nil
	}
}

func texts(segs []segment) []string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s.text
	}
	return out
}

func provenance(segs []segment) []bool {
	out := make([]bool, len(segs))
	for i, s := range segs {
		out[i] = s.inPaste
	}
	return out
}

func TestTerminalSegmentProvenance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		in    string
		texts []string
		paste []bool
	}{
		{"plain", "a\nb\n", []string{"a", "b"}, []bool{false, false}},
		{"two goals one read", "goal1\ngoal2\n", []string{"goal1", "goal2"}, []bool{false, false}},
		{"paste", pasteOn + "a\nb\n" + pasteOff + "c\n", []string{"a", "b", "c"}, []bool{true, true, false}},
		{"crlf paste", pasteOn + "a\r\nb\r\n" + pasteOff, []string{"a", "b"}, []bool{true, true}},
		{"typed prefix then paste", "fix " + pasteOn + "a\nb\n" + pasteOff, []string{"fix a", "b"}, []bool{true, true}},
		{"alternating typed and pasted", "t1\n" + pasteOn + "p1\n" + pasteOff + "t2\n" + pasteOn + "p2\n" + pasteOff + "t3\n",
			[]string{"t1", "p1", "t2", "p2", "t3"}, []bool{false, true, false, true, false}},
		{"typed esc before lf", "foo\x1b\nbar\n", []string{"foo", "bar"}, []bool{false, false}},
		{"typed esc before cr", "foo\x1b\rbar\r", []string{"foo", "bar"}, []bool{false, false}},
		{"pasted esc before lf", pasteOn + "foo\x1b\nbar\n" + pasteOff, []string{"foo", "bar"}, []bool{true, true}},
		{"csi with cr inside", "\x1b[\ra\n", []string{"", "a"}, []bool{false, false}},
		{"esc then esc then newline", "\x1b\x1b\na\n", []string{"", "a"}, []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			segs, _ := runTerminal(t, tc.in)
			if got := texts(segs); !equalStrings(got, tc.texts) {
				t.Errorf("lines = %q, want %q", got, tc.texts)
			}
			if got := provenance(segs); !equalBools(got, tc.paste) {
				t.Errorf("provenance = %v, want %v", got, tc.paste)
			}
		})
	}
}

func TestTerminalOneEventPerDeliveryLeavesNoRemainder(t *testing.T) {
	// Framing's core guarantee: a delivery never extends past a line
	// terminator, so x/term cannot hold a second complete line and one pending
	// flag suffices. Exactly one flag must be available after each line.
	f := newKeyFilter(strings.NewReader("goal1\ngoal2\n"), maxGoalBytes)
	tm := term.NewTerminal(termHarness{f}, "> ")
	tm.SetBracketedPasteMode(true)

	first, err := tm.ReadLine()
	if err != nil || first != "goal1" {
		t.Fatalf("first = %q err = %v", first, err)
	}
	if _, ok := f.PopSegmentFlag(); !ok {
		t.Fatal("first line had no flag")
	}
	if _, ok := f.PopSegmentFlag(); ok {
		t.Fatal("a second flag was queued; the delivery ran past its terminator")
	}
	second, err := tm.ReadLine()
	if err != nil || second != "goal2" {
		t.Fatalf("second = %q err = %v", second, err)
	}
	if _, ok := f.PopSegmentFlag(); !ok {
		t.Fatal("second line had no flag")
	}
}

func TestTerminalSwallowedTerminatorIsDetected(t *testing.T) {
	// If x/term ever consumes a terminator without producing a line, its next
	// Read must fail loudly rather than let every later segment inherit the
	// wrong provenance.
	f := newKeyFilter(strings.NewReader("a\nb\n"), maxGoalBytes)
	tm := term.NewTerminal(termHarness{f}, "> ")
	tm.SetBracketedPasteMode(true)
	if _, err := tm.ReadLine(); err != nil {
		t.Fatalf("first ReadLine: %v", err)
	}
	// Deliberately do not pop the flag, simulating a swallowed terminator.
	if _, err := tm.ReadLine(); !errors.Is(err, errTerminatorSwallowed) {
		t.Fatalf("err = %v, want errTerminatorSwallowed", err)
	}
}

func TestTerminalNoLivelockOnLongEscape(t *testing.T) {
	for _, span := range []int{253, 254, 255, 256, 257, 300, 1000} {
		in := "\x1b" + strings.Repeat("1", span) + "a\n"
		segs, _ := runTerminal(t, in)
		if len(segs) == 0 {
			t.Fatalf("span %d: no line returned", span)
		}
	}
}

func TestTerminalArrowKeysStillEdit(t *testing.T) {
	segs, _ := runTerminal(t, "ac\x1b[Db\r")
	if got := texts(segs); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("lines = %q, want [abc]; left-arrow editing broke", got)
	}
}

func TestTerminalNonEmptyLineCtrlDKeepsReading(t *testing.T) {
	// Ctrl-D on a non-empty line deletes a character and x/term reads again.
	// No pending-flag guard may fire for it. io.EOF at the end of the fixture
	// is expected; errTerminatorSwallowed is not.
	segs, err := runTerminal(t, "ab\x04c\n")
	if errors.Is(err, errTerminatorSwallowed) {
		t.Fatalf("Ctrl-D on a non-empty line tripped the pending-flag guard")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if got := texts(segs); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("lines = %q, want [abc]", got)
	}
}

func TestTerminalRejectsInvalidUTF8(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want error
	}{
		{"invalid lead byte", "\xb2\n", errInvalidUTF8},
		{"lone continuation", "a\x80b\n", errInvalidUTF8},
		{"truncated two byte", "a\xc3", errInvalidUTF8},
		{"truncated three byte", "a\xe4\xb8", errInvalidUTF8},
		{"truncated four byte", "a\xf0\x9d\x84", errInvalidUTF8},
		{"overlong", "a\xc0\xaf\n", errInvalidUTF8},
		{"surrogate half", "a\xed\xa0\x80\n", errInvalidUTF8},
		{"literal replacement char", "a�b\n", errLiteralReplacement},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runTerminal(t, tc.in)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestTerminalRejectionPreservesLaterTypeahead(t *testing.T) {
	// The offending line is drained; typeahead after it survives and is
	// classified correctly once the Terminal is retired and the filter reset.
	for _, tc := range []struct {
		name  string
		in    string
		after []string
		paste []bool
	}{
		{"typed then clean", "bad\xb2input\nclean\n", []string{"clean"}, []bool{false}},
		{"pasted then clean", pasteOn + "bad\xb2\n" + pasteOff + "clean\n", []string{"clean"}, []bool{false}},
		{"invalid then pasted goal", "bad\xb2\n" + pasteOn + "p\n" + pasteOff, []string{"p"}, []bool{true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKeyFilter(strings.NewReader(tc.in), maxGoalBytes)
			tm := term.NewTerminal(termHarness{f}, "> ")
			tm.SetBracketedPasteMode(true)
			if _, err := tm.ReadLine(); !errors.Is(err, errInvalidUTF8) {
				t.Fatalf("first read err = %v, want errInvalidUTF8", err)
			}

			// Retire the Terminal only. reject already reset the coupled
			// filter state and preserved the typeahead after the offending
			// line; calling Discard here would throw that typeahead away.
			tm = term.NewTerminal(termHarness{f}, "> ")
			tm.SetBracketedPasteMode(true)

			var got []segment
			for i := 0; i < 8; i++ {
				line, err := tm.ReadLine()
				if err != nil && !errors.Is(err, term.ErrPasteIndicator) {
					break
				}
				inPaste, ok := f.PopSegmentFlag()
				if !ok {
					t.Fatal("line returned with no flag after reset")
				}
				got = append(got, segment{line, inPaste})
			}
			if g := texts(got); !equalStrings(g, tc.after) {
				t.Fatalf("after rejection lines = %q, want %q", g, tc.after)
			}
			if g := provenance(got); !equalBools(g, tc.paste) {
				t.Fatalf("after rejection provenance = %v, want %v", g, tc.paste)
			}
		})
	}
}

// blockingReader delivers its chunk then blocks forever, modelling a live tty
// where no further key has been pressed.
type blockingReader struct {
	data []byte
	done bool
	hold chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if !b.done {
		b.done = true
		return copy(p, b.data), nil
	}
	<-b.hold
	return 0, io.EOF
}

func TestTerminalRejectionDoesNotWaitForAnotherKeystroke(t *testing.T) {
	// A complete but invalid line must be rejected immediately. Upstream would
	// have held it until another read returned bytes.
	r := &blockingReader{data: []byte("\xb2\n"), hold: make(chan struct{})}
	defer close(r.hold)
	if _, err := runTerminalReader(t, r); !errors.Is(err, errInvalidUTF8) {
		t.Fatalf("err = %v, want errInvalidUTF8 without another keystroke", err)
	}
}

func TestTerminalDiscardClearsFlagAndBufferTogether(t *testing.T) {
	f := newKeyFilter(strings.NewReader("a\nb\n"), maxGoalBytes)
	tm := term.NewTerminal(termHarness{f}, "> ")
	tm.SetBracketedPasteMode(true)
	if _, err := tm.ReadLine(); err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	f.Discard()
	if _, ok := f.PopSegmentFlag(); ok {
		t.Fatal("Discard left a flag behind")
	}
	if got := f.TakeTerminator(); got != termNone {
		t.Fatalf("Discard left terminator %v", got)
	}
}

func TestTerminalPastedContentSurvivesEscapeStripping(t *testing.T) {
	segs, _ := runTerminal(t, pasteOn+"al\x1bpha\nbeta\n"+pasteOff)
	if got := texts(segs); len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("lines = %q, want [alpha beta]", got)
	}
}

func TestTerminalRedundantPasteStartNotForwarded(t *testing.T) {
	segs, _ := runTerminal(t, pasteOn+pasteOn+"abc\n"+pasteOff)
	if got := texts(segs); len(got) != 1 || got[0] != "abc" {
		t.Fatalf("lines = %q, want [abc]", got)
	}
}

func FuzzTerminalSegmentProvenance(f *testing.F) {
	// Inputs are built from a provenance-aware generator rather than raw
	// bytes, so the oracle can catch a stale flag that shifts every later
	// segment without ever under-counting -- which a flags-versus-lines count
	// cannot see.
	f.Add(uint16(0x0000), "ab")
	f.Add(uint16(0xAAAA), "x")
	f.Add(uint16(0xFFFF), "hello")
	f.Add(uint16(0x1234), "é中")
	f.Fuzz(func(t *testing.T, shape uint16, body string) {
		if !utf8.ValidString(body) || strings.ContainsAny(body, "\r\n\x1b\x03\x04") ||
			strings.ContainsRune(body, utf8.RuneError) || body == "" || len(body) > 16 {
			t.Skip()
		}
		var in strings.Builder
		var want []bool
		for i := 0; i < 8; i++ {
			pasted := shape&(1<<i) != 0
			if pasted {
				in.WriteString(pasteOn)
			}
			in.WriteString(body)
			in.WriteString("\n")
			if pasted {
				in.WriteString(pasteOff)
			}
			want = append(want, pasted)
		}
		segs, err := runTerminalNoFatal(strings.NewReader(in.String()))
		if err != nil {
			t.Fatalf("unexpected error %v on %q", err, in.String())
		}
		if got := provenance(segs); !equalBools(got, want) {
			t.Fatalf("provenance = %v, want %v", got, want)
		}
	})
}

// runTerminalNoFatal is the fuzz-safe form: it reports errors instead of
// calling t.Fatalf from a goroutine.
func runTerminalNoFatal(r io.Reader) ([]segment, error) {
	f := newKeyFilter(r, maxGoalBytes)
	tm := term.NewTerminal(termHarness{f}, "> ")
	tm.SetBracketedPasteMode(true)
	var got []segment
	for i := 0; i < 512; i++ {
		line, err := tm.ReadLine()
		if err != nil && !errors.Is(err, term.ErrPasteIndicator) {
			if errors.Is(err, io.EOF) {
				return got, nil
			}
			return got, err
		}
		inPaste, ok := f.PopSegmentFlag()
		if !ok {
			return got, errors.New("line returned with no segment flag")
		}
		got = append(got, segment{line, inPaste})
	}
	return got, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
