package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
)

// This subprocess fixture implements only the AgentFlow protocol used by the
// real CLI invocation below. It does not replace any Golem execution boundary.
func TestProjectTrustAgentflowProcess(t *testing.T) {
	if os.Getenv("GOLEM_TRUST_AF_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	args = args[1:]
	stateDir := os.Getenv("GOLEM_TRUST_AF_STATE")
	log, err := os.OpenFile(filepath.Join(stateDir, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	fmt.Fprintln(log, strings.Join(args, " "))
	log.Close()
	reply := "{}"
	switch {
	case args[0] == "--version":
		reply = "agentflow 0.4.0"
	case args[0] == "--help" || len(args) > 1 && args[1] == "--help":
		reply = "init init-execution lock-plan record-file-change run finish-step finish-run next-step next-action doctor status claim-step recommend-workflow workflow-contract --root --from-json --json --agent --step --attempt --path --gate --confirm-risk --stdin --selected-profile --reason"
	case args[0] == "recommend-workflow":
		reply = trustWorkflowRecommendation
	case args[0] == "next-step":
		data, _ := os.ReadFile(filepath.Join(stateDir, "step"))
		step, _ := strconv.Atoi(string(data))
		if step >= 2 {
			reply = "null"
		} else {
			reply = fmt.Sprintf(`{"id":"P%d"}`, step+1)
		}
	case args[0] == "claim-step":
		reply = `{"attempt_id":"A1"}`
		if os.Getenv("GOLEM_TRUST_AF_EDIT") != "" {
			if err := os.WriteFile(os.Getenv("GOLEM_TRUST_AF_EDIT"), []byte("edited-during-invocation"), 0600); err != nil {
				os.Exit(2)
			}
		}
	case args[0] == "finish-step":
		data, _ := os.ReadFile(filepath.Join(stateDir, "step"))
		step, _ := strconv.Atoi(string(data))
		if err := os.WriteFile(filepath.Join(stateDir, "step"), []byte(strconv.Itoa(step+1)), 0600); err != nil {
			os.Exit(2)
		}
	case args[0] == "finish-run":
		reply = `{"ok":true}`
	}
	fmt.Print(reply)
	os.Exit(0)
}

const trustWorkflowRecommendation = `{
"schema_version":"0.1.0",
"recommended":{"pack":"agentflow-default","profile":"small-bugfix"},
"selected":{"pack":"agentflow-default","profile":"small-bugfix"},
"signals":["task_type=bugfix","declared_risk=low"],
"rationale":"Recommended small-bugfix: bounded low-risk fix.",
"alternatives":[],"override":null,
"workflow_contract_candidate":{"schema_version":"0.1.0","workflow_pack":"agentflow-default","workflow_profile":"small-bugfix","selected_by":"recommend-workflow","selection_reason":"Recommended small-bugfix: bounded low-risk fix.","required_capabilities":[],"review_depth":"light","validation_policy":{"required_gates":["unit-tests"]},"proof_policy":{"hunk_attribution":"enforce","require_review_run":false}}}`

func installTrustAgentflow(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	state := t.TempDir()
	script := "#!/bin/sh\nexec \"$GOLEM_TRUST_TEST_BINARY\" -test.run=^TestProjectTrustAgentflowProcess$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "agentflow"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOLEM_TRUST_TEST_BINARY", os.Args[0])
	t.Setenv("GOLEM_TRUST_AF_PROCESS", "1")
	t.Setenv("GOLEM_TRUST_AF_STATE", state)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return state
}

func TestProjectTrustAgentflowProviderWire(t *testing.T) {
	for _, mode := range []string{"goal", "plan"} {
		for _, approved := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/approved=%v", mode, approved), func(t *testing.T) {
				config, root, bodies := dispatchOneShotHarness(t)
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				installTrustAgentflow(t)
				writeTrustDocument(t, root, "frozen-agentflow-guidance")
				args := []string{"-config", config, "-root", root, "-no-probe", "-no-cap-probe", "-no-git-context", "-no-rag", "-no-auto-index"}
				if approved {
					args = append(args, "-trust-project-context", trustFixtureDigest(t, root))
				}
				if mode == "goal" {
					args = append(args, "-goal", "inspect", "-approve-plan-lock")
				} else {
					plan := agentflow.Compile(validTraceableIR())
					plan.Steps[0].ID = "P1"
					next := plan.Steps[0]
					next.ID = "P2"
					plan.Steps = append(plan.Steps, next)
					data, err := json.Marshal(plan)
					if err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(root, "plan.json")
					if err := os.WriteFile(path, data, 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, "-plan", path, "-approve-plan-edits", "-approve-plan-gates")
					t.Setenv("GOLEM_TRUST_AF_EDIT", filepath.Join(root, "AGENTS.md"))
				}
				in, out, diag := runTestFiles(t)
				err := run(args, in, out, diag)
				if mode == "goal" && !errors.Is(err, errPlannerNoSubmission) || mode == "plan" && err != nil {
					t.Fatalf("%s run=%v; stdout=%s; stderr=%s", mode, err, readRunTestFile(t, out), readRunTestFile(t, diag))
				}
				want := 1
				if mode == "plan" {
					want = 2
				}
				if len(bodies()) != want {
					t.Fatalf("%s calls=%d, want %d", mode, len(bodies()), want)
				}
				for i, body := range bodies() {
					system := gitContextSystemFromChatBody(t, body)
					if strings.Contains(system, "frozen-agentflow-guidance") != approved || strings.Contains(system, "edited-during-invocation") {
						t.Fatalf("%s step %d lost frozen consent=%v: %s", mode, i, approved, system)
					}
				}
			})
		}
	}
}
