package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// recordingJournal captures every record that flows through the preparing
// path and can be told to fail at Prepare or Commit.
type recordingJournal struct {
	records    []MutationRecord
	prepareErr error
	commitErr  error
}

func (j *recordingJournal) Record(rec MutationRecord) { j.records = append(j.records, rec) }
func (j *recordingJournal) Prepare(rec MutationRecord) (PreparedMutation, error) {
	if j.prepareErr != nil {
		return nil, j.prepareErr
	}
	return &recordingPrepared{j: j, rec: rec}, nil
}

type recordingPrepared struct {
	j   *recordingJournal
	rec MutationRecord
}

func (p *recordingPrepared) Commit() error {
	if p.j.commitErr != nil {
		return p.j.commitErr
	}
	p.j.records = append(p.j.records, p.rec)
	return nil
}
func (p *recordingPrepared) Abort() error { return nil }

// promoteFixture builds a canonical workspace, a promotion-enabled runtime,
// and one captured outcome containing dir/new.txt (promotable) plus
// binary.bin (report-only).
type promoteFixture struct {
	root    string
	ws      *Workspace
	rt      *scratchRuntime
	journal *recordingJournal
	tool    *PromoteArtifact
	id      string
	content []byte
}

func newPromoteFixture(t *testing.T) *promoteFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir/existing.txt"), []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal := &recordingJournal{}
	rt, err := newScratchRuntime(root, ScratchConfig{Enabled: true}, journal)
	if err != nil {
		t.Fatal(err)
	}
	rt.tempBase = t.TempDir()
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	spec := execSpec{Path: "/bin/sh", Argv: []string{"sh"}, Dir: rt.root, Env: []string{"PATH=/usr/bin:/bin"}, WorkspaceRoot: rt.root}
	session, _, err := beginScratchSession(context.Background(), rt, spec)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("hello\nworld\n")
	if err := os.WriteFile(filepath.Join(session.work, "dir/new.txt"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.work, "binary.bin"), []byte{0xff, 0x00, 0x01}, 0o644); err != nil {
		t.Fatal(err)
	}
	session.finish(context.Background())
	if _, status := rt.store.get(session.id); status != scratchStatusCaptured {
		t.Fatal("fixture capture failed")
	}
	return &promoteFixture{
		root: root, ws: ws, rt: rt, journal: journal,
		tool: NewPromoteArtifact(ws, rt), id: session.id, content: content,
	}
}

func promoteArgs(id, path string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"id": id, "path": path})
	return raw
}

func TestPromoteArtifactEffectAndKey(t *testing.T) {
	f := newPromoteFixture(t)
	e := f.tool.Effect()
	if e.Class != agent.Write || e.Approval != agent.ApprovalAlways {
		t.Fatalf("promote_artifact must be Write/ApprovalAlways: %+v", e)
	}
	plan, err := f.tool.Plan(context.Background(), promoteArgs(f.id, "dir/new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.ApprovalKey != "" {
		t.Fatalf("promotion must be structurally ungrantable, key=%q", plan.ApprovalKey)
	}
	for _, want := range []string{`"dir/new.txt"`, "size=12", "mode=0640", `+ "hello\n"`, `+ "world\n"`} {
		if !strings.Contains(plan.Preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, plan.Preview)
		}
	}
}

