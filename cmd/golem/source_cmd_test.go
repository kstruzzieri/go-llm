package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/rag"
)

func TestRunSourceUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no subcommand", nil, "usage: golem source"},
		{"unknown subcommand", []string{"bogus"}, `unknown source command "bogus"`},
		{"add no path no text", []string{"add"}, "path is required"},
		{"add text without name", []string{"add", "-text"}, "-name is required"},
		{"add text with path", []string{"add", "-text", "-name", "n", "x.txt"}, "cannot combine -text with a path"},
		{"add two paths", []string{"add", "a.txt", "b.txt"}, "exactly one path"},
		{"add trailing flag", []string{"add", "a.txt", "-title", "T"}, "flags must come before"},
		{"rm no id", []string{"rm"}, "id is required"},
		{"rm two ids", []string{"rm", "a", "b"}, "exactly one id"},
		{"rm trailing flag", []string{"rm", "a", "-root", "."}, "flags must come before"},
		{"rm short id", []string{"rm", "0123456789abcdef"}, "full 32-character lowercase hexadecimal"},
		{"rm uppercase id", []string{"rm", strings.Repeat("A", 32)}, "full 32-character lowercase hexadecimal"},
		{"rm nonhex id", []string{"rm", strings.Repeat("z", 32)}, "full 32-character lowercase hexadecimal"},
		{"reindex no id", []string{"reindex"}, "id is required"},
		{"reindex trailing flag", []string{"reindex", "a", "-root", "."}, "flags must come before"},
		{"reindex short id", []string{"reindex", "0123456789abcdef"}, "full 32-character lowercase hexadecimal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runSource(context.Background(), tc.args, strings.NewReader(""), &out, &errOut)
			if !errors.Is(err, errSourceFailed) {
				t.Fatalf("error = %v, want errSourceFailed", err)
			}
			combined := out.String() + errOut.String() + err.Error()
			if !strings.Contains(combined, tc.want) {
				t.Fatalf("want %q in output, got err=%v out=%q errOut=%q", tc.want, err, out.String(), errOut.String())
			}
		})
	}
}

func TestSourceRejectsRagDBAsRenderedUsage(t *testing.T) {
	tests := [][]string{
		{"add", "-rag-db", "ignored", "doc.txt"},
		{"list", "-rag-db", "ignored"},
		{"rm", "-rag-db", "ignored", strings.Repeat("a", 32)},
		{"reindex", "-rag-db", "ignored", strings.Repeat("a", 32)},
	}
	for _, args := range tests {
		var out, errOut bytes.Buffer
		err := runSource(context.Background(), args, strings.NewReader(""), &out, &errOut)
		if !errors.Is(err, errSourceFailed) {
			t.Fatalf("%v: error = %v, want errSourceFailed", args, err)
		}
		if out.Len() != 0 || !strings.Contains(errOut.String(), "flag provided but not defined: -rag-db") {
			t.Fatalf("%v: stdout=%q stderr=%q", args, out.String(), errOut.String())
		}
	}
}

// sourceTestEnv returns a getenv seam that puts the golem data dir in a temp
// dir, so indexDBPathForWorkspace resolves under the test's control.
func sourceTestEnv(t *testing.T) (func(string) string, string) {
	t.Helper()
	dataHome := t.TempDir()
	getenv := func(key string) string {
		if key == "XDG_DATA_HOME" {
			return dataHome
		}
		return ""
	}
	return getenv, dataHome
}

func sourceTestDeps(t *testing.T, vsid string) (sourceDeps, string) {
	t.Helper()
	getenv, dataHome := sourceTestEnv(t)
	return sourceDeps{
		getenv:   getenv,
		embedder: autoIndexTestEmbedder(vsid, ""),
		embChain: []string{"test-model"},
	}, dataHome
}

