package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrDurabilityUncertain marks a save whose bytes ARE published (rename or
// link succeeded) but whose directory-entry durability could not be confirmed
// (parent fsync failed). Callers must treat the on-disk state as current.
var ErrDurabilityUncertain = errors.New("config: published but durability uncertain")

// ErrRevisionConflict marks a save refused because the target's current
// digest differs from the expected revision (external edit). The on-disk
// file is untouched. Consumers that must not expose paths (the profile
// store) match this sentinel rather than parsing the path-bearing message.
var ErrRevisionConflict = errors.New("config: revision conflict")

// syncDir fsyncs a directory so a just-published rename/link is durable.
// Injectable for durability-failure tests.
var syncDir = syncDirectory

// writeSiblingTemp writes data to a unique 0600 sibling temp file, fsyncs and
// closes it. Pre-publication phase: any error leaves the target untouched and
// the temp removed.
func writeSiblingTemp(path string, data []byte) (tmp string, err error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return "", diagWrap(CodeIO, SubjectNone, "", fmt.Errorf("config: create temp for %q: %w", path, err))
	}
	tmp = f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return "", diagWrap(CodeIO, SubjectNone, "", fmt.Errorf("config: chmod %q: %w", tmp, err))
	}
	if _, err = f.Write(data); err != nil {
		return "", diagWrap(CodeIO, SubjectNone, "", fmt.Errorf("config: write %q: %w", tmp, err))
	}
	if err = f.Sync(); err != nil {
		return "", diagWrap(CodeIO, SubjectNone, "", fmt.Errorf("config: sync %q: %w", tmp, err))
	}
	if err = f.Close(); err != nil {
		return "", diagWrap(CodeIO, SubjectNone, "", fmt.Errorf("config: close %q: %w", tmp, err))
	}
	return tmp, nil
}

// publishReplace atomically replaces path with data (temp + rename + parent
// sync). When expectedRevision is non-empty, the target is checked after the
// synced temp is ready. Pre-rename failure → target untouched. Post-rename
// sync failure → bytes are live; returns ErrDurabilityUncertain (wrapped).
func publishReplace(path string, data []byte, expectedRevision string) error {
	tmp, err := writeSiblingTemp(path, data)
	if err != nil {
		return err
	}
	if expectedRevision != "" {
		// ponytail: this closes the temp-preparation window, not the final
		// check-to-rename window; true arbitrary-writer CAS requires every
		// writer to use a shared lock protocol or a versioned store.
		if _, err := readExpectedRevision(path, expectedRevision); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return diagWrap(CodeIO, SubjectNone, "", fmt.Errorf("config: rename %q: %w", path, err))
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return diagWrap(CodeDurabilityUncertain, SubjectNone, "",
			fmt.Errorf("%w: %s: dir sync: %v", ErrDurabilityUncertain, path, err))
	}
	return nil
}

// publishReplaceFn is the injectable publication seam (byte-stability test).
var publishReplaceFn = publishReplace

// saveLocks serializes save windows per cleaned absolute target path. This is
// a COOPERATIVE in-process lock: it makes this package's own read-compare-
// publish windows atomic with respect to each other. Arbitrary external
// writers cannot be excluded by portable pathname APIs; the final revision
// check catches only changes that happen before it.
var saveLocks sync.Map // string → *sync.Mutex

