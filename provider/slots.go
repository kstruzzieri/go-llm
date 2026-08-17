package provider

// SlotSource reports backend parallel slot capacity for governed model keys.
// Implementations must be safe for concurrent use.
//
// The two-value Capacity is the seam #400 (slot-aware admission) reads.
// ok=false means the key's provider is NOT governed by this source
// (unmanaged or remote): callers MUST preserve existing behavior and never
// gate or serialize such keys. ok=true implies n >= 1; a governed key whose
// capacity is unknown, unprobed, expired, or whose last probe failed
// reports (1, true) — fail-safe serial.
//
// SlotSource emits no change events: capacity raises take effect when the
// consumer next reads Capacity (for #400, on the next admission decision —
// ramp-up delay is bounded by one in-flight call). Consumers re-read per
// decision; they do not cache.
type SlotSource interface {
	// Capacity returns the parallel slot capacity for key. It never
	// performs I/O; it is a pure cache read.
	Capacity(key ModelKey) (n int, ok bool)
	// RecordUse signals that key just successfully served a request. It
	// may trigger an asynchronous cache refresh; it never blocks on I/O.
	RecordUse(key ModelKey)
	// Close stops background work and waits for in-flight probes to exit.
	// After Close returns, RecordUse is a no-op and no probe goroutines
	// remain. Safe to call multiple times.
	Close() error
}