func TestSourceAddFileNoIndexCreatesGeneration(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	doc := filepath.Join(root, "notes.md")
	if err := os.WriteFile(doc, []byte("golem managed cli acceptance body"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := runSourceWith(context.Background(), []string{"add", "-root", root, doc}, strings.NewReader(""), &out, &errOut, deps)
	if err != nil {
		t.Fatalf("add: %v\nout=%s errOut=%s", err, out.String(), errOut.String())
	}
	// Published generation exists and contains the managed doc.
	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if err != nil {
		t.Fatalf("no published generation after add: %v", err)
	}
	if gen.metadata.SourceCount != 1 || gen.metadata.ChunkCount < 1 {
		t.Fatalf("unexpected metadata: %+v", gen.metadata)
	}
	if !strings.Contains(out.String(), "indexed") {
		t.Fatalf("add output missing document summary: %q", out.String())
	}
}

func TestSourceMutationsPropagatePrimaryOutputFailure(t *testing.T) {
	writeErr := errors.New("write failed")
	t.Run("add", func(t *testing.T) {
		deps, _ := sourceTestDeps(t, "test/space")
		root := t.TempDir()
		doc := filepath.Join(root, "notes.md")
		if err := os.WriteFile(doc, []byte("managed output failure"), 0o600); err != nil {
			t.Fatal(err)
		}
		var errOut bytes.Buffer
		err := runSourceWith(context.Background(), []string{"add", "-root", root, doc}, strings.NewReader(""),
			sourceErrorWriter{err: writeErr}, &errOut, deps)
		if !errors.Is(err, errSourceFailed) || !strings.Contains(errOut.String(), writeErr.Error()) {
			t.Fatalf("error=%v stderr=%q", err, errOut.String())
		}
	})

	t.Run("rm", func(t *testing.T) {
		deps, _ := sourceTestDeps(t, "test/space")
		root := t.TempDir()
		doc := filepath.Join(root, "notes.md")
		if err := os.WriteFile(doc, []byte("managed output failure"), 0o600); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(root, "other.md")
		if err := os.WriteFile(other, []byte("keep one managed document"), 0o600); err != nil {
			t.Fatal(err)
		}
		var addOut, errOut bytes.Buffer
		if err := runSourceWith(context.Background(), []string{"add", "-root", root, doc}, strings.NewReader(""),
			&addOut, &errOut, deps); err != nil {
			t.Fatalf("seed add: %v: %s", err, errOut.String())
		}
		id := strings.Fields(addOut.String())[0]
		if err := runSourceWith(context.Background(), []string{"add", "-root", root, other}, strings.NewReader(""),
			io.Discard, &errOut, deps); err != nil {
			t.Fatalf("seed second add: %v: %s", err, errOut.String())
		}
		errOut.Reset()
		err := runSourceWith(context.Background(), []string{"rm", "-root", root, id}, strings.NewReader(""),
			sourceErrorWriter{err: writeErr}, &errOut, deps)
		if !errors.Is(err, errSourceFailed) || !strings.Contains(errOut.String(), writeErr.Error()) {
			t.Fatalf("error=%v stderr=%q", err, errOut.String())
		}
	})

	t.Run("reindex", func(t *testing.T) {
		deps, _ := sourceTestDeps(t, "test/space")
		root := t.TempDir()
		doc := filepath.Join(root, "notes.md")
		if err := os.WriteFile(doc, []byte("managed output failure"), 0o600); err != nil {
			t.Fatal(err)
		}
		var addOut, errOut bytes.Buffer
		if err := runSourceWith(context.Background(), []string{"add", "-root", root, doc}, strings.NewReader(""),
			&addOut, &errOut, deps); err != nil {
			t.Fatalf("seed add: %v: %s", err, errOut.String())
		}
		id := strings.Fields(addOut.String())[0]
		errOut.Reset()
		err := runSourceWith(context.Background(), []string{"reindex", "-root", root, id}, strings.NewReader(""),
			sourceErrorWriter{err: writeErr}, &errOut, deps)
		if !errors.Is(err, errSourceFailed) || !strings.Contains(errOut.String(), writeErr.Error()) {
			t.Fatalf("error=%v stderr=%q", err, errOut.String())
		}
	})
}

func TestSourceAddTextFromStdin(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	var out, errOut bytes.Buffer
	err := runSourceWith(context.Background(),
		[]string{"add", "-root", root, "-text", "-name", "policy.md", "-collection", "ops", "-tag", "a", "-tag", "b"},
		strings.NewReader("text body via stdin"), &out, &errOut, deps)
	if err != nil {
		t.Fatalf("add -text: %v\nerrOut=%s", err, errOut.String())
	}
	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	managed, err := rag.NewManagedSources(nil, ro)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := managed.ListDocuments(context.Background(), rag.DocumentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Kind != rag.DocumentKindText || docs[0].Collection != "ops" || len(docs[0].Tags) != 2 {
		t.Fatalf("unexpected doc: %+v", docs)
	}
}

func TestSourceAddLeaseHeldFailsLoudly(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	doc := filepath.Join(root, "notes.md")
	if err := os.WriteFile(doc, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, dbPath, _, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireIndexWriterLease(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	var out, errOut bytes.Buffer
	err = runSourceWith(context.Background(), []string{"add", "-root", root, doc}, strings.NewReader(""), &out, &errOut, deps)
	if !errors.Is(err, errSourceFailed) {
		t.Fatalf("want errSourceFailed, got %v", err)
	}
	if !strings.Contains(out.String()+errOut.String(), "lease already held") {
		t.Fatalf("want loud lease message, got out=%q errOut=%q", out.String(), errOut.String())
	}
	// Nothing was created: no pointer, no staging leftovers.
	if fileExists(activePointerPath(dbPath)) {
		t.Fatal("lease-contended add must not publish")
	}
}

func TestSourceAddEmptyInputFailsBeforeProvider(t *testing.T) {
	getenv, _ := sourceTestEnv(t)
	root := t.TempDir()
	emptyFile := filepath.Join(root, "empty.md")
	if err := os.WriteFile(emptyFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missingConfig := filepath.Join(t.TempDir(), "missing-models.json")
	tests := []struct {
		name  string
		args  []string
		stdin io.Reader
	}{
		{
			name:  "file",
			args:  []string{"add", "-config", missingConfig, "-root", root, emptyFile},
			stdin: strings.NewReader(""),
		},
		{
			name:  "stdin",
			args:  []string{"add", "-config", missingConfig, "-root", root, "-text", "-name", "empty.md"},
			stdin: strings.NewReader(""),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := runSourceWith(context.Background(), tc.args, tc.stdin, &out, &errOut, sourceDeps{getenv: getenv})
			if !errors.Is(err, errSourceFailed) ||
				!strings.Contains(errOut.String(), "must not be empty") ||
				strings.Contains(errOut.String(), missingConfig) {
				t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
			}
		})
	}
}

func TestSourceAddFileTruncatedAfterPreflightDoesNotPublish(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	keep := filepath.Join(root, "keep.md")
	victim := filepath.Join(root, "victim.md")
	if err := os.WriteFile(keep, []byte("keep this source"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"add", "-root", root, keep},
		strings.NewReader(""), &out, &errOut, deps); err != nil {
		t.Fatal(err)
	}
	_, dbPath, _, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	pointerBefore, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("present during preflight"), 0o600); err != nil {
		t.Fatal(err)
	}
	racingDeps := deps
	var truncateErr error
	racingDeps.afterInputPreflight = func() {
		truncateErr = os.WriteFile(victim, nil, 0o600)
	}
	out.Reset()
	errOut.Reset()
	err = runSourceWith(context.Background(), []string{"add", "-root", root, victim},
		strings.NewReader(""), &out, &errOut, racingDeps)
	if truncateErr != nil {
		t.Fatal(truncateErr)
	}
	if !errors.Is(err, errSourceFailed) ||
		!strings.Contains(errOut.String(), "no indexable content") {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
	pointerAfter, err := os.ReadFile(activePointerPath(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pointerBefore, pointerAfter) {
		t.Fatal("post-preflight empty file changed the active pointer")
	}
}

// addTestDoc runs `source add` and returns the created document ID via list.
func addTestDoc(t *testing.T, deps sourceDeps, root, path string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"add", "-root", root, path}, strings.NewReader(""), &out, &errOut, deps); err != nil {
		t.Fatalf("add: %v\n%s%s", err, out.String(), errOut.String())
	}
	fields := strings.Fields(out.String())
	if len(fields) == 0 || len(fields[0]) != 32 {
		t.Fatalf("cannot parse doc id from %q", out.String())
	}
	return fields[0]
}

func TestSourceRmRemovesChunksOffline(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	// Offline means no provider work at all: poison process-level config
	// discovery so any accidental bootstrap fails loudly instead of quietly
	// resolving the developer's real models.json.
	t.Setenv("GO_LLM_CONFIG", filepath.Join(t.TempDir(), "missing-models.json"))
	root := t.TempDir()
	keep := filepath.Join(root, "keep.md")
	gone := filepath.Join(root, "gone.md")
	for _, p := range []string{keep, gone} {
		if err := os.WriteFile(p, []byte("content of "+p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = addTestDoc(t, deps, root, keep)
	id := addTestDoc(t, deps, root, gone)

	// rm must work with NO embedder seam (offline): getenv only.
	rmDeps := sourceDeps{getenv: deps.getenv}
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"rm", "-root", root, id}, strings.NewReader(""), &out, &errOut, rmDeps); err != nil {
		t.Fatalf("rm: %v\n%s%s", err, out.String(), errOut.String())
	}

	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if gen.metadata.SourceCount != 1 {
		t.Fatalf("want 1 surviving source, got %+v", gen.metadata)
	}
	ro, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	managed, err := rag.NewManagedSources(nil, ro)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := managed.ListDocuments(context.Background(), rag.DocumentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 {
		t.Fatalf("want 1 doc after rm, got %d", len(docs))
	}
	if docs[0].ID == id {
		t.Fatal("deleted doc still listed")
	}
}

func TestSourceRmUnknownIDFails(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	doc := filepath.Join(root, "a.md")
	if err := os.WriteFile(doc, []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = addTestDoc(t, deps, root, doc)
	var out, errOut bytes.Buffer
	err := runSourceWith(context.Background(), []string{"rm", "-root", root, strings.Repeat("f", 32)},
		strings.NewReader(""), &out, &errOut, sourceDeps{getenv: deps.getenv})
	if !errors.Is(err, errSourceFailed) {
		t.Fatalf("want rendered failure, got %v", err)
	}
	if !strings.Contains(out.String()+errOut.String(), "not found") {
		t.Fatalf("want not-found message, got %q %q", out.String(), errOut.String())
	}
}

func TestSourceRmNoIndexFails(t *testing.T) {
	getenv, _ := sourceTestEnv(t)
	root := t.TempDir()
	var out, errOut bytes.Buffer
	err := runSourceWith(context.Background(), []string{"rm", "-root", root, strings.Repeat("a", 32)},
		strings.NewReader(""), &out, &errOut, sourceDeps{getenv: getenv})
	if !errors.Is(err, errSourceFailed) {
		t.Fatalf("want rendered failure, got %v", err)
	}
	if !strings.Contains(out.String()+errOut.String(), "no workspace index") {
		t.Fatalf("want no-index message, got %q %q", out.String(), errOut.String())
	}
}

func TestSourceReindexChecksPreconditionsBeforeProvider(t *testing.T) {
	getenv, _ := sourceTestEnv(t)
	root := t.TempDir()
	missingConfig := filepath.Join(t.TempDir(), "missing-models.json")
	id := strings.Repeat("a", 32)

	t.Run("no index", func(t *testing.T) {
		var out, errOut bytes.Buffer
		err := runSourceWith(context.Background(),
			[]string{"reindex", "-config", missingConfig, "-root", root, id},
			strings.NewReader(""), &out, &errOut, sourceDeps{getenv: getenv})
		if !errors.Is(err, errSourceFailed) ||
			!strings.Contains(errOut.String(), "no workspace index") ||
			strings.Contains(errOut.String(), missingConfig) {
			t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
		}
	})

	t.Run("lease held", func(t *testing.T) {
		_, dbPath, _, err := sourceWorkspace(root, getenv)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := acquireIndexWriterLease(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lease.Close() }()
		var out, errOut bytes.Buffer
		err = runSourceWith(context.Background(),
			[]string{"reindex", "-config", missingConfig, "-root", root, id},
			strings.NewReader(""), &out, &errOut, sourceDeps{getenv: getenv})
		if !errors.Is(err, errSourceFailed) ||
			!strings.Contains(errOut.String(), "lease already held") ||
			strings.Contains(errOut.String(), missingConfig) {
			t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
		}
	})
}

func TestSourceReindexRefreshesStaleFile(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	doc := filepath.Join(root, "live.md")
	if err := os.WriteFile(doc, []byte("original body"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := addTestDoc(t, deps, root, doc)
	if err := os.WriteFile(doc, []byte("rewritten body"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"reindex", "-root", root, id}, strings.NewReader(""), &out, &errOut, deps); err != nil {
		t.Fatalf("reindex: %v\n%s%s", err, out.String(), errOut.String())
	}
	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := rag.OpenSQLiteStoreReadOnly(gen.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	managed, err := rag.NewManagedSources(nil, ro)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := managed.ListDocuments(context.Background(), rag.DocumentFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Freshness != rag.DocumentFreshnessFresh {
		t.Fatalf("want fresh after reindex, got %+v", docs)
	}
}

func TestSourceListShowsTransitionsWithoutMutatingGeneration(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	doc := filepath.Join(root, "live.md")
	if err := os.WriteFile(doc, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := addTestDoc(t, deps, root, doc)

	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := resolveActiveGeneration(context.Background(), dbPath, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	before := hashTestFile(t, gen.dbPath)

	listDeps := sourceDeps{getenv: deps.getenv} // list needs no embedder
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"list", "-root", root}, strings.NewReader(""), &out, &errOut, listDeps); err != nil {
		t.Fatalf("list: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), id) || !strings.Contains(out.String(), "fresh") {
		t.Fatalf("want fresh row for %s, got %q", id, out.String())
	}

	if err := os.WriteFile(doc, []byte("changed on disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runSourceWith(context.Background(), []string{"list", "-root", root}, strings.NewReader(""), &out, &errOut, listDeps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "stale") {
		t.Fatalf("want stale after origin change, got %q", out.String())
	}
	if after := hashTestFile(t, gen.dbPath); after != before {
		t.Fatal("list mutated the published generation database")
	}
}

func TestSourceListJSON(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	doc := filepath.Join(root, "j.md")
	if err := os.WriteFile(doc, []byte("json body"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := addTestDoc(t, deps, root, doc)
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"list", "-root", root, "-json"}, strings.NewReader(""), &out, &errOut, sourceDeps{getenv: deps.getenv}); err != nil {
		t.Fatal(err)
	}
	var docs []rag.Document
	if err := json.Unmarshal(out.Bytes(), &docs); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out.String())
	}
	if len(docs) != 1 || docs[0].ID != id || docs[0].UpdatedAt == 0 {
		t.Fatalf("unexpected JSON docs: %+v", docs)
	}
}

func TestSourceListNoIndexExitsZero(t *testing.T) {
	getenv, _ := sourceTestEnv(t)
	root := t.TempDir()
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"list", "-root", root}, strings.NewReader(""), &out, &errOut, sourceDeps{getenv: getenv}); err != nil {
		t.Fatalf("no-index list must exit zero, got %v", err)
	}
	if !strings.Contains(errOut.String(), "no workspace index") {
		t.Fatalf("want no-index notice on stderr, got %q", errOut.String())
	}
	if out.String() != "" {
		t.Fatalf("human no-index list must keep stdout empty, got %q", out.String())
	}
}

func TestSourceCommandsRejectDanglingActivePointer(t *testing.T) {
	deps, _ := sourceTestDeps(t, "test/space")
	root := t.TempDir()
	doc := filepath.Join(root, "notes.md")
	if err := os.WriteFile(doc, []byte("must not replace a dangling generation"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, dbPath, workspaceID, err := sourceWorkspace(root, deps.getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishActiveGeneration(context.Background(), dbPath, activeGenerationPointer{
		SchemaVersion: activePointerSchemaVersion,
		WorkspaceID:   workspaceID,
		Generation:    strings.Repeat("a", 32),
	}, nil); err != nil {
		t.Fatal(err)
	}

	id := strings.Repeat("b", 32)
	for _, args := range [][]string{
		{"list", "-root", root, "-json"},
		{"add", "-root", root, doc},
		{"rm", "-root", root, id},
		{"reindex", "-root", root, id},
	} {
		var out, errOut bytes.Buffer
		err := runSourceWith(context.Background(), args, strings.NewReader(""), &out, &errOut, deps)
		if !errors.Is(err, errSourceFailed) || out.Len() != 0 ||
			strings.Contains(errOut.String(), "no workspace index") ||
			!strings.Contains(errOut.String(), "read generation metadata") {
			t.Fatalf("args=%v error=%v stdout=%q stderr=%q", args, err, out.String(), errOut.String())
		}
	}
}

func TestSourceListRejectsIncompleteLegacyIndex(t *testing.T) {
	getenv, _ := sourceTestEnv(t)
	root := t.TempDir()
	_, dbPath, _, err := sourceWorkspace(root, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("orphaned legacy database"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err = runSourceWith(context.Background(), []string{"list", "-root", root, "-json"}, strings.NewReader(""),
		&out, &errOut, sourceDeps{getenv: getenv})
	if !errors.Is(err, errSourceFailed) || out.Len() != 0 ||
		strings.Contains(errOut.String(), "no workspace index") ||
		!strings.Contains(errOut.String(), "incomplete legacy index") {
		t.Fatalf("error=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

// TestSourceListJSONKeepsStdoutMachineReadable: the -json contract — stdout
// is exactly one JSON document even with no index ([]), notices go to stderr.
func TestSourceListJSONKeepsStdoutMachineReadable(t *testing.T) {
	getenv, _ := sourceTestEnv(t)
	root := t.TempDir()
	var out, errOut bytes.Buffer
	if err := runSourceWith(context.Background(), []string{"list", "-root", root, "-json"}, strings.NewReader(""), &out, &errOut, sourceDeps{getenv: getenv}); err != nil {
		t.Fatalf("no-index -json list must exit zero, got %v", err)
	}
	if out.String() != "[]\n" {
		t.Fatalf("want exactly []\\n on stdout, got %q", out.String())
	}
	var docs []rag.Document
	if err := json.Unmarshal(out.Bytes(), &docs); err != nil {
		t.Fatalf("stdout is not valid JSON: %v", err)
	}
}

func TestSourceListNoIndexPropagatesJSONWriteFailure(t *testing.T) {
	getenv, _ := sourceTestEnv(t)
	root := t.TempDir()
	writeErr := errors.New("write failed")
	var errOut bytes.Buffer
	err := runSourceWith(context.Background(), []string{"list", "-root", root, "-json"}, strings.NewReader(""),
		sourceErrorWriter{err: writeErr}, &errOut, sourceDeps{getenv: getenv})
	if !errors.Is(err, errSourceFailed) || !strings.Contains(errOut.String(), writeErr.Error()) {
		t.Fatalf("error = %v, stderr = %q; want rendered write failure", err, errOut.String())
	}
}

func TestOpenResolvedSourceListStoreRetriesOneDisappearedGeneration(t *testing.T) {
	dir := t.TempDir()
	oldGeneration := indexGeneration{dbPath: filepath.Join(dir, "old.db")}
	newGeneration := indexGeneration{dbPath: filepath.Join(dir, "new.db")}
	for _, path := range []string{oldGeneration.dbPath, newGeneration.dbPath} {
		store, err := rag.NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(oldGeneration.dbPath); err != nil {
		t.Fatal(err)
	}
	var calls []string
	resolve := func(context.Context, string, string) (indexGeneration, error) {
		calls = append(calls, "resolve")
		return newGeneration, nil
	}
	open := func(path string) (*rag.SQLiteStore, error) {
		calls = append(calls, "open:"+path)
		return rag.OpenSQLiteStoreReadOnly(path)
	}
	got, err := openResolvedSourceListStore(context.Background(), "base.db", "workspace:test",
		oldGeneration, resolve, open)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = got.Close() }()
	wantCalls := []string{"open:" + oldGeneration.dbPath, "resolve", "open:" + newGeneration.dbPath}
	if !slices.Equal(calls, wantCalls) {
		t.Fatalf("calls=%v, want %v", calls, wantCalls)
	}
}

func TestOpenResolvedSourceListStoreReportsRetirementRace(t *testing.T) {
	dir := t.TempDir()
	oldGeneration := indexGeneration{dbPath: filepath.Join(dir, "old.db")}
	store, err := rag.NewSQLiteStore(oldGeneration.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(oldGeneration.dbPath); err != nil {
		t.Fatal(err)
	}
	resolve := func(context.Context, string, string) (indexGeneration, error) {
		return indexGeneration{}, errNoActiveGeneration
	}
	got, err := openResolvedSourceListStore(context.Background(), "base.db", "workspace:test",
		oldGeneration, resolve, rag.OpenSQLiteStoreReadOnly)
	if got != nil {
		_ = got.Close()
	}
	if !errors.Is(err, errNoActiveGeneration) {
		t.Fatalf("retry error = %v, want retired-generation sentinel", err)
	}
}

func TestRenderSourceDocumentsEscapesHumanControlsButPreservesJSON(t *testing.T) {
	const hostile = "title\n\t\x1b\x7f\u009b"
	document := rag.Document{
		ID: strings.Repeat("a", 32), Title: hostile, Kind: rag.DocumentKindFile, Origin: hostile,
		State: rag.DocumentStateFailed, Freshness: rag.DocumentFreshnessStale,
		ChunkCount: 1, UpdatedAt: 1700000000, LastError: "failed: " + hostile,
	}
	var summary bytes.Buffer
	if err := printSourceDocument(&summary, document); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(summary.String(), "\t\x1b\x7f\u009b") ||
		strings.Count(summary.String(), "\n") != 1 ||
		!strings.Contains(summary.String(), `\x1b`) {
		t.Fatalf("summary contains raw controls: %q", summary.String())
	}
	var human bytes.Buffer
	if err := renderSourceDocuments(&human, []rag.Document{document}, false); err != nil {
		t.Fatal(err)
	}
	rendered := human.String()
	if strings.ContainsAny(rendered, "\t\x1b\x7f\u009b") || strings.Count(rendered, "\n") != 3 {
		t.Fatalf("human output contains raw controls: %q", rendered)
	}
	for _, escaped := range []string{`\n`, `\t`, `\x1b`, `\x7f`, `\u009b`,
		"2023-11-14T22:13:20Z", "last error:", "failed", "stale"} {
		if !strings.Contains(rendered, escaped) {
			t.Fatalf("human output missing %q: %q", escaped, rendered)
		}
	}

	var machine bytes.Buffer
	if err := renderSourceDocuments(&machine, []rag.Document{document}, true); err != nil {
		t.Fatal(err)
	}
	var decoded []rag.Document
	if err := json.Unmarshal(machine.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].Title != hostile || decoded[0].Origin != hostile ||
		decoded[0].LastError != document.LastError || decoded[0].UpdatedAt != document.UpdatedAt ||
		decoded[0].State != document.State || decoded[0].Freshness != document.Freshness ||
		decoded[0].ChunkCount != document.ChunkCount {
		t.Fatalf("JSON changed document values: %#v", decoded)
	}
}

type sourceErrorWriter struct{ err error }

func (w sourceErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRenderSourceDocumentsPropagatesWriteErrors(t *testing.T) {
	writeErr := errors.New("write failed")
	docs := []rag.Document{{ID: strings.Repeat("a", 32), Kind: rag.DocumentKindText, Title: "notes"}}
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", asJSON), func(t *testing.T) {
			err := renderSourceDocuments(sourceErrorWriter{err: writeErr}, docs, asJSON)
			if !errors.Is(err, writeErr) {
				t.Fatalf("error = %v, want write failure", err)
			}
		})
	}
}

func hashTestFile(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

type fakeLister struct {
	pages [][]rag.Document
	errs  []error
	calls int
	after []string
}

func (f *fakeLister) ListDocuments(_ context.Context, filter rag.DocumentFilter) ([]rag.Document, error) {
	f.after = append(f.after, filter.AfterID)
	i := f.calls
	f.calls++
	return f.pages[i], f.errs[i]
}

func TestCollectSourceDocumentsPaginates(t *testing.T) {
	full := make([]rag.Document, rag.MaxManagedListLimit)
	for i := range full {
		full[i] = rag.Document{ID: fmt.Sprintf("%032d", i)}
	}
	scanErr := &rag.ManagedListScanLimitError{AfterID: "cursor-1", Scanned: 100}
	f := &fakeLister{
		pages: [][]rag.Document{full, {{ID: strings.Repeat("b", 32)}}, {}},
		errs:  []error{scanErr, nil, nil},
	}
	docs, err := collectSourceDocuments(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != rag.MaxManagedListLimit+1 {
		t.Fatalf("want %d docs, got %d", rag.MaxManagedListLimit+1, len(docs))
	}
	if f.after[1] != "cursor-1" {
		t.Fatalf("scan-limit cursor not resumed: %v", f.after)
	}
}

func TestCollectSourceDocumentsEmptyIsNonNil(t *testing.T) {
	f := &fakeLister{pages: [][]rag.Document{{}}, errs: []error{nil}}
	docs, err := collectSourceDocuments(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if docs == nil {
		t.Fatal("empty result must be non-nil so JSON renders [] not null")
	}
}

func TestCollectSourceDocumentsFullPageContinues(t *testing.T) {
	full := make([]rag.Document, rag.MaxManagedListLimit)
	for i := range full {
		full[i] = rag.Document{ID: fmt.Sprintf("%032d", i)}
	}
	f := &fakeLister{
		pages: [][]rag.Document{full, {}},
		errs:  []error{nil, nil},
	}
	docs, err := collectSourceDocuments(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != rag.MaxManagedListLimit {
		t.Fatalf("want %d docs, got %d", rag.MaxManagedListLimit, len(docs))
	}
	if f.after[1] != full[len(full)-1].ID {
		t.Fatalf("full-page cursor not advanced: %v", f.after)
	}
}
