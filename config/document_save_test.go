package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// P0 LINEARIZATION (spec amendment 6): a mutation cannot interleave between
// a save's snapshot and its commit. With publication paused mid-save, a
// concurrent mutation must BLOCK until the save completes; afterwards the
// document's revision matches the on-disk bytes (pre-mutation content) and
// the mutation's edit lives only in the draft.
func TestSaveMutationLinearization(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "lin.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	m := d.authored.Models["agent"]
	m.Description = "pre-save edit"
	d.authored.Models["agent"] = m

	publishEntered := make(chan struct{})
	releasePublish := make(chan struct{})
	orig := publishReplaceFn
	publishReplaceFn = func(path string, data []byte, rev string) error {
		close(publishEntered)
		<-releasePublish
		return orig(path, data, rev)
	}
	t.Cleanup(func() { publishReplaceFn = orig })

	saveDone := make(chan error, 1)
	rev := d.Revision()
	go func() { saveDone <- d.SaveReplace(p, rev) }()
	<-publishEntered

	// Any d.mu acquirer stands in for a draft mutation (BindUseCase arrives
	// in a later task and re-exercises this via its concurrent test).
	mutDone := make(chan struct{})
	go func() {
		d.mu.Lock()
		m := d.authored.Models["agent"]
		m.Description = "post-save edit"
		d.authored.Models["agent"] = m
		d.mu.Unlock()
		close(mutDone)
	}()

	select {
	case <-mutDone:
		t.Fatal("mutation completed while save held the document — not linearized")
	case <-time.After(50 * time.Millisecond):
	}
	close(releasePublish)
	if err := <-saveDone; err != nil {
		t.Fatal(err)
	}
	<-mutDone
	onDisk, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(onDisk)
	if d.Revision() != hex.EncodeToString(sum[:]) {
		t.Fatal("revision does not match published bytes after interleaved mutation")
	}
	if !bytes.Contains(onDisk, []byte("pre-save edit")) {
		t.Fatal("save lost the snapshot content")
	}
}

