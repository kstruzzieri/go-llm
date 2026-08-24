package tools

import "sync"

// tailRing is a concurrency-safe single-stream byte ring for background-exec
// output. It always consumes complete writes (a full ring must never block or
// shorten a child process's pipe write), retains only the newest capBytes, and
// exposes the stream through absolute cursors so a reader can resume tailing
// across turns; eviction is reported explicitly via dropped, never as a silent
// gap. floor and end are monotonically non-decreasing for the ring's lifetime.
//
// It is deliberately distinct from cappedBuffer (exec.go), which keeps the
// prefix; a tail ring keeps the newest bytes.
type tailRing struct {
	mu    sync.Mutex
	buf   []byte // retained bytes; backing array allocated once, never regrown
	floor uint64 // absolute offset of buf[0], the oldest retained byte
	end   uint64 // absolute total bytes ever written
}

// newTailRing returns a ring retaining the newest capBytes bytes. capBytes
// must be > 0; violation is a construction bug, so it panics.
func newTailRing(capBytes int) *tailRing {
	if capBytes <= 0 {
		panic("tools: newTailRing capBytes must be > 0")
	}
	return &tailRing{buf: make([]byte, 0, capBytes)}
}

// Write implements io.Writer. It always consumes all of p and returns
// (len(p), nil); when p overflows the ring, the oldest bytes are evicted so
// only the newest cap(buf) bytes remain.
// ponytail: linear shift-down storage — O(capBytes) per overflow write is fine
// at the 64 KiB per-job cap; switch to circular indices if a profile says so.
func (r *tailRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	capBytes := cap(r.buf)
	r.end += uint64(n)
	if n >= capBytes {
		// A single write at least as large as the ring: keep only its newest tail.
		r.buf = r.buf[:capBytes]
		copy(r.buf, p[n-capBytes:])
		r.floor = r.end - uint64(capBytes)
		return n, nil
	}
	if over := len(r.buf) + n - capBytes; over > 0 {
		copy(r.buf, r.buf[over:])
		r.buf = r.buf[:len(r.buf)-over]
		r.floor += uint64(over)
	}
	r.buf = append(r.buf, p...)
	return n, nil
}

// Read returns a fresh copy of stream bytes. With cursor == nil it is the
// initial newest-tail mode: the NEWEST min(maxBytes, retained) bytes, next =
// end, dropped = 0. With a cursor it returns at most maxBytes bytes starting
// at that absolute offset; a cursor behind floor is clamped to floor with the
// evicted gap reported in dropped, and a cursor at or past end returns no
// bytes with next = end. next is always the position after the last returned
// byte (or the clamped cursor when nothing is returned). maxBytes < 1 is a
// caller construction bug (Task 5 tools own validation) and panics.
func (r *tailRing) Read(cursor *uint64, maxBytes int) (data []byte, next uint64, dropped uint64) {
	if maxBytes < 1 {
		panic("tools: tailRing.Read maxBytes must be >= 1")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cursor == nil {
		n := len(r.buf)
		if n > maxBytes {
			n = maxBytes
		}
		data = make([]byte, n)
		copy(data, r.buf[len(r.buf)-n:])
		return data, r.end, 0
	}
	c := *cursor
	if c > r.end {
		return nil, r.end, 0
	}
	if c < r.floor {
		dropped = r.floor - c
		c = r.floor
	}
	start := int(c - r.floor)
	n := len(r.buf) - start
	if n > maxBytes {
		n = maxBytes
	}
	data = make([]byte, n)
	copy(data, r.buf[start:start+n])
	return data, c + uint64(n), dropped
}

// Bounds reports the absolute offset of the oldest retained byte (floor) and
// the absolute total bytes ever written (end).
func (r *tailRing) Bounds() (floor, end uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.floor, r.end
}
