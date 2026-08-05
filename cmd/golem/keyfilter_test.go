package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"
)

// chunkReader delivers a fixed sequence of chunks, then a terminal error. A
// chunk may carry data alongside that error when withErrOnLast is set, which is
// the (n > 0, err) case x/term mishandles if keyFilter forwards it.
type chunkReader struct {
	chunks        [][]byte
	err           error
	withErrOnLast bool
	i             int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		if r.err == nil {
			return 0, io.EOF
		}
		return 0, r.err
	}
	c := r.chunks[r.i]
	r.i++
	n := copy(p, c)
	if n < len(c) {
		// Put the unread tail back so a small p cannot silently drop bytes.
		rest := make([]byte, len(c)-n)
		copy(rest, c[n:])
		r.chunks[r.i-1] = rest
		r.i--
		return n, nil
	}
	if r.withErrOnLast && r.i == len(r.chunks) {
		e := r.err
		if e == nil {
			e = io.EOF
		}
		return n, e
	}
	return n, nil
}

// zeroThenReader returns (0, nil) a fixed number of times before delivering
// data. io.Reader permits it; keyFilter must retry rather than pass it through.
type zeroThenReader struct {
	zeros int
	data  []byte
	done  bool
}

func (r *zeroThenReader) Read(p []byte) (int, error) {
	if r.zeros > 0 {
		r.zeros--
		return 0, nil
	}
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), nil
}

// drainedFlags records the flags drain consumed, so popFlags can report them
// after the fact. Under single-event framing a consumer must claim each flag
// before the next Read, so drain cannot simply leave them queued. No test here
// runs in parallel, so a package-level map is safe scaffolding.
var drainedFlags = map[*keyFilter][]bool{}

// drain reads f to completion, claiming each segment flag as it is published
// exactly as a real consumer must, and returns delivered bytes plus the
// terminal error.
func drain(t *testing.T, f *keyFilter) ([]byte, error) {
	t.Helper()
	var out []byte
	var flags []bool
	buf := make([]byte, 64)
	for i := 0; i < 10000; i++ {
		n, err := f.Read(buf)
		if n == 0 && err == nil {
			t.Fatalf("Read returned (0, nil) with non-empty buffer")
		}
		out = append(out, buf[:n]...)
		if inPaste, ok := f.PopSegmentFlag(); ok {
			flags = append(flags, inPaste)
		}
		if err != nil {
			drainedFlags[f] = flags
			return out, err
		}
	}
	t.Fatalf("drain did not terminate")
	return nil, nil
}

// popFlags reports every segment flag observed for f: those drain already
// claimed, followed by any still outstanding.
func popFlags(f *keyFilter) []bool {
	got := drainedFlags[f]
	delete(drainedFlags, f)
	for {
		inPaste, ok := f.PopSegmentFlag()
		if !ok {
			return got
		}
		got = append(got, inPaste)
	}
}

func newTestFilter(r io.Reader) *keyFilter { return newKeyFilter(r, maxGoalBytes) }

const (
	pasteOn  = "\x1b[200~"
	pasteOff = "\x1b[201~"
)

func TestKeyFilterInvalidPasteLimitPanics(t *testing.T) {
	for _, limit := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("newKeyFilter(%d) did not panic", limit)
				}
			}()
			newKeyFilter(strings.NewReader(""), limit)
		}()
	}
}

func TestKeyFilterForwardsMarkersAndTracksPasteState(t *testing.T) {
	// Markers must reach x/term unchanged or it never enters pasteActive and
	// treats pasted newlines as Enter presses.
	in := pasteOn + "a\n" + pasteOff + "b\n"
	f := newTestFilter(strings.NewReader(in))
	out, err := drain(t, f)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if string(out) != in {
		t.Fatalf("out = %q, want %q (markers must pass through unchanged)", out, in)
	}
	if got, want := popFlags(f), []bool{true, false}; !equalBools(got, want) {
		t.Fatalf("flags = %v, want %v", got, want)
	}
}

