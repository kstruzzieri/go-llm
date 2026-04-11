package provider

import (
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// mockWarmthSource — reusable mock for Tasks 6, 8, 9, and 11
// ---------------------------------------------------------------------------

// mockWarmthSource is a thread-safe in-memory WarmthSource implementation used
// for testing throughout the provider package. It tracks warm/cold state per
// ModelKey and provides test helper methods for setup.
type mockWarmthSource struct {
	mu     sync.RWMutex
	models map[ModelKey]*WarmthInfo
}

func newMockWarmthSource() *mockWarmthSource {
	return &mockWarmthSource{
		models: make(map[ModelKey]*WarmthInfo),
	}
}

// SetWarm marks a model as loaded with the given VRAM consumption.
// ExpiresAt is set to 5 minutes from now (Ollama default keep_alive).
func (m *mockWarmthSource) SetWarm(key ModelKey, vram float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	m.models[key] = &WarmthInfo{
		Loaded:    true,
		Since:     now,
		ExpiresAt: now.Add(5 * time.Minute),
		VRAM:      vram,
	}
}

// SetCold marks a model as unloaded (cold).
func (m *mockWarmthSource) SetCold(key ModelKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.models[key]
	if ok {
		info.Loaded = false
	}
}

// IsWarm reports whether the model is loaded and its expiry has not passed.
func (m *mockWarmthSource) IsWarm(key ModelKey) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	info, ok := m.models[key]
	if !ok {
		return false
	}
	return info.Loaded && time.Now().Before(info.ExpiresAt)
}

// WarmthState returns a copy of the WarmthInfo for key, or nil if unknown.
func (m *mockWarmthSource) WarmthState(key ModelKey) *WarmthInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	info, ok := m.models[key]
	if !ok {
		return nil
	}
	// Return a copy so callers cannot mutate internal state.
	cp := *info
	return &cp
}

// RecordUse extends ExpiresAt for a known model or creates a new warm entry.
func (m *mockWarmthSource) RecordUse(key ModelKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.models[key]
	if ok {
		info.ExpiresAt = time.Now().Add(5 * time.Minute)
	} else {
		now := time.Now()
		m.models[key] = &WarmthInfo{
			Loaded:    true,
			Since:     now,
			ExpiresAt: now.Add(5 * time.Minute),
			VRAM:      0,
		}
	}
}

// Snapshot returns all currently loaded models.
func (m *mockWarmthSource) Snapshot() []WarmModel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []WarmModel
	for k, info := range m.models {
		if info.Loaded {
			out = append(out, WarmModel{Key: k, Info: *info})
		}
	}
	return out
}

// Close is a no-op for the mock.
func (m *mockWarmthSource) Close() error {
	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestMockWarmthSourceInterface(t *testing.T) {
	// Compile-time interface satisfaction check.
	var _ WarmthSource = (*mockWarmthSource)(nil)

	t.Run("initially cold", func(t *testing.T) {
		ws := newMockWarmthSource()
		key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

		if ws.IsWarm(key) {
			t.Error("unknown model should not be warm")
		}
		if ws.WarmthState(key) != nil {
			t.Error("unknown model should return nil WarmthState")
		}
	})

	t.Run("set warm", func(t *testing.T) {
		ws := newMockWarmthSource()
		key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

		ws.SetWarm(key, 4.5)

		if !ws.IsWarm(key) {
			t.Fatal("model should be warm after SetWarm")
		}

		info := ws.WarmthState(key)
		if info == nil {
			t.Fatal("WarmthState should not be nil after SetWarm")
		}
		if !info.Loaded {
			t.Error("Loaded should be true")
		}
		if info.VRAM != 4.5 {
			t.Errorf("VRAM = %v, want 4.5", info.VRAM)
		}
		if info.Since.IsZero() {
			t.Error("Since should not be zero")
		}
		if info.ExpiresAt.IsZero() {
			t.Error("ExpiresAt should not be zero")
		}
		if !info.ExpiresAt.After(info.Since) {
			t.Error("ExpiresAt should be after Since")
		}
	})

	t.Run("warmth state returns copy", func(t *testing.T) {
		ws := newMockWarmthSource()
		key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

		ws.SetWarm(key, 4.5)
		info1 := ws.WarmthState(key)
		info2 := ws.WarmthState(key)

		// Mutating the returned copy should not affect subsequent calls.
		info1.VRAM = 999.0
		if info2.VRAM != 4.5 {
			t.Error("WarmthState should return independent copies")
		}
	})

	t.Run("set cold", func(t *testing.T) {
		ws := newMockWarmthSource()
		key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

		ws.SetWarm(key, 4.5)
		ws.SetCold(key)

		if ws.IsWarm(key) {
			t.Error("model should not be warm after SetCold")
		}

		info := ws.WarmthState(key)
		if info == nil {
			t.Fatal("WarmthState should still return info after SetCold")
		}
		if info.Loaded {
			t.Error("Loaded should be false after SetCold")
		}
	})

	t.Run("snapshot returns only loaded models", func(t *testing.T) {
		ws := newMockWarmthSource()
		warm := ModelKey{Provider: "ollama", Model: "qwen3:8b"}
		cold := ModelKey{Provider: "ollama", Model: "qwen3:32b"}

		ws.SetWarm(warm, 4.5)
		ws.SetWarm(cold, 16.0)
		ws.SetCold(cold)

		snap := ws.Snapshot()
		if len(snap) != 1 {
			t.Fatalf("Snapshot should have 1 model, got %d", len(snap))
		}
		if snap[0].Key != warm {
			t.Errorf("Snapshot[0].Key = %v, want %v", snap[0].Key, warm)
		}
		if snap[0].Info.VRAM != 4.5 {
			t.Errorf("Snapshot[0].Info.VRAM = %v, want 4.5", snap[0].Info.VRAM)
		}
	})

	t.Run("record use on unknown model creates warm entry", func(t *testing.T) {
		ws := newMockWarmthSource()
		key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

		ws.RecordUse(key)

		if !ws.IsWarm(key) {
			t.Error("RecordUse on unknown model should create warm entry")
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
	})

	t.Run("record use extends expiry for known model", func(t *testing.T) {
		ws := newMockWarmthSource()
		key := ModelKey{Provider: "ollama", Model: "qwen3:8b"}

		ws.SetWarm(key, 4.5)
		original := ws.WarmthState(key)

		// Small sleep to ensure time advances.
		time.Sleep(2 * time.Millisecond)

		ws.RecordUse(key)
		updated := ws.WarmthState(key)

		if !updated.ExpiresAt.After(original.ExpiresAt) {
			t.Error("RecordUse should extend ExpiresAt")
		}
		// VRAM should be preserved.
		if updated.VRAM != 4.5 {
			t.Errorf("VRAM = %v, want 4.5 (should be preserved by RecordUse)", updated.VRAM)
		}
	})

	t.Run("close returns nil", func(t *testing.T) {
		ws := newMockWarmthSource()
		if err := ws.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	})

	t.Run("empty snapshot", func(t *testing.T) {
		ws := newMockWarmthSource()
		snap := ws.Snapshot()
		if len(snap) != 0 {
			t.Errorf("empty source should return 0 models, got %d", len(snap))
		}
	})

	t.Run("set cold on unknown is no-op", func(t *testing.T) {
		ws := newMockWarmthSource()
		key := ModelKey{Provider: "ollama", Model: "unknown"}

		// Should not panic or create entry.
		ws.SetCold(key)

		if ws.WarmthState(key) != nil {
			t.Error("SetCold on unknown model should not create an entry")
		}
	})
}
