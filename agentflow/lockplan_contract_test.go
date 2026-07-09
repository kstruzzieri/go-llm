package agentflow

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// agentflowRunnerForTest returns a Runner for the installed binary, or the
// python-src fallback checkout from AGENTFLOW_SRC, or skips.
func agentflowRunnerForTest(t *testing.T, dir string) Runner {
	if _, err := exec.LookPath("agentflow"); err == nil {
		return NewExecRunner(dir)
	}
	if src := os.Getenv("AGENTFLOW_SRC"); src != "" {
		return NewSrcExecRunner(dir, src)
	}
	t.Skip("agentflow CLI not available (set AGENTFLOW_SRC=<checkout> to run)")
	return nil
}

func TestLockPlan_RealCLI(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "../testdata/agentflow", dir)
	r := agentflowRunnerForTest(t, dir)
	c := NewClient(r, dir)
	ctx := context.Background()
	if err := c.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.LockPlan(ctx, filepath.Join(dir, "plan.json")); err != nil {
		t.Fatalf("lock-plan on the fixture must succeed: %v", err)
	}
}

// copyTree recursively copies the directory tree rooted at src into dst,
// preserving the subdirectory structure.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
