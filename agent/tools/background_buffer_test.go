package tools

import (
	"bytes"
	"runtime"
	"sync"
	"testing"
)

// patByte is the deterministic byte at absolute stream offset i, shared by every
// test that verifies content survives wrapping. 251 is prime, so the pattern
// never aligns with any power-of-two capacity.
func patByte(i uint64) byte { return byte(i % 251) }

// patSlice returns the pattern bytes for absolute offsets [from, from+n).
func patSlice(from uint64, n int) []byte {
	out := make([]byte, n)
	for k := range out {
		out[k] = patByte(from + uint64(k))
	}
	return out
}

// mustWrite writes p and asserts the io.Writer contract the plan freezes:
// every Write consumes all of p and returns (len(p), nil).
func mustWrite(t *testing.T, r *tailRing, p []byte) {
	t.Helper()
	n, err := r.Write(p)
	if n != len(p) || err != nil {
		t.Fatalf("Write(%d bytes) = (%d, %v), want (%d, nil)", len(p), n, err, len(p))
	}
}

// checkBounds asserts floor/end against independently tracked totals: end equals
// every byte the test wrote, retained (end-floor) equals min(total, capacity),
// and both counters never decrease versus the previous observation.
func checkBounds(t *testing.T, r *tailRing, total uint64, capBytes int, prevFloor, prevEnd uint64) (floor, end uint64) {
	t.Helper()
	floor, end = r.Bounds()
	if end != total {
		t.Fatalf("end = %d, want total written %d", end, total)
	}
	if floor > end {
		t.Fatalf("floor %d > end %d", floor, end)
	}
	wantRetained := total
	if wantRetained > uint64(capBytes) {
		wantRetained = uint64(capBytes)
	}
	if end-floor != wantRetained {
		t.Fatalf("retained = %d, want %d (floor=%d end=%d)", end-floor, wantRetained, floor, end)
	}
	if floor < prevFloor || end < prevEnd {
		t.Fatalf("bounds rewound: floor %d->%d end %d->%d", prevFloor, floor, prevEnd, end)
	}
	return floor, end
}

func TestTailRingEmpty(t *testing.T) {
	r := newTailRing(8)
	floor, end := r.Bounds()
	if floor != 0 || end != 0 {
		t.Fatalf("Bounds() = (%d, %d), want (0, 0)", floor, end)
	}
	data, next, dropped := r.Read(nil, 16)
	if len(data) != 0 || next != 0 || dropped != 0 {
		t.Fatalf("Read(nil) = (%d bytes, %d, %d), want (0 bytes, 0, 0)", len(data), next, dropped)
	}
	zero := uint64(0)
	data, next, dropped = r.Read(&zero, 16)
	if len(data) != 0 || next != 0 || dropped != 0 {
		t.Fatalf("Read(&0) = (%d bytes, %d, %d), want (0 bytes, 0, 0)", len(data), next, dropped)
	}
}

func TestTailRingBelowCapacity(t *testing.T) {
	r := newTailRing(8)
	mustWrite(t, r, []byte("hel"))
	mustWrite(t, r, []byte("lo"))
	floor, end := r.Bounds()
	if floor != 0 || end != 5 {
		t.Fatalf("Bounds() = (%d, %d), want (0, 5)", floor, end)
	}
	zero := uint64(0)
	data, next, dropped := r.Read(&zero, 100)
	if string(data) != "hello" || next != 5 || dropped != 0 {
		t.Fatalf("Read(&0) = (%q, %d, %d), want (\"hello\", 5, 0)", data, next, dropped)
	}
}

func TestTailRingExactCapacity(t *testing.T) {
	r := newTailRing(8)
	mustWrite(t, r, []byte("abcdefgh"))
	floor, end := r.Bounds()
	if floor != 0 || end != 8 {
		t.Fatalf("Bounds() = (%d, %d), want (0, 8)", floor, end)
	}
	zero := uint64(0)
	data, next, dropped := r.Read(&zero, 100)
	if string(data) != "abcdefgh" || next != 8 || dropped != 0 {
		t.Fatalf("Read(&0) = (%q, %d, %d), want all 8 bytes, 8, 0", data, next, dropped)
	}
}

func TestTailRingMultiWrapOverwrite(t *testing.T) {
	const capBytes = 8
	r := newTailRing(capBytes)
	sizes := []int{3, 5, 7, 2, 8, 6, 4} // total 35: wraps the 8-byte ring 4x over
	var total uint64
	var prevFloor, prevEnd uint64
	for _, n := range sizes {
		mustWrite(t, r, patSlice(total, n))
		total += uint64(n)
		prevFloor, prevEnd = checkBounds(t, r, total, capBytes, prevFloor, prevEnd)
	}
	// A cursor-0 read must report the whole evicted gap and return exactly the
	// NEWEST capBytes of the stream (tail retention, not prefix retention).
	zero := uint64(0)
	data, next, dropped := r.Read(&zero, 100)
	if dropped != total-capBytes {
		t.Fatalf("dropped = %d, want %d", dropped, total-capBytes)
	}
	if want := patSlice(total-capBytes, capBytes); !bytes.Equal(data, want) {
		t.Fatalf("data = %v, want newest %d pattern bytes %v", data, capBytes, want)
	}
	if next != total {
		t.Fatalf("next = %d, want %d", next, total)
	}
}

