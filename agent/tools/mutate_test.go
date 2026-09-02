package tools

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestContentHashDistinctFromAbsent(t *testing.T) {
	if ContentHash([]byte("")) == absentHash {
		t.Fatal("hash of empty content must differ from the absent sentinel")
	}
	if ContentHash([]byte("a")) == ContentHash([]byte("b")) {
		t.Fatal("different content must hash differently")
	}
	h1 := ContentHash([]byte("a"))
	h2 := ContentHash([]byte("a"))
	if h1 != h2 {
		t.Fatal("hash must be stable")
	}
}

func TestUnifiedDiffNewFile(t *testing.T) {
	d := unifiedDiff("a.txt", nil, []byte("one\ntwo\n"), false)
	if !strings.Contains(d, "new file: a.txt") || !strings.Contains(d, "+one") || !strings.Contains(d, "+two") {
		t.Fatalf("new-file diff wrong:\n%s", d)
	}
}

func TestUnifiedDiffEmpty(t *testing.T) {
	d := unifiedDiff("a.txt", []byte("one\ntwo\n"), nil, true)
	if !strings.Contains(d, "empty file: a.txt") || !strings.Contains(d, "-one") {
		t.Fatalf("empty diff wrong:\n%s", d)
	}
}

func TestUnifiedDiffChangeTrimsCommon(t *testing.T) {
	before := []byte("ctx1\nold\nctx2\n")
	after := []byte("ctx1\nnew\nctx2\n")
	d := unifiedDiff("a.txt", before, after, true)
	if !strings.Contains(d, "-old") || !strings.Contains(d, "+new") {
		t.Fatalf("change diff missing -/+ lines:\n%s", d)
	}
	if strings.Contains(d, "-ctx1") || strings.Contains(d, "+ctx1") {
		t.Fatalf("common prefix must not be marked changed:\n%s", d)
	}
}

func TestPendingPlanStoreConsumeByArgsHash(t *testing.T) {
	var base mutatingBase
	pp := pendingPlan{path: "a.txt", afterContent: []byte("x")}
	base.store("HASH1", pp)
	if _, ok := base.consume("WRONG"); ok {
		t.Fatal("consume with wrong hash must fail")
	}
	got, ok := base.consume("HASH1")
	if !ok || got.path != "a.txt" {
		t.Fatalf("consume HASH1: ok=%v got=%+v", ok, got)
	}
	if _, ok := base.consume("HASH1"); ok {
		t.Fatal("second consume must fail (plan cleared)")
	}
}

func TestUnifiedDiffIdenticalNoChangeLines(t *testing.T) {
	d := unifiedDiff("a.txt", []byte("x\ny\n"), []byte("x\ny\n"), true)
	for _, ln := range strings.Split(d, "\n") {
		if strings.HasPrefix(ln, "+") || strings.HasPrefix(ln, "-") {
			// the +++/--- header lines are allowed; only single +/- change lines are not
			if !strings.HasPrefix(ln, "+++") && !strings.HasPrefix(ln, "---") {
				t.Fatalf("identical inputs produced a change line: %q\nfull:\n%s", ln, d)
			}
		}
	}
}

func TestNewMutatingTools(t *testing.T) {
	ws := mustWorkspace(t, t.TempDir())
	tools := NewMutatingTools(ws, nil)
	want := map[string]bool{"write_file": false, "edit_file": false}
	for _, tl := range tools {
		name := tl.Spec().Name
		if _, ok := want[name]; !ok {
			t.Fatalf("unexpected tool %q", name)
		}
		want[name] = true
		if e := tl.Effect(); e.Class != agent.Write || e.Approval != agent.ApprovalOnWrite {
			t.Fatalf("%q Effect = %+v, want Write/ApprovalOnWrite", name, e)
		}
		if _, ok := tl.(agent.PlanningTool); !ok {
			t.Fatalf("%q must implement PlanningTool", name)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("missing %q", name)
		}
	}
}

// --- #443 Task 8: tracked after-mode validation ---

func TestMutationRecordTrackedModeValidation(t *testing.T) {
	base := MutationRecord{Path: "a.txt", AfterHash: ContentHash([]byte("x"))}
	cases := []struct {
		name    string
		mut     func(*MutationRecord)
		wantErr bool
	}{
		{"legacy zero-value ok", func(r *MutationRecord) {}, false},
		{"tracked create rwx ok", func(r *MutationRecord) { r.TrackedMode = true; r.AfterMode = 0o640 }, false},
		{"after-mode without tracking", func(r *MutationRecord) { r.AfterMode = 0o640 }, true},
		{"tracked on update", func(r *MutationRecord) { r.TrackedMode = true; r.AfterMode = 0o640; r.Existed = true }, true},
		{"tracked with setuid", func(r *MutationRecord) { r.TrackedMode = true; r.AfterMode = 0o640 | fs.ModeSetuid }, true},
		{"tracked with sticky", func(r *MutationRecord) { r.TrackedMode = true; r.AfterMode = 0o640 | fs.ModeSticky }, true},
		{"tracked with type bit", func(r *MutationRecord) { r.TrackedMode = true; r.AfterMode = 0o640 | fs.ModeDir }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := base
			tc.mut(&rec)
			wrote := false
			toolErr, internalErr := runJournaledWrite(context.Background(), nil, rec, func() error {
				wrote = true
				return nil
			})
			if internalErr != nil {
				t.Fatalf("internal error: %v", internalErr)
			}
			if tc.wantErr {
				if toolErr == nil {
					t.Fatal("invalid tracked-mode record must be rejected before the write")
				}
				if wrote {
					t.Fatal("rejected record must not write")
				}
				return
			}
			if toolErr != nil {
				t.Fatalf("valid record rejected: %v", toolErr)
			}
			if !wrote {
				t.Fatal("valid record must write")
			}
		})
	}
}

func TestReadFileWithModeForUndo(t *testing.T) {
	root := t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}
	data, mode, err := ws.ReadFileWithModeForUndo("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "data" || mode.Perm() != 0o640 || mode&fs.ModeType != 0 {
		t.Fatalf("data=%q mode=%v", data, mode)
	}
	// Same containment semantics as ReadFileForUndo: a symlink is refused.
	if err := os.Symlink("f.txt", filepath.Join(root, "ln")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ws.ReadFileWithModeForUndo("ln"); err == nil {
		t.Fatal("symlink must be refused")
	}
	if _, _, err := ws.ReadFileWithModeForUndo("missing.txt"); !os.IsNotExist(err) {
		t.Fatalf("missing file must report IsNotExist, got %v", err)
	}
}
