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

// recJournal is a test Journal capturing records.
type recJournal struct{ recs []MutationRecord }

func (r *recJournal) Record(m MutationRecord) { r.recs = append(r.recs, m) }

// planThenInvoke runs Plan then Invoke with the SAME raw args, mirroring dispatch.
func planThenInvoke(t *testing.T, tool agent.Tool, args any) (agent.ToolPlan, agent.ToolResult) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	pt, ok := tool.(agent.PlanningTool)
	if !ok {
		t.Fatal("tool is not a PlanningTool")
	}
	plan, perr := pt.Plan(context.Background(), raw)
	if perr != nil {
		t.Fatalf("Plan: %v", perr)
	}
	res, ierr := tool.Invoke(context.Background(), raw)
	if ierr != nil {
		t.Fatalf("Invoke returned a Go error: %v", ierr)
	}
	return plan, res
}

func TestWriteFileCreate(t *testing.T) {
	root := t.TempDir()
	jr := &recJournal{}
	wf := NewWriteFile(mustWorkspace(t, root), jr)

	plan, res := planThenInvoke(t, wf, map[string]any{"path": "new.txt", "content": "hello\n"})
	if res.IsError {
		t.Fatalf("create errored: %s", res.Content)
	}
	if !strings.Contains(plan.Preview, "new file: new.txt") {
		t.Fatalf("plan preview wrong: %s", plan.Preview)
	}
	got, _ := os.ReadFile(filepath.Join(root, "new.txt"))
	if string(got) != "hello\n" {
		t.Fatalf("file content = %q", got)
	}
	if len(jr.recs) != 1 || jr.recs[0].Existed || jr.recs[0].AfterHash != contentHash([]byte("hello\n")) {
		t.Fatalf("journal record wrong: %+v", jr.recs)
	}
}

func TestWriteFileOverwriteRecordsPrior(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("OLD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jr := &recJournal{}
	wf := NewWriteFile(mustWorkspace(t, root), jr)

	_, res := planThenInvoke(t, wf, map[string]any{"path": "a.txt", "content": "NEW\n"})
	if res.IsError {
		t.Fatalf("overwrite errored: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "NEW\n" {
		t.Fatalf("content = %q", got)
	}
	if len(jr.recs) != 1 || !jr.recs[0].Existed || string(jr.recs[0].PriorContent) != "OLD\n" {
		t.Fatalf("journal must capture prior bytes: %+v", jr.recs)
	}
}

func TestWriteFilePlanDoesNotMutate(t *testing.T) {
	root := t.TempDir()
	wf := NewWriteFile(mustWorkspace(t, root), nil)
	raw, _ := json.Marshal(map[string]any{"path": "x.txt", "content": "data"})
	if _, err := wf.Plan(context.Background(), raw); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); !os.IsNotExist(err) {
		t.Fatal("Plan must not create the file")
	}
}

func TestWriteFileMissingPendingPlanFails(t *testing.T) {
	root := t.TempDir()
	wf := NewWriteFile(mustWorkspace(t, root), nil)
	res := invoke(t, wf, map[string]any{"path": "x.txt", "content": "data"})
	if !res.IsError || !strings.Contains(res.Content, "preview missing") {
		t.Fatalf("invoke without plan must fail: %+v", res)
	}
}

func TestWriteFilePreviewStateChangedFails(t *testing.T) {
	root := t.TempDir()
	wf := NewWriteFile(mustWorkspace(t, root), nil)
	raw, _ := json.Marshal(map[string]any{"path": "a.txt", "content": "NEW"})
	if _, err := wf.Plan(context.Background(), raw); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("SNUCK"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := wf.Invoke(context.Background(), raw)
	if !res.IsError || !strings.Contains(res.Content, "changed since preview") {
		t.Fatalf("preview-state mismatch must fail: %+v", res)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "SNUCK" {
		t.Fatalf("failed write must not clobber: %q", got)
	}
}

func TestWriteFileSizeCap(t *testing.T) {
	root := t.TempDir()
	wf := NewWriteFile(mustWorkspace(t, root), nil)
	big := strings.Repeat("x", mutateMaxBytes+1)
	raw, _ := json.Marshal(map[string]any{"path": "big.txt", "content": big})
	plan, err := wf.Plan(context.Background(), raw)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Preview != "" {
		t.Fatalf("oversize Plan should not produce an approvable preview: %q", plan.Preview)
	}
	res, _ := wf.Invoke(context.Background(), raw)
	if !res.IsError || !strings.Contains(res.Content, "preview missing") {
		t.Fatalf("oversize write must fail closed via missing plan: %+v", res)
	}
}

func TestWriteFileEffect(t *testing.T) {
	wf := NewWriteFile(mustWorkspace(t, t.TempDir()), nil)
	e := wf.Effect()
	if e.Class != agent.Write || e.Approval != agent.ApprovalOnWrite {
		t.Fatalf("Effect = %+v, want Write/ApprovalOnWrite", e)
	}
}

func TestWriteFileParentDirMissing(t *testing.T) {
	wf := NewWriteFile(mustWorkspace(t, t.TempDir()), nil)
	raw, _ := json.Marshal(map[string]any{"path": "nosuchdir/file.txt", "content": "x"})
	plan, err := wf.Plan(context.Background(), raw)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Preview != "" {
		t.Fatalf("missing parent must not produce an approvable preview: %q", plan.Preview)
	}
	res, _ := wf.Invoke(context.Background(), raw)
	if !res.IsError {
		t.Fatal("Invoke with no plan must error")
	}
}

func TestWriteFileDeletedBetweenPlanAndInvoke(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("OLD\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wf := NewWriteFile(mustWorkspace(t, root), nil)
	raw, _ := json.Marshal(map[string]any{"path": "a.txt", "content": "NEW\n"})
	if _, err := wf.Plan(context.Background(), raw); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatal(err)
	}
	res, _ := wf.Invoke(context.Background(), raw)
	if !res.IsError || !strings.Contains(res.Content, "changed since preview") {
		t.Fatalf("deletion between plan and invoke must fail: %+v", res)
	}
}
