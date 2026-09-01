package tools

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
)

// fixedReader yields the given byte blocks in order, then fails.
type fixedReader struct {
	blocks [][]byte
}

func (r *fixedReader) Read(p []byte) (int, error) {
	if len(r.blocks) == 0 {
		return 0, errors.New("entropy exhausted")
	}
	n := copy(p, r.blocks[0])
	r.blocks = r.blocks[1:]
	return n, nil
}

func newTestStore(random *fixedReader) *scratchStore {
	return newScratchStore(random)
}

func TestScratchStoreIDFormat(t *testing.T) {
	s := newTestStore(&fixedReader{blocks: [][]byte{bytes.Repeat([]byte{0xab}, 16)}})
	id, err := s.newID()
	if err != nil {
		t.Fatal(err)
	}
	if id != "scr-"+strings.Repeat("ab", 16) {
		t.Fatalf("id = %q, want scr- plus 32 hex chars (128-bit)", id)
	}
}

func TestScratchStoreEntropyFailure(t *testing.T) {
	s := newTestStore(&fixedReader{})
	if _, err := s.newID(); err == nil {
		t.Fatal("entropy failure must surface, not degrade")
	}
}

func TestScratchStoreCollisionRegenerates(t *testing.T) {
	same := bytes.Repeat([]byte{0x01}, 16)
	other := bytes.Repeat([]byte{0x02}, 16)
	s := newTestStore(&fixedReader{blocks: [][]byte{same, same, other}})
	id1, err := s.newID()
	if err != nil {
		t.Fatal(err)
	}
	s.beginPending(id1)
	id2, err := s.newID()
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("collision with a pending id must regenerate")
	}
}

func TestScratchStoreFIFOEvictionSparesPending(t *testing.T) {
	s := newTestStore(&fixedReader{})
	s.beginPending("scr-pending")
	for i := 0; i < scratchStoreRetainedCap+1; i++ {
		id := "scr-" + strings.Repeat("0", 31) + string(rune('a'+i))
		s.beginPending(id)
		s.completePending(id, scratchOutcome{id: id})
	}
	if _, status := s.get("scr-" + strings.Repeat("0", 31) + "a"); status != scratchStatusUnknown {
		t.Fatalf("oldest completed outcome must be evicted, status=%v", status)
	}
	if _, status := s.get("scr-" + strings.Repeat("0", 31) + "b"); status != scratchStatusCaptured {
		t.Fatalf("second-oldest must survive, status=%v", status)
	}
	if _, status := s.get("scr-pending"); status != scratchStatusPending {
		t.Fatal("pending sessions must never be evicted")
	}
}

func TestScratchStoreDeepCopyGet(t *testing.T) {
	s := newTestStore(&fixedReader{})
	s.beginPending("scr-x")
	s.completePending("scr-x", scratchOutcome{
		id:      "scr-x",
		changes: []scratchChange{{path: "a.txt", kind: scratchChangeCreate, data: []byte("orig"), promotable: true}},
	})
	out, status := s.get("scr-x")
	if status != scratchStatusCaptured {
		t.Fatalf("status = %v", status)
	}
	out.changes[0].data[0] = 'X'
	out.changes[0].path = "mutated"
	again, _ := s.get("scr-x")
	if string(again.changes[0].data) != "orig" || again.changes[0].path != "a.txt" {
		t.Fatal("get must return a deep copy; caller mutation reached the store")
	}
}

func TestScratchStoreClaimReleaseConsume(t *testing.T) {
	s := newTestStore(&fixedReader{})
	s.beginPending("scr-x")
	s.completePending("scr-x", scratchOutcome{id: "scr-x", changes: []scratchChange{{path: "a.txt"}}})
	if err := s.claim("scr-x", "a.txt"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := s.claim("scr-x", "a.txt"); err == nil {
		t.Fatal("duplicate claim must fail")
	}
	s.release("scr-x", "a.txt")
	if err := s.claim("scr-x", "a.txt"); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	s.consume("scr-x", "a.txt")
	if err := s.claim("scr-x", "a.txt"); err == nil {
		t.Fatal("consumed path must never be claimable again")
	}
	if err := s.claim("scr-missing", "a.txt"); err == nil {
		t.Fatal("claim on unknown id must fail")
	}
}

func TestScratchStoreConcurrency(t *testing.T) {
	s := newTestStore(&fixedReader{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "scr-" + strings.Repeat("0", 31) + string(rune('a'+i))
			s.beginPending(id)
			s.completePending(id, scratchOutcome{id: id, changes: []scratchChange{{path: "p"}}})
			_, _ = s.get(id)
			_ = s.claim(id, "p")
			s.release(id, "p")
			s.delete(id)
		}(i)
	}
	wg.Wait()
}
