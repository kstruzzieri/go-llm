package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func writeSeed(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEditFileSingleMatch(t *testing.T) {
	root := t.TempDir()
	writeSeed(t, root, "a.txt", "alpha\nbravo\ncharlie\n")
	jr := &recJournal{}
	ef := NewEditFile(mustWorkspace(t, root), jr)

	_, res := planThenInvoke(t, ef, map[string]any{
		"path": "a.txt", "old_string": "bravo", "new_string": "BRAVO",
	})
	if res.IsError {
		t.Fatalf("edit errored: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "alpha\nBRAVO\ncharlie\n" {
		t.Fatalf("content = %q", got)
	}
	if len(jr.recs) != 1 || !jr.recs[0].Existed {
		t.Fatalf("journal record wrong: %+v", jr.recs)
	}
}

func TestEditFileZeroMatch(t *testing.T) {
	root := t.TempDir()
	writeSeed(t, root, "a.txt", "alpha\n")
	ef := NewEditFile(mustWorkspace(t, root), nil)
	raw, _ := json.Marshal(map[string]any{"path": "a.txt", "old_string": "zzz", "new_string": "q"})
	plan, _ := ef.Plan(context.Background(), raw)
	if plan.Preview != "" {
		t.Fatalf("zero-match must not produce an approvable preview: %q", plan.Preview)
	}
	res, _ := ef.Invoke(context.Background(), raw)
	if !res.IsError {
		t.Fatal("zero-match invoke must be IsError")
	}
}

func TestEditFileAmbiguousMatch(t *testing.T) {
	root := t.TempDir()
	writeSeed(t, root, "a.txt", "x\nx\n")
	ef := NewEditFile(mustWorkspace(t, root), nil)
	raw, _ := json.Marshal(map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y"})
	plan, _ := ef.Plan(context.Background(), raw)
	if plan.Preview != "" {
		t.Fatalf("ambiguous match must not produce a preview: %q", plan.Preview)
	}
	res, _ := ef.Invoke(context.Background(), raw)
	if !res.IsError || !strings.Contains(res.Content, "ambiguous") {
		t.Fatalf("ambiguous invoke must fail: %+v", res)
	}
}

func TestEditFileEmptyNewStringDeletes(t *testing.T) {
	root := t.TempDir()
	writeSeed(t, root, "a.txt", "keep\nDROP\n")
	ef := NewEditFile(mustWorkspace(t, root), nil)
	_, res := planThenInvoke(t, ef, map[string]any{
		"path": "a.txt", "old_string": "DROP\n", "new_string": "",
	})
	if res.IsError {
		t.Fatalf("deletion edit errored: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "keep\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestEditFileMissingFile(t *testing.T) {
	root := t.TempDir()
	ef := NewEditFile(mustWorkspace(t, root), nil)
	res := invoke(t, ef, map[string]any{"path": "nope.txt", "old_string": "a", "new_string": "b"})
	if !res.IsError {
		t.Fatal("editing a missing file must be IsError")
	}
}

func TestEditFileBinaryRejected(t *testing.T) {
	root := t.TempDir()
	writeSeed(t, root, "bin", "a\x00b")
	ef := NewEditFile(mustWorkspace(t, root), nil)
	res := invoke(t, ef, map[string]any{"path": "bin", "old_string": "a", "new_string": "b"})
	if !res.IsError {
		t.Fatal("binary file edit must be IsError")
	}
}

func TestEditFileChangedSincePreviewFails(t *testing.T) {
	root := t.TempDir()
	writeSeed(t, root, "a.txt", "one\ntwo\n")
	ef := NewEditFile(mustWorkspace(t, root), nil)
	raw, _ := json.Marshal(map[string]any{"path": "a.txt", "old_string": "two", "new_string": "TWO"})
	if _, err := ef.Plan(context.Background(), raw); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	writeSeed(t, root, "a.txt", "one\ntwo\nthree\n") // mutated after Plan
	res, _ := ef.Invoke(context.Background(), raw)
	if !res.IsError || !strings.Contains(res.Content, "changed since preview") {
		t.Fatalf("hash mismatch must fail: %+v", res)
	}
}

func TestEditFileEffect(t *testing.T) {
	ef := NewEditFile(mustWorkspace(t, t.TempDir()), nil)
	e := ef.Effect()
	if e.Class != agent.Write || e.Approval != agent.ApprovalOnWrite {
		t.Fatalf("Effect = %+v, want Write/ApprovalOnWrite", e)
	}
}
