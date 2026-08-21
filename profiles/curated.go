// Package profiles is the profile catalog behind the Firn config panel:
// vetted curated configurations embedded at build time plus (via Store) a
// user save/load area with stable IDs. Curated files are credential-free by
// construction — any api_key must be a whole-value ${ENV} reference, pinned
// by test.
package profiles

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

//go:embed curated/*.json
var curatedFS embed.FS

// ID is a stable profile identity: "curated/<slug>" or "user/<slug>".
type ID string

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ParseID validates the namespace and slug shape. Errors carry the offending
// id only (length-capped) — never filesystem paths.
func ParseID(s string) (ID, error) {
	ns, slug, ok := strings.Cut(s, "/")
	if !ok || (ns != "curated" && ns != "user") || !slugRe.MatchString(slug) {
		shown := s
		if len(shown) > 80 {
			shown = shown[:80] + "…"
		}
		return "", fmt.Errorf("profiles: invalid id %q", shown)
	}
	return ID(s), nil
}

// Info describes one profile.
type Info struct {
	ID          ID
	Description string
	Curated     bool
	Revision    string
}

// envRefRe accepts ONLY a whole-value single environment reference —
// curated files may never carry literal or mixed api_key material.
var envRefRe = regexp.MustCompile(`^\$\{[A-Z_][A-Z0-9_]*\}$`)

// envRefCapture finds every ${NAME} reference in raw config bytes.
var envRefCapture = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// curatedDescriptions pins provenance per curated id. Adding a curated file
// requires a row here — the metadata-coverage test fails loudly otherwise.
var curatedDescriptions = map[ID]string{
	"curated/local": "Vetted local llama-swap lineup (committed models.json defaults; restraint evidence GOAT eval 2026-08-17)",
}

// curatedIDs returns every embedded curated profile id, sorted. The embed FS
// is fixed at build time, so a mis-named file is a programmer error: it
// panics, which any test run of this package surfaces loudly.
func curatedIDs() []ID {
	entries, err := curatedFS.ReadDir("curated")
	if err != nil {
		panic("profiles: embedded curated dir unreadable: " + err.Error())
	}
	ids := make([]ID, 0, len(entries))
	for _, e := range entries {
		stem := strings.TrimSuffix(e.Name(), ".json")
		id, perr := ParseID("curated/" + stem)
		if perr != nil {
			panic("profiles: mis-named embedded curated file " + e.Name())
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// curatedBytes returns the embedded raw bytes for a curated id.
func curatedBytes(id ID) ([]byte, error) {
	slug := strings.TrimPrefix(string(id), "curated/")
	return curatedFS.ReadFile("curated/" + slug + ".json")
}

// envRefsIn lists the distinct environment variable names referenced by raw
// config bytes, in first-appearance order.
func envRefsIn(raw []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range envRefCapture.FindAllSubmatch(raw, -1) {
		name := string(m[1])
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// curatedInfo returns the catalog metadata for a curated id. Revision is the
// sha256 of the embedded bytes — identical to the Revision a Document loaded
// from those bytes reports.
func curatedInfo(id ID) (Info, bool) {
	desc, ok := curatedDescriptions[id]
	if !ok {
		return Info{}, false
	}
	raw, err := curatedBytes(id)
	if err != nil {
		return Info{}, false
	}
	sum := sha256.Sum256(raw)
	return Info{ID: id, Description: desc, Curated: true, Revision: hex.EncodeToString(sum[:])}, true
}
