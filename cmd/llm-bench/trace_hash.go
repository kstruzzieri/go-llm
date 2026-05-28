package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// canonicalTraceHash returns a stable sha256 fingerprint of a trace,
// excluding its ID. Two traces with identical content but different IDs
// hash to the same value; two traces with the same ID but different
// content hash to different values. The hash format is the raw hex
// digest (no "sha256:" prefix) so callers can re-prefix once when
// composing manifest entries.
func canonicalTraceHash(t Trace) string {
	// Marshal with a copy that zeroes ID so only content participates.
	clone := t
	clone.ID = ""
	data, err := json.Marshal(clone)
	if err != nil {
		// Trace fields are all JSON-marshalable today; a non-nil error
		// here means a future field introduced an un-marshalable type.
		// Surface as an obviously-wrong sentinel rather than crashing
		// the benchmark; report-time tests will catch it.
		return "unhashable"
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// traceSetManifestHash returns a stable sha256 fingerprint of an entire
// trace set: sorted pairs of (trace_id, canonical_trace_hash) are
// concatenated and hashed once. Per spec §5.2 this lets a benchmark
// artifact pin "the set of traces I ran on" without committing the
// trace contents themselves.
func traceSetManifestHash(traces []Trace) string {
	entries := make([]string, 0, len(traces))
	for _, tr := range traces {
		entries = append(entries, tr.ID+"|"+canonicalTraceHash(tr))
	}
	sort.Strings(entries)
	h := sha256.New()
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{0}) // delimiter; prevents id||hash collisions
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
