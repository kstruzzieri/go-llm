package provider

import "time"

// WarmthSource abstracts warm-model detection across providers. A model is
// "warm" when it is loaded in the provider's memory and can serve requests
// without a cold-start delay.
//
// Implementations must be safe for concurrent use.
type WarmthSource interface {
	// IsWarm reports whether the model identified by key is currently loaded
	// and has not expired. Returns false for unknown models.
	IsWarm(key ModelKey) bool

	// WarmthState returns a copy of the warmth information for key, or nil
	// if the model is unknown. The returned pointer is safe to mutate without
	// affecting the source's internal state.
	WarmthState(key ModelKey) *WarmthInfo

	// RecordUse notes that a request was sent to the model, extending its
	// expiry window. If the model is unknown, a new warm entry is created.
	RecordUse(key ModelKey)

	// Snapshot returns all currently loaded (warm) models. Cold or expired
	// models are excluded.
	Snapshot() []WarmModel

	// Close releases any resources held by the source (background pollers,
	// connections, etc.). After Close returns, other methods must not be called.
	Close() error
}

// WarmModel pairs a model key with its current warmth information.
type WarmModel struct {
	Key  ModelKey
	Info WarmthInfo
}

// WarmthInfo describes the current warmth state of a model.
type WarmthInfo struct {
	// Loaded is true when the model is resident in the provider's memory.
	Loaded bool
	// Since is the time the model was loaded (or last reloaded).
	Since time.Time
	// ExpiresAt is the time after which the provider will unload the model
	// if no further requests arrive. For Ollama the default keep_alive is
	// 5 minutes.
	ExpiresAt time.Time
	// VRAM is the approximate GPU memory consumed by the model, in GB.
	VRAM float64
}