func TestKeyFilterRepeatedPasteStartDoesNotNest(t *testing.T) {
	// x/term models pasteActive as a boolean; a depth counter would leave the
	// filter still "in paste" after the single end marker.
	in := pasteOn + pasteOn + "a\n" + pasteOff + "b\n"
	f := newTestFilter(strings.NewReader(in))
	if _, err := drain(t, f); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if got, want := popFlags(f), []bool{true, false}; !equalBools(got, want) {
		t.Fatalf("flags = %v, want %v", got, want)
	}
}

func TestKeyFilterMarkerSplitAtEveryByteBoundary(t *testing.T) {
	in := "x" + pasteOn + "a\nb\n" + pasteOff + "y\n"
	for split := 1; split < len(in); split++ {
		f := newTestFilter(&chunkReader{chunks: [][]byte{
			[]byte(in[:split]), []byte(in[split:]),
		}})
		out, err := drain(t, f)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("split %d: err = %v", split, err)
		}
		if string(out) != in {
			t.Fatalf("split %d: out = %q, want %q", split, out, in)
		}
		if got, want := popFlags(f), []bool{true, true, false}; !equalBools(got, want) {
			t.Fatalf("split %d: flags = %v, want %v", split, got, want)
		}
	}
}

func TestKeyFilterPartialMarkerAtEOFSurfacesLiterally(t *testing.T) {
	// A truncated prefix must not be swallowed, and EOF must not ride along
	// with the bytes: x/term drops n when err != nil.
	in := "ab\x1b[20"
	f := newTestFilter(strings.NewReader(in))
	out, err := drain(t, f)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if string(out) != in {
		t.Fatalf("out = %q, want %q", out, in)
	}
}

func TestKeyFilterDefersTerminalErrorUntilBytesDelivered(t *testing.T) {
	custom := errors.New("boom")
	for _, tc := range []struct {
		name string
		err  error
	}{{"eof", io.EOF}, {"custom", custom}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFilter(&chunkReader{
				chunks:        [][]byte{[]byte("abc\n")},
				err:           tc.err,
				withErrOnLast: true,
			})
			buf := make([]byte, 64)
			n, err := f.Read(buf)
			if err != nil {
				t.Fatalf("first Read returned err %v with n=%d; bytes must come first", err, n)
			}
			if string(buf[:n]) != "abc\n" {
				t.Fatalf("first Read = %q, want %q", buf[:n], "abc\n")
			}
			// Claim the line's flag, as a consumer must before reading again.
			if _, ok := f.PopSegmentFlag(); !ok {
				t.Fatal("delivered line terminator published no flag")
			}
			if _, err := f.Read(buf); !errors.Is(err, tc.err) {
				t.Fatalf("second Read err = %v, want %v", err, tc.err)
			}
		})
	}
}

func TestKeyFilterRetriesEmptyProgress(t *testing.T) {
	f := newTestFilter(&zeroThenReader{zeros: 3, data: []byte("hi\n")})
	out, err := drain(t, f)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if string(out) != "hi\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestKeyFilterEmitsLoneCRImmediately(t *testing.T) {
	// Holding a trailing CR to see whether LF follows would stall Enter until
	// the user types again.
	f := newTestFilter(&chunkReader{chunks: [][]byte{[]byte("ab\r")}})
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("Read err = %v", err)
	}
	if string(buf[:n]) != "ab\r" {
		t.Fatalf("Read = %q, want %q", buf[:n], "ab\r")
	}
	if got, want := popFlags(f), []bool{false}; !equalBools(got, want) {
		t.Fatalf("flags = %v, want %v", got, want)
	}
}

