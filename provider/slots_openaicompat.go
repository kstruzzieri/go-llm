package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultSlotTTL          = 5 * time.Minute
	defaultSlotProbeTimeout = 5 * time.Second
)

// SlotBackend describes one governed backend endpoint.
type SlotBackend struct {
	// BaseURL is the server root, without /v1 (matching openaicompat.NewClient).
	BaseURL string
	// APIKey, when non-empty, is sent as a Bearer token — the same auth
	// convention as the provider's own client. Never logged.
	APIKey string
}

// slotsPropsResponse is the subset of llama-server's GET /props JSON we read.
type slotsPropsResponse struct {
	TotalSlots int `json:"total_slots"`
}

// fetchSlotCapacity queries {base}/props?model={model} and returns
// total_slots. The query is ALWAYS model-qualified: llama-swap (and
// llama-server's model-dispatched router mode) requires it to route to the
// right upstream, and single-model llama-server ignores the unknown
// parameter — one code path covers both. A response without a positive
// total_slots is an error; callers translate errors to fail-safe capacity 1.
func fetchSlotCapacity(ctx context.Context, hc *http.Client, be SlotBackend, model string) (int, error) {
	u := strings.TrimRight(be.BaseURL, "/") + "/props?model=" + url.QueryEscape(model)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, fmt.Errorf("provider: slot probe %q: %w", model, err)
	}
	if be.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+be.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("provider: slot probe %q: %w", model, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("provider: slot probe %q: %s", model, resp.Status)
	}
	var props slotsPropsResponse
	if err := json.NewDecoder(resp.Body).Decode(&props); err != nil {
		return 0, fmt.Errorf("provider: slot probe %q: decode /props: %w", model, err)
	}
	if props.TotalSlots < 1 {
		return 0, fmt.Errorf("provider: slot probe %q: /props total_slots %d", model, props.TotalSlots)
	}
	return props.TotalSlots, nil
}

// slotEntry is one cached capacity observation.
type slotEntry struct {
	capacity  int
	fetchedAt time.Time
}

// OpenAICompatSlotSource discovers parallel slot capacity for openai-compat
// backends via GET /props?model=... (total_slots) and caches it per ModelKey.
//
// Refresh is triggered by RecordUse, never by a background poll loop: through
// llama-swap, /props?model=X is proxied to model X's upstream, which the
// proxy starts on demand — blanket polling would churn model swaps. RecordUse
// fires only after key successfully served a request, so its model is
// normally resident and the probe is swap-free (if another model evicted it
// in the window, the probe either swaps it back or errors into fail-safe 1 —
// bounded to one probe per key per TTL). Idle models are never probed.
type OpenAICompatSlotSource struct {
	mu       sync.RWMutex
	backends map[string]SlotBackend // provider name -> endpoint; read-only after construction
	entries  map[ModelKey]slotEntry
	inflight map[ModelKey]struct{}
	closed   bool
	wg       sync.WaitGroup
	ttl      time.Duration
	client   *http.Client
	nowFn    func() time.Time
	ctx      context.Context
	cancel   context.CancelFunc

	// launch starts a probe goroutine (default: go f()). Test seam: a
	// capturing launcher makes spawn decisions and probe completion
	// deterministic without sleeps.
	launch func(func())
}

// SlotSourceOption configures an OpenAICompatSlotSource.
type SlotSourceOption func(*OpenAICompatSlotSource)

// WithSlotTTL sets how long a cached capacity is considered fresh. Values
// <= 0 are ignored and the default (5m) is used.
func WithSlotTTL(d time.Duration) SlotSourceOption {
	return func(ss *OpenAICompatSlotSource) {
		if d > 0 {
			ss.ttl = d
		}
	}
}

// WithSlotHTTPClient overrides the probe HTTP client (default: 5s timeout).
func WithSlotHTTPClient(hc *http.Client) SlotSourceOption {
	return func(ss *OpenAICompatSlotSource) {
		if hc != nil {
			ss.client = hc
		}
	}
}

// NewOpenAICompatSlotSource creates a SlotSource governing the given
// backends (provider name -> endpoint). Providers not in the map are
// ungoverned: Capacity reports (0, false) and RecordUse is a no-op for
// them. The map is copied; trailing slashes on base URLs are stripped.
func NewOpenAICompatSlotSource(backends map[string]SlotBackend, opts ...SlotSourceOption) *OpenAICompatSlotSource {
	ss := &OpenAICompatSlotSource{
		backends: make(map[string]SlotBackend, len(backends)),
		entries:  make(map[ModelKey]slotEntry),
		inflight: make(map[ModelKey]struct{}),
		ttl:      defaultSlotTTL,
		client:   &http.Client{Timeout: defaultSlotProbeTimeout},
		nowFn:    time.Now,
		launch:   func(f func()) { go f() },
	}
	for k, v := range backends {
		v.BaseURL = strings.TrimRight(v.BaseURL, "/")
		ss.backends[k] = v
	}
	for _, opt := range opts {
		opt(ss)
	}
	ss.ctx, ss.cancel = context.WithCancel(context.Background())
	return ss
}

// Capacity implements SlotSource. It is a pure cache read.
func (ss *OpenAICompatSlotSource) Capacity(key ModelKey) (int, bool) {
	if _, governed := ss.backends[key.Provider]; !governed {
		return 0, false
	}
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	if e, ok := ss.entries[key]; ok {
		return e.capacity, true
	}
	return 1, true
}

// RecordUse implements SlotSource. Spawn decisions are linearized under the
// mutex with Close: no probe can be launched after Close returns.
func (ss *OpenAICompatSlotSource) RecordUse(key ModelKey) {
	be, governed := ss.backends[key.Provider]
	if !governed {
		return
	}
	ss.mu.Lock()
	if ss.closed {
		ss.mu.Unlock()
		return
	}
	if _, busy := ss.inflight[key]; busy {
		ss.mu.Unlock()
		return
	}
	ss.inflight[key] = struct{}{}
	ss.wg.Add(1)
	ss.mu.Unlock()
	ss.launch(func() { ss.probe(key, be) })
}

// probe fetches capacity for key and records the result. Errors degrade to
// capacity 1 (fail-safe serial) at the same TTL cadence, so a broken,
// evicted, or auth-failing backend is retried and recovers. A probe that
// loses to Close writes nothing (a cancellation must not clobber state).
func (ss *OpenAICompatSlotSource) probe(key ModelKey, be SlotBackend) {
	defer ss.wg.Done()
	ctx, cancel := context.WithTimeout(ss.ctx, defaultSlotProbeTimeout)
	defer cancel()
	n, err := fetchSlotCapacity(ctx, ss.client, be, key.Model)

	ss.mu.Lock()
	defer ss.mu.Unlock()
	delete(ss.inflight, key)
	if ss.closed {
		return
	}
	if err != nil {
		n = 1
	}
	ss.entries[key] = slotEntry{capacity: n, fetchedAt: ss.nowFn()}
}

// Close implements SlotSource. closed and the probe WaitGroup are
// manipulated under the same mutex as RecordUse's spawn decision, so
// "spawned after Close returned" cannot happen; Wait drains in-flight
// probes (their HTTP aborts via the cancelled ctx).
func (ss *OpenAICompatSlotSource) Close() error {
	ss.mu.Lock()
	if !ss.closed {
		ss.closed = true
		ss.cancel()
	}
	ss.mu.Unlock()
	ss.wg.Wait()
	return nil
}

// compile-time interface check
var _ SlotSource = (*OpenAICompatSlotSource)(nil)
