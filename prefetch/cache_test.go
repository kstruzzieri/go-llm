package prefetch

import (
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/rag"
)

func makeResults(ids ...string) []rag.ScoredResult {
	results := make([]rag.ScoredResult, len(ids))
	for i, id := range ids {
		results[i] = rag.ScoredResult{
			SearchResult: rag.SearchResult{
				Chunk: rag.Chunk{ID: id, Content: "content-" + id},
				Score: 0.9,
			},
			Signals: map[string]float64{"semantic": 0.9},
		}
	}
	return results
}

func TestWarmCache_GetPut(t *testing.T) {
	c := NewWarmCache(10, 5*time.Minute)

	// Miss on empty cache.
	if _, ok := c.Get("key1"); ok {
		t.Fatal("expected miss on empty cache")
	}

	results := makeResults("a", "b")
	c.Put("key1", results)

	got, ok := c.Get("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].Chunk.ID != "a" {
		t.Errorf("expected chunk ID 'a', got %q", got[0].Chunk.ID)
	}
}

func TestWarmCache_TTLExpiry(t *testing.T) {
	c := NewWarmCache(10, 100*time.Millisecond)

	// Override the clock for deterministic testing.
	now := time.Now()
	c.now = func() time.Time { return now }

	c.Put("key1", makeResults("a"))

	// Advance time past TTL.
	now = now.Add(200 * time.Millisecond)

	if _, ok := c.Get("key1"); ok {
		t.Fatal("expected miss after TTL expiry")
	}

	// Verify the expired entry was evicted.
	if c.Len() != 0 {
		t.Errorf("expected 0 entries after TTL eviction, got %d", c.Len())
	}
}

func TestWarmCache_LRUEviction(t *testing.T) {
	c := NewWarmCache(3, 5*time.Minute)

	c.Put("key1", makeResults("a"))
	c.Put("key2", makeResults("b"))
	c.Put("key3", makeResults("c"))

	// Access key1 to make it recently used.
	c.Get("key1")

	// Adding key4 should evict key2 (least recently used).
	c.Put("key4", makeResults("d"))

	if _, ok := c.Get("key2"); ok {
		t.Error("expected key2 to be evicted (LRU)")
	}
	if _, ok := c.Get("key1"); !ok {
		t.Error("expected key1 to still be present (recently accessed)")
	}
	if _, ok := c.Get("key3"); !ok {
		t.Error("expected key3 to still be present")
	}
	if _, ok := c.Get("key4"); !ok {
		t.Error("expected key4 to be present")
	}
}

func TestWarmCache_UpdateExisting(t *testing.T) {
	c := NewWarmCache(10, 5*time.Minute)

	c.Put("key1", makeResults("a"))
	c.Put("key1", makeResults("b", "c"))

	got, ok := c.Get("key1")
	if !ok {
		t.Fatal("expected cache hit")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results after update, got %d", len(got))
	}
	if got[0].Chunk.ID != "b" {
		t.Errorf("expected updated chunk ID 'b', got %q", got[0].Chunk.ID)
	}

	// Capacity should not have increased.
	if c.Len() != 1 {
		t.Errorf("expected 1 entry after update, got %d", c.Len())
	}
}

func TestWarmCache_Clear(t *testing.T) {
	c := NewWarmCache(10, 5*time.Minute)
	c.Put("key1", makeResults("a"))
	c.Put("key2", makeResults("b"))

	c.Clear()

	if c.Len() != 0 {
		t.Errorf("expected 0 entries after clear, got %d", c.Len())
	}
	if _, ok := c.Get("key1"); ok {
		t.Error("expected miss after clear")
	}
}

func TestWarmCache_ConcurrentAccess(t *testing.T) {
	c := NewWarmCache(100, 5*time.Minute)

	var wg sync.WaitGroup
	const goroutines = 50
	const opsPerGoroutine = 100

	// Writer goroutines.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := "key-" + string(rune('A'+id%26))
				c.Put(key, makeResults("chunk"))
			}
		}(i)
	}

	// Reader goroutines.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				key := "key-" + string(rune('A'+id%26))
				c.Get(key)
			}
		}(i)
	}

	wg.Wait()

	// If we get here without a race condition, the test passes.
	// Exact count depends on timing, just verify it's within bounds.
	if c.Len() > 100 {
		t.Errorf("cache exceeded capacity: %d > 100", c.Len())
	}
}

func TestWarmCache_ZeroCapacity(t *testing.T) {
	c := NewWarmCache(0, 5*time.Minute)
	c.Put("key1", makeResults("a"))

	// With zero capacity, nothing should be stored.
	if _, ok := c.Get("key1"); ok {
		t.Error("expected miss with zero capacity")
	}
	if c.Len() != 0 {
		t.Errorf("expected 0 entries with zero capacity, got %d", c.Len())
	}
}

func TestWarmCache_TTLRefreshOnUpdate(t *testing.T) {
	c := NewWarmCache(10, 200*time.Millisecond)

	now := time.Now()
	c.now = func() time.Time { return now }

	c.Put("key1", makeResults("a"))

	// Advance 150ms (within TTL).
	now = now.Add(150 * time.Millisecond)

	// Update the entry, which should refresh the TTL.
	c.Put("key1", makeResults("b"))

	// Advance another 150ms (350ms from original, but only 150ms from update).
	now = now.Add(150 * time.Millisecond)

	got, ok := c.Get("key1")
	if !ok {
		t.Fatal("expected hit: TTL should have been refreshed on update")
	}
	if got[0].Chunk.ID != "b" {
		t.Errorf("expected chunk ID 'b', got %q", got[0].Chunk.ID)
	}
}
