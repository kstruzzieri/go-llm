//go:build agentflow_integration

package agentflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// lockGateFixture initializes Agentflow in dir and locks a one-step plan whose
// only gate is argv, then claims P1 and returns the client and attempt.
func lockGateFixture(t *testing.T, dir string, r Runner, label string, argv []string) (*Client, string) {
	t.Helper()
	ctx := t.Context()
	c := NewClient(r, dir)
	if err := c.Probe(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Init(ctx); err != nil {
		t.Fatal(err)
	}
	plan := Compile(PlanIR{
		Objective: "exercise the child environment policy", Scope: []string{"src"}, Invariants: []string{"stay in scope"},
		RiskLevel: "low", RollbackPlan: "discard the fixture", AllowedFiles: []string{"src/*"},
		Steps: []StepIR{{
			ID: "P1", Action: "run the gate", Files: []string{"src/a.go"}, ExpectedDiff: []string{"no source change"},
			Validations: []GateIR{{Label: label, Argv: argv}},
		}},
	})
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(planPath, planBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.LockPlan(ctx, planPath); err != nil {
		t.Fatal(err)
	}
	if err := c.InitExecution(ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := c.ClaimStep(ctx, "P1")
	if err != nil {
		t.Fatal(err)
	}
	return c, attempt
}

func TestAgentflowEnv_RealCLI_GateSeesOnlyThePolicy(t *testing.T) {
	t.Setenv(envCanaryName, envCanaryValue)
	t.Setenv("OPENAI_API_KEY", envCanaryValue)
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "gate-env.json")
	argv := append([]string{os.Args[0]}, envProbeArgv(out)...)
	c, attempt := lockGateFixture(t, dir, agentflowRunnerForTest(t, dir), "env-probe", argv)
	if err := c.RunGate(t.Context(), "P1", attempt, "env-probe", argv); err != nil {
		t.Fatal(err)
	}
	report := readEnvProbe(t, out)
	if report.Canary || slices.Contains(report.Names, "OPENAI_API_KEY") {
		t.Fatalf("parent secret reached the gate; names=%v", report.Names)
	}
	if !slices.Contains(report.Names, "PATH") {
		t.Fatalf("baseline PATH missing from the gate; names=%v", report.Names)
	}
}

func writeTaggedGoModule(t *testing.T, dir string) {
	t.Helper()
	for name, body := range map[string]string{
		"go.mod":         "module golem577\n\ngo 1.22\n",
		"tagged.go":      "//go:build golem577\n\npackage golem577\n\nfunc Tagged() bool { return true }\n",
		"tagged_test.go": "package golem577\n\nimport \"testing\"\n\nfunc TestTagged(t *testing.T) {\n\tif !Tagged() {\n\t\tt.Fatal(\"tagged symbol\")\n\t}\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// gateReceiptStderr returns the captured stderr of the only command receipt
// Agentflow recorded in dir.
func gateReceiptStderr(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".agent", "command-receipts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		StderrPath string `json:"stderr_path"`
	}
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.StderrPath == "" {
		t.Fatalf("want one command receipt with captured stderr; err=%v receipts=%s", err, data)
	}
	stderr, err := os.ReadFile(filepath.Join(dir, receipt.StderrPath))
	if err != nil {
		t.Fatal(err)
	}
	return string(stderr)
}

// The untagged test references a symbol that exists only under the golem577
// build tag, so the gate cannot pass vacuously: it compiles only when the
// approved GOFLAGS reaches it.
func TestAgentflowEnv_RealCLI_ApprovedVariableReachesGate(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatal("the go toolchain is required for the approved-variable gate")
	}
	t.Setenv("GOFLAGS", "-tags=golem577")
	argv := []string{"go", "test", "./..."}
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprintf("approved=%v", approved), func(t *testing.T) {
			dir := t.TempDir()
			writeTaggedGoModule(t, dir)
			r := agentflowRunnerForTest(t, dir).(*ExecRunner)
			if approved {
				if err := r.AllowEnv("GOFLAGS"); err != nil {
					t.Fatal(err)
				}
			}
			c, attempt := lockGateFixture(t, dir, r, "go-test", argv)
			err := c.RunGate(t.Context(), "P1", attempt, "go-test", argv)
			if approved && err != nil {
				t.Fatalf("gate failed with GOFLAGS approved: %v", err)
			}
			if !approved && err == nil {
				t.Fatal("gate passed without GOFLAGS approved; the tagged symbol must not compile")
			}
			if !approved {
				// Fail for the right reason: the gate ran and its build lacked the tag.
				if stderr := gateReceiptStderr(t, dir); !strings.Contains(stderr, "undefined: Tagged") {
					t.Fatalf("unapproved gate failed for another reason: err=%v gate stderr=%q", err, stderr)
				}
			}
		})
	}
}

// Golem's real finish-step invocation over an attested receipt: Agentflow
// accepts it normally and rejects it in strict mode, so the outcome shows
// whether AGENTFLOW_STRICT=1 reached the child.
func TestAgentflowEnv_RealCLI_StrictModeIsPreserved(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			if strict {
				t.Setenv("AGENTFLOW_STRICT", "1")
			} else {
				t.Setenv("AGENTFLOW_STRICT", "")
			}
			dir := t.TempDir()
			r := agentflowRunnerForTest(t, dir)
			c, attempt := lockGateFixture(t, dir, r, "true", []string{"true"})
			_, errb, exit, err := r.Run(t.Context(), []string{"record-command", "--root", dir, "--step", "P1",
				"--attempt", attempt, "--gate", "true", "--exit-code", "0", "--agent", "golem", "--json", "--", "true"}, nil)
			if err != nil || exit != 0 {
				t.Fatalf("record-command: exit=%d err=%v stderr=%s", exit, err, errb)
			}
			err = c.FinishStep(t.Context(), "P1", attempt)
			var ce *CommandError
			switch {
			case !strict && err != nil:
				t.Fatalf("non-strict finish-step: %v", err)
			case strict && (!errors.As(err, &ce) || ce.Exit != 1 || !strings.Contains(err.Error(), "uses attested provenance")):
				t.Fatalf("strict finish-step error = %v, want exit 1 for attested provenance", err)
			}
		})
	}
}