func TestKeyFilterCoalescesCRLF(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks [][]byte
	}{
		{"same buffer", [][]byte{[]byte("a\r\nb\r\n")}},
		{"split buffer", [][]byte{[]byte("a\r"), []byte("\nb\r"), []byte("\n")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFilter(&chunkReader{chunks: tc.chunks})
			out, err := drain(t, f)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("err = %v", err)
			}
			if string(out) != "a\rb\r" {
				t.Fatalf("out = %q, want %q (LF after CR suppressed)", out, "a\rb\r")
			}
			if got, want := popFlags(f), []bool{false, false}; !equalBools(got, want) {
				t.Fatalf("flags = %v, want %v (one flag per logical terminator)", got, want)
			}
		})
	}
}

func TestKeyFilterCoalescesCRLFInsidePaste(t *testing.T) {
	// Terminals send CRLF inside bracketed pastes, so suppression must not be
	// gated on paste state. If it were, each pasted line would queue two flags
	// while x/term returned one line, desynchronizing composition from the
	// first pasted newline onward.
	in := pasteOn + "a\r\nb\r\n" + pasteOff + "c\r\n"
	f := newTestFilter(strings.NewReader(in))
	out, err := drain(t, f)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	want := pasteOn + "a\rb\r" + pasteOff + "c\r"
	if string(out) != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	if got, want := popFlags(f), []bool{true, true, false}; !equalBools(got, want) {
		t.Fatalf("flags = %v, want %v (one flag per pasted line, not two)", got, want)
	}
}

func TestKeyFilterLoneLFStillCounts(t *testing.T) {
	f := newTestFilter(strings.NewReader("a\n\nb\n"))
	out, err := drain(t, f)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if string(out) != "a\n\nb\n" {
		t.Fatalf("out = %q", out)
	}
	if got, want := popFlags(f), []bool{false, false, false}; !equalBools(got, want) {
		t.Fatalf("flags = %v, want %v", got, want)
	}
}

func TestKeyFilterClassifiesTerminators(t *testing.T) {
	// Production consults the terminator the moment Terminal.ReadLine returns,
	// which for Ctrl-C/Ctrl-D is the read that delivered the byte. Draining
	// past it to EOF would be a sequence the editor never performs, and EOF
	// legitimately overwrites (see TestKeyFilterCtrlDLeavesNoStaleTag).
	for _, tc := range []struct {
		name string
		in   string
		want terminator
	}{
		{name: "ctrl-c", in: "ab\x03", want: termCtrlC},
		{name: "ctrl-d", in: "ab\x04", want: termCtrlD},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFilter(strings.NewReader(tc.in))
			buf := make([]byte, 64)
			if _, err := f.Read(buf); err != nil {
				t.Fatalf("Read err = %v", err)
			}
			if got := f.TakeTerminator(); got != tc.want {
				t.Fatalf("TakeTerminator = %v, want %v", got, tc.want)
			}
			if got := f.TakeTerminator(); got != termNone {
				t.Fatalf("second TakeTerminator = %v, want termNone (must consume)", got)
			}
		})
	}

	t.Run("eof", func(t *testing.T) {
		f := newTestFilter(strings.NewReader("ab"))
		if _, err := drain(t, f); !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v", err)
		}
		if got := f.TakeTerminator(); got != termEOF {
			t.Fatalf("TakeTerminator = %v, want termEOF", got)
		}
	})
}

