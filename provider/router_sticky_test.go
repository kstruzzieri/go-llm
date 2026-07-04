package provider

import (
	"testing"
	"time"
)

func TestStickyCacheGetMiss(t *testing.T) {
	sc := newStickyCache(time.Minute, 100)

	got, ok := sc.get("nonexistent")
	if ok {
		t.Fatal("expected miss on empty cache, got hit")
	}
	if got.key != "" {
		t.Fatalf("expected zero entry on miss, got %+v", got)
	}
}

func TestStickyCachePutAndGet(t *testing.T) {
	sc := newStickyCache(time.Minute, 100)

	entry := &routeSticky{
		key:         "key1",
		providerKey: ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		score:       0.85,
		reason:      "warmth+quality",
		createdAt:   time.Now(),
		lastUsedAt:  time.Now(),
		expiresAt:   time.Now().Add(time.Minute),
	}
	sc.put(entry)

	got, ok := sc.get("key1")
	if !ok {
		t.Fatal("expected hit after put, got miss")
	}
	if got.score != 0.85 {
		t.Fatalf("expected score 0.85, got %f", got.score)
	}
	if got.providerKey.Model != "qwen3:8b" {
		t.Fatalf("expected model qwen3:8b, got %s", got.providerKey.Model)
	}
}

func TestStickyCacheExpiry(t *testing.T) {
	sc := newStickyCache(10*time.Millisecond, 100)

	now := time.Now()
	entry := &routeSticky{
		key:         "ephemeral",
		providerKey: ModelKey{Provider: "ollama", Model: "tiny"},
		score:       0.5,
		reason:      "test",
		createdAt:   now,
		lastUsedAt:  now,
		expiresAt:   now.Add(10 * time.Millisecond),
	}
	sc.put(entry)

	// Verify it's there initially.
	if _, ok := sc.get("ephemeral"); !ok {
		t.Fatal("expected hit immediately after put")
	}

	time.Sleep(20 * time.Millisecond)

	got, ok := sc.get("ephemeral")
	if ok {
		t.Fatal("expected miss after TTL expiry, got hit")
	}
	if got.key != "" {
		t.Fatal("expected zero entry after expiry")
	}
}

func TestStickyCacheTouch(t *testing.T) {
	sc := newStickyCache(50*time.Millisecond, 100)

	now := time.Now()
	entry := &routeSticky{
		key:         "touchme",
		providerKey: ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		score:       0.7,
		reason:      "touch test",
		createdAt:   now,
		lastUsedAt:  now,
		expiresAt:   now.Add(50 * time.Millisecond),
	}
	sc.put(entry)

	// Wait 30ms (past 60% of TTL), then touch to extend.
	time.Sleep(30 * time.Millisecond)
	sc.touch("touchme")

	// Wait another 30ms (total 60ms from creation, but only 30ms from touch).
	time.Sleep(30 * time.Millisecond)

	got, ok := sc.get("touchme")
	if !ok {
		t.Fatal("expected hit after touch extended TTL, got miss")
	}
	if got.score != 0.7 {
		t.Fatalf("expected score 0.7, got %f", got.score)
	}
}

func TestStickyCacheLRUEviction(t *testing.T) {
	sc := newStickyCache(time.Minute, 3)

	now := time.Now()
	// Insert 3 entries with staggered lastUsedAt so LRU order is deterministic.
	for i, key := range []string{"a", "b", "c"} {
		sc.put(&routeSticky{
			key:         key,
			providerKey: ModelKey{Provider: "ollama", Model: key},
			score:       float64(i) * 0.1,
			reason:      "lru test",
			createdAt:   now,
			lastUsedAt:  now.Add(time.Duration(i) * time.Millisecond),
			expiresAt:   now.Add(time.Minute),
		})
	}

	// Insert a 4th entry, which should evict "a" (oldest lastUsedAt).
	sc.put(&routeSticky{
		key:         "d",
		providerKey: ModelKey{Provider: "ollama", Model: "d"},
		score:       0.9,
		reason:      "new entry",
		createdAt:   now,
		lastUsedAt:  now.Add(3 * time.Millisecond),
		expiresAt:   now.Add(time.Minute),
	})

	// "a" should be evicted.
	if _, ok := sc.get("a"); ok {
		t.Fatal("expected 'a' to be evicted (oldest LRU), but it was found")
	}

	// "b", "c", "d" should still be present.
	for _, key := range []string{"b", "c", "d"} {
		if _, ok := sc.get(key); !ok {
			t.Fatalf("expected '%s' to be present after LRU eviction", key)
		}
	}
}