func lockForPath(path string) *sync.Mutex {
	key := documentPathIdentity(path)
	mu, _ := saveLocks.LoadOrStore(key, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// SaveNew writes the document to a path that must not exist (create-only,
// race-free via link-based publication). Equivalent to SaveNewAs with an
// empty origin source (same-path Source preservation, else explicit-path).
func (d *Document) SaveNew(path string) error {
	return d.SaveNewAs(path, "")
}

// SaveNewAs is SaveNew with an explicit origin source recorded on success —
// profile stores pass OriginProfile so provenance survives the save.
//
// LINEARIZATION (spec amendment 6): d.mu is held from canonical snapshot
// through publication AND state commit, so no draft mutation can interleave
// between what was published and what is recorded. Lock ordering invariant:
// saveLock (per target path) is always taken BEFORE d.mu, never nested the
// other way. Holding d.mu across file I/O blocks reads during a save —
// saves are rare and small; correctness wins by design.
func (d *Document) SaveNewAs(path string, src OriginSource) error {
	mu := lockForPath(path)
	mu.Lock()
	defer mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.readOnlyErrLocked(); err != nil {
		return err
	}
	out, err := d.canonicalBytes()
	if err != nil {
		return err
	}
	if err := publishNew(path, out); err != nil {
		if errors.Is(err, ErrDurabilityUncertain) {
			d.commitSavedLocked(out, path, src)
		}
		return err
	}
	d.commitSavedLocked(out, path, src)
	return nil
}

// SaveReplace overwrites path after its digest matches expectedRevision at the
// start of the save and again after the replacement temp is synced. Equivalent
// to SaveReplaceAs with an empty origin source.
func (d *Document) SaveReplace(path, expectedRevision string) error {
	return d.SaveReplaceAs(path, expectedRevision, "")
}

// SaveReplaceAs is SaveReplace with an explicit origin source recorded on
// success. The per-target lock prevents cooperating in-process savers from
// interleaving, and d.mu is held from snapshot through publication and commit
// (spec amendment 6) so saves and draft mutations are linearized. An
// arbitrary EXTERNAL writer can still modify path between the final digest
// check and rename because portable pathname APIs do not provide
// compare-and-replace. Unchanged canonical content publishes nothing
// (byte-stability) but still converges document state after a final digest
// check.
func (d *Document) SaveReplaceAs(path, expectedRevision string, src OriginSource) error {
	mu := lockForPath(path)
	mu.Lock()
	defer mu.Unlock()
	d.mu.Lock()
	defer d.mu.Unlock()
	// Read-only gate BEFORE any target I/O: duplicate_keys beats missing-
	// target io and revision conflicts (save precedence, pinned by test).
	if err := d.readOnlyErrLocked(); err != nil {
		return err
	}
	cur, err := readExpectedRevision(path, expectedRevision)
	if err != nil {
		return err
	}
	out, err := d.canonicalBytes()
	if err != nil {
		return err
	}
	if bytes.Equal(out, cur) {
		if _, err := readExpectedRevision(path, expectedRevision); err != nil {
			return err
		}
		d.commitSavedLocked(out, path, src) // state converges; no publication
		return nil
	}
	if err := publishReplaceFn(path, out, expectedRevision); err != nil {
		if errors.Is(err, ErrDurabilityUncertain) {
			// Bytes ARE live: document state must reflect the published truth.
			d.commitSavedLocked(out, path, src)
		}
		return err
	}
	d.commitSavedLocked(out, path, src)
	return nil
}

func readExpectedRevision(path, expectedRevision string) ([]byte, error) {
	cur, err := os.ReadFile(path)
	if err != nil {
		return nil, diagWrap(CodeIO, SubjectNone, "", fmt.Errorf("config: save replace %q: %w", path, err))
	}
	sum := sha256.Sum256(cur)
	if hex.EncodeToString(sum[:]) != expectedRevision {
		return nil, diagWrap(CodeRevisionConflict, SubjectNone, "",
			fmt.Errorf("config: save replace %q: %w", path, ErrRevisionConflict))
	}
	return cur, nil
}

// commitSavedLocked updates rawBytes/revision/origin to the published bytes.
// Callers hold d.mu. With an empty src, saving back to the document's own
// path preserves its discovery Source (an env-override config saved in place
// is still the env-override config) and a NEW path is an explicit-path act;
// a non-empty src always wins (profile provenance). Path identity is lexical
// after absolute cleaning; symlink aliases intentionally remain distinct
// origins. Baseline profile ID and Dirty are slice-3 caller-owned state —
// deliberately absent here.
func (d *Document) commitSavedLocked(out []byte, path string, src OriginSource) {
	sum := sha256.Sum256(out)
	d.rawBytes = out
	d.providerDrops = nil
	d.revision = hex.EncodeToString(sum[:])
	if src != "" {
		d.origin = Origin{Source: src, Path: path}
		d.originIdentity = documentPathIdentity(path)
		return
	}
	if d.origin.Source != "" && d.originIdentity != "" && d.originIdentity == documentPathIdentity(path) {
		return // same file: provenance rule unchanged
	}
	d.origin = Origin{Source: OriginExplicit, Path: path}
	d.originIdentity = documentPathIdentity(path)
}

// mergeNode merges one authored value onto its raw counterpart under the
// schema node: struct levels mirror authored known keys (absent = deleted)
// and preserve unknown raw member/value content; map levels merge entry-by-entry
// (authored drives existence); leaves take the authored value.
func mergeNode(raw, auth json.RawMessage, n *schemaNode) (json.RawMessage, error) {
	switch {
	case n.isStruct():
		var authMap map[string]json.RawMessage
		if err := json.Unmarshal(auth, &authMap); err != nil {
			return nil, fmt.Errorf("config: merge parse authored object: %w", err)
		}
		rawMap := map[string]json.RawMessage{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rawMap); err != nil {
				return nil, fmt.Errorf("config: merge parse raw object: %w", err)
			}
		}
		out := map[string]json.RawMessage{}
		for k, v := range authMap {
			// Known non-leaf children ALWAYS recurse (raw may be empty) so
			// every level re-encodes key-sorted — render stays idempotent.
			if child, ok := n.known[k]; ok && child != nil {
				merged, err := mergeNode(rawMap[k], v, child)
				if err != nil {
					return nil, fmt.Errorf("config: merge %q: %w", k, err)
				}
				out[k] = merged
				continue
			}
			out[k] = v
		}
		// Authored keys are always a subset of known at struct levels (both
		// derive from the same struct tags), so this raw-unknown preservation
		// loop can never overwrite an authored value.
		for k, v := range rawMap {
			if _, isKnown := n.known[k]; !isKnown {
				out[k] = v // unknown: preserved (content-wise; re-encoded canonically)
			}
		}
		return json.Marshal(out) // Go maps marshal key-sorted → deterministic
	case n.isMap():
		var authEntries map[string]json.RawMessage
		if err := json.Unmarshal(auth, &authEntries); err != nil {
			return nil, fmt.Errorf("config: merge parse map: %w", err)
		}
		rawEntries := map[string]json.RawMessage{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rawEntries); err != nil {
				return nil, fmt.Errorf("config: merge parse raw map: %w", err)
			}
		}
		out := map[string]json.RawMessage{}
		for name, authEntry := range authEntries {
			// Non-leaf entries ALWAYS recurse (raw may be empty): a fresh
			// entry re-encodes key-sorted, keeping render idempotent. Leaf
			// entries (defaults, elem=nil) are authored wholesale.
			if n.elem != nil {
				merged, err := mergeNode(rawEntries[name], authEntry, n.elem)
				if err != nil {
					return nil, fmt.Errorf("config: merge %q: %w", name, err)
				}
				out[name] = merged
				continue
			}
			out[name] = authEntry
		}
		return json.Marshal(out)
	default:
		return auth, nil
	}
}

