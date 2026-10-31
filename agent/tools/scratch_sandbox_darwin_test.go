//go:build darwin

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

// TestScratchSeatbeltPreviewNamesRuntime covers the native production caller of the
// runtime-aware scratch wording: the real sandboxed factory must hand the
// scratch engine the backend's own runtime, so the preview names seatbelt, not
// the host.
func TestScratchSeatbeltPreviewNamesRuntime(t *testing.T) {
	rc, _ := scratchSeatbeltTools(t, buildIsolationWorkspace(t))
	plan, err := rc.Plan(context.Background(), json.RawMessage(`{"argv":["sh","-c","true"]}`))
	if err != nil {
		t.Fatal(err)
	}
	const want = "scratch: disposable cwd copy under the seatbelt sandbox git-metadata=omitted "
	if !strings.Contains(plan.Preview, want) {
		t.Fatalf("preview missing %q:\n%s", want, plan.Preview)
	}
	if strings.Contains(plan.Preview, "host process remains unrestricted") {
		t.Fatalf("native preview claims the host is unrestricted:\n%s", plan.Preview)
	}
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

// TestScratchSeatbeltExecConfinement is the exec analog of the composition
// test (#708): under Seatbelt+scratch, a clone-local executable runs, while an
// executable addressed by its absolute path in the canonical workspace is
// refused. Scratch rewrites the workspace root, cwd and a workspace-local
// executable to the clone, but leaves argv and the environment as approved, so
// an absolute canonical child path is outside the scratch profile's exec
// allowance. M-scratch (add the canonical root to the exec filter) turns the
// refusal leg green and so is caught here.
func TestScratchSeatbeltExecConfinement(t *testing.T) {
	cc := seatbeltHasCC(t)
	root := buildIsolationWorkspace(t)
	canonExe := buildSeatbeltExe(t, cc, root, "canon-exe", markerExeSource())
	rc, _ := scratchSeatbeltTools(t, root)

	// Clone-local exec: copy a system binary into the scratch cwd and run it
	// by a relative path. It lives in the clone, so the exec filter allows it.
	res := planAndInvoke(t, rc, `{"argv":["sh","-c","cp /usr/bin/true ./clone-exe && ./clone-exe && echo CLONE-EXEC-OK"]}`)
	if res.IsError {
		t.Fatalf("clone-local exec is a normal observation, not an infra error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "exit code: 0") || !strings.Contains(res.Content, "CLONE-EXEC-OK") {
		t.Fatalf("clone-local executable must run under Seatbelt+scratch:\n%s", res.Content)
	}

	// Canonical absolute exec: the executable exists in the canonical tree,
	// but that path is outside the scratch profile, so the launch is refused
	// and its marker never appears.
	raw := fmt.Sprintf(`{"argv":["sh","-c","%s"]}`, canonExe)
	res = planAndInvoke(t, rc, raw)
	if res.IsError {
		t.Fatalf("denied exec is a normal non-zero observation: %s", res.Content)
	}
	// Route oracle, as the sibling exec-confinement tests use: the shell's
	// exec of the canonical path is refused with exit 126 and an EPERM
	// diagnostic naming it, and the marker never appears.
	if strings.Contains(res.Content, seatbeltMarker) {
		t.Fatalf("canonical absolute exec ran under Seatbelt+scratch:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "exit code: 126") ||
		!strings.Contains(res.Content, canonExe+": Operation not permitted") {
		t.Fatalf("canonical absolute exec must be refused (exit 126, EPERM on %s):\n%s", canonExe, res.Content)
	}
}
