//go:build darwin

package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchSeatbeltTools builds the Seatbelt + scratch composition, skipping
// (or failing under GO_LLM_REQUIRE_SEATBELT=1) when the host cannot run
// sandbox-exec.
func scratchSeatbeltTools(t *testing.T, root string) (*RunCommand, *scratchRuntime) {
	t.Helper()
	requireSeatbeltCapability(t)
	tools, err := NewSandboxedExecToolsWithOptions(root,
		SandboxConfig{Runtime: SandboxRuntimeSeatbelt},
		ExecToolsOptions{Scratch: ScratchConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	rc := tools[0].(*RunCommand)
	// The scratch temp base must stay /private/tmp: the Seatbelt profile
	// binds its write allowance to the rewritten workspace root, and the
	// metadata-ancestor allowances assume the platform base.
	return rc, rc.scratchRT
}

// TestScratchSeatbeltComposition proves the enforced-isolation claim on real
// processes: the same feature pair that lets a cwd-relative write succeed in
// the scratch root denies a write addressed at the canonical root by
// absolute path. This test is also the constructed distinguishing input for
// the Task 5 mutant that kept the canonical WorkspaceRoot while rewriting
// only Dir — host isolation stays green under that mutant, this denial does
// not.
func TestScratchSeatbeltComposition(t *testing.T) {
	root := buildIsolationWorkspace(t)
	rc, rt := scratchSeatbeltTools(t, root)
	before := treeHash(t, root)

	// Inside write: cwd-relative, lands in scratch, captured.
	res := planAndInvoke(t, rc, `{"argv":["sh","-c","echo ok > inside.txt"]}`)
	if res.IsError {
		t.Fatalf("in-scratch write must succeed under Seatbelt: %s", res.Content)
	}
	if !strings.Contains(res.Content, "exit code: 0") {
		t.Fatalf("in-scratch write exit: %s", res.Content)
	}
	id := scratchIDFromResult(t, res.Content)
	out, status := rt.store.get(id)
	if status != scratchStatusCaptured {
		t.Fatalf("status=%v", status)
	}
	captured := false
	for _, c := range out.changes {
		if c.path == "inside.txt" && c.kind == scratchChangeCreate {
			captured = true
		}
	}
	if !captured {
		t.Fatalf("inside.txt not captured: %+v", out.changes)
	}

	// Absolute canonical write: denied by the sandbox, never lands.
	leak := filepath.Join(root, "leak.txt")
	raw := fmt.Sprintf(`{"argv":["sh","-c","echo leak > %s"]}`, leak)
	res = planAndInvoke(t, rc, raw)
	if res.IsError {
		t.Fatalf("denied write is a normal non-zero observation: %s", res.Content)
	}
	if strings.Contains(res.Content, "exit code: 0") {
		t.Fatalf("canonical absolute write must be denied under Seatbelt+scratch:\n%s", res.Content)
	}
	if _, err := os.Lstat(leak); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leak.txt reached the canonical tree: err=%v", err)
	}
	if !mapsEqualStr(before, treeHash(t, root)) {
		t.Fatal("canonical workspace changed under the composed sandbox")
	}
}

// TestScratchSeatbeltClonedTreePassesLinkValidation proves end-to-end that
// the pre-spawn hardlink walk accepts a CoW-cloned workspace (fresh
// single-link inodes), including for canonical files that are hard-linked
// outside the scratch tree.
func TestScratchSeatbeltClonedTreePassesLinkValidation(t *testing.T) {
	root := buildIsolationWorkspace(t)
	// Hard-link a canonical file elsewhere: the canonical tree itself would
	// fail validateSandboxWorkspaceLinks, but its clone must pass.
	outside := filepath.Join(t.TempDir(), "outside-link.txt")
	if err := os.Link(filepath.Join(root, "tracked.txt"), outside); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	rc, _ := scratchSeatbeltTools(t, root)
	res := planAndInvoke(t, rc, `{"argv":["sh","-c","cat tracked.txt"]}`)
	if res.IsError || !strings.Contains(res.Content, "exit code: 0") {
		t.Fatalf("cloned tree must pass the pre-spawn link validation: %s", res.Content)
	}
	if !strings.Contains(res.Content, "tracked content") {
		t.Fatalf("command must see the cloned bytes: %s", res.Content)
	}
}
