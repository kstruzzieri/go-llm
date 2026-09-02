//go:build linux

package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scratchBwrapTools builds the bwrap + scratch composition, skipping (or
// failing under GO_LLM_REQUIRE_BWRAP=1) on incapable hosts.
func scratchBwrapTools(t *testing.T, root string) (*RunCommand, *scratchRuntime) {
	t.Helper()
	requireBwrapCapability(t)
	tools, err := NewSandboxedExecToolsWithOptions(root,
		SandboxConfig{Runtime: SandboxRuntimeBwrap},
		ExecToolsOptions{Scratch: ScratchConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	rc := tools[0].(*RunCommand)
	return rc, rc.scratchRT
}

// TestBwrapScratchComposition is the Linux twin of the Seatbelt composition
// proof: an in-scratch cwd-relative write succeeds and is captured, an
// absolute write addressed at the canonical root is denied by the
// read-only bind, and — because the scratch execution root lives under the
// host /tmp — the pass also proves the workspace bind stays visible over
// bwrap's private tmpfs ordering.
func TestBwrapScratchComposition(t *testing.T) {
	root := buildIsolationWorkspace(t)
	rc, rt := scratchBwrapTools(t, root)
	before := treeHash(t, root)

	res := planAndInvoke(t, rc, `{"argv":["sh","-c","echo ok > inside.txt && cat inside.txt"]}`)
	if res.IsError || !strings.Contains(res.Content, "exit code: 0") {
		t.Fatalf("in-scratch write must succeed under bwrap: %s", res.Content)
	}
	if !strings.Contains(res.Content, "ok") {
		t.Fatalf("scratch workspace under host /tmp must be visible over the private tmpfs:\n%s", res.Content)
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

	leak := filepath.Join(root, "leak.txt")
	raw := fmt.Sprintf(`{"argv":["sh","-c","echo leak > %s"]}`, leak)
	res = planAndInvoke(t, rc, raw)
	if res.IsError {
		t.Fatalf("denied write is a normal non-zero observation: %s", res.Content)
	}
	if strings.Contains(res.Content, "exit code: 0") {
		t.Fatalf("canonical absolute write must be denied under bwrap+scratch:\n%s", res.Content)
	}
	if _, err := os.Lstat(leak); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("leak.txt reached the canonical tree: err=%v", err)
	}
	if !mapsEqualStr(before, treeHash(t, root)) {
		t.Fatal("canonical workspace changed under the composed sandbox")
	}
}

// TestBwrapScratchClonedTreePassesLinkValidation mirrors the Darwin case:
// a canonical file hard-linked outside the tree fails the pre-spawn link
// walk for the canonical root, but its CoW/copy clone (fresh single-link
// inodes) must pass.
func TestBwrapScratchClonedTreePassesLinkValidation(t *testing.T) {
	root := buildIsolationWorkspace(t)
	outside := filepath.Join(t.TempDir(), "outside-link.txt")
	if err := os.Link(filepath.Join(root, "tracked.txt"), outside); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	rc, _ := scratchBwrapTools(t, root)
	res := planAndInvoke(t, rc, `{"argv":["sh","-c","cat tracked.txt"]}`)
	if res.IsError || !strings.Contains(res.Content, "exit code: 0") {
		t.Fatalf("cloned tree must pass the pre-spawn link validation: %s", res.Content)
	}
	if !strings.Contains(res.Content, "tracked content") {
		t.Fatalf("command must see the cloned bytes: %s", res.Content)
	}
}
