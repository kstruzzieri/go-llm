package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	defaultPollInterval = 30 * time.Second
	defaultKeepAlive    = 5 * time.Minute
)

// OllamaWarmthSource polls Ollama's /api/ps endpoint to track which models
// are loaded in memory. It implements the WarmthSource interface and is safe
// for concurrent use.
//
// A background goroutine periodically queries the Ollama server to discover
// loaded models, their VRAM consumption, and estimated expiry times. Between
// polls, callers can reactively extend expiry via RecordUse.
type OllamaWarmthSource struct {
	mu           sync.RWMutex
	baseURL      string
	providerName string
	models       map[string]*WarmthInfo // model name -> warmth info
	pollInterval time.Duration
	keepAlive    time.Duration
	client       *http.Client
	cancel       context.CancelFunc
	closeOnce    sync.Once
	// bind attaches the warmth-poll destination capability per poll (#477).
	// nil disables binding (ungated). Read-only after construction.
	bind func(ctx context.Context, providerName string) (context.Context, error)
}

// OllamaWarmthOption configures an OllamaWarmthSource.
type OllamaWarmthOption func(*OllamaWarmthSource)

// WithPollInterval sets the interval between /api/ps polls. Values <= 0 are
// ignored and the default (30s) is used.
func WithPollInterval(d time.Duration) OllamaWarmthOption {
	return func(ws *OllamaWarmthSource) {
		if d > 0 {
			ws.pollInterval = d
		}
	}
}

// WithWarmthHTTPClient replaces the poll HTTP client (#477: the destination
// guard binds a transport per destination, so a gated deployment supplies a
// guarded client for the polled backend). nil is ignored.
func WithWarmthHTTPClient(hc *http.Client) OllamaWarmthOption {
	return func(ws *OllamaWarmthSource) {
		if hc != nil {
			ws.client = hc
		}
	}
}

// WithWarmthPollBinder installs a per-poll context binder (#477): before each
// /api/ps request, bind is called and its returned context — carrying the
// warmth-poll destination capability — is what the request runs under. A bind
// error skips the poll with ZERO requests; warmth simply stays stale, the
// existing degraded behavior for an unreachable backend.
//
// The binder runs on every poll, never cached: a capability is bound to the
// gate generation that issued it, so caching one would leave polling
// permanently dead after a revoke-and-re-admit cycle.
func WithWarmthPollBinder(bind func(ctx context.Context, providerName string) (context.Context, error)) OllamaWarmthOption {
	return func(ws *OllamaWarmthSource) {
		ws.bind = bind
	}
}

// WithKeepAlive sets the assumed keep_alive duration used when creating new
// entries via RecordUse. Values <= 0 are ignored and the default (5m) is used.
func WithKeepAlive(d time.Duration) OllamaWarmthOption {
	return func(ws *OllamaWarmthSource) {
		if d > 0 {
			ws.keepAlive = d
		}
	}
}

