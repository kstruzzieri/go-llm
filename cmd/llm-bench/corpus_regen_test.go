package main

// Corpus regeneration gates (#331 slice 3c, Task 8): both committed assembly
// corpora — the 3a flat/progressive corpus and the 3c mixed corpus — must be
// byte-for-byte what their committed fixtures rebuild to. The shared
// comparator here backs TestAssemblyCommittedCorpusUpToDate (assembly_test.go)
// and TestMixedCorpusRegeneration, and TestRegenGateDetectsDrift proves the
// comparator itself detects every drift mode (changed bytes, extra file,
// missing file) so the gates cannot silently pass on a comparison bug.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// corpusRepoRoot locates the module root (the directory holding go.mod), so
// corpus paths depend only on the test binary running somewhere inside the
// repo, not on a fixed ../.. depth from the package directory.
func corpusRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above the test working directory")
		}
		dir = parent
	}
}

// corpusDirDiff byte-compares a rebuilt corpus directory against the
// committed one and returns a description of the first difference in sorted
// filename order ("" when identical). The file SET is part of the contract: a
// file present on only one side is drift exactly like differing bytes —
// per-file comparison alone would bless an extra or vanished trace. The error
// return is I/O trouble, never drift.
func corpusDirDiff(rebuiltDir, committedDir string) (string, error) {
	rebuilt, err := corpusDirNames(rebuiltDir)
	if err != nil {
		return "", err
	}
	committed, err := corpusDirNames(committedDir)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(rebuilt)+len(committed))
	for n := range rebuilt {
		names = append(names, n)
	}
	for n := range committed {
		if _, ok := rebuilt[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		_, inRebuilt := rebuilt[name]
		_, inCommitted := committed[name]
		switch {
		case !inCommitted:
			return fmt.Sprintf("%s: rebuilt output has a file the committed corpus lacks", name), nil
		case !inRebuilt:
			return fmt.Sprintf("%s: committed corpus has a file the rebuild did not produce", name), nil
		}
		got, err := os.ReadFile(filepath.Join(rebuiltDir, name))
		if err != nil {
			return "", err
		}
		want, err := os.ReadFile(filepath.Join(committedDir, name))
		if err != nil {
			return "", err
		}
		if !bytes.Equal(got, want) {
			return fmt.Sprintf("%s: rebuilt bytes differ from the committed file", name), nil
		}
	}
	return "", nil
}

func corpusDirNames(dir string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		names[e.Name()] = struct{}{}
	}
	return names, nil
}

// TestMixedCorpusRegeneration is the mixed-corpus twin of
// TestAssemblyCommittedCorpusUpToDate (assembly_test.go): rebuild the
// committed mixed corpus from its committed fixture into a temp dir and
// byte-compare every artifact, manifest included. Until the fixture is
// authored (Task 10) this skips LOUDLY naming the task; every other failure —
// parse, validation, gate, drift — is a hard FAIL, never a skip.
//
// Regenerate after intentional changes with:
//
//	go run ./cmd/llm-bench -assembly-build docs/llm/assembly-corpus/mixed/mixed-cases.json \
//	  -assembly-out-mixed docs/llm/assembly-corpus/mixed/traces
func TestMixedCorpusRegeneration(t *testing.T) {
	root := corpusRepoRoot(t)
	fixture := filepath.Join(root, "docs", "llm", "assembly-corpus", "mixed", "mixed-cases.json")
	raw, err := os.ReadFile(fixture)
	if os.IsNotExist(err) {
		t.Skipf("mixed fixture not yet authored (Task 10): %v", err)
	}
	if err != nil {
		t.Fatalf("read mixed fixture: %v", err)
	}
	out := t.TempDir()
	if err := runMixedFixture(context.Background(), raw, out, io.Discard); err != nil {
		t.Fatalf("rebuild mixed corpus: %v", err)
	}
	diff, err := corpusDirDiff(out, filepath.Join(root, "docs", "llm", "assembly-corpus", "mixed", "traces"))
	if err != nil {
		t.Fatal(err)
	}
	if diff != "" {
		t.Fatalf("committed mixed corpus is stale; rebuild it with -assembly-build: %s", diff)
	}
}

// TestRegenGateDetectsDrift proves the comparator both regeneration gates
// stand on, against a real corpus build in temp dirs (the committed corpus is
// never touched): identical copies pass, and each simulated drift mode — a
// one-byte edit, an extra file, a missing file — is reported naming the
// drifted file.
func TestRegenGateDetectsDrift(t *testing.T) {
	root := corpusRepoRoot(t)
	dirA := t.TempDir()
	if err := assemblyBuild(context.Background(),
		filepath.Join(root, "docs", "llm", "assembly-corpus", "cases.json"), dirA); err != nil {
		t.Fatal(err)
	}
	names, err := corpusDirNames(dirA)
	if err != nil {
		t.Fatal(err)
	}
	target := ""
	for n := range names {
		if strings.HasSuffix(n, ".json") && (target == "" || n < target) {
			target = n
		}
	}
	if target == "" {
		t.Fatal("build produced no trace files")
	}

	copyDir := func(t *testing.T, src string) string {
		t.Helper()
		dst := t.TempDir()
		entries, err := os.ReadDir(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			raw, err := os.ReadFile(filepath.Join(src, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, e.Name()), raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dst
	}
	mustReport := func(t *testing.T, rebuilt, committed, wantFile string) {
		t.Helper()
		diff, err := corpusDirDiff(rebuilt, committed)
		if err != nil {
			t.Fatal(err)
		}
		if diff == "" {
			t.Fatalf("comparator reported no drift; want a difference naming %s", wantFile)
		}
		if !strings.Contains(diff, wantFile) {
			t.Errorf("drift report %q does not name the drifted file %s", diff, wantFile)
		}
	}

	t.Run("identical copies pass", func(t *testing.T) {
		diff, err := corpusDirDiff(dirA, copyDir(t, dirA))
		if err != nil {
			t.Fatal(err)
		}
		if diff != "" {
			t.Fatalf("identical corpus copies reported as drifted: %s", diff)
		}
	})
	t.Run("one-byte edit detected", func(t *testing.T) {
		dirB := copyDir(t, dirA)
		path := filepath.Join(dirB, target)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)/2] ^= 0xff
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		mustReport(t, dirA, dirB, target)
	})
	t.Run("extra rebuilt file detected", func(t *testing.T) {
		mut := copyDir(t, dirA)
		if err := os.WriteFile(filepath.Join(mut, "zz-extra-flat.json"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustReport(t, mut, dirA, "zz-extra-flat.json")
	})
	t.Run("missing rebuilt file detected", func(t *testing.T) {
		mut := copyDir(t, dirA)
		if err := os.Remove(filepath.Join(mut, target)); err != nil {
			t.Fatal(err)
		}
		mustReport(t, mut, dirA, target)
	})
}