func TestKeyFilterControlBytesInsidePasteAreData(t *testing.T) {
	// x/term gates both keyCtrlC and keyCtrlD on !pasteActive (readLine:820,
	// :826), so the filter must not steal them either.
	//
	// Asserting only on drained bytes plus the final terminator cannot detect
	// the failure: a filter that treats 0x03 as a terminator still delivers
	// every byte, just split across an extra Read, and the trailing EOF
	// overwrites the bogus tag. The distinguishing observation is that no
	// split and no tag occur at all.
	in := pasteOn + "a\x03b\x04c\n" + pasteOff
	f := newTestFilter(strings.NewReader(in))
	buf := make([]byte, 256)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("Read err = %v", err)
	}
	// Framing stops the delivery at the line terminator and nowhere else. A
	// filter that treated 0x03 or 0x04 inside a paste as a terminator would
	// cut the delivery short at one of them instead.
	if want := pasteOn + "a\x03b\x04c\n"; string(buf[:n]) != want {
		t.Fatalf("first Read = %q, want %q; the filter split on a control byte inside a paste", buf[:n], want)
	}
	if got := f.TakeTerminator(); got != termNone {
		t.Fatalf("TakeTerminator = %v, want termNone; a control byte inside a paste was tagged as a terminator", got)
	}
	if got, want := popFlags(f), []bool{true}; !equalBools(got, want) {
		t.Fatalf("flags = %v, want %v", got, want)
	}
	// The paste-end marker trails the line terminator, so it arrives in the
	// next delivery before the stream ends.
	n, err = f.Read(buf)
	if err != nil || string(buf[:n]) != pasteOff {
		t.Fatalf("second Read = %q err = %v, want %q", buf[:n], err, pasteOff)
	}
	if _, err := f.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("third Read err = %v, want io.EOF", err)
	}
}

func TestKeyFilterCtrlDLeavesNoStaleTag(t *testing.T) {
	// Ctrl-D on a non-empty line edits instead of ending the read, so a later
	// genuine EOF must not report the earlier Ctrl-D.
	f := newTestFilter(&chunkReader{chunks: [][]byte{[]byte("ab\x04"), []byte("c\r")}})
	if _, err := drain(t, f); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if got := f.TakeTerminator(); got != termEOF {
		t.Fatalf("TakeTerminator = %v, want termEOF; the Ctrl-D tag went stale", got)
	}
}

func TestKeyFilterCtrlCWinsOverFollowingCtrlD(t *testing.T) {
	f := newTestFilter(strings.NewReader("\x03\x04"))
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("Read err = %v", err)
	}
	if string(buf[:n]) != "\x03" {
		t.Fatalf("Read = %q, want %q (split after the terminator)", buf[:n], "\x03")
	}
	if got := f.TakeTerminator(); got != termCtrlC {
		t.Fatalf("TakeTerminator = %v, want termCtrlC", got)
	}
}

func TestKeyFilterSplitsChunkAfterTerminator(t *testing.T) {
	// Bytes typed after Ctrl-C must not reach the Terminal that is about to be
	// discarded.
	f := newTestFilter(strings.NewReader("ab\x03cde\n"))
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatalf("Read err = %v", err)
	}
	if string(buf[:n]) != "ab\x03" {
		t.Fatalf("Read = %q, want %q", buf[:n], "ab\x03")
	}
	n, err = f.Read(buf)
	if err != nil {
		t.Fatalf("second Read err = %v", err)
	}
	if string(buf[:n]) != "cde\n" {
		t.Fatalf("second Read = %q, want %q", buf[:n], "cde\n")
	}
}

func TestKeyFilterDiscardDropsRetainedBytesAndFlags(t *testing.T) {
	f := newTestFilter(strings.NewReader("ab\x03cde\n"))
	buf := make([]byte, 64)
	if _, err := f.Read(buf); err != nil {
		t.Fatalf("Read err = %v", err)
	}
	f.Discard()
	if got := f.TakeTerminator(); got != termNone {
		t.Fatalf("TakeTerminator after Discard = %v, want termNone", got)
	}
	if _, ok := f.PopSegmentFlag(); ok {
		t.Fatalf("PopSegmentFlag after Discard returned ok=true")
	}
	out, err := drain(t, f)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("out = %q, want empty; retained post-Ctrl-C bytes must be dropped", out)
	}
}

func TestKeyFilterEmptyFlagQueueReportsNotOK(t *testing.T) {
	// An empty FIFO must never look like "a typed newline"; composition relies
	// on the distinction.
	f := newTestFilter(strings.NewReader("abc"))
	if _, err := drain(t, f); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	inPaste, ok := f.PopSegmentFlag()
	if ok {
		t.Fatalf("PopSegmentFlag = (%v, true), want ok=false", inPaste)
	}
}

