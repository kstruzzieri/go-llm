//go:build agentflow_integration

package main

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
)

// TestAgentflowSmoke drives the full spine over the fixture with the real CLI to
// a verify-proof pass. Honors AGENTFLOW_SRC when set, otherwise uses an installed
// binary or skips. Asserts driver.run returns a non-empty proof path and no error, the
// fixture gate passes, and the proof pack exists on disk.
func TestAgentflowSmoke(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, "../../testdata/agentflow", dir)
	gitInit(t, dir) // record-file-change shells out to `git status`; the root must be a repo

	runner := agentflowRunnerOrSkip(t, dir)
	client := agentflow.NewClient(runner, dir)

	planBytes, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan agentflow.Plan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		t.Fatal(err)
	}

	runStep := func(ctx context.Context, step agentflow.Step, attempt, _ string) error {
		if err := os.WriteFile(filepath.Join(dir, "src", "answer.txt"), []byte("expected\n"), 0o600); err != nil {
			return err
		}
		return client.RecordFileChange(ctx, step.ID, attempt, "src/answer.txt")
	}

	d := &driver{
		af:        client,
		plan:      &plan,
		planPath:  filepath.Join(dir, "plan.json"),
		taskBrief: agentflow.TaskBriefFromPlan(plan, "feature"),
		runStep:   runStep,
		out:       io.Discard,
	}
	proof, err := d.run(context.Background())
	if err != nil {
		t.Fatalf("driver.run: %v", err)
	}
	t.Logf("proof pack: %s", proof)
	if proof == "" {
		t.Fatal("driver.run returned an empty proof path")
	}
	if _, err := os.Stat(proof); err != nil {
		t.Fatalf("proof pack %s not found on disk: %v", proof, err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "src", "answer.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "expected\n" {
		t.Fatalf("src/answer.txt = %q, want %q", got, "expected\n")
	}
}

// agentflowRunnerOrSkip honors the explicit AGENTFLOW_SRC checkout, otherwise
// uses an installed binary or skips. Mirrors
// agentflow.agentflowRunnerForTest, which is unexported in another package.
func agentflowRunnerOrSkip(t *testing.T, dir string) agentflow.Runner {
	t.Helper()
	if src := os.Getenv("AGENTFLOW_SRC"); src != "" {
		return agentflow.NewSrcExecRunner(dir, src)
	}
	if _, err := exec.LookPath("agentflow"); err == nil {
		return agentflow.NewExecRunner(dir)
	}
	t.Skip("agentflow CLI not available (set AGENTFLOW_SRC=<checkout> to run)")
	return nil
}

// gitInit initializes a git repository at dir and commits the fixture as it
// stands. AgentFlow's record-file-change shells out to `git status`
// unconditionally, so the copy needs to be a repo; finish-run's drift audit
// additionally diffs against that baseline commit, so plan.json and the
// fixture's starting files must already be committed or drift audit flags
// them as out-of-scope changes (everything AgentFlow itself writes under
// .agent/ is separately exempted via the plan's own allowed_files entry).
func gitInit(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=agentflow-smoke", "GIT_AUTHOR_EMAIL=agentflow-smoke@example.com",
			"GIT_COMMITTER_NAME=agentflow-smoke", "GIT_COMMITTER_EMAIL=agentflow-smoke@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("add", "-A")
	run("commit", "-m", "fixture baseline")
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
