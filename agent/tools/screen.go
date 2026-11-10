package tools

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
)

// screenHoldBound is the longest record the screen holds while waiting for its
// terminator; it equals the stdout capture cap.
const screenHoldBound = execStdoutCap

const (
	screenMarkerSpan     = "[withheld: credential-path content (credential_path)]"
	screenMarkerPatchCut = "[output withheld from here on: a patch of a credential-path file begins (credential_path); rerun as a direct argv call that excludes the file to see the rest, for example: git log -p -- . ':(exclude,glob)**/.env']"
	screenMarkerLongCut  = "[output withheld from here on: a line over 32768 bytes cannot be screened (credential_path)]"
)

// errNoOutputScreen reports an exec spec built without an output screen.
var errNoOutputScreen = errors.New("tools: exec spec has no output screen")

// outputScreen holds the credential values a command's output is screened
// against, and the notes its streams earn. Key bytes are plaintext: never pass
// them to fmt, a logger, an error or a note.
type outputScreen struct {
	keys     [][]byte // CSI-stripped, longest first
	withhold string   // non-empty: both streams deliver nothing; the cause text
	changed  int      // entries that changed during the pre-run read

	mu       sync.Mutex // guards keys (after construction), released and matches
	released bool
	matches  int
}

// newOutputScreen returns a screen over keys, sorted longest first.
func newOutputScreen(keys [][]byte) *outputScreen {
	ks := make([][]byte, 0, len(keys))
	for _, k := range keys {
		if len(k) > 0 {
			ks = append(ks, k)
		}
	}
	sort.SliceStable(ks, func(i, j int) bool { return len(ks[i]) > len(ks[j]) })
	return &outputScreen{keys: ks}
}

// probeScreen returns a screen with no keys. Availability probes only: their
// output never reaches a model.
func probeScreen() *outputScreen { return &outputScreen{} }

// release drops the screen's reference to the key bytes. Call it only after
// every stream of the screen has finished. A stream written or finished after
// release drops its output instead of delivering it unscreened. Safe to call
// more than once and concurrently; notes keeps working.
func (s *outputScreen) release() {
	s.mu.Lock()
	s.keys = nil
	s.released = true
	s.mu.Unlock()
}

// notes returns the notes the run earned, or nil.
func (s *outputScreen) notes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.withhold != "" {
		return []string{fmt.Sprintf("credential screen: output withheld; credential files could not be read completely (%s)", s.withhold)}
	}
	var out []string
	if s.changed > 0 {
		out = append(out, fmt.Sprintf("credential screen: %d entr(ies) changed during the read and were skipped; output screened without them", s.changed))
	}
	if s.matches > 0 {
		out = append(out, fmt.Sprintf("credential screen: %d match(es) withheld from output (credential_path)", s.matches))
	}
	return out
}

func (s *outputScreen) mark() {
	s.mu.Lock()
	s.matches++
	s.mu.Unlock()
}

func (s *outputScreen) snapshot() (keys [][]byte, withhold string, released bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.keys, s.withhold, s.released
}

// screenStream is one output stream's writer. Write never fails and never
// writes short, so a child blocked on a full pipe cannot deadlock.
type screenStream struct {
	s   *outputScreen
	dst io.Writer

	mu   sync.Mutex
	held []byte
	dead bool // cut or finished: later writes are consumed and dropped
}

// stream returns a writer that screens records on their way to dst.
func (s *outputScreen) stream(dst io.Writer) *screenStream {
	return &screenStream{s: s, dst: dst}
}

// Write screens p record by record; it always reports len(p), nil.
func (w *screenStream) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	if w.dead {
		return n, nil
	}
	keys, withhold, released := w.s.snapshot()
	if withhold != "" || released {
		w.dead = true
		w.held = nil
		return n, nil
	}
	var out []byte
	for len(p) > 0 && !w.dead {
		i := bytes.IndexAny(p, "\r\n")
		if i < 0 {
			w.held = append(w.held, p...)
			if len(w.held) > screenHoldBound {
				out = w.cut(out, screenMarkerLongCut)
			}
			break
		}
		rec := p[:i]
		if len(w.held) > 0 {
			rec = append(w.held, rec...)
		}
		out = w.record(out, rec, p[i], keys)
		w.held = w.held[:0]
		p = p[i+1:]
	}
	w.flush(out)
	return n, nil
}

// finish screens and delivers a held unterminated record, then silences the
// stream. Idempotent.
func (w *screenStream) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead {
		return
	}
	keys, _, released := w.s.snapshot()
	var out []byte
	if released {
		w.held = nil
	}
	if len(w.held) > 0 {
		out = w.record(out, w.held, 0, keys)
	}
	w.held = nil
	w.dead = true
	w.flush(out)
}

func (w *screenStream) flush(out []byte) {
	if len(out) > 0 {
		_, _ = w.dst.Write(out) // never surface a sink error to the child
	}
}

func (w *screenStream) cut(out []byte, marker string) []byte {
	w.s.mark()
	w.dead = true
	w.held = nil
	return append(append(out, marker...), '\n')
}

// record applies the rules to one complete record; term is its terminator or 0.
func (w *screenStream) record(out, rec []byte, term byte, keys [][]byte) []byte {
	if len(rec) > screenHoldBound {
		return w.cut(out, screenMarkerLongCut)
	}
	stripped := stripCSI(rec)
	if credentialPatchHeader(stripped) {
		return w.cut(out, screenMarkerPatchCut)
	}
	if credentialPathPrefix(stripped) {
		w.s.mark()
		out = append(out, screenMarkerSpan...)
	} else if scrubbed, hits := replaceKeys(stripped, keys); hits > 0 {
		for i := 0; i < hits; i++ {
			w.s.mark()
		}
		out = append(out, scrubbed...)
	} else {
		out = append(out, rec...)
	}
	if term != 0 {
		out = append(out, term)
	}
	return out
}

// replaceKeys swaps every key occurrence for the span marker, longest key
// first and never inside an earlier replacement.
// ponytail: one scan per key; a single multi-pattern pass (Aho-Corasick) is the upgrade.
func replaceKeys(rec []byte, keys [][]byte) ([]byte, int) {
	type piece struct {
		b      []byte
		marked bool
	}
	pieces := []piece{{b: rec}}
	hits := 0
	for _, k := range keys {
		var next []piece
		for _, p := range pieces {
			if p.marked || !bytes.Contains(p.b, k) {
				next = append(next, p)
				continue
			}
			rest := p.b
			for {
				i := bytes.Index(rest, k)
				if i < 0 {
					break
				}
				if i > 0 {
					next = append(next, piece{b: rest[:i]})
				}
				next = append(next, piece{marked: true})
				hits++
				rest = rest[i+len(k):]
			}
			if len(rest) > 0 {
				next = append(next, piece{b: rest})
			}
		}
		pieces = next
	}
	if hits == 0 {
		return rec, 0
	}
	var out []byte
	for _, p := range pieces {
		if p.marked {
			out = append(out, screenMarkerSpan...)
		} else {
			out = append(out, p.b...)
		}
	}
	return out, hits
}
