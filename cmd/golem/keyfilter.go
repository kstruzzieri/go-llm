package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// terminator records why a Terminal read ended. golang.org/x/term collapses
// Ctrl-C (readLine:826) and Ctrl-D on an empty line (readLine:822) to the same
// io.EOF, so the distinction has to be recovered below the Terminal, from the
// byte stream itself.
type terminator int

const (
	termNone terminator = iota
	termCtrlC
	termCtrlD
	termEOF
)

// eventKind classifies the boundary that ended a delivery.
type eventKind int

const (
	eventNone eventKind = iota
	eventLine           // CR, LF, or a coalesced CRLF: must produce exactly one line
	eventCtrl           // Ctrl-C or Ctrl-D: no line, so no segment flag
)

const (
	byteCtrlC = 0x03
	byteCtrlD = 0x04
	byteLF    = '\n'
	byteCR    = '\r'
	byteEsc   = 0x1b

	// escBreaker closes an escape sequence x/term would otherwise keep
	// scanning past. bytesToKey ends an unrecognized sequence at the first
	// [A-Za-z~] (terminal.go:256-266, predicate at :261), and every sequence it does recognize is
	// matched positionally on b[2], b[3], b[5], or b[:6] against A, B, C, D,
	// H, F, or '~'. 'z' appears in none of them, so the breaker terminates the
	// scan without ever completing a real key.
	escBreaker = 'z'

	// maxEscSpan bounds how many forwarded bytes an unresolved escape may span
	// before the breaker is injected. readLine sets
	// readBuf = inBuf[len(remainder):] over a 256-byte inBuf, so an escape
	// still unresolved at byte 256 leaves a zero-length read and no forward
	// progress. Injecting the breaker as byte 256 keeps a complete sequence
	// inside inBuf. Measured against v0.42.0: a 254-byte span is fine, 255 and
	// above livelock without this.
	maxEscSpan = 255
)

var (
	pasteStartSeq = []byte{byteEsc, '[', '2', '0', '0', '~'}
	pasteEndSeq   = []byte{byteEsc, '[', '2', '0', '1', '~'}
)

// errTerminatorSwallowed reports that x/term asked for more input while a line
// terminator it had already been given was still unaccounted for. Upstream
// swallows terminators in two known ways -- the unknown-escape scan and the
// utf8.RuneError sentinel -- and both used to corrupt segment provenance
// silently. Single-event framing turns them into this error instead.
var errTerminatorSwallowed = errors.New("golem: terminal requested input while a line terminator was still pending")

// errInvalidUTF8 and errLiteralReplacement reject input this editor cannot
// represent. Neither is sanitized: the provider transport is JSON, which would
// substitute U+FFFD for malformed bytes anyway, so rejecting loudly beats
// corrupting quietly. Latin-1 needs a declared decoding and binary needs
// hex/base64; a rune-based terminal editor can infer neither.
var (
	errInvalidUTF8 = errors.New("golem: input is not valid UTF-8")

	// errLiteralReplacement applies only on this path. utf8.RuneError IS
	// U+FFFD, and readLine treats that value as "incomplete rune, read more"
	// (terminal.go:815-817), so a correctly encoded U+FFFD is indistinguishable
	// from a decode failure and is silently deleted. The scanner and /edit
	// paths do not go through x/term and represent it fine.
	errLiteralReplacement = errors.New("golem: input contains U+FFFD, which the line editor cannot distinguish from a decode error")
)

// errZeroLengthRead fails closed rather than answering (0, nil), which would
// let a caller with an exhausted buffer spin instead of surfacing the problem.
var errZeroLengthRead = fmt.Errorf("golem: zero-length read buffer: %w", io.ErrShortBuffer)

