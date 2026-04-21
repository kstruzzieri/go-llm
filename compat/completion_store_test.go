package compat

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestCompletionRecordStore_PutGet(t *testing.T) {
	store := newCompletionRecordStore(time.Minute, 10)
	store.put(CompletionRecord{ID: "a", Provider: "ollama", Model: "qwen3:8b"})
	rec, ok := store.get("a")
	if !ok {
		t.Fatal("expected hit")
	}
	if rec.Provider != "ollama" {
		t.Errorf("provider = %q", rec.Provider)
	}
}

func TestCompletionRecordStore_Expiration(t *testing.T) {
	store := newCompletionRecordStore(10*time.Millisecond, 10)
	store.put(CompletionRecord{ID: "a"})
	time.Sleep(20 * time.Millisecond)
	if _, ok := store.get("a"); ok {
		t.Error("expected expired miss")
	}
}

func TestCompletionRecordStore_CapacityEviction(t *testing.T) {
	store := newCompletionRecordStore(time.Minute, 2)
	store.put(CompletionRecord{ID: "a"})
	store.put(CompletionRecord{ID: "b"})
	store.put(CompletionRecord{ID: "c"})
	if _, ok := store.get("a"); ok {
		t.Error("oldest entry must be evicted at capacity")
	}
	if _, ok := store.get("c"); !ok {
		t.Error("newest entry should survive")
	}
}

// TestCompletionRecordStore_EmptyIDRejected guards the defense-in-depth empty
// ID short-circuit in both put and get. Without the guard, a caller bug that
// constructed an empty ID would let the first empty-ID record "stick" and
// collide across unrelated requests.
func TestCompletionRecordStore_EmptyIDRejected(t *testing.T) {
	store := newCompletionRecordStore(time.Minute, 10)
	store.put(CompletionRecord{ID: "", Provider: "ollama"})
	rec, ok := store.get("")
	if ok {
		t.Errorf("get(\"\") returned ok; want (zero, false)")
	}
	if rec != (CompletionRecord{}) {
		t.Errorf("get(\"\") returned %+v; want zero value", rec)
	}
	if got := len(store.index); got != 0 {
		t.Errorf("store index length = %d; want 0 (empty-ID put must be silently ignored)", got)
	}
}

// TestCompletionRecordStore_ConcurrentAccess feeds the race detector: 8
// goroutines each put 100 unique records and read them back. The assertion is
// weak (no goroutine loses data), but "go test -race" is what this test
// really exercises.
func TestCompletionRecordStore_ConcurrentAccess(t *testing.T) {
	const (
		goroutines = 8
		perG       = 100
	)
	store := newCompletionRecordStore(time.Minute, goroutines*perG)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				id := fmt.Sprintf("g%d-i%d", g, i)
				store.put(CompletionRecord{ID: id, Provider: "ollama"})
				if _, ok := store.get(id); !ok {
					t.Errorf("get(%q) miss immediately after put", id)
					return
				}
			}
		}()
	}
	wg.Wait()
}
