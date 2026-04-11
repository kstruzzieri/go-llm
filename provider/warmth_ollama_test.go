package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// compile-time interface check
var _ WarmthSource = (*OllamaWarmthSource)(nil)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// psHandler returns an http.HandlerFunc that serves a canned /api/ps response.
// The response can be swapped atomically via setModels.
type psHandler struct {
	mu     sync.Mutex
	models []ollamaPsModel
}

func newPsHandler(models []ollamaPsModel) *psHandler {
	return &psHandler{models: models}
}

func (h *psHandler) setModels(models []ollamaPsModel) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.models = models
}

func (h *psHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/ps" {
		http.NotFound(w, r)
		return
	}
	h.mu.Lock()
	models := make([]ollamaPsModel, len(h.models))
	copy(models, h.models)
	h.mu.Unlock()

	resp := ollamaPsResponse{Models: models}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestOllamaWarmthSourcePoll(t *testing.T) {
	expires := time.Now().Add(5 * time.Minute)
	handler := newPsHandler([]ollamaPsModel{
		{
			Name:      "qwen3:8b",
			Size:      5_000_000_000,
			SizeVRAM:  4_500_000_000,
			ExpiresAt: expires,
		},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ws := NewOllamaWarmthSource(srv.URL, "ollama",
		WithPollInterval(50*time.Millisecond),
	)
	defer ws.Close()

	// Wait for at least one poll cycle to complete.
	time.Sleep(100 * time.Millisecond)

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	// IsWarm should be true.
	if !ws.IsWarm(key) {
		t.Fatal("expected qwen3:8b to be warm after poll")
	}

	// WarmthState should have VRAM and Loaded.
	info := ws.WarmthState(key)
	if info == nil {
		t.Fatal("WarmthState should not be nil for polled model")
	}
	if !info.Loaded {
		t.Error("Loaded should be true")
	}
	expectedVRAM := float64(4_500_000_000) / (1024 * 1024 * 1024)
	if info.VRAM < expectedVRAM-0.01 || info.VRAM > expectedVRAM+0.01 {
		t.Errorf("VRAM = %v, want ~%v", info.VRAM, expectedVRAM)
	}
	if info.Since.IsZero() {
		t.Error("Since should not be zero")
	}

	// Snapshot should contain exactly 1 model.
	snap := ws.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot should have 1 model, got %d", len(snap))
	}
	if snap[0].Key != key {
		t.Errorf("Snapshot[0].Key = %v, want %v", snap[0].Key, key)
	}
}

func TestOllamaWarmthSourceCold(t *testing.T) {
	// Empty /api/ps response — no models loaded.
	handler := newPsHandler(nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ws := NewOllamaWarmthSource(srv.URL, "ollama",
		WithPollInterval(50*time.Millisecond),
	)
	defer ws.Close()

	// Wait for initial poll.
	time.Sleep(100 * time.Millisecond)

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	if ws.IsWarm(key) {
		t.Error("model should not be warm with empty /api/ps response")
	}

	if ws.WarmthState(key) != nil {
		t.Error("WarmthState should return nil for unknown model")
	}

	snap := ws.Snapshot()
	if len(snap) != 0 {
		t.Errorf("Snapshot should be empty, got %d models", len(snap))
	}
}

func TestOllamaWarmthSourceRecordUse(t *testing.T) {
	// Use a very long poll interval so no polling happens during the test.
	handler := newPsHandler(nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ws := NewOllamaWarmthSource(srv.URL, "ollama",
		WithPollInterval(1*time.Hour),
	)
	defer ws.Close()

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	// Initially cold (poll returned empty, and even if initial poll ran, it was empty).
	// Wait a moment for initial poll to complete.
	time.Sleep(50 * time.Millisecond)

	if ws.IsWarm(key) {
		t.Error("model should be cold initially")
	}

	// RecordUse should create a warm entry.
	ws.RecordUse(key)

	if !ws.IsWarm(key) {
		t.Fatal("model should be warm after RecordUse")
	}

	info := ws.WarmthState(key)
	if info == nil {
		t.Fatal("WarmthState should not be nil after RecordUse")
	}
	if !info.Loaded {
		t.Error("Loaded should be true after RecordUse")
	}
	if info.VRAM != 0 {
		t.Errorf("VRAM = %v, want 0 for RecordUse-created entry", info.VRAM)
	}

	// RecordUse for a different provider should be a no-op.
	otherKey := ModelKey{Provider: "openai", Model: "gpt-4o"}
	ws.RecordUse(otherKey)
	if ws.IsWarm(otherKey) {
		t.Error("RecordUse should ignore models from different providers")
	}

	// RecordUse on an existing model should extend expiry.
	before := ws.WarmthState(key)
	time.Sleep(2 * time.Millisecond)
	ws.RecordUse(key)
	after := ws.WarmthState(key)

	if !after.ExpiresAt.After(before.ExpiresAt) {
		t.Error("RecordUse should extend ExpiresAt for existing model")
	}
}

func TestOllamaWarmthSourceClose(t *testing.T) {
	handler := newPsHandler(nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ws := NewOllamaWarmthSource(srv.URL, "ollama",
		WithPollInterval(1*time.Hour),
	)

	// First close should succeed.
	if err := ws.Close(); err != nil {
		t.Errorf("first Close() = %v, want nil", err)
	}

	// Second close should also succeed (idempotent).
	if err := ws.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil", err)
	}
}

func TestOllamaWarmthSourceProviderFilter(t *testing.T) {
	expires := time.Now().Add(5 * time.Minute)
	handler := newPsHandler([]ollamaPsModel{
		{
			Name:      "qwen3:8b",
			Size:      5_000_000_000,
			SizeVRAM:  4_500_000_000,
			ExpiresAt: expires,
		},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ws := NewOllamaWarmthSource(srv.URL, "ollama",
		WithPollInterval(50*time.Millisecond),
	)
	defer ws.Close()

	// Wait for poll.
	time.Sleep(100 * time.Millisecond)

	// Same model name but different provider should be filtered out.
	wrongProvider := ModelKey{Provider: "openai", Model: "qwen3:8b"}
	if ws.IsWarm(wrongProvider) {
		t.Error("IsWarm should return false for wrong provider")
	}
	if ws.WarmthState(wrongProvider) != nil {
		t.Error("WarmthState should return nil for wrong provider")
	}
}

func TestOllamaWarmthSourceModelUnloaded(t *testing.T) {
	expires := time.Now().Add(5 * time.Minute)
	handler := newPsHandler([]ollamaPsModel{
		{
			Name:      "qwen3:8b",
			Size:      5_000_000_000,
			SizeVRAM:  4_500_000_000,
			ExpiresAt: expires,
		},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ws := NewOllamaWarmthSource(srv.URL, "ollama",
		WithPollInterval(50*time.Millisecond),
	)
	defer ws.Close()

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

	// Wait for initial poll to see the model.
	time.Sleep(100 * time.Millisecond)
	if !ws.IsWarm(key) {
		t.Fatal("model should be warm after first poll")
	}

	// Remove the model from /api/ps.
	handler.setModels(nil)

	// Wait for another poll cycle to pick up the change.
	time.Sleep(100 * time.Millisecond)

	if ws.IsWarm(key) {
		t.Error("model should be cold after being removed from /api/ps")
	}

	info := ws.WarmthState(key)
	if info == nil {
		t.Fatal("WarmthState should still return info for previously loaded model")
	}
	if info.Loaded {
		t.Error("Loaded should be false after model is unloaded")
	}
}

func TestOllamaWarmthSourceHTTPError(t *testing.T) {
	// Server that always returns 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ws := NewOllamaWarmthSource(srv.URL, "ollama",
		WithPollInterval(50*time.Millisecond),
	)
	defer ws.Close()

	// Wait for poll attempt — should not crash.
	time.Sleep(100 * time.Millisecond)

	key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	if ws.IsWarm(key) {
		t.Error("model should not be warm when server returns errors")
	}
}

func TestOllamaWarmthSourceInvalidJSON(t *testing.T) {
	// Server that returns invalid JSON.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{invalid json`))
	}))
	defer srv.Close()

	ws := NewOllamaWarmthSource(srv.URL, "ollama",
		WithPollInterval(50*time.Millisecond),
	)
	defer ws.Close()

	// Wait for poll attempt — should not crash.
	time.Sleep(100 * time.Millisecond)

	snap := ws.Snapshot()
	if len(snap) != 0 {
		t.Errorf("Snapshot should be empty after invalid JSON, got %d", len(snap))
	}
}

func TestOllamaWarmthSourceOptions(t *testing.T) {
	t.Run("WithPollInterval ignores zero", func(t *testing.T) {
		handler := newPsHandler(nil)
		srv := httptest.NewServer(handler)
		defer srv.Close()

		ws := NewOllamaWarmthSource(srv.URL, "ollama",
			WithPollInterval(0),
		)
		defer ws.Close()

		if ws.pollInterval != defaultPollInterval {
			t.Errorf("pollInterval = %v, want %v", ws.pollInterval, defaultPollInterval)
		}
	})

	t.Run("WithPollInterval ignores negative", func(t *testing.T) {
		handler := newPsHandler(nil)
		srv := httptest.NewServer(handler)
		defer srv.Close()

		ws := NewOllamaWarmthSource(srv.URL, "ollama",
			WithPollInterval(-1*time.Second),
		)
		defer ws.Close()

		if ws.pollInterval != defaultPollInterval {
			t.Errorf("pollInterval = %v, want %v", ws.pollInterval, defaultPollInterval)
		}
	})

	t.Run("WithKeepAlive ignores zero", func(t *testing.T) {
		handler := newPsHandler(nil)
		srv := httptest.NewServer(handler)
		defer srv.Close()

		ws := NewOllamaWarmthSource(srv.URL, "ollama",
			WithKeepAlive(0),
		)
		defer ws.Close()

		if ws.keepAlive != defaultKeepAlive {
			t.Errorf("keepAlive = %v, want %v", ws.keepAlive, defaultKeepAlive)
		}
	})

	t.Run("WithKeepAlive accepts positive", func(t *testing.T) {
		handler := newPsHandler(nil)
		srv := httptest.NewServer(handler)
		defer srv.Close()

		ws := NewOllamaWarmthSource(srv.URL, "ollama",
			WithKeepAlive(10*time.Minute),
		)
		defer ws.Close()

		if ws.keepAlive != 10*time.Minute {
			t.Errorf("keepAlive = %v, want %v", ws.keepAlive, 10*time.Minute)
		}
	})
}

func TestOllamaWarmthSourceString(t *testing.T) {
	handler := newPsHandler(nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ws := NewOllamaWarmthSource(srv.URL, "ollama",
		WithPollInterval(1*time.Hour),
	)
	defer ws.Close()

	s := ws.String()
	if s == "" {
		t.Error("String() should not return empty string")
	}
}