// keyFilter sits between the terminal and x/term.Terminal. It forwards the byte
// stream while recovering what the Terminal cannot report, and it frames
// delivery so that at most one line terminator is in flight at a time.
//
// Framing is the load-bearing property. A Read never delivers bytes past a
// line terminator, so x/term's remainder can never hold a second complete
// line, one pending flag replaces a queue, and a terminator x/term consumed
// without producing a line is detected on its next Read instead of silently
// shifting every later segment's provenance.
//
// Not safe for concurrent use; the editor's sole reader owns it.
type keyFilter struct {
	r   io.Reader
	buf []byte // scratch for the wrapped read

	work    []byte // read but not yet transformed
	stalled bool   // work ends in an incomplete marker or rune; needs more input
	out     []byte // transformed, awaiting delivery; never extends past an event

	// outEvent describes the boundary at the tail of out. It becomes a
	// published flag only once the terminator byte itself has been copied to
	// the caller, so a line longer than x/term's 256-byte buffer does not
	// falsely trip the pending-flag guard.
	outEvent  eventKind
	outPaste  bool
	flagReady bool // a line terminator was delivered and its flag is unclaimed
	flagPaste bool

	pendingErr error // wrapped error, surfaced once and then cleared
	failErr    error // rejection (validation or paste budget), surfaced only once the drain completes

	// draining spans reads. A rejection's boundary -- a typed CR/LF or a
	// complete paste-end marker -- can arrive in any later chunk, so the drain
	// is a state machine rather than a single pass over the current buffer.
	// Surfacing the rejection before the boundary is consumed would hand the
	// rest of the offending line to the replacement Terminal as a new goal.
	draining       bool
	drainPendingLF bool // a CR ended a chunk; a paired LF may open the next

	inPaste bool
	prevCR  bool

	escUnresolved bool
	escSpan       int

	term terminator

	maxPasteBytes int

	// pasteBytes counts every non-marker byte consumed inside the current
	// bracketed paste, including ESC bytes that are dropped rather than
	// forwarded. It resets only when a start marker transitions inPaste from
	// false to true, so a redundant start marker cannot re-arm the budget.
	pasteBytes int

	// discardingPaste marks a drain armed by a paste-budget overflow. It
	// changes what a stream end mid-drain means: the rejection and the stream
	// error surface as one joined answer instead of sequentially, so the
	// editor can warn and classify the end in a single step.
	discardingPaste bool
}

// newKeyFilter wraps r. maxPasteBytes bounds a single bracketed paste; zero or
// negative is a configuration error and panics here rather than misbehaving
// inside the read loop.
func newKeyFilter(r io.Reader, maxPasteBytes int) *keyFilter {
	if maxPasteBytes <= 0 {
		panic("golem: keyFilter requires a positive paste limit")
	}
	return &keyFilter{r: r, buf: make([]byte, 4096), maxPasteBytes: maxPasteBytes}
}

