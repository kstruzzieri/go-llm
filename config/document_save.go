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
	"reflect"
	"strings"
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

// knownKeys maps a struct's json tag names to their field types (pointers
// dereferenced). Reflection-derived so the merge can never drift from the
// schema structs.
func knownKeys(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		out[tag] = ft
	}
	return out
}

// mergeableStruct reports whether a known field's type should recurse during
// the merge. Only plain structs recurse; custom-marshal types (Duration) are
// leaves — their JSON shape is not their field set.
func mergeableStruct(ft reflect.Type) bool {
	return ft.Kind() == reflect.Struct && ft != reflect.TypeOf(Duration{})
}

// mergeKnownObject merges one authored object onto its raw counterpart:
// known keys mirror the authored marshal exactly (absent authored key =
// DELETED), unknown raw keys are preserved verbatim, and known struct-typed
// fields recurse so nested unknowns (options, think_tags) survive too.
func mergeKnownObject(raw, auth json.RawMessage, t reflect.Type) (json.RawMessage, error) {
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
	known := knownKeys(t)
	out := map[string]json.RawMessage{}
	for k, v := range authMap {
		if ft, ok := known[k]; ok && mergeableStruct(ft) && len(rawMap[k]) > 0 {
			merged, err := mergeKnownObject(rawMap[k], v, ft)
			if err != nil {
				return nil, err
			}
			out[k] = merged
			continue
		}
		out[k] = v
	}
	for k, v := range rawMap {
		if _, isKnown := known[k]; !isKnown {
			out[k] = v // unknown at this level: preserved verbatim
		}
	}
	return json.Marshal(out) // Go maps marshal key-sorted → deterministic
}

// mergeEntryMap re-merges a providers/models section entry-by-entry: authored
// entries drive existence (deleted entry = deleted), and each surviving entry
// merges against its raw counterpart with the element type so nested unknowns
// survive.
func mergeEntryMap(top, rawTop map[string]json.RawMessage, section string, elem reflect.Type) error {
	authSection, ok := top[section]
	if !ok {
		return nil
	}
	var authEntries map[string]json.RawMessage
	if err := json.Unmarshal(authSection, &authEntries); err != nil {
		return fmt.Errorf("config: merge parse %s: %w", section, err)
	}
	rawEntries := map[string]json.RawMessage{}
	if len(rawTop[section]) > 0 {
		if err := json.Unmarshal(rawTop[section], &rawEntries); err != nil {
			return fmt.Errorf("config: merge parse raw %s: %w", section, err)
		}
	}
	out := map[string]json.RawMessage{}
	for name, authEntry := range authEntries {
		merged, err := mergeKnownObject(rawEntries[name], authEntry, elem)
		if err != nil {
			return err
		}
		out[name] = merged
	}
	buf, err := json.Marshal(out)
	if err != nil {
		return err
	}
	top[section] = buf
	return nil
}

// canonicalBytes renders the document for disk: the AUTHORED config merged
// onto the retained raw tree. Known schema fields mirror the authored struct
// (cleared = deleted); unknown fields survive at top, entry, and nested
// levels; ${ENV} api_key literals never expand. Output is MarshalIndent(2) +
// trailing newline, so repeated calls are byte-identical. Callers hold d.mu.
func (d *Document) canonicalBytes() ([]byte, error) {
	var rawTop map[string]json.RawMessage
	if err := json.Unmarshal(d.rawBytes, &rawTop); err != nil {
		return nil, fmt.Errorf("config: reparse raw tree: %w", err)
	}
	authBytes, err := json.Marshal(d.authored)
	if err != nil {
		return nil, err
	}
	merged, err := mergeKnownObject(d.rawBytes, authBytes, reflect.TypeOf(Config{}))
	if err != nil {
		return nil, err
	}
	// mergeKnownObject treated the three known map sections as leaves
	// (authored wholesale); re-merge providers/models entry-by-entry with
	// their element types so per-entry unknown fields survive. Defaults is
	// map[string]string — no unknowns inside, authored wholesale is right.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(merged, &top); err != nil {
		return nil, err
	}
	if err := mergeEntryMap(top, rawTop, "providers", reflect.TypeOf(ProviderConfig{})); err != nil {
		return nil, err
	}
	if err := mergeEntryMap(top, rawTop, "models", reflect.TypeOf(ModelConfig{})); err != nil {
		return nil, err
	}
	buf, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(buf, '\n'), nil
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
