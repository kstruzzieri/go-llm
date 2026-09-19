package mcpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fsKey = "dce7cce055566bed799f788cd0048e209a27a473c0f48b956fa1f1780e80d2c1"
const upperFSKey = "f71f7b443cf23e606f946dc774dcaac9a51d3e10df3bbfa0106351c56e0cf93f"

func pinStoreForTest(t *testing.T) *PinStore {
	t.Helper()
	s, err := newPinStore(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pinCatalog(t *testing.T, alias, desc string) toolCatalog {
	t.Helper()
	c, err := newToolCatalog([]catalogEntry{{Name: "mcp__" + alias + "__read", Description: desc, InputSchema: json.RawMessage(`{"type":"object"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readPinBytes(t *testing.T, s *PinStore) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, fsKey+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPinAdmission(t *testing.T) {
	ctx := context.Background()
	s := pinStoreForTest(t)
	a := pinCatalog(t, "fs", "A")
	b := pinCatalog(t, "fs", "B")
	if _, created, err := s.admit(ctx, "fs", a, true); !errors.Is(err, errPinMissing) || created {
		t.Fatalf("require: %v %v", created, err)
	}
	if _, created, err := s.admit(ctx, "fs", a, false); err != nil || !created {
		t.Fatalf("first: %v %v", created, err)
	}
	raw := readPinBytes(t, s)
	before, _ := os.Stat(filepath.Join(s.dir, fsKey+".json"))
	reopened, err := newPinStore(s.workspace, s.base)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := reopened.admit(ctx, "fs", a, true); err != nil || created {
		t.Fatalf("match: %v %v", created, err)
	}
	after, _ := os.Stat(filepath.Join(s.dir, fsKey+".json"))
	if !os.SameFile(before, after) {
		t.Fatal("match rewrote pin")
	}
	if prior, created, err := s.admit(ctx, "fs", b, false); !errors.Is(err, errPinMismatch) || created || prior.digest() != a.digest() {
		t.Fatalf("mismatch: %v %v", created, err)
	}
	if !bytes.Equal(raw, readPinBytes(t, s)) {
		t.Fatal("mismatch changed literal record")
	}
	empty, _ := newToolCatalog(nil)
	if _, _, err := s.admit(ctx, "empty", empty, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.admit(ctx, "empty", pinCatalog(t, "empty", "new"), false); !errors.Is(err, errPinMismatch) {
		t.Fatalf("empty was absence: %v", err)
	}
}

func TestPinIsolationAndAliasValidation(t *testing.T) {
	s := pinStoreForTest(t)
	ctx := context.Background()
	for _, a := range []string{"fs", "FS"} {
		if _, _, err := s.admit(ctx, a, pinCatalog(t, a, a), false); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{fsKey, upperFSKey} {
		for _, ext := range []string{".json", ".lock"} {
			if _, err := os.Stat(filepath.Join(s.dir, key+ext)); err != nil {
				t.Fatal(err)
			}
		}
	}
	other, err := newPinStore(t.TempDir(), s.base)
	if err != nil {
		t.Fatal(err)
	}
	if other.dir == s.dir {
		t.Fatal("shared workspace namespace")
	}
	if _, rev, err := other.capturePin(ctx, "fs"); err != nil || rev.exists {
		t.Fatalf("cross workspace pin: %+v %v", rev, err)
	}
	before, _ := os.ReadDir(s.dir)
	for _, alias := range []string{"", "../escape", "a/b", "a\\b", strings.Repeat("x", 65)} {
		if _, _, err := s.admit(ctx, alias, toolCatalog{}, false); err == nil {
			t.Fatalf("admit accepted %q", alias)
		}
		if _, _, err := s.capturePin(ctx, alias); err == nil {
			t.Fatalf("capture accepted %q", alias)
		}
		if err := s.replacePin(ctx, alias, pinRevision{}, toolCatalog{}); err == nil {
			t.Fatalf("replace accepted %q", alias)
		}
	}
	after, _ := os.ReadDir(s.dir)
	if len(before) != len(after) {
		t.Fatal("invalid alias created path")
	}
}

func TestPinCorruptNeverAbsent(t *testing.T) {
	for name, raw := range map[string]string{
		"syntax": "{broken", "duplicate": `{"version":1,"version":1}`, "unicode": `{"alias":"\ud800"}`,
		"version": `{"version":2,"workspace":"x","alias":"fs","digest":"x","tools":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := pinStoreForTest(t)
			path := filepath.Join(s.dir, fsKey+".json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, created, err := s.admit(context.Background(), "fs", pinCatalog(t, "fs", "A"), false); err == nil || created {
				t.Fatalf("corrupt admitted: %v %v", created, err)
			}
			if got := readPinBytes(t, s); string(got) != raw {
				t.Fatalf("corrupt bytes overwritten: %s", got)
			}
		})
	}
}

func TestPinRecordValidation(t *testing.T) {
	for _, field := range []string{"version", "workspace", "alias", "digest", "tools", "unknown", "description", "name", "schema", "order", "duplicate", "case-key"} {
		t.Run(field, func(t *testing.T) {
			s := pinStoreForTest(t)
			ctx := context.Background()
			a := pinCatalog(t, "fs", "A")
			if _, _, err := s.admit(ctx, "fs", a, false); err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(readPinBytes(t, s), &record); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "version":
				record[field] = 2
			case "workspace", "alias", "digest":
				record[field] = "wrong"
			case "tools":
				record[field] = nil
			case "unknown":
				record[field] = true
			case "case-key":
				record["Version"] = record["version"]
				delete(record, "version")
			default:
				entries := a.entriesCopy()
				switch field {
				case "description":
					entries[0].Description = strings.Repeat("x", 8193)
				case "name":
					entries[0].Name = "mcp__other__read"
				case "schema":
					entries[0].InputSchema = json.RawMessage(`null`)
				case "order":
					entries = append(entries, catalogEntry{Name: "mcp__fs__aaa", InputSchema: json.RawMessage(`{}`)})
				case "duplicate":
					entries = append(entries, entries[0])
				}
				canonical, err := newToolCatalog(entries)
				if err != nil {
					t.Fatal(err)
				}
				record["tools"] = entries
				record["digest"] = canonical.digest()
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(s.dir, fsKey+".json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.capturePin(ctx, "fs"); err == nil {
				t.Fatalf("accepted invalid %s", field)
			}
			if !bytes.Equal(raw, readPinBytes(t, s)) {
				t.Fatal("invalid record rewritten")
			}
		})
	}
}

func TestPinRevisionCompareAndReplace(t *testing.T) {
	s := pinStoreForTest(t)
	ctx := context.Background()
	a := pinCatalog(t, "fs", "A")
	b := pinCatalog(t, "fs", "B")
	_, absent, err := s.capturePin(ctx, "fs")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.admit(ctx, "fs", a, false); err != nil {
		t.Fatal(err)
	}
	if err = s.replacePin(ctx, "fs", absent, b); !errors.Is(err, errPinRevisionConflict) {
		t.Fatalf("first pin conflict: %v", err)
	}
	_, prior, err := s.capturePin(ctx, "fs")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.replacePin(ctx, "fs", prior, b); err != nil {
		t.Fatal(err)
	}
	raw := readPinBytes(t, s)
	if err = s.replacePin(ctx, "fs", prior, b); !errors.Is(err, errPinRevisionConflict) {
		t.Fatalf("same candidate competing approval: %v", err)
	}
	if !bytes.Equal(raw, readPinBytes(t, s)) {
		t.Fatal("conflict rewrote pin")
	}
	// Equivalent JSON still changes the exact revision bytes.
	_, prior, err = s.capturePin(ctx, "fs")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.dir, fsKey+".json"), append(raw, 10), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.replacePin(ctx, "fs", prior, a); !errors.Is(err, errPinRevisionConflict) {
		t.Fatalf("exact byte conflict: %v", err)
	}
}

func TestPinWorkspaceBoundary(t *testing.T) {
	ws := t.TempDir()
	for _, bad := range []string{"", filepath.Join(ws, "missing")} {
		if _, err := newPinStore(bad, t.TempDir()); err == nil {
			t.Fatalf("accepted root %q", bad)
		}
	}
	file := filepath.Join(ws, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newPinStore(file, t.TempDir()); err == nil {
		t.Fatal("file workspace")
	}
	if _, err := newPinStore(ws, filepath.Join(ws, "data")); err == nil {
		t.Fatal("store inside workspace")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(ws, link); err != nil {
		t.Skip(err)
	}
	if _, err := newPinStore(ws, link); err == nil {
		t.Fatal("symlink store inside workspace")
	}
}

func TestPinMaximalEscapedCatalog(t *testing.T) {
	s := pinStoreForTest(t)
	entries := make([]catalogEntry, 128)
	for i := range entries {
		entries[i] = catalogEntry{Name: fmt.Sprintf("mcp__fs__t%03d", i), Description: strings.Repeat("\x01", 8192), InputSchema: json.RawMessage(`{"x":"` + strings.Repeat("a", 32760) + `"}`)}
	}
	c, err := newToolCatalog(entries)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.admit(context.Background(), "fs", c, false); err != nil {
		t.Fatal(err)
	}
	if len(readPinBytes(t, s)) < 10*1024*1024 {
		t.Fatal("fixture did not exercise JSON expansion")
	}
	got, rev, err := s.capturePin(context.Background(), "fs")
	if err != nil || !rev.exists || got.digest() != c.digest() {
		t.Fatalf("maximal reopen: %v", err)
	}
}

func TestPinPublishesCanonicalBytesWithinBound(t *testing.T) {
	s := pinStoreForTest(t)
	// HTML-significant bytes stay literal on disk: the persisted tools value is
	// exactly the canonical catalog, so the validated catalog bounds also bound
	// the record (about 10 MiB at every limit, under maxPinBytes). encoding/json's
	// default escaping would instead write six-byte escapes and let a bounded
	// catalog exceed maxPinBytes at publication.
	c, err := newToolCatalog([]catalogEntry{
		{Name: "mcp__fs__read", Description: "<>&" + strings.Repeat("<>", 32), InputSchema: json.RawMessage(`{"properties":{"q":{"description":"<script>&amp;</script>","type":"string"}},"type":"object"}`)},
		{Name: "mcp__fs__write", Description: "plain", InputSchema: json.RawMessage(`{"type":"object","x":"<>&"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.admit(context.Background(), "fs", c, false); err != nil || !created {
		t.Fatalf("publish: created=%v err=%v", created, err)
	}
	raw := readPinBytes(t, s)
	if bytes.Contains(raw, []byte(`\u003c`)) || bytes.Contains(raw, []byte(`\u0026`)) || !bytes.Contains(raw, c.canonicalBytes()) {
		t.Fatalf("published tools are not the literal canonical bytes: %s", raw)
	}
	var record pinRecord
	if err = json.Unmarshal(raw, &record); err != nil || !bytes.Equal(record.Tools, c.canonicalBytes()) {
		t.Fatalf("persisted tools differ from canonical: %v", err)
	}
	// The record adds only its fixed identity header to the canonical bytes.
	var header bytes.Buffer
	enc := json.NewEncoder(&header)
	enc.SetEscapeHTML(false)
	if err = enc.Encode(pinRecord{Version: c.version(), Workspace: s.workspace, Alias: "fs", Digest: c.digest(), Tools: json.RawMessage("[]")}); err != nil {
		t.Fatal(err)
	}
	if want := len(bytes.TrimSpace(header.Bytes())) - len("[]") + len(c.canonicalBytes()); len(raw) != want {
		t.Fatalf("published size %d, want canonical %d plus header = %d", len(raw), len(c.canonicalBytes()), want)
	}
	got, rev, err := s.capturePin(context.Background(), "fs")
	if err != nil || !rev.exists || got.digest() != c.digest() {
		t.Fatalf("reopen: %v", err)
	}
}

func TestPinPublicConstructor(t *testing.T) {
	base := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("XDG_DATA_HOME", base)
	s, err := NewPinStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.base, "golem", "mcp-pins", fmt.Sprintf("%x", sha256.Sum256([]byte(s.workspace))))
	if s.dir != want {
		t.Fatalf("workspace namespace = %s, want %s", s.dir, want)
	}
	link := filepath.Join(t.TempDir(), "workspace-alias")
	if err = os.Symlink(workspace, link); err != nil {
		t.Skip(err)
	}
	same, err := NewPinStore(link)
	if err != nil {
		t.Fatal(err)
	}
	if same.dir != s.dir {
		t.Fatal("canonical workspace alias selected different namespace")
	}
	nested, err := newPinStore(workspace, filepath.Join(base, "new", "nested"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = nested.admit(context.Background(), "fs", pinCatalog(t, "fs", "A"), false); err != nil {
		t.Fatal(err)
	}
}

func TestPinDedicatedAncestorSymlinks(t *testing.T) {
	for _, name := range []string{"golem", "mcp-pins"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			parent := base
			if name == "mcp-pins" {
				parent = filepath.Join(base, "golem")
				if err := os.Mkdir(parent, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(t.TempDir(), filepath.Join(parent, name)); err != nil {
				t.Skip(err)
			}
			if _, err := newPinStore(t.TempDir(), base); err == nil {
				t.Fatal("accepted symlink dedicated ancestor")
			}
		})
	}
}

func TestPinSerializedBound(t *testing.T) {
	s := pinStoreForTest(t)
	// Exercise the final serialized bound directly; normal admission also
	// rejects this invalid overlong description before reaching publication.
	candidate, err := newToolCatalog([]catalogEntry{{Name: "mcp__fs__read", Description: strings.Repeat("x", 16*1024*1024), InputSchema: json.RawMessage(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.openRoot(false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	err = s.publish(context.Background(), root, fsKey+".json", "fs", candidate)
	if err == nil || !strings.Contains(err.Error(), "serialized pin exceeds") {
		t.Fatalf("serialized bound: %v", err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("oversized publication created files")
	}
}