// Read implements io.Reader with the contract x/term actually requires.
//
// readLine returns on a non-nil error before folding n into its remainder
// (terminal.go:876-880), so a wrapped (n > 0, err) is never forwarded as-is:
// the bytes would be lost. Bytes are delivered first and the error surfaces on
// a later call. Read never answers (0, nil) for a non-empty p, never withholds
// a lone CR while waiting to learn whether an LF follows, and never delivers
// past a line terminator.
func (f *keyFilter) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, errZeroLengthRead
	}
	if f.flagReady {
		// x/term consumed a terminator without returning its line.
		return 0, errTerminatorSwallowed
	}
	for {
		if len(f.out) > 0 {
			n := copy(p, f.out)
			f.out = f.out[:copy(f.out, f.out[n:])]
			if len(f.out) == 0 {
				f.publishEvent()
			}
			return n, nil
		}
		if f.draining {
			switch {
			case f.drainStep():
				f.draining, f.drainPendingLF, f.discardingPaste = false, false, false
			case f.pendingErr != nil:
				// The stream ended before the boundary arrived. Terminate the
				// drain deterministically rather than waiting forever; the
				// rejection still surfaces, and the stream error follows it.
				f.draining, f.drainPendingLF = false, false
				f.work = f.work[:0]
				if f.discardingPaste {
					// A paste-budget overflow whose end marker never came.
					// Join the rejection with the stream error so the editor
					// can warn and classify the end in one answer; surfacing
					// them sequentially would make it warn, recreate, and
					// only then learn the stream was already dead.
					f.discardingPaste = false
					err := errors.Join(f.failErr, f.pendingErr)
					f.failErr, f.pendingErr = nil, nil
					return 0, err
				}
			default:
				f.fill()
				continue
			}
		}
		// Checked before consuming more work: a rejection must surface before
		// the next clean line, or the caller would see valid input where the
		// failure should have retired the Terminal.
		if f.failErr != nil {
			err := f.failErr
			f.failErr = nil
			return 0, err
		}
		if len(f.work) > 0 && !f.stalled {
			f.consume()
			continue
		}
		if f.pendingErr != nil {
			if len(f.work) > 0 {
				// A partial marker or rune prefix that never completed. A
				// truncated rune is a rejection; an unfinished marker is real
				// input and is surfaced literally.
				f.flushStalledAtEnd()
				continue
			}
			err := f.pendingErr
			// Cleared so a recoverable error cannot poison the filter
			// permanently. A reader still at EOF simply reports it again.
			f.pendingErr = nil
			if errors.Is(err, io.EOF) {
				f.term = termEOF
			}
			return 0, err
		}
		f.fill()
		// A wrapped (0, nil) is legal but means nothing happened; loop.
	}
}

// fill performs one wrapped read, appending whatever it produced and recording
// any error for later. It never surfaces (n > 0, err) to the caller.
func (f *keyFilter) fill() {
	n, err := f.r.Read(f.buf)
	if n > 0 {
		f.work = append(f.work, f.buf[:n]...)
		f.stalled = false
	}
	if err != nil {
		f.pendingErr = err
	}
}

// publishEvent runs when out drains. Only then has the terminator byte
// actually reached x/term, which is what makes the pending-flag guard sound
// for lines longer than one buffer.
func (f *keyFilter) publishEvent() {
	if f.outEvent == eventLine {
		f.flagReady = true
		f.flagPaste = f.outPaste
	}
	f.outEvent = eventNone
}

// flushStalledAtEnd resolves whatever is still held when the stream ends: an
// incomplete rune is truncated input and is rejected, while an unfinished
// escape or marker prefix is ordinary text and is surfaced literally.
func (f *keyFilter) flushStalledAtEnd() {
	if len(f.work) > 0 && f.work[0] >= utf8.RuneSelf {
		f.reject(errInvalidUTF8)
		return
	}
	f.out = append(f.out, f.work...)
	f.work = f.work[:0]
	f.stalled = false
}

// TakeTerminator returns and clears the reason the last read ended. Callers
// consult it exactly when Terminal.ReadLine returns io.EOF; a second call
// without an intervening read reports termNone rather than repeating a stale
// answer. Each new terminator byte overwrites the previous one, so a Ctrl-D
// that merely deleted a character cannot be mistaken for a later EOF.
func (f *keyFilter) TakeTerminator() terminator {
	t := f.term
	f.term = termNone
	return t
}

// PopSegmentFlag reports whether the delivered line terminator arrived inside a
// bracketed paste. ok=false means no line terminator is outstanding, which
// callers must never conflate with "a typed newline" -- term.ErrPasteIndicator
// is set only for wholly pasted lines, which is why composition cannot use it.
//
// At most one flag exists at a time. Framing guarantees x/term cannot hold a
// second complete line, so there is nothing to queue.
func (f *keyFilter) PopSegmentFlag() (inPaste bool, ok bool) {
	if !f.flagReady {
		return false, false
	}
	f.flagReady = false
	return f.flagPaste, true
}