func TestTailRingWriteLargerThanCapacity(t *testing.T) {
	const capBytes = 8
	r := newTailRing(capBytes)
	mustWrite(t, r, patSlice(0, 20))
	floor, end := r.Bounds()
	if floor != 12 || end != 20 {
		t.Fatalf("Bounds() = (%d, %d), want (12, 20)", floor, end)
	}
	data, next, dropped := r.Read(nil, 100)
	if want := patSlice(12, 8); !bytes.Equal(data, want) {
		t.Fatalf("data = %v, want newest 8 bytes %v", data, want)
	}
	if next != 20 || dropped != 0 {
		t.Fatalf("(next, dropped) = (%d, %d), want (20, 0)", next, dropped)
	}
}

func TestTailRingCursorBehindFloor(t *testing.T) {
	r := newTailRing(8)
	mustWrite(t, r, patSlice(0, 20)) // floor=12 end=20
	cursor := uint64(5)
	data, next, dropped := r.Read(&cursor, 100)
	if dropped != 7 {
		t.Fatalf("dropped = %d, want 7 (floor 12 - cursor 5)", dropped)
	}
	if want := patSlice(12, 8); !bytes.Equal(data, want) {
		t.Fatalf("data = %v, want bytes from floor %v", data, want)
	}
	if next != 20 {
		t.Fatalf("next = %d, want 20", next)
	}
	// Behind floor AND limited: data still starts at floor, next = floor + n.
	cursor = 3
	data, next, dropped = r.Read(&cursor, 4)
	if dropped != 9 {
		t.Fatalf("dropped = %d, want 9", dropped)
	}
	if want := patSlice(12, 4); !bytes.Equal(data, want) {
		t.Fatalf("data = %v, want first 4 retained bytes %v", data, want)
	}
	if next != 16 {
		t.Fatalf("next = %d, want 16 (floor 12 + 4)", next)
	}
}

func TestTailRingCursorAtAndAheadOfEnd(t *testing.T) {
	r := newTailRing(8)
	mustWrite(t, r, patSlice(0, 5))
	at := uint64(5)
	data, next, dropped := r.Read(&at, 10)
	if len(data) != 0 || next != 5 || dropped != 0 {
		t.Fatalf("Read(&end) = (%d bytes, %d, %d), want (0 bytes, 5, 0)", len(data), next, dropped)
	}
	ahead := uint64(99)
	data, next, dropped = r.Read(&ahead, 10)
	if len(data) != 0 || next != 5 || dropped != 0 {
		t.Fatalf("Read(&99) = (%d bytes, %d, %d), want (0 bytes, 5, 0)", len(data), next, dropped)
	}
}

func TestTailRingInitialTailNewest(t *testing.T) {
	r := newTailRing(8)
	mustWrite(t, r, []byte("abcdefgh"))
	// maxBytes < retained: must be the NEWEST 3 bytes (a true tail), and next
	// must be end so a follow-up cursor read resumes after what was shown.
	data, next, dropped := r.Read(nil, 3)
	if string(data) != "fgh" {
		t.Fatalf("tail data = %q, want \"fgh\" (newest bytes, not oldest)", data)
	}
	if next != 8 || dropped != 0 {
		t.Fatalf("(next, dropped) = (%d, %d), want (8, 0)", next, dropped)
	}
	// maxBytes >= retained: everything retained.
	data, next, dropped = r.Read(nil, 100)
	if string(data) != "abcdefgh" || next != 8 || dropped != 0 {
		t.Fatalf("full tail = (%q, %d, %d), want (\"abcdefgh\", 8, 0)", data, next, dropped)
	}
}

func TestTailRingReadLimit(t *testing.T) {
	r := newTailRing(16)
	mustWrite(t, r, patSlice(0, 10))
	cursor := uint64(2)
	data, next, dropped := r.Read(&cursor, 3)
	if want := patSlice(2, 3); !bytes.Equal(data, want) {
		t.Fatalf("data = %v, want %v", data, want)
	}
	if next != 5 || dropped != 0 {
		t.Fatalf("(next, dropped) = (%d, %d), want (5, 0)", next, dropped)
	}
	// Resume from next: the limit chunks a stream without losing bytes.
	data, next, dropped = r.Read(&next, 100)
	if want := patSlice(5, 5); !bytes.Equal(data, want) {
		t.Fatalf("resumed data = %v, want %v", data, want)
	}
	if next != 10 || dropped != 0 {
		t.Fatalf("(next, dropped) = (%d, %d), want (10, 0)", next, dropped)
	}
}

