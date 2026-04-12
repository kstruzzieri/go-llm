// router_sticky.go implements a sticky routing cache with LRU eviction and
// touch-based TTL expiry. When the router selects a provider/model for a
// request, the decision is cached under a deterministic sticky key (derived
// from RoutingRequest fields). Subsequent requests with the same key reuse
// the cached route, providing session affinity and reducing re-scoring.
//
// The cache is bounded by maxEntries. Expired entries are lazily evicted on
// lookup. When inserting at capacity, expired entries are purged first,
// then the least-recently-used entry is evicted.
package provider

import (
	"sync"
	"time"
)

// routeSticky holds a cached routing decision.
type routeSticky struct {
	key         string
	providerKey ModelKey
	score       float64
	reason      string
	createdAt   time.Time
	lastUsedAt  time.Time
	expiresAt   time.Time
}

// StickyRouteInfo is the exported observability view of a cached route.
type StickyRouteInfo struct {
	Key        ModelKey
	Score      float64
	Reason     string
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
}

// stickyCache is an LRU-bounded, TTL-expiring cache of routing decisions.
type stickyCache struct {
	mu         sync.Mutex
	entries    map[string]*routeSticky
	ttl        time.Duration
	maxEntries int
}

// newStickyCache creates a stickyCache with the given TTL and capacity.
func newStickyCache(ttl time.Duration, maxEntries int) *stickyCache {
	return &stickyCache{
		entries:    make(map[string]*routeSticky),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

// get retrieves an entry by key. Returns the zero value and false if not found
// or expired. The returned struct is a copy — safe to read without holding the
// lock. Expired entries are lazily evicted on lookup.
func (sc *stickyCache) get(key string) (routeSticky, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	entry, ok := sc.entries[key]
	if !ok {
		return routeSticky{}, false
	}

	// Lazy expiry: if the entry has expired, delete it and report a miss.
	if time.Now().After(entry.expiresAt) {
		delete(sc.entries, key)
		return routeSticky{}, false
	}

	return *entry, true
}

// put inserts or replaces an entry. If at capacity, expired entries are
// purged first, then the least-recently-used entry is evicted.
func (sc *stickyCache) put(entry *routeSticky) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	// Overwrite: if the key already exists, just replace — no eviction needed.
	if _, exists := sc.entries[entry.key]; exists {
		sc.entries[entry.key] = entry
		return
	}

	// At capacity: evict to make room.
	if len(sc.entries) >= sc.maxEntries {
		sc.evictLocked()
	}

	sc.entries[entry.key] = entry
}

// touch updates lastUsedAt and extends expiresAt to now + TTL.
func (sc *stickyCache) touch(key string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	entry, ok := sc.entries[key]
	if !ok {
		return
	}

	now := time.Now()
	entry.lastUsedAt = now
	entry.expiresAt = now.Add(sc.ttl)
}

// invalidate removes a specific entry by key.
func (sc *stickyCache) invalidate(key string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	delete(sc.entries, key)
}

// invalidateProvider removes all entries routed to the given provider name.
// This is used when a circuit breaker opens for a provider, ensuring stale
// affinity routes are cleared.
func (sc *stickyCache) invalidateProvider(provider string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	for k, entry := range sc.entries {
		if entry.providerKey.Provider == provider {
			delete(sc.entries, k)
		}
	}
}

// snapshot returns an exported copy of all current entries for observability.
// Expired entries are not included.
func (sc *stickyCache) snapshot() map[string]StickyRouteInfo {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	now := time.Now()
	result := make(map[string]StickyRouteInfo, len(sc.entries))
	for k, entry := range sc.entries {
		if now.After(entry.expiresAt) {
			// Skip expired entries in the snapshot (but don't evict here
			// to keep snapshot read-only from a mutation perspective).
			continue
		}
		result[k] = StickyRouteInfo{
			Key:        entry.providerKey,
			Score:      entry.score,
			Reason:     entry.reason,
			CreatedAt:  entry.createdAt,
			LastUsedAt: entry.lastUsedAt,
			ExpiresAt:  entry.expiresAt,
		}
	}
	return result
}

// evictLocked removes entries to free capacity. It must be called with sc.mu
// held. Phase 1: remove all expired entries. Phase 2: if still at capacity,
// remove the entry with the oldest lastUsedAt (LRU).
func (sc *stickyCache) evictLocked() {
	now := time.Now()

	// Phase 1: remove expired entries.
	for k, entry := range sc.entries {
		if now.After(entry.expiresAt) {
			delete(sc.entries, k)
		}
	}

	// Phase 2: if still at capacity, evict the LRU entry.
	if len(sc.entries) >= sc.maxEntries {
		var oldestKey string
		var oldestTime time.Time
		first := true

		for k, entry := range sc.entries {
			if first || entry.lastUsedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = entry.lastUsedAt
				first = false
			}
		}

		if !first {
			delete(sc.entries, oldestKey)
		}
	}
}