// Discard atomically resets every piece of state coupled to the Terminal being
// retired, INCLUDING buffered input. It is the Ctrl-C path, where cooked-mode
// SIGINT would have flushed the input queue, so dropping retained bytes is the
// point. It cannot flush the kernel queue, so only filter-owned state is
// affected.
//
// Do NOT call it after a rejection. reject already clears everything coupled to
// the retired Terminal while deliberately preserving typeahead that arrived
// after the offending line; discarding there would throw that typeahead away.
// A rejection needs only a fresh Terminal.
func (f *keyFilter) Discard() {
	f.work = f.work[:0]
	f.out = f.out[:0]
	f.outEvent = eventNone
	f.outPaste = false
	f.flagReady = false
	f.flagPaste = false
	f.failErr = nil
	f.draining = false
	f.drainPendingLF = false
	f.stalled = false
	f.prevCR = false
	f.inPaste = false
	f.escUnresolved = false
	f.escSpan = 0
	f.pasteBytes = 0
	f.discardingPaste = false
	f.term = termNone
}

// reject discards everything staged for delivery and arms a streaming drain of
// the offending input. The error surfaces only once the drain reaches its
// boundary, so nothing partial is handed to x/term and Read never returns
// (n > 0, err).
func (f *keyFilter) reject(err error) {
	f.out = f.out[:0]
	f.outEvent = eventNone
	f.escUnresolved = false
	f.escSpan = 0
	f.prevCR = false
	f.stalled = false
	f.draining = true
	f.drainPendingLF = false
	f.failErr = err
}

// drainStep discards input toward the end of the current typed line or
// bracketed paste, reporting whether the boundary has now been fully consumed.
// It resumes across reads: the boundary may be a CRLF or a paste-end marker
// split at any byte, and a partial marker or a lone trailing CR is carried into
// the next chunk rather than guessed at.
func (f *keyFilter) drainStep() bool {
	if f.drainPendingLF {
		// A CR ended the previous chunk. Suppress its paired LF if that is
		// what opens this one, then the boundary is complete either way.
		f.drainPendingLF = false
		if len(f.work) > 0 && f.work[0] == byteLF {
			f.work = f.work[:copy(f.work, f.work[1:])]
		}
		return true
	}
	i := 0
	for i < len(f.work) {
		if f.inPaste {
			if f.work[i] == byteEsc {
				n, st := matchPasteMarker(f.work[i:])
				switch st {
				case markerEnd:
					f.inPaste = false
					i += n
					f.work = f.work[:copy(f.work, f.work[i:])]
					return true
				case markerIncomplete:
					// Retain the prefix so a marker split across reads still
					// completes.
					f.work = f.work[:copy(f.work, f.work[i:])]
					return false
				}
			}
			i++
			continue
		}
		c := f.work[i]
		i++
		if c == byteLF {
			f.work = f.work[:copy(f.work, f.work[i:])]
			return true
		}
		if c == byteCR {
			if i < len(f.work) {
				if f.work[i] == byteLF {
					i++
				}
				f.work = f.work[:copy(f.work, f.work[i:])]
				return true
			}
			// CR at the very end: its paired LF may open the next chunk.
			f.work = f.work[:0]
			f.drainPendingLF = true
			return false
		}
	}
	f.work = f.work[:0]
	return false
}