// NewOllamaWarmthSource creates a WarmthSource that polls the Ollama server
// at baseURL for loaded models. The providerName is used to filter ModelKey
// values — only keys whose Provider matches are considered.
//
// A background goroutine is started immediately and runs until Close is called.
func NewOllamaWarmthSource(baseURL, providerName string, opts ...OllamaWarmthOption) *OllamaWarmthSource {
	ws := &OllamaWarmthSource{
		baseURL:      baseURL,
		providerName: providerName,
		models:       make(map[string]*WarmthInfo),
		pollInterval: defaultPollInterval,
		keepAlive:    defaultKeepAlive,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(ws)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ws.cancel = cancel
	go ws.pollLoop(ctx)

	return ws
}

// ---------------------------------------------------------------------------
// WarmthSource interface
// ---------------------------------------------------------------------------

// IsWarm reports whether the model identified by key is currently loaded and
// has not expired. Returns false if key.Provider does not match or if the
// model is unknown.
func (ws *OllamaWarmthSource) IsWarm(key ModelKey) bool {
	if key.Provider != ws.providerName {
		return false
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	info, ok := ws.models[key.Model]
	if !ok {
		return false
	}
	return info.Loaded && time.Now().Before(info.ExpiresAt)
}

// WarmthState returns a copy of the WarmthInfo for key, or nil if the model
// is unknown or belongs to a different provider.
func (ws *OllamaWarmthSource) WarmthState(key ModelKey) *WarmthInfo {
	if key.Provider != ws.providerName {
		return nil
	}
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	info, ok := ws.models[key.Model]
	if !ok {
		return nil
	}
	cp := *info
	return &cp
}

// RecordUse notes that a request was sent to the model, extending its expiry
// window by the configured keep_alive duration. If the model is unknown, a
// new warm entry is created. Models belonging to a different provider are
// silently ignored.
func (ws *OllamaWarmthSource) RecordUse(key ModelKey) {
	if key.Provider != ws.providerName {
		return
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	info, ok := ws.models[key.Model]
	if ok {
		info.ExpiresAt = time.Now().Add(ws.keepAlive)
	} else {
		now := time.Now()
		ws.models[key.Model] = &WarmthInfo{
			Loaded:    true,
			Since:     now,
			ExpiresAt: now.Add(ws.keepAlive),
			VRAM:      0,
		}
	}
}

// Snapshot returns all currently loaded and non-expired models. Cold or
// expired models are excluded.
func (ws *OllamaWarmthSource) Snapshot() []WarmModel {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	now := time.Now()
	var out []WarmModel
	for name, info := range ws.models {
		if info.Loaded && now.Before(info.ExpiresAt) {
			out = append(out, WarmModel{
				Key:  ModelKey{Provider: ws.providerName, Model: name},
				Info: *info,
			})
		}
	}
	return out
}

// Close stops the background poller and releases resources. It is safe to
// call Close multiple times; subsequent calls are no-ops.
func (ws *OllamaWarmthSource) Close() error {
	ws.closeOnce.Do(func() {
		ws.cancel()
	})
	return nil
}

// ---------------------------------------------------------------------------
// Polling
// ---------------------------------------------------------------------------

// pollLoop runs the initial poll and then ticks at the configured interval
// until ctx is cancelled.
func (ws *OllamaWarmthSource) pollLoop(ctx context.Context) {
	ws.poll(ctx)

	ticker := time.NewTicker(ws.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ws.poll(ctx)
		}
	}
}

// ollamaPsResponse is the JSON shape returned by Ollama's /api/ps endpoint.
type ollamaPsResponse struct {
	Models []ollamaPsModel `json:"models"`
}

// ollamaPsModel represents a single model entry in the /api/ps response.
type ollamaPsModel struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	SizeVRAM  int64     `json:"size_vram"`
	ExpiresAt time.Time `json:"expires_at"`
}

// poll queries the Ollama /api/ps endpoint and updates internal state. HTTP
// errors and JSON parse errors are silently ignored to avoid crashing the
// background poller.
func (ws *OllamaWarmthSource) poll(ctx context.Context) {
	if ws.bind != nil {
		bctx, err := ws.bind(ctx, ws.providerName)
		if err != nil {
			// Denied: zero requests this poll; warmth stays stale exactly
			// as it would for an unreachable backend.
			return
		}
		ctx = bctx
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ws.baseURL+"/api/ps", nil)
	if err != nil {
		return
	}

	resp, err := ws.client.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var psResp ollamaPsResponse
	if err := json.NewDecoder(resp.Body).Decode(&psResp); err != nil {
		return
	}

	// Build a set of model names currently loaded.
	loaded := make(map[string]struct{}, len(psResp.Models))
	for _, m := range psResp.Models {
		loaded[m.Name] = struct{}{}
	}

	ws.mu.Lock()
	defer ws.mu.Unlock()

	now := time.Now()

	// Update or create entries for loaded models.
	for _, m := range psResp.Models {
		vram := float64(m.SizeVRAM) / (1024 * 1024 * 1024) // bytes to GB
		info, ok := ws.models[m.Name]
		if ok {
			info.Loaded = true
			info.ExpiresAt = m.ExpiresAt
			info.VRAM = vram
		} else {
			ws.models[m.Name] = &WarmthInfo{
				Loaded:    true,
				Since:     now,
				ExpiresAt: m.ExpiresAt,
				VRAM:      vram,
			}
		}
	}

	// Mark models not in the response as cold.
	for name, info := range ws.models {
		if _, ok := loaded[name]; !ok {
			info.Loaded = false
		}
	}
}

// compile-time interface check
var _ WarmthSource = (*OllamaWarmthSource)(nil)

// ---------------------------------------------------------------------------
// Debug / diagnostic helpers
// ---------------------------------------------------------------------------

// String returns a human-readable summary of the warmth source state, useful
// for debugging and logging.
func (ws *OllamaWarmthSource) String() string {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	warm := 0
	now := time.Now()
	for _, info := range ws.models {
		if info.Loaded && now.Before(info.ExpiresAt) {
			warm++
		}
	}
	return fmt.Sprintf("OllamaWarmthSource{url=%s, provider=%s, warm=%d, total=%d}",
		ws.baseURL, ws.providerName, warm, len(ws.models))
}