// renderCanonical merges authored onto raw under the shared schema and
// emits the canonical form: MarshalIndent(2) + trailing newline, so repeated
// calls are byte-identical. Known schema fields mirror the authored struct
// (cleared = deleted); unknown fields survive at top, entry, and nested
// levels; ${ENV} api_key literals never expand. rawBytes may be []byte("{}")
// for from-scratch documents.
func renderCanonical(rawBytes []byte, authored *Config) ([]byte, error) {
	authBytes, err := json.Marshal(authored)
	if err != nil {
		return nil, diagWrap(CodeRenderError, SubjectNone, "", fmt.Errorf("config: render marshal: %w", err))
	}
	merged, err := mergeNode(rawBytes, authBytes, configSchema())
	if err != nil {
		return nil, diagWrap(CodeRenderError, SubjectNone, "", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(merged, &top); err != nil {
		return nil, diagWrap(CodeRenderError, SubjectNone, "", err)
	}
	buf, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, diagWrap(CodeRenderError, SubjectNone, "", err)
	}
	return append(buf, '\n'), nil
}

// canonicalBytes renders the document for disk: the AUTHORED config merged
// onto the retained raw tree via the shared schema walker. Callers hold d.mu.
// Defense-in-depth: a collision-marked document never renders — the merge
// would silently collapse the colliding keys.
func (d *Document) canonicalBytes() ([]byte, error) {
	if err := d.readOnlyErrLocked(); err != nil {
		return nil, err
	}
	rawBytes := d.rawBytes
	if len(d.providerDrops) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(rawBytes, &raw); err != nil {
			return nil, diagWrap(CodeRenderError, SubjectNone, "", err)
		}
		var providers map[string]json.RawMessage
		if err := json.Unmarshal(raw["providers"], &providers); err != nil {
			return nil, diagWrap(CodeRenderError, SubjectNone, "", err)
		}
		for name := range d.providerDrops {
			delete(providers, name)
		}
		providersRaw, err := json.Marshal(providers)
		if err != nil {
			return nil, diagWrap(CodeRenderError, SubjectNone, "", err)
		}
		raw["providers"] = providersRaw
		rawBytes, err = json.Marshal(raw)
		if err != nil {
			return nil, diagWrap(CodeRenderError, SubjectNone, "", err)
		}
	}
	return renderCanonical(rawBytes, d.authored)
}

// publishNew creates path with data only if it does not exist: the synced
// temp is hard-linked to the target (link fails with ErrExist on a race — no
// stat-then-act window), then the temp name is dropped.
func publishNew(path string, data []byte) error {
	tmp, err := writeSiblingTemp(path, data)
	if err != nil {
		return err
	}
	if err := os.Link(tmp, path); err != nil {
		_ = os.Remove(tmp)
		werr := fmt.Errorf("config: create %q: %w", path, err)
		if errors.Is(err, os.ErrExist) {
			return diagWrap(CodeTargetExists, SubjectNone, "", werr)
		}
		return diagWrap(CodeIO, SubjectNone, "", werr)
	}
	_ = os.Remove(tmp)
	if err := syncDir(filepath.Dir(path)); err != nil {
		return diagWrap(CodeDurabilityUncertain, SubjectNone, "",
			fmt.Errorf("%w: %s: dir sync: %v", ErrDurabilityUncertain, path, err))
	}
	return nil
}