// consume transforms work into out, stopping at the first event boundary so a
// delivery never extends past a line terminator, or on an incomplete marker or
// rune so the remainder can be resolved with more input.
func (f *keyFilter) consume() {
	i := 0
	for i < len(f.work) {
		c := f.work[i]

		if c == byteEsc {
			n, st := matchPasteMarker(f.work[i:])
			switch st {
			case markerIncomplete:
				f.work = f.work[:copy(f.work, f.work[i:])]
				f.stalled = true
				return
			case markerStart:
				if f.inPaste {
					// A redundant start marker fails x/term's !pasteActive
					// guard, falls into the unknown-sequence scan, and injects
					// a surrogate keyUnknown rune into the pasted line. It is
					// consumed without touching pasteBytes: only a false->true
					// transition may reset the budget.
					i += n
					continue
				}
				f.breakEscape()
				f.out = append(f.out, f.work[i:i+n]...)
				f.inPaste = true
				f.pasteBytes = 0
				f.prevCR = false
				i += n
				continue
			case markerEnd:
				f.breakEscape()
				f.out = append(f.out, f.work[i:i+n]...)
				f.inPaste = false
				f.prevCR = false
				i += n
				continue
			}
			if f.inPaste {
				// A bare ESC in paste content would make x/term consume
				// forward to the next [A-Za-z~], eating pasted newlines and
				// characters, so the byte is unusable anyway. It still counts
				// against the budget: dropped or not, it is paste content.
				if !f.chargePaste(1) {
					f.overflowPaste(i)
					return
				}
				i++
				continue
			}
			f.breakEscape()
			f.out = append(f.out, byteEsc)
			f.escUnresolved = true
			f.escSpan = 1
			f.prevCR = false
			i++
			continue
		}

		if c >= utf8.RuneSelf {
			if f.inPaste && utf8.FullRune(f.work[i:]) {
				// Charged whole: forwarding part of a rune and rejecting the
				// rest would hand x/term a torn encoding.
				_, size := utf8.DecodeRune(f.work[i:])
				if !f.chargePaste(size) {
					f.overflowPaste(i)
					return
				}
			}
			size, ok := f.acceptRune(f.work[i:])
			if !ok {
				if size == 0 {
					// Incomplete rune: at most utf8.UTFMax-1 bytes retained.
					f.work = f.work[:copy(f.work, f.work[i:])]
					f.stalled = true
					return
				}
				// Rejected. reject() drains from the offending byte onward.
				f.work = f.work[:copy(f.work, f.work[i:])]
				f.reject(f.rejectReason(f.work))
				return
			}
			i += size
			continue
		}

		if f.inPaste && !f.chargePaste(1) {
			f.overflowPaste(i)
			return
		}
		ev := f.transform(c)
		i++
		if ev == eventLine && c == byteCR && i < len(f.work) && f.work[i] == byteLF {
			// CRLF is one event; consume the LF now so it cannot start the
			// next delivery as a stray terminator. The suppressed LF is still
			// paste content, so it is charged -- without an overflow check,
			// since it is never forwarded; the next content byte overflows.
			if f.inPaste {
				f.pasteBytes++
			}
			i++
			f.prevCR = false
		}
		if ev != eventNone {
			f.outEvent = ev
			f.outPaste = f.inPaste
			f.work = f.work[:copy(f.work, f.work[i:])]
			return
		}
	}
	f.work = f.work[:0]
}

// chargePaste counts n non-marker paste bytes against the budget, reporting
// false -- without charging -- when they would push the paste past its bound.
func (f *keyFilter) chargePaste(n int) bool {
	if f.pasteBytes+n > f.maxPasteBytes {
		return false
	}
	f.pasteBytes += n
	return true
}

// overflowPaste arms the oversized-paste drain at work[i]: nothing more is
// forwarded, drainStep consumes through the paste-end marker, and
// errPasteTooLarge surfaces once every byte already staged has been delivered.
// Unlike reject, staged output is kept: the bytes up to the bound were within
// budget, and "stops forwarding at the bound" means exactly them.
func (f *keyFilter) overflowPaste(i int) {
	f.work = f.work[:copy(f.work, f.work[i:])]
	f.discardingPaste = true
	f.draining = true
	f.failErr = errPasteTooLarge
	f.prevCR = false
}

// rejectReason distinguishes malformed input from a literal U+FFFD so the
// message names the real problem.
func (f *keyFilter) rejectReason(b []byte) error {
	if r, size := utf8.DecodeRune(b); r == utf8.RuneError && size == 3 {
		return errLiteralReplacement
	}
	return errInvalidUTF8
}