func TestKeyFilterMarkerLikeSequencesArePassedThrough(t *testing.T) {
	// \x1b[3~ (Delete) and a doubled ESC share a prefix with the paste markers
	// only briefly; neither may be consumed or reordered.
	//
	// Bytes alone are not enough: the doubled-ESC case must also leave the
	// filter genuinely inside the paste the second marker opened, so its flags
	// are asserted too.
	// A sequence that resolves on its own is forwarded untouched. One that
	// does not is closed with the breaker before the byte whose meaning x/term
	// would otherwise swallow — here, the ESC that opens a paste marker.
	for _, tc := range []struct {
		in    string
		want  string
		flags []bool
	}{
		{in: "\x1b[3~", want: "\x1b[3~"},
		{in: "\x1b[2~", want: "\x1b[2~"},
		{in: "\x1b[A", want: "\x1b[A"},
		{in: "\x1b\x1b[200~a\n" + pasteOff, want: "\x1bz\x1b[200~a\n" + pasteOff, flags: []bool{true}},
		{in: "\x1b\x1b[200~a\n" + pasteOff + "b\n", want: "\x1bz\x1b[200~a\n" + pasteOff + "b\n", flags: []bool{true, false}},
	} {
		f := newTestFilter(strings.NewReader(tc.in))
		out, err := drain(t, f)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("%q: err = %v", tc.in, err)
		}
		if string(out) != tc.want {
			t.Fatalf("in %q: out = %q, want %q", tc.in, out, tc.want)
		}
		if got := popFlags(f); !equalBools(got, tc.flags) {
			t.Fatalf("%q: flags = %v, want %v", tc.in, got, tc.flags)
		}
	}
}

func TestKeyFilterBreaksUnresolvedEscapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"before lf", "a\x1b\nb\n", "a\x1bz\nb\n"},
		{"before cr", "a\x1b\rb\r", "a\x1bz\rb\r"},
		{"before ctrl-c", "a\x1b\x03", "a\x1bz\x03"},
		{"before ctrl-d", "a\x1b\x04", "a\x1bz\x04"},
		{"before second esc", "\x1b\x1bA", "\x1bz\x1bA"},
		{"self resolving", "\x1b[1;3C", "\x1b[1;3C"},
		{"partial csi then lf", "\x1b[\na\n", "\x1b[z\na\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFilter(strings.NewReader(tc.in))
			out, _ := drain(t, f)
			if string(out) != tc.want {
				t.Fatalf("out = %q, want %q", out, tc.want)
			}
		})
	}
}

func TestKeyFilterBreaksLongUnresolvedEscapeAtByte256(t *testing.T) {
	// x/term's inBuf is 256 bytes; an escape still unresolved at byte 256
	// leaves a zero-length read and no progress. Measured against v0.42.0: a
	// 254-byte span is safe, 255 and above livelock without the breaker.
	for _, span := range []int{253, 254, 255, 256, 300} {
		f := newTestFilter(strings.NewReader("\x1b" + strings.Repeat("1", span) + "a\n"))
		out, _ := drain(t, f)
		longest := longestUnresolvedEscapeRun(out)
		if longest > 256 {
			t.Fatalf("span %d: longest unresolved escape run = %d bytes, must not exceed 256", span, longest)
		}
	}
}

func TestKeyFilterDropsBareEscapeInsidePaste(t *testing.T) {
	// Inside a paste x/term would consume from the ESC to the next [A-Za-z~],
	// eating pasted newlines and characters, so the byte is unusable. Content
	// around it is preserved and terminators still count.
	in := pasteOn + "al\x1bpha\nbeta\n" + pasteOff
	want := pasteOn + "alpha\nbeta\n" + pasteOff
	f := newTestFilter(strings.NewReader(in))
	out, _ := drain(t, f)
	if string(out) != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	if got, wantFlags := popFlags(f), []bool{true, true}; !equalBools(got, wantFlags) {
		t.Fatalf("flags = %v, want %v", got, wantFlags)
	}
}

