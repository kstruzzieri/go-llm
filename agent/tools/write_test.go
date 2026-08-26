package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// recJournal is a test Journal capturing records.
type recJournal struct{ recs []MutationRecord }

func (r *recJournal) Record(m MutationRecord) { r.recs = append(r.recs, m) }

// prepJournal is a PreparingJournal test double recording protocol events in
// order, with injectable failures for each protocol step.
type prepJournal struct {
	events     []string
	recs       []MutationRecord
	prepared   []MutationRecord
	prepareErr error
	commitErr  error
	abortErr   error
	onPrepare  func(MutationRecord)
}

func (p *prepJournal) Record(m MutationRecord) {
	p.events = append(p.events, "record")
	p.recs = append(p.recs, m)
}

func (p *prepJournal) Prepare(m MutationRecord) (PreparedMutation, error) {
	p.events = append(p.events, "prepare")
	if p.onPrepare != nil {
		p.onPrepare(m)
	}
	if p.prepareErr != nil {
		return nil, p.prepareErr
	}
	p.prepared = append(p.prepared, m)
	return &prepHandle{p: p}, nil
}

type prepHandle struct{ p *prepJournal }

func (h *prepHandle) Commit() error {
	h.p.events = append(h.p.events, "commit")
	return h.p.commitErr
}