// acceptRune forwards one complete rune as a unit. It reports size=0, ok=false
// when more bytes are needed, and size>0, ok=false when the rune is rejected.
// Forwarding whole runes is what keeps the escape breaker from ever landing
// between the bytes of one.
func (f *keyFilter) acceptRune(b []byte) (int, bool) {
	if !utf8.FullRune(b) {
		return 0, false
	}
	r, size := utf8.DecodeRune(b)
	if r == utf8.RuneError {
		// size 1 is malformed; size 3 is a correctly encoded U+FFFD, which
		// x/term cannot tell apart from its own decode sentinel.
		return size, false
	}
	// A multi-byte rune can never be an escape final byte, so an unresolved
	// escape must be closed before it rather than part-way through it.
	if f.escUnresolved && f.escSpan+size > maxEscSpan {
		f.injectBreaker()
	}
	f.out = append(f.out, b[:size]...)
	if f.escUnresolved {
		f.escSpan += size
		if f.escSpan >= maxEscSpan {
			f.injectBreaker()
		}
	}
	f.prevCR = false
	return size, true
}

// transform handles one ASCII byte and reports the event boundary it creates.
func (f *keyFilter) transform(c byte) eventKind {
	switch {
	case c == byteLF && f.prevCR:
		// Same keypress as the CR just delivered. Suppressed uniformly across
		// the same-buffer and split-buffer cases; x/term only swallows an LF
		// already in the same buffer (readLine:843-845).
		f.prevCR = false
		return eventNone
	case c == byteCR:
		f.breakEscape()
		f.forward(c)
		f.prevCR = true
		return eventLine
	case c == byteLF:
		f.breakEscape()
		f.forward(c)
		f.prevCR = false
		return eventLine
	case c == byteCtrlC && !f.inPaste:
		f.breakEscape()
		f.forward(c)
		f.term = termCtrlC
		f.prevCR = false
		return eventCtrl
	case c == byteCtrlD && !f.inPaste:
		// No pending-flag guard follows a Ctrl-D: on a non-empty line x/term
		// deletes a character and legitimately reads again.
		f.breakEscape()
		f.forward(c)
		f.term = termCtrlD
		f.prevCR = false
		return eventCtrl
	default:
		// Inside a paste, 0x03 and 0x04 fall through as data, matching x/term,
		// which gates both handlers on !pasteActive.
		f.forward(c)
		f.prevCR = false
		return eventNone
	}
}

// forward appends one ASCII byte, maintaining escape-span bookkeeping so an
// unresolved sequence can never span far enough to stall x/term's reader.
func (f *keyFilter) forward(c byte) {
	f.out = append(f.out, c)
	if !f.escUnresolved {
		return
	}
	f.escSpan++
	if isEscFinal(c) {
		f.escUnresolved = false
		f.escSpan = 0
		return
	}
	if f.escSpan >= maxEscSpan {
		f.injectBreaker()
	}
}

// breakEscape closes an unresolved escape before a byte whose meaning x/term
// would otherwise swallow into it: a line terminator, Ctrl-C/Ctrl-D, a paste
// marker, or another ESC.
func (f *keyFilter) breakEscape() {
	if f.escUnresolved {
		f.injectBreaker()
	}
}

func (f *keyFilter) injectBreaker() {
	f.out = append(f.out, escBreaker)
	f.escUnresolved = false
	f.escSpan = 0
}

// isEscFinal mirrors the final-byte rule in x/term's bytesToKey
// (terminal.go:256-266, predicate at :261).
func isEscFinal(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '~'
}

type markerStatus int

const (
	markerNone markerStatus = iota
	markerIncomplete
	markerStart
	markerEnd
)

// matchPasteMarker classifies the start of b against the bracketed-paste
// markers, reporting the matched length for a complete marker.
func matchPasteMarker(b []byte) (int, markerStatus) {
	for _, m := range []struct {
		seq []byte
		st  markerStatus
	}{{pasteStartSeq, markerStart}, {pasteEndSeq, markerEnd}} {
		if len(b) >= len(m.seq) {
			if bytes.Equal(b[:len(m.seq)], m.seq) {
				return len(m.seq), m.st
			}
			continue
		}
		if bytes.Equal(b, m.seq[:len(b)]) {
			return 0, markerIncomplete
		}
	}
	return 0, markerNone
}