func TestKeyFilterConsumesRedundantPasteStart(t *testing.T) {
	in := pasteOn + pasteOn + "abc\n" + pasteOff
	want := pasteOn + "abc\n" + pasteOff
	f := newTestFilter(strings.NewReader(in))
	out, _ := drain(t, f)
	if string(out) != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

// longestUnresolvedEscapeRun returns the greatest number of bytes from an ESC
// up to and including the byte that terminates it under x/term's final-byte
// rule, or to end of input if it never terminates.
func longestUnresolvedEscapeRun(b []byte) int {
	longest, run := 0, -1
	for _, c := range b {
		if run >= 0 {
			run++
			if run > longest {
				longest = run
			}
			if isEscFinal(c) {
				run = -1
				continue
			}
		}
		if c == 0x1b {
			run = 1
			if run > longest {
				longest = run
			}
		}
	}
	return longest
}

func FuzzKeyFilterPassesBytesThrough(f *testing.F) {
	f.Add("a\r\nb")
	f.Add(pasteOn + "x\ny" + pasteOff)
	f.Add("\x1b[20")
	f.Add("\x03\x04\r\n")
	f.Add("\x1b\x1b[200~\r\n" + pasteOff)
	// A marker between CR and LF: they are no longer adjacent, so the LF is
	// its own terminator. Unreachable from a real terminal, but it pins the
	// rule to "immediately follows" rather than "follows eventually".
	f.Add("\r" + pasteOff + "\n")
	f.Add("\r" + pasteOn + "\n")
	f.Add("\r\r\n\n")
	f.Fuzz(func(t *testing.T, in string) {
		if !utf8.ValidString(in) || strings.ContainsRune(in, utf8.RuneError) {
			// Rejected input delivers nothing, so the pass-through rule does
			// not apply. TestKeyFilterRejects* and TestTerminalRejects* cover
			// these deterministically.
			t.Skip()
		}
		if strings.ContainsRune(in, 0x1b) {
			// With an ESC present the byte contract also covers breaker
			// injection and paste-content stripping, which cannot be stated
			// as a one-line rule without restating the implementation.
			// FuzzTerminalFIFOSync covers those inputs against the real
			// Terminal instead, which is the property that actually matters.
			t.Skip()
		}
		kf := newKeyFilter(strings.NewReader(in), maxGoalBytes)
		var out []byte
		buf := make([]byte, 7) // deliberately small: exercises the pending path
		for i := 0; i < 100000; i++ {
			n, err := kf.Read(buf)
			if n == 0 && err == nil {
				t.Fatalf("(0, nil) with non-empty buffer")
			}
			out = append(out, buf[:n]...)
			// Claim each published flag, as a consumer must; otherwise the
			// next Read trips the pending-flag guard.
			kf.PopSegmentFlag()
			if err != nil {
				break
			}
		}
		// The byte-level contract is exact and simple: everything is forwarded
		// unchanged except an LF that immediately follows a delivered CR. That
		// is a complete independent rule, so assert equality rather than a
		// subsequence — a subsequence check is satisfied by a filter that
		// drops arbitrary bytes, including all of them.
		if want := strings.ReplaceAll(in, "\r\n", "\r"); string(out) != want {
			t.Fatalf("out = %q, want %q (input %q)", out, want, in)
		}
	})
}

func equalBools(a, b []bool) bool {
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

func TestKeyFilterSmallBufferDeliversEverything(t *testing.T) {
	// x/term reads into a 256-byte inBuf and can offer far less when its
	// remainder is nearly full, so delivery must be correct at every buffer
	// size, including one byte at a time.
	in := pasteOn + "alpha\r\nbeta\n" + pasteOff + "gamma\n"
	want := pasteOn + "alpha\rbeta\n" + pasteOff + "gamma\n"
	for _, size := range []int{1, 2, 3, 7, 64, 4096} {
		f := newTestFilter(strings.NewReader(in))
		var out []byte
		var flags []bool
		buf := make([]byte, size)
		for i := 0; i < 100000; i++ {
			n, err := f.Read(buf)
			out = append(out, buf[:n]...)
			if inPaste, ok := f.PopSegmentFlag(); ok {
				flags = append(flags, inPaste)
			}
			if err != nil {
				break
			}
		}
		if string(out) != want {
			t.Fatalf("size %d: out = %q, want %q", size, out, want)
		}
		if wantFlags := []bool{true, true, false}; !equalBools(flags, wantFlags) {
			t.Fatalf("size %d: flags = %v, want %v", size, flags, wantFlags)
		}
	}
}

func TestKeyFilterBreakerNeverSplitsARune(t *testing.T) {
	// The breaker closes an escape before a rune, never between its bytes.
	// Asserting on the delivered bytes is the direct check: a split rune would
	// leave the output invalid UTF-8 and lose the character.
	for _, r := range []string{"é", "中", "𝄞"} {
		for pad := 248; pad <= 258; pad++ {
			in := "\x1b" + strings.Repeat("1", pad) + r + "\n"
			f := newTestFilter(strings.NewReader(in))
			out, _ := drain(t, f)
			if !utf8.Valid(out) {
				t.Fatalf("rune %q pad %d: delivered bytes are not valid UTF-8; the breaker split a rune", r, pad)
			}
			if !bytes.Contains(out, []byte(r)) {
				t.Fatalf("rune %q pad %d: rune missing from delivered bytes", r, pad)
			}
			if run := longestUnresolvedEscapeRun(out); run > 256 {
				t.Fatalf("rune %q pad %d: unresolved escape run %d exceeds 256", r, pad, run)
			}
		}
	}
}

func TestKeyFilterZeroLengthReadFailsClosed(t *testing.T) {
	f := newTestFilter(strings.NewReader("a\n"))
	n, err := f.Read(nil)
	if n != 0 || !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("Read(nil) = (%d, %v), want (0, io.ErrShortBuffer)", n, err)
	}
}

func TestKeyFilterRecoverableErrorDoesNotPoison(t *testing.T) {
	// A transient error must be reported once, not latch forever.
	boom := errors.New("transient")
	r := &chunkReader{chunks: [][]byte{[]byte("a\n")}, err: boom}
	f := newTestFilter(r)
	buf := make([]byte, 64)
	if n, err := f.Read(buf); err != nil || string(buf[:n]) != "a\n" {
		t.Fatalf("first Read = %q err = %v", buf[:n], err)
	}
	f.PopSegmentFlag()
	if _, err := f.Read(buf); !errors.Is(err, boom) {
		t.Fatalf("second Read err = %v, want %v", err, boom)
	}
	// The reader still reports the error, but the filter is not latched: it
	// asks the reader again rather than replaying a stored failure.
	r.chunks = [][]byte{[]byte("b\n")}
	r.i = 0
	r.err = nil
	if n, err := f.Read(buf); err != nil || string(buf[:n]) != "b\n" {
		t.Fatalf("third Read = %q err = %v; the filter latched a recoverable error", buf[:n], err)
	}
}

func TestKeyFilterTruncatedRuneAtEOFIsRejected(t *testing.T) {
	for _, in := range []string{"a\xc3", "a\xe4\xb8", "a\xf0\x9d\x84"} {
		f := newTestFilter(strings.NewReader(in))
		_, err := drain(t, f)
		if !errors.Is(err, errInvalidUTF8) {
			t.Fatalf("in %q: err = %v, want errInvalidUTF8", in, err)
		}
	}
}

func TestKeyFilterSplitRuneAcrossReadsIsAccepted(t *testing.T) {
	// A rune split across chunk boundaries is held, not rejected: at most
	// utf8.UTFMax-1 prefix bytes are retained.
	r := []byte("𝄞")
	for split := 1; split < len(r); split++ {
		f := newTestFilter(&chunkReader{chunks: [][]byte{
			append([]byte("a"), r[:split]...), append(append([]byte{}, r[split:]...), '\n'),
		}})
		out, _ := drain(t, f)
		if string(out) != "a𝄞\n" {
			t.Fatalf("split %d: out = %q, want %q", split, out, "a𝄞\n")
		}
	}
}

// TestKeyFilterRejectionDrainSpansReads pins the defect where the drain only
// covered the buffer that happened to hold the invalid byte: the rest of the
// offending line then reached the replacement Terminal as a fresh goal.
func TestKeyFilterRejectionDrainSpansReads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		after  string
	}{
		{"boundary in a later chunk", []string{"bad\xb2", "tail\nclean\n"}, "clean\n"},
		{"boundary two chunks later", []string{"bad\xb2", "more", "tail\nclean\n"}, "clean\n"},
		{"crlf split across chunks", []string{"bad\xb2tail\r", "\nclean\n"}, "clean\n"},
		{"lone cr boundary", []string{"bad\xb2tail\r", "clean\n"}, "clean\n"},
		{"paste end in a later chunk", []string{pasteOn + "bad\xb2", "tail" + pasteOff, "clean\n"}, "clean\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := make([][]byte, len(tc.chunks))
			for i, c := range tc.chunks {
				chunks[i] = []byte(c)
			}
			f := newTestFilter(&chunkReader{chunks: chunks})
			buf := make([]byte, 64)
			if _, err := f.Read(buf); !errors.Is(err, errInvalidUTF8) {
				t.Fatalf("err = %v, want errInvalidUTF8", err)
			}
			out, _ := drain(t, f)
			if string(out) != tc.after {
				t.Fatalf("after rejection out = %q, want %q; the drain stopped at the chunk boundary", out, tc.after)
			}
		})
	}
}

