package compat

import (
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