func (h *prepHandle) Abort() error {
	h.p.events = append(h.p.events, "abort")
	return h.p.abortErr
}

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
	if len(jr.recs) != 1 || jr.recs[0].Existed || jr.recs[0].AfterHash != ContentHash([]byte("hello\n")) {
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
	if _, err := wf.Plan(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversize Plan error = %v, want size limit", err)
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
	if _, err := wf.Plan(context.Background(), raw); err == nil {
		t.Fatal("missing parent must fail during Plan")
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

func TestWriteFileRejectsNulContent(t *testing.T) {
	root := t.TempDir()
	wf := NewWriteFile(mustWorkspace(t, root), nil)
	raw, _ := json.Marshal(map[string]any{"path": "bin.txt", "content": "a\x00b"})
	if _, err := wf.Plan(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("NUL content Plan error = %v, want NUL", err)
	}
	res, _ := wf.Invoke(context.Background(), raw)
	if !res.IsError {
		t.Fatal("NUL content write must fail")
	}
	if _, err := os.Stat(filepath.Join(root, "bin.txt")); !os.IsNotExist(err) {
		t.Fatal("rejected NUL write must not create the file")
	}
}

func TestWriteFileRejectsBinaryPrior(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bin.txt"), []byte("a\x00b"), 0o600); err != nil {
		t.Fatal(err)
	}
	wf := NewWriteFile(mustWorkspace(t, root), nil)
	raw, _ := json.Marshal(map[string]any{"path": "bin.txt", "content": "text"})
	if _, err := wf.Plan(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary prior Plan error = %v, want binary", err)
	}
}

func TestWriteFilePreparingJournalOrder(t *testing.T) {
	root := t.TempDir()
	pj := &prepJournal{}
	var existsAtPrepare bool
	pj.onPrepare = func(MutationRecord) {
		_, err := os.Stat(filepath.Join(root, "new.txt"))
		existsAtPrepare = err == nil
	}
	wf := NewWriteFile(mustWorkspace(t, root), pj)

	_, res := planThenInvoke(t, wf, map[string]any{"path": "new.txt", "content": "hello\n"})
	if res.IsError {
		t.Fatalf("write errored: %s", res.Content)
	}
	if existsAtPrepare {
		t.Fatal("file existed at Prepare time; the intent must precede the workspace write")
	}
	if want := []string{"prepare", "commit"}; !slices.Equal(pj.events, want) {
		t.Fatalf("events = %v, want %v", pj.events, want)
	}
	if len(pj.prepared) != 1 || pj.prepared[0].Existed || pj.prepared[0].AfterHash != ContentHash([]byte("hello\n")) {
		t.Fatalf("prepared record wrong: %+v", pj.prepared)
	}
	got, _ := os.ReadFile(filepath.Join(root, "new.txt"))
	if string(got) != "hello\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestPreparingJournalFailureLeavesWorkspaceUntouched(t *testing.T) {
	root := t.TempDir()
	pj := &prepJournal{prepareErr: errors.New("store down")}
	wf := NewWriteFile(mustWorkspace(t, root), pj)

	_, res := planThenInvoke(t, wf, map[string]any{"path": "new.txt", "content": "hello\n"})
	if !res.IsError {
		t.Fatal("want tool error when prepare fails")
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("workspace touched despite prepare failure: %v", err)
	}
	if want := []string{"prepare"}; !slices.Equal(pj.events, want) {
		t.Fatalf("events = %v, want %v", pj.events, want)
	}
}

func TestRunJournaledWriteAbortsOnWriteFailure(t *testing.T) {
	pj := &prepJournal{}
	writeErr := errors.New("disk full")
	toolErr, internalErr := runJournaledWrite(pj, MutationRecord{Path: "a"}, func() error { return writeErr })
	if internalErr != nil {
		t.Fatalf("internal = %v, want nil (a failed write is model-visible)", internalErr)
	}
	if !errors.Is(toolErr, writeErr) {
		t.Fatalf("toolErr = %v, want the write error", toolErr)
	}
	if want := []string{"prepare", "abort"}; !slices.Equal(pj.events, want) {
		t.Fatalf("events = %v, want %v", pj.events, want)
	}
}

func TestRunJournaledWriteJoinsAbortFailure(t *testing.T) {
	pj := &prepJournal{abortErr: errors.New("abort broke")}
	writeErr := errors.New("disk full")
	toolErr, internalErr := runJournaledWrite(pj, MutationRecord{Path: "a"}, func() error { return writeErr })
	if internalErr != nil {
		t.Fatalf("internal = %v, want nil", internalErr)
	}
	if !errors.Is(toolErr, writeErr) || !strings.Contains(toolErr.Error(), "abort broke") {
		t.Fatalf("toolErr = %v, want write and abort errors joined", toolErr)
	}
}

func TestWriteFilePreparingJournalAbortsOnWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("write-failure injection via chmod is a no-op for root")
	}
	root := t.TempDir()
	pj := &prepJournal{}
	wf := NewWriteFile(mustWorkspace(t, root), pj)
	raw, err := json.Marshal(map[string]any{"path": "new.txt", "content": "hello\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Plan(context.Background(), raw); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	res, ierr := wf.Invoke(context.Background(), raw)
	if ierr != nil {
		t.Fatalf("Invoke internal error: %v", ierr)
	}
	if !res.IsError {
		t.Fatal("want tool error for failed workspace write")
	}
	if want := []string{"prepare", "abort"}; !slices.Equal(pj.events, want) {
		t.Fatalf("events = %v, want %v", pj.events, want)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("file created despite write failure")
	}
}

func TestWriteFilePreparingJournalCommitErrorIsInternal(t *testing.T) {
	root := t.TempDir()
	pj := &prepJournal{commitErr: errors.New("wal broke")}
	wf := NewWriteFile(mustWorkspace(t, root), pj)
	raw, err := json.Marshal(map[string]any{"path": "new.txt", "content": "hello\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.Plan(context.Background(), raw); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	_, ierr := wf.Invoke(context.Background(), raw)
	if ierr == nil {
		t.Fatal("want internal invocation error: commit failed after the file changed")
	}
	if !strings.Contains(ierr.Error(), "wal broke") {
		t.Fatalf("internal error = %v", ierr)
	}
	got, rerr := os.ReadFile(filepath.Join(root, "new.txt"))
	if rerr != nil || string(got) != "hello\n" {
		t.Fatalf("file = %q,%v — the write landed before commit failed", got, rerr)
	}
}

func TestPlainJournalRecordsAfterWrite(t *testing.T) {
	root := t.TempDir()
	jr := &recJournal{}
	wf := NewWriteFile(mustWorkspace(t, root), jr)
	_, res := planThenInvoke(t, wf, map[string]any{"path": "p.txt", "content": "x\n"})
	if res.IsError || res.Content != "write p.txt" || res.Preview != "write p.txt" {
		t.Fatalf("result = %+v", res)
	}
	if len(jr.recs) != 1 {
		t.Fatalf("records = %d, want exactly 1 post-success Record", len(jr.recs))
	}
}
