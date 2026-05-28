package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		// Trace fields are all JSON-marshalable today. A future field
		// breaking that invariant must be caught: the manifest hash is
		// the provenance anchor for benchmark artifacts, so silently
		// returning a sentinel would collapse all corrupt traces to one
		// shared hash. Panic loudly.
		panic(fmt.Sprintf("canonicalTraceHash: json.Marshal failed for trace %q: %v", t.ID, err))
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		// json.Marshal output is always valid JSON, so json.Compact
		// cannot fail; panic if encoding/json ever changes.
		panic(fmt.Sprintf("canonicalTraceHash: json.Compact failed for trace %q: %v", t.ID, err))
	}
	sum := sha256.Sum256(compact.Bytes())
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