func TestPromoteArtifactPlanRefusals(t *testing.T) {
	f := newPromoteFixture(t)
	cases := []struct {
		name string
		raw  json.RawMessage
	}{
		{"unknown id", promoteArgs("scr-ffffffffffffffffffffffffffffffff", "dir/new.txt")},
		{"missing path", promoteArgs(f.id, "")},
		{"absent path", promoteArgs(f.id, "nope.txt")},
		{"non-promotable path", promoteArgs(f.id, "binary.bin")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.tool.Plan(context.Background(), tc.raw); err == nil {
				t.Fatal("Plan must refuse")
			}
		})
	}
	// Pending session id: refuse (not yet captured).
	f.rt.store.beginPending("scr-11111111111111111111111111111111")
	if _, err := f.tool.Plan(context.Background(), promoteArgs("scr-11111111111111111111111111111111", "x")); err == nil {
		t.Fatal("pending outcome must refuse")
	}
	// Truncated outcome: nothing promotable.
	f.rt.store.beginPending("scr-22222222222222222222222222222222")
	f.rt.store.completePending("scr-22222222222222222222222222222222", scratchOutcome{
		truncated: true,
		changes:   []scratchChange{{path: "t.txt", kind: scratchChangeCreate}},
	})
	if _, err := f.tool.Plan(context.Background(), promoteArgs("scr-22222222222222222222222222222222", "t.txt")); err == nil {
		t.Fatal("truncated outcome must refuse")
	}
}

func TestPromoteArtifactCreateLandsExactly(t *testing.T) {
	f := newPromoteFixture(t)
	raw := promoteArgs(f.id, "dir/new.txt")
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err := f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("promotion failed: %s", res.Content)
	}
	p := filepath.Join(f.root, "dir/new.txt")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(f.content) {
		t.Fatalf("promoted bytes = %q", data)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 || !fi.Mode().IsRegular() {
		t.Fatalf("promoted mode = %v", fi.Mode())
	}
	entries, err := os.ReadDir(filepath.Join(f.root, "dir"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), scratchPromoteTempPrefix) {
			t.Fatalf("successful promotion left staging residue %q", entry.Name())
		}
	}
	if len(f.journal.records) != 1 {
		t.Fatalf("journal records = %d, want 1", len(f.journal.records))
	}
	rec := f.journal.records[0]
	if rec.Existed || rec.PriorContent != nil || rec.AfterHash != ContentHash(f.content) ||
		!rec.TrackedMode || rec.AfterMode != 0o640 || rec.Path != "dir/new.txt" {
		t.Fatalf("journal record wrong: %+v", rec)
	}
	// The path is consumed: a second promotion of the same artifact refuses.
	if _, err := f.tool.Plan(context.Background(), raw); err == nil {
		res, err := f.tool.Invoke(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatal("re-promotion of a consumed path must refuse")
		}
	}
}