func TestTailRingReturnedBytesCopied(t *testing.T) {
	r := newTailRing(8)
	mustWrite(t, r, patSlice(0, 8))
	zero := uint64(0)
	data, _, _ := r.Read(&zero, 100)
	snapshot := append([]byte(nil), data...)
	// Overwrite the ring completely; an aliased return would now show new bytes.
	mustWrite(t, r, patSlice(8, 8))
	mustWrite(t, r, patSlice(16, 8))
	if !bytes.Equal(data, snapshot) {
		t.Fatalf("returned bytes mutated by later writes: %v, want %v", data, snapshot)
	}
	tail, _, _ := r.Read(nil, 100)
	tailSnapshot := append([]byte(nil), tail...)
	mustWrite(t, r, patSlice(24, 8))
	if !bytes.Equal(tail, tailSnapshot) {
		t.Fatalf("tail-mode bytes mutated by later writes: %v, want %v", tail, tailSnapshot)
	}
}

func TestTailRingBackingStorageNeverReallocated(t *testing.T) {
	const capBytes = 8
	r := newTailRing(capBytes)
	base := &r.buf[:capBytes][0]
	for i := 0; i < 120; i++ {
		// Alternate overflow shapes: bigger than capacity and partial-overflow.
		mustWrite(t, r, patSlice(0, capBytes+5))
		mustWrite(t, r, patSlice(0, capBytes-1))
	}
	if cap(r.buf) != capBytes {
		t.Fatalf("backing cap = %d, want %d (storage regrew)", cap(r.buf), capBytes)
	}
	if &r.buf[:capBytes][0] != base {
		t.Fatal("backing array reallocated across overflow writes")
	}
}

func TestTailRingConstructorPanicsOnBadCap(t *testing.T) {
	for _, capBytes := range []int{0, -1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("newTailRing(%d) did not panic", capBytes)
				}
			}()
			newTailRing(capBytes)
		}()
	}
}

func TestTailRingReadPanicsOnBadMaxBytes(t *testing.T) {
	r := newTailRing(8)
	for _, maxBytes := range []int{0, -3} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Read(nil, %d) did not panic", maxBytes)
				}
			}()
			r.Read(nil, maxBytes)
		}()
	}
}

func TestTailRingConcurrentWriteRead(t *testing.T) {
	const capBytes = 512
	r := newTailRing(capBytes)

	// Deterministic write sizes, total computed independently of the ring.
	sizes := make([]int, 5000)
	var total uint64
	for i := range sizes {
		sizes[i] = 1 + (i*17)%37
		total += uint64(sizes[i])
	}

	var wg sync.WaitGroup
	writerDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(writerDone)
		var off uint64
		for _, n := range sizes {
			wn, err := r.Write(patSlice(off, n))
			if wn != n || err != nil {
				t.Errorf("Write(%d) = (%d, %v), want (%d, nil)", n, wn, err, n)
				return
			}
			off += uint64(n)
		}
	}()

	// Reader: cursor-mode from 0. Every returned byte must match the writer's
	// pattern at its absolute offset (catches torn reads), and returned+dropped
	// must account for every byte the writer produced.
	var got, droppedTotal uint64
	cursor := uint64(0)
	done := false
	const maxIters = 50_000_000 // bounded spin: counts, not timing
	for iter := 0; ; iter++ {
		if iter > maxIters {
			t.Fatalf("reader did not reach total %d after %d iterations (cursor=%d)", total, maxIters, cursor)
		}
		select {
		case <-writerDone:
			done = true
		default:
		}
		data, next, dropped := r.Read(&cursor, 128)
		start := cursor + dropped
		for k, b := range data {
			if want := patByte(start + uint64(k)); b != want {
				t.Fatalf("byte at offset %d = %d, want %d (torn read)", start+uint64(k), b, want)
			}
		}
		got += uint64(len(data))
		droppedTotal += dropped
		cursor = next
		if done && cursor == total {
			break
		}
		if len(data) == 0 {
			runtime.Gosched()
		}
	}
	wg.Wait()

	floor, end := r.Bounds()
	if end != total {
		t.Fatalf("end = %d, want every accepted byte %d", end, total)
	}
	if end-floor != capBytes {
		t.Fatalf("retained = %d, want %d", end-floor, capBytes)
	}
	if got+droppedTotal != total {
		t.Fatalf("returned %d + dropped %d = %d, want %d", got, droppedTotal, got+droppedTotal, total)
	}
}