func TestStickyCacheInvalidate(t *testing.T) {
	sc := newStickyCache(time.Minute, 100)

	now := time.Now()
	sc.put(&routeSticky{
		key:         "doomed",
		providerKey: ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		score:       0.6,
		reason:      "will be invalidated",
		createdAt:   now,
		lastUsedAt:  now,
		expiresAt:   now.Add(time.Minute),
	})

	// Verify it exists.
	if _, ok := sc.get("doomed"); !ok {
		t.Fatal("expected entry to exist before invalidation")
	}

	sc.invalidate("doomed")

	if _, ok := sc.get("doomed"); ok {
		t.Fatal("expected miss after invalidation, got hit")
	}
}

func TestStickyCacheInvalidateProvider(t *testing.T) {
	sc := newStickyCache(time.Minute, 100)

	now := time.Now()
	// Two entries for "ollama", one for "openai".
	sc.put(&routeSticky{
		key:         "ollama1",
		providerKey: ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		score:       0.7,
		reason:      "ollama entry 1",
		createdAt:   now,
		lastUsedAt:  now,
		expiresAt:   now.Add(time.Minute),
	})
	sc.put(&routeSticky{
		key:         "ollama2",
		providerKey: ModelKey{Provider: "ollama", Model: "qwen3:72b"},
		score:       0.9,
		reason:      "ollama entry 2",
		createdAt:   now,
		lastUsedAt:  now,
		expiresAt:   now.Add(time.Minute),
	})
	sc.put(&routeSticky{
		key:         "openai1",
		providerKey: ModelKey{Provider: "openai", Model: "gpt-4o"},
		score:       0.8,
		reason:      "openai entry",
		createdAt:   now,
		lastUsedAt:  now,
		expiresAt:   now.Add(time.Minute),
	})

	sc.invalidateProvider("ollama")

	// Both ollama entries should be gone.
	if _, ok := sc.get("ollama1"); ok {
		t.Fatal("expected ollama1 to be invalidated")
	}
	if _, ok := sc.get("ollama2"); ok {
		t.Fatal("expected ollama2 to be invalidated")
	}

	// openai entry should survive.
	if _, ok := sc.get("openai1"); !ok {
		t.Fatal("expected openai1 to survive provider invalidation")
	}
}

func TestStickyCacheSnapshot(t *testing.T) {
	sc := newStickyCache(time.Minute, 100)

	now := time.Now()
	sc.put(&routeSticky{
		key:         "snap1",
		providerKey: ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		score:       0.75,
		reason:      "snapshot test",
		createdAt:   now,
		lastUsedAt:  now,
		expiresAt:   now.Add(time.Minute),
	})

	snap := sc.snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry in snapshot, got %d", len(snap))
	}

	info, ok := snap["snap1"]
	if !ok {
		t.Fatal("expected 'snap1' in snapshot")
	}
	if info.Key != (ModelKey{Provider: "ollama", Model: "qwen3:8b"}) {
		t.Fatalf("unexpected key in snapshot: %+v", info.Key)
	}
	if info.Score != 0.75 {
		t.Fatalf("expected score 0.75 in snapshot, got %f", info.Score)
	}
}

func TestHysteresisCheck(t *testing.T) {
	// The hysteresis formula: keep incumbent if
	//   challengerScore < incumbentScore * (1 + margin)
	// i.e. challenger must exceed incumbent*(1+margin) to unseat.
	const margin = 0.15

	tests := []struct {
		name            string
		incumbentScore  float64
		challengerScore float64
		margin          float64
		wantKeep        bool // true = keep incumbent
	}{
		{
			name:            "challenger far better",
			incumbentScore:  0.70,
			challengerScore: 0.90,
			margin:          margin,
			wantKeep:        false, // 0.90 >= 0.70*1.15=0.805
		},
		{
			name:            "challenger marginally better",
			incumbentScore:  0.70,
			challengerScore: 0.78,
			margin:          margin,
			wantKeep:        true, // 0.78 < 0.805
		},
		{
			name:            "challenger at margin boundary",
			incumbentScore:  0.70,
			challengerScore: 0.805,
			margin:          margin,
			wantKeep:        false, // 0.805 >= 0.805
		},
		{
			name:            "challenger equal",
			incumbentScore:  0.70,
			challengerScore: 0.70,
			margin:          margin,
			wantKeep:        true, // 0.70 < 0.805
		},
		{
			name:            "challenger worse",
			incumbentScore:  0.70,
			challengerScore: 0.60,
			margin:          margin,
			wantKeep:        true, // 0.60 < 0.805
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			threshold := tt.incumbentScore * (1 + tt.margin)
			keepIncumbent := tt.challengerScore < threshold

			if keepIncumbent != tt.wantKeep {
				t.Errorf(
					"challenger=%.3f incumbent=%.3f margin=%.2f threshold=%.3f: got keep=%v, want keep=%v",
					tt.challengerScore, tt.incumbentScore, tt.margin, threshold,
					keepIncumbent, tt.wantKeep,
				)
			}
		})
	}
}