func TestPromoteArtifactDirectInvokeRefused(t *testing.T) {
	f := newPromoteFixture(t)
	res, err := f.tool.Invoke(context.Background(), promoteArgs(f.id, "dir/new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("Invoke without a consumed Plan must refuse")
	}
	if _, err := os.Lstat(filepath.Join(f.root, "dir/new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused invoke must not write")
	}
}

func TestPromoteArtifactStoreDriftRefused(t *testing.T) {
	f := newPromoteFixture(t)
	raw := promoteArgs(f.id, "dir/new.txt")
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	f.rt.store.delete(f.id)
	res, err := f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("store drift between Plan and Invoke must refuse")
	}
	if _, err := os.Lstat(filepath.Join(f.root, "dir/new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("drift refusal must not write")
	}
}

func TestPromoteArtifactDestinationCollisionRefused(t *testing.T) {
	f := newPromoteFixture(t)
	raw := promoteArgs(f.id, "dir/new.txt")
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	// The destination appears after capture: never overwrite it.
	if err := os.WriteFile(filepath.Join(f.root, "dir/new.txt"), []byte("user content"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("existing destination must refuse")
	}
	data, err := os.ReadFile(filepath.Join(f.root, "dir/new.txt"))
	if err != nil || string(data) != "user content" {
		t.Fatalf("collision refusal must preserve the destination: %q err=%v", data, err)
	}
}

func TestPromoteArtifactParentReplacementRefused(t *testing.T) {
	f := newPromoteFixture(t)
	raw := promoteArgs(f.id, "dir/new.txt")
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	// Replace the parent via sibling-first rename (never remove+recreate:
	// inode reuse would blind identity checks — the recorded lesson).
	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.Mkdir(replacement, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.root, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(f.root, "dir")); err != nil {
		t.Fatal(err)
	}
	res, err := f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("replaced parent must refuse")
	}
	if _, err := os.Lstat(filepath.Join(f.root, "dir/new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("replaced-parent refusal must not write")
	}
}

func TestPromoteArtifactParentModeDriftRefused(t *testing.T) {
	f := newPromoteFixture(t)
	raw := promoteArgs(f.id, "dir/new.txt")
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(f.root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	res, err := f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("parent full-mode drift must refuse")
	}
}

func TestPromoteArtifactPrepareFailureIsRetryable(t *testing.T) {
	f := newPromoteFixture(t)
	raw := promoteArgs(f.id, "dir/new.txt")
	f.journal.prepareErr = fmt.Errorf("prepare refused")
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err := f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("prepare failure must surface")
	}
	if _, err := os.Lstat(filepath.Join(f.root, "dir/new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("prepare failure must not write")
	}
	// The claim was released: a healthy retry succeeds.
	f.journal.prepareErr = nil
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err = f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("retry after prepare failure must succeed: %s", res.Content)
	}
}

func TestPromoteArtifactCommitFailureIsIndeterminate(t *testing.T) {
	f := newPromoteFixture(t)
	raw := promoteArgs(f.id, "dir/new.txt")
	f.journal.commitErr = fmt.Errorf("commit exploded")
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err := f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "indeterminate") {
		t.Fatalf("commit failure must be indeterminate: %+v", res)
	}
	// The filesystem write landed; the path is consumed and never retried
	// automatically.
	if _, err := os.Lstat(filepath.Join(f.root, "dir/new.txt")); err != nil {
		t.Fatal("filesystem commit happened before the journal failure")
	}
	f.journal.commitErr = nil
	// Even after the landed file is removed (a later /undo or manual repair
	// — the destination-collision guard no longer blocks), the consumed
	// marker alone must refuse the retry. This is the distinguishing input
	// for a mutant that releases instead of consuming on indeterminate.
	if err := os.Remove(filepath.Join(f.root, "dir/new.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err = f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("an indeterminate path must never be retryable")
	}
	if _, err := os.Lstat(filepath.Join(f.root, "dir/new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused retry must not rewrite the file")
	}
}

func TestPromoteArtifactPostInstallFailureIsIndeterminate(t *testing.T) {
	f := newPromoteFixture(t)
	realInstall := f.tool.install
	f.tool.install = func(root string, change scratchChange) (bool, error) {
		installed, err := realInstall(root, change)
		if err != nil {
			return installed, err
		}
		return true, errors.New("sync parent failed")
	}
	raw := promoteArgs(f.id, "dir/new.txt")
	if _, err := f.tool.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	res, err := f.tool.Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "indeterminate") {
		t.Fatalf("post-install failure must be indeterminate: %+v", res)
	}
	if len(f.journal.records) != 1 {
		t.Fatalf("landed promotion must commit its journal intent, got %d records", len(f.journal.records))
	}
	if _, err := os.Lstat(filepath.Join(f.root, "dir/new.txt")); err != nil {
		t.Fatal("filesystem commit must remain visible")
	}
}

func TestPromoteArtifactFactoryRegistration(t *testing.T) {
	root := t.TempDir()
	tools, err := NewExecToolsWithOptions(root, ExecToolsOptions{Scratch: ScratchConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Spec().Name == "promote_artifact" {
			t.Fatal("promotion must not register without a journal")
		}
	}
	tools, err = NewExecToolsWithOptions(root, ExecToolsOptions{
		Scratch:          ScratchConfig{Enabled: true},
		PromotionJournal: &recordingJournal{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var pa *PromoteArtifact
	var rc *RunCommand
	for _, tool := range tools {
		switch v := tool.(type) {
		case *PromoteArtifact:
			pa = v
		case *RunCommand:
			rc = v
		}
	}
	if pa == nil {
		t.Fatal("promotion journal must register exactly one promote_artifact")
	}
	if pa.rt != rc.scratchRT {
		t.Fatal("promote_artifact must share the runtime with run_command")
	}
}