// Revision conflicts are a typed sentinel.
func TestSaveReplaceRevisionConflictSentinel(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "sc.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	rev := d.Revision()
	if err := os.WriteFile(p, []byte(`{"providers":{},"models":{},"defaults":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveReplace(p, rev); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("err = %v, want ErrRevisionConflict", err)
	}
}

// Profile saves record PROFILE provenance via the origin-aware variants.
func TestSaveNewAsProfileOrigin(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "prof.json")
	if err := d.SaveNewAs(p, OriginProfile); err != nil {
		t.Fatal(err)
	}
	if o := d.Origin(); o.Source != OriginProfile || o.Path != p {
		t.Fatalf("origin = %+v, want profile@%s", o, p)
	}
}

// Durability-uncertain via the As-variants: bytes live, state committed
// WITH profile origin, typed error at the config layer (the profile store
// converts it to a nil-error SaveOutcome warning — spec amendment 4).
func TestSaveReplaceAsDurabilityUncertainCommitsProfileOrigin(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "prof.json")
	if err := d.SaveNewAs(p, OriginProfile); err != nil {
		t.Fatal(err)
	}
	m := d.authored.Models["agent"]
	m.Description = "changed"
	d.authored.Models["agent"] = m

	orig := syncDir
	syncDir = func(string) error { return errors.New("injected") }
	t.Cleanup(func() { syncDir = orig })

	err := d.SaveReplaceAs(p, d.Revision(), OriginProfile)
	if !errors.Is(err, ErrDurabilityUncertain) {
		t.Fatalf("err = %v", err)
	}
	// Pins the diagWrap at the dir-sync site: a future outer CodeIO wrap
	// would shadow CodeDurabilityUncertain without tripping errors.Is above.
	assertDiag(t, err, CodeDurabilityUncertain, SubjectNone, "")
	if o := d.Origin(); o.Source != OriginProfile {
		t.Fatalf("lost profile origin: %+v", o)
	}
	onDisk, rerr := os.ReadFile(p)
	if rerr != nil {
		t.Fatal(rerr)
	}
	sum := sha256.Sum256(onDisk)
	if d.Revision() != hex.EncodeToString(sum[:]) {
		t.Fatal("state does not reflect published bytes")
	}
}

const rawWithUnknown = `{
  "x-team-note": "keep me",
  "providers": {
    "local": {"base_url": "http://localhost:8080", "api_key": "${DOC_TEST_KEY}", "x-provider-note": "keep me too"}
  },
  "models": {
    "agent": {"name": "m1", "provider": "local", "type": "dense",
      "capabilities": ["chat","tool_call"],
      "options": {"temperature": 0.2, "x-options-note": "nested unknown survives"},
      "x-model-note": "also me"}
  },
  "defaults": {"agent": "agent"}
}`

// Unknown fields survive at EVERY depth; secrets stay literal.
func TestCanonicalBytesPreservesUnknownRecursively(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	out, err := d.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"x-team-note", "x-provider-note", "x-model-note", "x-options-note", "${DOC_TEST_KEY}"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("canonical bytes lost %q", want) // deliberately no full dump
		}
	}
	if bytes.Contains(out, []byte("sekret-value")) {
		t.Fatal("canonical bytes leak an expanded secret") // no dump
	}
}

// A cleared known field is DELETED from output, not left stale in the raw tree.
func TestCanonicalBytesDeletesClearedKnownField(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	m := d.authored.Models["agent"]
	m.Capabilities = nil // simulates slice-3 SetRoleModel clearing the override
	d.authored.Models["agent"] = m
	out, err := d.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`"capabilities"`)) {
		t.Fatal("cleared capabilities survived")
	}
	if !bytes.Contains(out, []byte("x-model-note")) || !bytes.Contains(out, []byte("x-options-note")) {
		t.Fatal("unknown fields lost during deletion merge")
	}
}

// Round-trip: canonical bytes re-load to an equivalent authored config, and
// canonicalizing again is byte-identical (idempotence).
func TestCanonicalBytesRoundTripIdempotent(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	out1, err := d.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	d2, err := newDocument(out1, d.origin)
	if err != nil {
		t.Fatalf("canonical bytes do not re-load: %v", err)
	}
	if !reflect.DeepEqual(d.authored, d2.authored) {
		t.Fatal("authored config changed across round-trip")
	}
	out2, err := d2.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatal("canonicalization not idempotent")
	}
}

func TestSaveNewCreateOnly(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "new.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveNew(p); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second SaveNew err = %v, want ErrExist", err)
	}
}

func TestSaveReplaceConflictDetection(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "c.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	rev := d.Revision()
	external := []byte(`{"providers":{},"models":{},"defaults":{}}`)
	if err := os.WriteFile(p, external, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveReplace(p, rev); err == nil {
		t.Fatal("SaveReplace must refuse a revision mismatch")
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil || !bytes.Equal(got, external) {
		t.Fatal("refused save still modified the file")
	}
}

func TestSaveReplaceDetectsPublicationWindowConflict(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "publication-window.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	expectedRevision := d.Revision()
	m := d.authored.Models["agent"]
	m.Description = "local edit"
	d.authored.Models["agent"] = m

	external := []byte(`{"providers":{},"models":{},"defaults":{},"external":true}`)
	orig := publishReplaceFn
	publishReplaceFn = func(path string, data []byte, expectedRevision string) error {
		if err := os.WriteFile(path, external, 0o600); err != nil {
			return err
		}
		return orig(path, data, expectedRevision)
	}
	t.Cleanup(func() { publishReplaceFn = orig })

	err := d.SaveReplace(p, expectedRevision)
	if err == nil || !strings.Contains(err.Error(), "revision conflict") {
		t.Fatalf("SaveReplace err = %v, want revision conflict", err)
	}
	got, readErr := os.ReadFile(p)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, external) {
		t.Fatal("SaveReplace overwrote publication-window external edit")
	}
}

func TestSaveUpdatesDocumentState(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "s.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	onDisk, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(onDisk)
	if d.Revision() != hex.EncodeToString(sum[:]) {
		t.Fatal("revision does not match written bytes")
	}
	if o := d.Origin(); o.Source != OriginExplicit || o.Path != p {
		t.Fatalf("origin not updated: %+v", o)
	}
	// NOTE: baseline profile ID / Dirty are SLICE 3 caller-owned state —
	// Document deliberately has no such fields to update here.
}

// Saving back to the document's own path preserves its discovery Source; a
// new path is an explicit-path act.
func TestSavePreservesSameFileOriginSource(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "o.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	// Simulate an env-override-discovered document at this path.
	d.mu.Lock()
	d.origin = Origin{Source: OriginEnvOverride, Path: p}
	d.mu.Unlock()
	if err := d.SaveReplace(p, d.Revision()); err != nil {
		t.Fatal(err)
	}
	if o := d.Origin(); o.Source != OriginEnvOverride {
		t.Fatalf("same-path save rewrote origin source: %+v", o)
	}
	p2 := filepath.Join(t.TempDir(), "o2.json")
	if err := d.SaveNew(p2); err != nil {
		t.Fatal(err)
	}
	if o := d.Origin(); o.Source != OriginExplicit || o.Path != p2 {
		t.Fatalf("new-path save origin = %+v, want explicit-path", o)
	}
}

func TestSavePreservesOriginAcrossEquivalentPathSpellings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		originPath func(string) string
		savePath   func(string) string
	}{
		{"clean relative to dot relative", func(string) string { return "models.json" }, func(string) string { return "./models.json" }},
		{"dot relative to absolute", func(string) string { return "./models.json" }, func(dir string) string { return filepath.Join(dir, "models.json") }},
		{"absolute to relative", func(dir string) string { return filepath.Join(dir, "models.json") }, func(string) string { return "models.json" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "models.json")
			d := loadTestDoc(t, rawWithUnknown)
			if err := d.SaveNew(p); err != nil {
				t.Fatal(err)
			}
			d.mu.Lock()
			d.origin = Origin{Source: OriginWorkingDir, Path: tc.originPath(dir)}
			d.mu.Unlock()
			t.Chdir(dir)

			if err := d.SaveReplace(tc.savePath(dir), d.Revision()); err != nil {
				t.Fatal(err)
			}
			if got := d.Origin(); got.Source != OriginWorkingDir || got.Path != tc.originPath(dir) {
				t.Fatalf("origin = %+v, want working-dir path %q", got, tc.originPath(dir))
			}
		})
	}
}

func TestSavePreservesLoadedRelativeOriginAfterChdir(t *testing.T) {
	root := t.TempDir()
	originalDir := filepath.Join(root, "original")
	otherDir := filepath.Join(root, "other")
	for _, dir := range []string{originalDir, otherDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	originalPath := filepath.Join(originalDir, "models.json")
	if err := os.WriteFile(originalPath, []byte(rawWithUnknown), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOC_TEST_KEY", "sekret-value")
	unsetEnvForTest(t, "GO_LLM_CONFIG")
	t.Chdir(originalDir)
	d, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(otherDir)
	if err := d.SaveReplace(originalPath, d.Revision()); err != nil {
		t.Fatal(err)
	}
	if got := d.Origin(); got.Source != OriginWorkingDir || got.Path != "models.json" {
		t.Fatalf("origin = %+v, want original working-dir origin", got)
	}
}

func TestSaveDoesNotPreserveLoadedRelativeOriginInDifferentDirectory(t *testing.T) {
	root := t.TempDir()
	originalDir := filepath.Join(root, "original")
	otherDir := filepath.Join(root, "other")
	for _, dir := range []string{originalDir, otherDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(rawWithUnknown), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DOC_TEST_KEY", "sekret-value")
	unsetEnvForTest(t, "GO_LLM_CONFIG")
	t.Chdir(originalDir)
	d, err := DefaultDocument()
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(otherDir)
	if err := d.SaveReplace("models.json", d.Revision()); err != nil {
		t.Fatal(err)
	}
	if got := d.Origin(); got.Source != OriginExplicit || got.Path != "models.json" {
		t.Fatalf("origin = %+v, want explicit path in new working directory", got)
	}
}

func unsetEnvForTest(t *testing.T, name string) {
	t.Helper()
	value, ok := os.LookupEnv(name)
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(name, value)
		} else {
			_ = os.Unsetenv(name)
		}
	})
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

// Byte-stability proven by an injected publication counter, not timing.
func TestSaveReplaceByteStableNoRewrite(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "b.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	var publishes atomic.Int64
	orig := publishReplaceFn
	publishReplaceFn = func(path string, data []byte, expectedRevision string) error {
		publishes.Add(1)
		return orig(path, data, expectedRevision)
	}
	t.Cleanup(func() { publishReplaceFn = orig })
	if err := d.SaveReplace(p, d.Revision()); err != nil {
		t.Fatal(err)
	}
	if publishes.Load() != 0 {
		t.Fatalf("unchanged content published %d time(s), want 0", publishes.Load())
	}
}

func TestSaveNeverWritesExpandedSecret(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "sec.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("sekret-value")) {
		t.Fatal("expanded secret on disk") // no dump
	}
	if !bytes.Contains(got, []byte("${DOC_TEST_KEY}")) {
		t.Fatal("literal reference lost")
	}
}

// After a durability-uncertain publication the bytes ARE live: document state
// must converge to the published truth alongside the typed error.
func TestSaveReplaceDurabilityUncertainUpdatesState(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "du.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	// Change content so SaveReplace actually publishes.
	m := d.authored.Models["agent"]
	m.Description = "changed"
	d.authored.Models["agent"] = m

	origSync := syncDir
	syncDir = func(string) error { return errors.New("injected dir-sync failure") }
	t.Cleanup(func() { syncDir = origSync })

	err := d.SaveReplace(p, d.Revision())
	if !errors.Is(err, ErrDurabilityUncertain) {
		t.Fatalf("err = %v, want ErrDurabilityUncertain", err)
	}
	onDisk, rerr := os.ReadFile(p)
	if rerr != nil {
		t.Fatal(rerr)
	}
	sum := sha256.Sum256(onDisk)
	if d.Revision() != hex.EncodeToString(sum[:]) {
		t.Fatal("document revision does not reflect published bytes after durability-uncertain save")
	}
}

// SaveNew shares the durability-uncertain contract: bytes live → state
// converges alongside the typed error.
func TestSaveNewDurabilityUncertainUpdatesState(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "ndu.json")

	origSync := syncDir
	syncDir = func(string) error { return errors.New("injected dir-sync failure") }
	t.Cleanup(func() { syncDir = origSync })

	err := d.SaveNew(p)
	if !errors.Is(err, ErrDurabilityUncertain) {
		t.Fatalf("err = %v, want ErrDurabilityUncertain", err)
	}
	onDisk, rerr := os.ReadFile(p)
	if rerr != nil {
		t.Fatal(rerr)
	}
	sum := sha256.Sum256(onDisk)
	if d.Revision() != hex.EncodeToString(sum[:]) {
		t.Fatal("document revision does not reflect published bytes after durability-uncertain SaveNew")
	}
}

// Deleting a whole authored entry deletes it (and its raw unknowns) from the
// canonical output: authored entries drive existence.
func TestCanonicalBytesDeletesRemovedEntry(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	delete(d.authored.Models, "agent")
	d.authored.Defaults = map[string]string{} // keep config self-consistent
	out, err := d.canonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{`"agent"`, "x-model-note", "x-options-note"} {
		if bytes.Contains(out, []byte(gone)) {
			t.Fatalf("removed entry residue %q in canonical output", gone)
		}
	}
	if !bytes.Contains(out, []byte("x-team-note")) || !bytes.Contains(out, []byte("x-provider-note")) {
		t.Fatal("unrelated unknown fields lost")
	}
}

// Concurrent SaveReplace + reads under -race, exercising both mutexes.
func TestSaveConcurrentWithReads(t *testing.T) {
	d := loadTestDoc(t, rawWithUnknown)
	p := filepath.Join(t.TempDir(), "cc.json")
	if err := d.SaveNew(p); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = d.SaveReplace(p, d.Revision())
		}
	}()
	for i := 0; i < 200; i++ {
		_ = d.Config()
		_ = d.Revision()
	}
	<-done
}

// The known-key sets are reflection-derived — spot-pin them against the real
// structs so schema evolution cannot silently drift.
func TestKnownKeysCoverStructTags(t *testing.T) {
	mk := knownKeys(reflect.TypeOf(ModelConfig{}))
	for _, want := range []string{"name", "provider", "type", "parameters", "capabilities", "fallbacks", "options", "think_mode", "think_tags"} {
		if _, ok := mk[want]; !ok {
			t.Fatalf("ModelConfig known keys missing %q (check json tags)", want)
		}
	}
	pk := knownKeys(reflect.TypeOf(ProviderConfig{}))
	for _, want := range []string{"base_url", "timeout", "api_key", "api_format", "slot_discovery"} {
		if _, ok := pk[want]; !ok {
			t.Fatalf("ProviderConfig known keys missing %q", want)
		}
	}
}

func TestPublishReplaceWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.json")
	if err := publishReplace(p, []byte("hello\n"), ""); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("content = %q, err = %v", got, err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v, want 0600", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temp residue: %v", entries)
	}
}

// Create-only publication: existing target survives untouched.
func TestPublishNewRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.json")
	if err := os.WriteFile(p, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := publishNew(p, []byte("clobber"))
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil || string(got) != "original" {
		t.Fatal("existing target was modified")
	}
	entries, derr := os.ReadDir(dir)
	if derr != nil {
		t.Fatal(derr)
	}
	if len(entries) != 1 {
		t.Fatalf("temp residue after refusal: %v", entries)
	}
}

// Post-rename dir-sync failure = published but durability uncertain: typed
// error, bytes live.
func TestPublishDurabilityUncertain(t *testing.T) {
	orig := syncDir
	syncDir = func(string) error { return errors.New("injected dir-sync failure") }
	t.Cleanup(func() { syncDir = orig })

	p := filepath.Join(t.TempDir(), "out.json")
	err := publishReplace(p, []byte("data\n"), "")
	if !errors.Is(err, ErrDurabilityUncertain) {
		t.Fatalf("err = %v, want ErrDurabilityUncertain", err)
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil || string(got) != "data\n" {
		t.Fatal("bytes were not published despite post-rename phase")
	}
}
