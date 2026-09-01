package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// treeHash maps every entry of root to a stable identity string covering
// path, type, permission bits, content bytes, and symlink target.
func treeHash(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		id := fmt.Sprintf("type=%v mode=%v", fi.Mode().Type(), fi.Mode().Perm())
		switch {
		case fi.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			id += fmt.Sprintf(" sha=%x", sum)
		case fi.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			id += " link=" + target
		}
		out[rel] = id
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mapsEqualStr(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// buildIsolationWorkspace creates a canonical workspace fixture.
func buildIsolationWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for rel, data := range map[string]string{
		"tracked.txt": "tracked content",
		"gone.txt":    "to be deleted in scratch",
		"dir/sub.txt": "sub",
	} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("tracked.txt", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	return root
}

// scratchExecTools builds the scratch-enabled foreground tool set and
// returns the run tool, the shared runtime, and the scratch_changes tool.
func scratchExecTools(t *testing.T, root string, cfg ScratchConfig) (*RunCommand, *scratchRuntime, ScratchChanges) {
	t.Helper()
	tools, err := NewExecToolsWithOptions(root, ExecToolsOptions{Scratch: cfg})
	if err != nil {
		t.Fatal(err)
	}
	var rc *RunCommand
	var changes ScratchChanges
	var haveChanges bool
	for _, tool := range tools {
		switch v := tool.(type) {
		case *RunCommand:
			rc = v
		case ScratchChanges:
			changes = v
			haveChanges = true
		}
	}
	if rc == nil || !haveChanges {
		t.Fatalf("enabled factory must return run_command and scratch_changes, got %d tools", len(tools))
	}
	if rc.scratchRT == nil {
		t.Fatal("run_command missing the shared scratch runtime")
	}
	if rc.scratchRT != changes.rt {
		t.Fatal("run_command and scratch_changes must share one runtime")
	}
	rc.scratchRT.tempBase = t.TempDir()
	return rc, rc.scratchRT, changes
}

func planAndInvoke(t *testing.T, rc *RunCommand, raw string) agent.ToolResult {
	t.Helper()
	if _, err := rc.Plan(context.Background(), json.RawMessage(raw)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	res, err := rc.Invoke(context.Background(), json.RawMessage(raw))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return res
}

func TestScratchForegroundIsolationHost(t *testing.T) {
	root := buildIsolationWorkspace(t)
	rc, rt, _ := scratchExecTools(t, root, ScratchConfig{Enabled: true})
	before := treeHash(t, root)

	raw := `{"argv":["sh","-c","echo corrupt > tracked.txt && rm gone.txt && echo new > artifact.txt && rm alias && ln -s gone.txt alias"]}`
	res := planAndInvoke(t, rc, raw)
	if res.IsError {
		t.Fatalf("scratched command failed: %s", res.Content)
	}
	after := treeHash(t, root)
	if !mapsEqualStr(before, after) {
		t.Fatalf("canonical workspace changed:\nbefore=%v\nafter=%v", before, after)
	}
	// Scratch-first rendering: the id line leads the result so command
	// output can never truncate it away.
	if !strings.HasPrefix(res.Content, "scratch: id=scr-") {
		t.Fatalf("result must lead with the scratch line:\n%s", res.Content)
	}
	id := strings.TrimPrefix(strings.SplitN(res.Content, " ", 3)[1], "id=")
	out, status := rt.store.get(id)
	if status != scratchStatusCaptured {
		t.Fatalf("outcome not captured for %q: %v", id, status)
	}
	kinds := map[string]scratchChangeKind{}
	for _, c := range out.changes {
		kinds[c.path] = c.kind
	}
	if kinds["tracked.txt"] != scratchChangeUpdate || kinds["gone.txt"] != scratchChangeDelete ||
		kinds["artifact.txt"] != scratchChangeCreate || kinds["alias"] != scratchChangeOther {
		t.Fatalf("captured kinds wrong: %v", kinds)
	}
}

func TestScratchForegroundNonZeroExitStillCaptures(t *testing.T) {
	root := buildIsolationWorkspace(t)
	rc, rt, _ := scratchExecTools(t, root, ScratchConfig{Enabled: true})
	raw := `{"argv":["sh","-c","echo made > out.txt; exit 3"]}`
	res := planAndInvoke(t, rc, raw)
	if res.IsError {
		t.Fatalf("non-zero exit is an observation, not a tool error: %s", res.Content)
	}
	if !strings.HasPrefix(res.Content, "scratch: id=scr-") || !strings.Contains(res.Content, "exit code: 3") {
		t.Fatalf("result must carry scratch line and exit code:\n%s", res.Content)
	}
	id := strings.TrimPrefix(strings.SplitN(res.Content, " ", 3)[1], "id=")
	out, status := rt.store.get(id)
	if status != scratchStatusCaptured {
		t.Fatalf("status = %v", status)
	}
	found := false
	for _, c := range out.changes {
		if c.path == "out.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("outcome missing out.txt: %+v", out.changes)
	}
}

// failingRunner simulates an infrastructure spawn failure.
type failingRunner struct{ err error }

func (r failingRunner) Run(ctx context.Context, spec execSpec) (execResult, error) {
	return execResult{}, r.err
}

func TestScratchForegroundSpawnFailureDeliversOutcome(t *testing.T) {
	root := buildIsolationWorkspace(t)
	rc, rt, _ := scratchExecTools(t, root, ScratchConfig{Enabled: true})
	rc.runner = failingRunner{err: fmt.Errorf("spawn exploded")}
	raw := `{"argv":["sh","-c","true"]}`
	res := planAndInvoke(t, rc, raw)
	if !res.IsError || !strings.Contains(res.Content, "spawn exploded") {
		t.Fatalf("spawn failure must surface: %+v", res)
	}
	if !strings.Contains(res.Content, "scratch: id=scr-") {
		t.Fatalf("spawn-failure result must still expose the scratch id:\n%s", res.Content)
	}
	idx := strings.Index(res.Content, "scratch: id=")
	id := strings.SplitN(res.Content[idx+len("scratch: id="):], " ", 2)[0]
	if _, status := rt.store.get(id); status != scratchStatusCaptured {
		t.Fatalf("spawn failure must still capture, status=%v", status)
	}
}

// cancellingRunner cancels the surrounding run context, simulating a
// parent-run cancellation racing the command.
type cancellingRunner struct{ cancel context.CancelFunc }

func (r cancellingRunner) Run(ctx context.Context, spec execSpec) (execResult, error) {
	r.cancel()
	return execResult{}, context.Canceled
}

func TestScratchForegroundParentCancelDiscards(t *testing.T) {
	root := buildIsolationWorkspace(t)
	rc, rt, _ := scratchExecTools(t, root, ScratchConfig{Enabled: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rc.runner = cancellingRunner{cancel: cancel}
	raw := `{"argv":["sh","-c","true"]}`
	if _, err := rc.Plan(context.Background(), json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	res, err := rc.Invoke(ctx, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "canceled") {
		t.Fatalf("cancelled run: %+v", res)
	}
	// The unreachable outcome is discarded, and the temp base is empty.
	entries, err := os.ReadDir(rt.tempBase)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("parent cancel must clean the scratch roots, found %v", entries)
	}
	if strings.Contains(res.Content, "scratch: id=") {
		t.Fatalf("discarded outcome must not advertise an id:\n%s", res.Content)
	}
	rt.store.mu.Lock()
	completed := len(rt.store.completed)
	rt.store.mu.Unlock()
	if completed != 0 {
		t.Fatalf("parent cancel must discard, not publish: %d completed outcomes", completed)
	}
}

func TestScratchForegroundApprovalKeyOrderAndPreview(t *testing.T) {
	root := buildIsolationWorkspace(t)
	rc, _, _ := scratchExecTools(t, root, ScratchConfig{Enabled: true})
	// Inject a fake sandbox component to pin the documented order:
	// prefix, scr:, sb:, fingerprint.
	rc.sandbox = sandboxApproval{keyComponent: "sb:feedbead:", preview: "runtime=\"fake\""}
	plan, err := rc.Plan(context.Background(), json.RawMessage(`{"argv":["sh","-c","true"]}`))
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := execApprovalKeyPrefix + rc.scratchRT.approval.keyComponent + "sb:feedbead:"
	if !strings.HasPrefix(plan.ApprovalKey, wantPrefix) {
		t.Fatalf("key order must be prefix+scr:+sb:+fingerprint:\n key=%s\nwant=%s...", plan.ApprovalKey, wantPrefix)
	}
	if strings.Count(plan.Preview, "scratch:") != 1 {
		t.Fatalf("preview must carry exactly one scratch line:\n%s", plan.Preview)
	}
	if !strings.Contains(plan.Preview, "disposable cwd copy") {
		t.Fatalf("scratch preview line missing:\n%s", plan.Preview)
	}
	// The ephemeral path never leaks into approval material.
	if strings.Contains(plan.Preview, rc.scratchRT.tempBase) || strings.Contains(plan.ApprovalKey, rc.scratchRT.tempBase) {
		t.Fatal("temp path leaked into approval material")
	}
}

func TestScratchForegroundEffectBudget(t *testing.T) {
	root := buildIsolationWorkspace(t)
	rc, rt, _ := scratchExecTools(t, root, ScratchConfig{Enabled: true})
	plan, err := rc.Plan(context.Background(), json.RawMessage(`{"argv":["sh","-c","true"],"timeout_seconds":90}`))
	if err != nil {
		t.Fatal(err)
	}
	want := rt.cfg.SnapshotTimeout + 90*time.Second + rt.cfg.CaptureTimeout + scratchEffectGrace
	if plan.Effect.Timeout != want {
		t.Fatalf("outer effect timeout = %v, want %v", plan.Effect.Timeout, want)
	}
}

// deadlineRunner records the deadline of the context it runs under.
type deadlineRunner struct {
	deadline *time.Time
	start    *time.Time
}

func (r deadlineRunner) Run(ctx context.Context, spec execSpec) (execResult, error) {
	*r.start = time.Now()
	if d, ok := ctx.Deadline(); ok {
		*r.deadline = d
	}
	return execResult{ExitCode: 0}, nil
}

func TestScratchForegroundRunnerGetsFreshCommandBudget(t *testing.T) {
	root := buildIsolationWorkspace(t)
	rc, rt, _ := scratchExecTools(t, root, ScratchConfig{Enabled: true})
	// A slow clone must not eat into the command budget.
	slow := rt.clone
	rt.clone = func(f *os.File, dst string) error {
		time.Sleep(20 * time.Millisecond)
		return slow(f, dst)
	}
	var deadline, start time.Time
	rc.runner = deadlineRunner{deadline: &deadline, start: &start}
	// 90s is deliberately distinct from the 30s snapshot budget, so a
	// mutant reusing the setup context is distinguishable.
	raw := `{"argv":["sh","-c","true"],"timeout_seconds":90}`
	res := planAndInvoke(t, rc, raw)
	if res.IsError {
		t.Fatalf("%s", res.Content)
	}
	if deadline.IsZero() {
		t.Fatal("runner context must carry the command deadline")
	}
	got := deadline.Sub(start)
	if got < 89*time.Second || got > 91*time.Second {
		t.Fatalf("runner budget = %v, want ~90s independent of setup time", got)
	}
}

func TestScratchDisabledByteCompatible(t *testing.T) {
	root := buildIsolationWorkspace(t)
	legacy, err := NewExecTools(root)
	if err != nil {
		t.Fatal(err)
	}
	viaOptions, err := NewExecToolsWithOptions(root, ExecToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != len(viaOptions) {
		t.Fatalf("zero options must reproduce the legacy tool list: %d vs %d", len(legacy), len(viaOptions))
	}
	raw := json.RawMessage(`{"argv":["sh","-c","echo hi"]}`)
	lp, err := legacy[0].(*RunCommand).Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	op, err := viaOptions[0].(*RunCommand).Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if lp.ApprovalKey != op.ApprovalKey || lp.Preview != op.Preview ||
		lp.Effect.Timeout != op.Effect.Timeout || lp.Effect.Class != op.Effect.Class ||
		lp.Effect.Approval != op.Effect.Approval || lp.Effect.OutputCap != op.Effect.OutputCap {
		t.Fatal("zero-options plan must be byte-identical to the legacy constructor")
	}
	// Absolute shape: no scratch material anywhere when disabled.
	if strings.Contains(op.Preview, "scratch") || strings.Contains(op.ApprovalKey, "scr:") {
		t.Fatalf("disabled options leaked scratch material:\n%s", op.Preview)
	}
	res, err := viaOptions[0].(*RunCommand).Invoke(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "scratch") {
		t.Fatalf("disabled result must carry no scratch text:\n%s", res.Content)
	}
}

func TestScratchOptionsRejectJournalWithoutScratch(t *testing.T) {
	root := buildIsolationWorkspace(t)
	if _, err := NewExecToolsWithOptions(root, ExecToolsOptions{PromotionJournal: nopPreparingJournal{}}); err == nil {
		t.Fatal("a promotion journal without scratch is a misconfiguration and must fail construction")
	}
}

// nopPreparingJournal satisfies PreparingJournal for construction tests.
type nopPreparingJournal struct{}

func (nopPreparingJournal) Record(MutationRecord) {}
func (nopPreparingJournal) Prepare(MutationRecord) (PreparedMutation, error) {
	return nopPrepared{}, nil
}

type nopPrepared struct{}

func (nopPrepared) Commit() error { return nil }
func (nopPrepared) Abort() error  { return nil }

func TestScratchSandboxedFactoryZeroDelegates(t *testing.T) {
	root := buildIsolationWorkspace(t)
	tools, err := NewSandboxedExecToolsWithOptions(root, SandboxConfig{}, ExecToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("zero sandbox+options must reproduce the single-tool set, got %d", len(tools))
	}
	tools, err = NewSandboxedExecToolsWithOptions(root, SandboxConfig{}, ExecToolsOptions{Scratch: ScratchConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("scratch-enabled sandboxed factory must add scratch_changes, got %d", len(tools))
	}
}