// TestKeyFilterRejectionDrainAtEverySplit walks the boundary across every byte
// offset, for a typed line and for a bracketed paste.
func TestKeyFilterRejectionDrainAtEverySplit(t *testing.T) {
	for _, tc := range []struct{ name, in, after string }{
		{"typed", "bad\xb2tail\r\nclean\n", "clean\n"},
		{"paste", pasteOn + "bad\xb2tail" + pasteOff + "clean\n", "clean\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for split := 1; split < len(tc.in); split++ {
				f := newTestFilter(&chunkReader{chunks: [][]byte{
					[]byte(tc.in[:split]), []byte(tc.in[split:]),
				}})
				buf := make([]byte, 64)
				var err error
				for i := 0; i < 100; i++ {
					if _, err = f.Read(buf); err != nil {
						break
					}
					f.PopSegmentFlag()
				}
				if !errors.Is(err, errInvalidUTF8) {
					t.Fatalf("split %d: err = %v, want errInvalidUTF8", split, err)
				}
				out, _ := drain(t, f)
				if string(out) != tc.after {
					t.Fatalf("split %d: after rejection out = %q, want %q", split, out, tc.after)
				}
			}
		})
	}
}

func TestKeyFilterRejectionDrainTerminatesAtStreamEnd(t *testing.T) {
	// No boundary ever arrives: the drain must still end and surface the
	// rejection rather than waiting for input that will not come.
	f := newTestFilter(strings.NewReader("bad\xb2tail"))
	buf := make([]byte, 64)
	if _, err := f.Read(buf); !errors.Is(err, errInvalidUTF8) {
		t.Fatalf("err = %v, want errInvalidUTF8", err)
	}
	if _, err := f.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("second err = %v, want io.EOF", err)
	}
}
