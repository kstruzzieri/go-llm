package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
)

// smokeIR is the authored PlanIR for the end-to-end -goal smoke: a single step
// that targets src/answer.txt with a grep validation, matching the shape the
// testdata/agentflow fixture locks.
func smokeIR(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(agentflow.PlanIR{
		Objective:    "ensure the answer token",
		Scope:        []string{"src"},
		Invariants:   []string{"only src/answer.txt changes"},
		RiskLevel:    "low",
		RollbackPlan: "git checkout -- .",
		AllowedFiles: []string{"src/*"},
		Steps: []agentflow.StepIR{{
			ID:           "P1",
			Action:       "ensure src/answer.txt contains the expected token",
			Files:        []string{"src/answer.txt"},
			ExpectedDiff: []string{"answer.txt changes pending to expected"},
			Validations:  []agentflow.GateIR{{Label: "grep", Argv: []string{"grep", "-q", "expected", "src/answer.txt"}}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestGoalAuthor_RealCLI drives the real runAgentflowAuthor end to end against
// the fixture: a scripted submit_plan compiles, pre-checks, and locks a plan with
// the real agentflow CLI. It then re-locks that same artifact to pin the #209
// execute-separately handoff (spec §9 re-lock idempotence). It skips only when the
// CLI is genuinely unavailable.
func TestGoalAuthor_RealCLI(t *testing.T) {
	src := os.Getenv("AGENTFLOW_SRC")
	if _, err := exec.LookPath("agentflow"); err != nil && src == "" {
		t.Skip("agentflow CLI not available (set AGENTFLOW_SRC=<checkout> to run)")
	}

	dir := t.TempDir()
	copyTreeLocal(t, "../../testdata/agentflow", dir)

	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(smokeIR(t))}}
	sess := newTestSession(t, caller, dir)

	var out, errb bytes.Buffer
	f := flags{goal: "ensure the answer token", goalSet: true, agentflowSrc: src}
	if err := runAgentflowAuthor(context.Background(), &out, &errb, sess, f, dir); err != nil {
		t.Fatalf("author flow: %v\n%s", err, errb.String())
	}

	lockedPath := filepath.Join(dir, ".agent", "plan.lock.json")
	b, err := os.ReadFile(lockedPath)
	if err != nil {
		t.Fatal(err)
	}
	var locked struct {
		Locked bool `json:"locked"`
	}
	if err := json.Unmarshal(b, &locked); err != nil {
		t.Fatalf("locked plan is not valid JSON: %v\n%s", err, b)
	}
	if !locked.Locked {
		t.Errorf(".agent/plan.lock.json is not locked: %s", b)
	}

	// Re-lock idempotence (spec §9): the locked artifact must survive a fresh
	// same-file LockPlan, proving the `golem -plan .agent/plan.lock.json`
	// execute-separately path can re-lock what the author produced. Mirror the
	// runner selection runAgentflowAuthor uses so both reach the same CLI.
	var runner agentflow.Runner
	if src != "" {
		runner = agentflow.NewSrcExecRunner(dir, src)
	} else {
		runner = agentflow.NewExecRunner(dir)
	}
	if err := agentflow.NewClient(runner, dir).LockPlan(context.Background(), lockedPath); err != nil {
		t.Fatalf("same-file re-lock: %v", err)
	}
}

// copyTreeLocal recursively copies the tree rooted at src into dst, preserving
// subdirectory structure. A local copy of the tagged smoke test's copyTree so
// this untagged test builds without the agentflow_integration tag.
func copyTreeLocal(t *testing.T, src, dst string) {
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
