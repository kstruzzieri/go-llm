package prefetch

import (
	"container/list"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/rag"
)

// cacheKey combines query text, result count, and context-sensitive fields
// so that different (query, k, context) combinations get independent entries.
type cacheKey struct {
	query         string
	k             int
	currentFile   string
	workspaceRoot string
	openFiles     []string
	timestampUnix int64
}

func newCacheKey(query string, k int, qCtx rag.QueryContext) cacheKey {
	openFiles := append([]string(nil), qCtx.OpenFiles...)
	sort.Strings(openFiles)

	var ts int64
	if !qCtx.Timestamp.IsZero() {
		ts = qCtx.Timestamp.Unix()
	}

	return cacheKey{
		query:         query,
		k:             k,
		currentFile:   qCtx.CurrentFile,
		workspaceRoot: qCtx.WorkspaceRoot,
		openFiles:     openFiles,
		timestampUnix: ts,
	}
}

func (ck cacheKey) String() string {
	var b strings.Builder
	b.WriteString(ck.query)
	b.WriteString("\x00")
	b.WriteString(strconv.Itoa(ck.k))
	b.WriteString("\x00")
	b.WriteString(ck.currentFile)
	b.WriteString("\x00")
	b.WriteString(ck.workspaceRoot)
	b.WriteString("\x00")
	b.WriteString(strings.Join(ck.openFiles, "\x1f"))
	b.WriteString("\x00")
	b.WriteString(strconv.FormatInt(ck.timestampUnix, 10))
	return b.String()
}

// cacheEntry holds a cached set of scored results with an expiration time.
type cacheEntry struct {
	key       string
	results   []rag.ScoredResult
	expiresAt time.Time
}

// WarmCache is a concurrency-safe, in-memory LRU cache with per-entry TTL.
// It stores scored retrieval results keyed by query string.
type WarmCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	items    map[string]*list.Element
	order    *list.List // front = most recently used
	now      func() time.Time
}

// NewWarmCache creates a WarmCache with the given maximum number of entries
// and time-to-live for each entry. Entries that exceed the TTL are treated
// as expired and evicted lazily on access.
func NewWarmCache(capacity int, ttl time.Duration) *WarmCache {
	return &WarmCache{
		capacity: capacity,
		ttl:      ttl,
		items:    make(map[string]*list.Element),
		order:    list.New(),
		now:      time.Now,
	}
}

// Get retrieves cached results for the given query key.
// Returns nil and false if the key is absent or expired.
func (c *WarmCache) Get(key string) ([]rag.ScoredResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return nil, false
	}

	entry := elem.Value.(*cacheEntry)
	if c.now().After(entry.expiresAt) {
		c.removeLocked(elem)
		return nil, false
	}

	// Move to front (most recently used).
	c.order.MoveToFront(elem)
	// Return a copy so callers cannot mutate the cached slice.
	cp := make([]rag.ScoredResult, len(entry.results))
	copy(cp, entry.results)
	return cp, true
}

// Put stores results for the given query key, evicting the least recently
// used entry if the cache is at capacity.
func (c *WarmCache) Put(key string, results []rag.ScoredResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Defensive copy so callers cannot mutate cached state.
	stored := make([]rag.ScoredResult, len(results))
	copy(stored, results)

	// Update existing entry in-place.
	if elem, ok := c.items[key]; ok {
		entry := elem.Value.(*cacheEntry)
		entry.results = stored
		entry.expiresAt = c.now().Add(c.ttl)
		c.order.MoveToFront(elem)
		return
	}

	// Zero capacity means caching is disabled.
	if c.capacity <= 0 {
		return
	}

	// Evict LRU if at capacity.
	for c.order.Len() >= c.capacity {
		c.evictLRULocked()
	}

	entry := &cacheEntry{
		key:       key,
		results:   stored,
		expiresAt: c.now().Add(c.ttl),
	}
	elem := c.order.PushFront(entry)
	c.items[key] = elem
}

// Len returns the number of entries currently in the cache (including expired).
func (c *WarmCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Clear removes all entries from the cache.
func (c *WarmCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*list.Element)
	c.order.Init()
}

// removeLocked removes a specific element. Caller must hold c.mu.
func (c *WarmCache) removeLocked(elem *list.Element) {
	entry := elem.Value.(*cacheEntry)
	delete(c.items, entry.key)
	c.order.Remove(elem)
}

// evictLRULocked removes the least recently used entry. Caller must hold c.mu.
func (c *WarmCache) evictLRULocked() {
	back := c.order.Back()
	if back == nil {
		return
	}
	c.removeLocked(back)
}
