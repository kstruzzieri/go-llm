package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

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
	if err := publishReplace(p, []byte("hello\n")); err != nil {
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
	err := publishReplace(p, []byte("data\n"))
	if !errors.Is(err, ErrDurabilityUncertain) {
		t.Fatalf("err = %v, want ErrDurabilityUncertain", err)
	}
	got, rerr := os.ReadFile(p)
	if rerr != nil || string(got) != "data\n" {
		t.Fatal("bytes were not published despite post-rename phase")
	}
}
