package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agentflow"
)

// fakeAgentflowOnPath is defined in agentflow_driver_test.go.

func writeAgentFile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, ".agent")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	v1Lock     = `{"schema_version":"1.0.0","objective":"x","steps":[{"id":"S1"}],"locked":true}`
	v1Contract = `{"schema_version":"1.0.0","plan":".agent/plan.lock.json"}`
	v1Row      = `{"schema_version":"1.0.0","step_id":"S1"}`
	v0Row      = `{"schema_version":"0.3.0","step_id":"S1"}`
)

// nestedRow is a well-formed schema 1.0.0 row whose "x" nests arrays depth
// deep, so only the depth can make it unreadable.
func nestedRow(depth int) string {
	return `{"schema_version":"1.0.0","x":` + strings.Repeat("[", depth) + strings.Repeat("]", depth) + "}"
}

var stateLedgers = []string{"step-runs.jsonl", "command-receipts.jsonl", "file-receipts.jsonl", "verification-runs.jsonl"}

// TestCheckAgentflowStateMajor is U5's table: every retained artifact AgentFlow
// 1.0 version-checks on read, each row independent, plus the trees that pass.
func TestCheckAgentflowStateMajor(t *testing.T) {
	type file struct{ name, body string }
	cases := []struct {
		name  string
		files []file
		dirs  []string // created as directories, so reading them fails
		want  string   // "" means the guard passes
	}{
		{name: "absent .agent"},
		{name: "1.x tree with ledger rows", files: []file{
			{"plan.lock.json", v1Lock}, {"execution.contract.json", v1Contract},
			{"step-runs.jsonl", v1Row + "\r\n  \t\r\n" + v1Row}, {"command-receipts.jsonl", v1Row + "\n"}, // CRLF, whitespace-only line, no final newline
			{"file-receipts.jsonl", v1Row + "\n"}, {"verification-runs.jsonl", v1Row + "\n"},
		}},
		{name: "empty ledgers", files: []file{
			{"plan.lock.json", v1Lock}, {"execution.contract.json", v1Contract},
			{"step-runs.jsonl", ""}, {"command-receipts.jsonl", ""}, {"file-receipts.jsonl", ""}, {"verification-runs.jsonl", ""},
		}},
		{name: "goal handoff: locked 1.x plan, no execution", files: []file{{"plan.lock.json", v1Lock}}},
		{name: "0.x plan lock", files: []file{{"plan.lock.json", `{"schema_version":"0.3.0","locked":true}`}},
			want: `.agent/plan.lock.json schema_version "0.3.0"`},
		{name: "0.x pristine scaffold", files: []file{{"plan.lock.json", `{"schema_version":"0.3.0","objective":"","steps":[],"locked":false}`}},
			want: `.agent/plan.lock.json schema_version "0.3.0"`},
		{name: "0.x execution contract", files: []file{{"plan.lock.json", v1Lock}, {"execution.contract.json", `{"schema_version":"0.3.0"}`}},
			want: `.agent/execution.contract.json schema_version "0.3.0"`},
		{name: "2.x execution contract", files: []file{{"execution.contract.json", `{"schema_version":"2.0.0"}`}},
			want: `.agent/execution.contract.json schema_version "2.0.0"`},
		{name: "malformed plan schema", files: []file{{"plan.lock.json", `{"schema_version":"1.x"}`}},
			want: `.agent/plan.lock.json schema_version "1.x"`},
		{name: "plan lock not JSON", files: []file{{"plan.lock.json", `{not json`}},
			want: ".agent/plan.lock.json unreadable"},
		{name: "contract without schema_version", files: []file{{"execution.contract.json", `{}`}},
			want: ".agent/execution.contract.json unreadable"},
		{name: "empty execution contract", files: []file{{"plan.lock.json", v1Lock}, {"execution.contract.json", ""}},
			want: ".agent/execution.contract.json unreadable"},
		{name: "plan lock is a directory", dirs: []string{"plan.lock.json"},
			want: ".agent/plan.lock.json unreadable"},
		{name: "malformed ledger row", files: []file{{"step-runs.jsonl", "{\n"}},
			want: ".agent/step-runs.jsonl:1 unreadable"},
		// AgentFlow is Python: it persists an unknown non-finite number as
		// NaN/Infinity, which encoding/json rejects. A real 1.0 tree is not
		// "unreadable" for that (#612, found by the real-CLI guard check).
		{name: "1.x lock with Python non-finite constants", files: []file{
			{"plan.lock.json", `{"schema_version":"1.0.0","a":Infinity,"b":-Infinity,"c":NaN}`},
			{"step-runs.jsonl", `{"schema_version":"1.0.0","step_id":"S1","x":NaN}` + "\n"},
		}},
		{name: "0.x lock with Python non-finite constants names the version", files: []file{{"plan.lock.json", `{"schema_version":"0.3.0","future":Infinity}`}},
			want: `.agent/plan.lock.json schema_version "0.3.0"`},
		{name: "duplicate schema_version: last wins (ok)", files: []file{{"plan.lock.json", `{"schema_version":"0.3.0","schema_version":"1.0.0"}`}}},
		{name: "duplicate schema_version: last wins (refused)", files: []file{{"plan.lock.json", `{"schema_version":"1.0.0","schema_version":"0.3.0"}`}},
			want: `.agent/plan.lock.json schema_version "0.3.0"`},
		{name: "numeric schema_version", files: []file{{"plan.lock.json", `{"schema_version":1}`}},
			want: ".agent/plan.lock.json unreadable"},
		{name: "null schema_version", files: []file{{"plan.lock.json", `{"schema_version":null}`}},
			want: ".agent/plan.lock.json unreadable"},
		{name: "top-level array", files: []file{{"plan.lock.json", `[]`}},
			want: ".agent/plan.lock.json unreadable"},
		// One deeply nested row is refused like encoding/json did, not a fatal
		// stack overflow before any AgentFlow call (#612).
		{name: "ledger row nested past the limit", files: []file{{"step-runs.jsonl", nestedRow(10001) + "\n"}},
			want: ".agent/step-runs.jsonl:1 unreadable"},
		{name: "plan lock nested past the limit", files: []file{{"plan.lock.json", nestedRow(10001)}},
			want: ".agent/plan.lock.json unreadable"},
		{name: "ledger row nested well past Python's limit but bounded", files: []file{{"step-runs.jsonl", nestedRow(2000) + "\n"}}},
	}
	for _, ledger := range stateLedgers {
		cases = append(cases, struct {
			name  string
			files []file
			dirs  []string
			want  string
		}{
			name:  "0.x row 3 in " + ledger,
			files: []file{{"plan.lock.json", v1Lock}, {"execution.contract.json", v1Contract}, {ledger, v1Row + "\n\n" + v0Row + "\n"}},
			want:  ".agent/" + ledger + `:3 schema_version "0.3.0"`,
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tc.files {
				writeAgentFile(t, root, f.name, f.body)
			}
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(root, ".agent", d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			err := checkAgentflowStateMajor(root)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("guard refused a supported tree: %v", err)
				}
				return
			}
			want := "this workspace holds incompatible AgentFlow state (" + tc.want + "); finish that run, or build its proof, with the AgentFlow version that wrote it, then move .agent/ aside before starting an AgentFlow 1.x run"
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v\nwant %s", err, want)
			}
		})
	}
}

// The guard runs before any AgentFlow call on fresh -plan and resume, and
// leaves .agent/ byte-identical.
func TestRunAgentflowTask_RefusesZeroXStateBeforeAgentflow(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "resume"}[resume], func(t *testing.T) {
			root := t.TempDir()
			writeAgentFile(t, root, "plan.lock.json", v1Lock)
			writeAgentFile(t, root, "execution.contract.json", `{"schema_version":"0.3.0"}`)
			before := snapshotAuditFixtureTree(t, filepath.Join(root, ".agent"))
			calls := fakeAgentflowOnPath(t, "1.0.0")
			planBytes, err := json.Marshal(agentflow.Compile(validTraceableIR()))
			if err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(t.TempDir(), "plan.json")
			if err := os.WriteFile(planPath, planBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			sess := &replSession{orch: agent.New(&scriptCaller{}, agent.ContextManager{})}
			var stdout, stderr bytes.Buffer
			err = runAgentflowTask(context.Background(), &stdout, &stderr, nil, sess, flags{
				planPath: planPath, approveEdits: true, approveGates: true, agentflowResume: resume,
			}, root)
			if err == nil || !strings.Contains(err.Error(), `incompatible AgentFlow state (.agent/execution.contract.json schema_version "0.3.0")`) {
				t.Fatalf("err = %v", err)
			}
			if _, statErr := os.Stat(calls); !os.IsNotExist(statErr) {
				got, _ := os.ReadFile(calls)
				t.Fatalf("AgentFlow ran before the guard: %q", got)
			}
			if after := snapshotAuditFixtureTree(t, filepath.Join(root, ".agent")); !reflect.DeepEqual(after, before) {
				t.Fatal("guard refusal changed .agent/")
			}
		})
	}
}

// A 0.x LOCKED plan gets the upgrade guidance, not the "already locked; reset
// the run" advice: AgentFlow has no reset command and 1.0 cannot touch 0.x state.
func TestRunAgentflowAuthor_ZeroXLockedPlanGetsUpgradeGuidance(t *testing.T) {
	root := t.TempDir()
	writeAgentFile(t, root, "plan.lock.json", `{"schema_version":"0.3.0","objective":"x","steps":[{"id":"S1"}],"locked":true}`)
	before := snapshotAuditFixtureTree(t, filepath.Join(root, ".agent"))
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{}
	err := runAgentflowAuthorWithClient(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, nil, sess, flags{goal: "x", goalSet: true}, root, client, nil)
	if err == nil || !strings.Contains(err.Error(), `incompatible AgentFlow state (.agent/plan.lock.json schema_version "0.3.0")`) {
		t.Fatalf("err = %v", err)
	}
	if client.probes != 0 || caller.i != 0 || client.inits != 0 {
		t.Fatalf("guard refusal reached probe=%d model=%d init=%d", client.probes, caller.i, client.inits)
	}
	if after := snapshotAuditFixtureTree(t, filepath.Join(root, ".agent")); !reflect.DeepEqual(after, before) {
		t.Fatal("guard refusal changed .agent/")
	}
}

// -goal refuses a 0.x pristine scaffold before probing or calling the model.
func TestRunAgentflowAuthor_RefusesZeroXScaffoldBeforeProbe(t *testing.T) {
	root := t.TempDir()
	writeAgentFile(t, root, "plan.lock.json", `{"schema_version":"0.3.0","objective":"","steps":[],"locked":false}`)
	before := snapshotAuditFixtureTree(t, filepath.Join(root, ".agent"))
	// One queued response: scriptCaller only advances i when it pops one, so an
	// empty caller would read 0 even after a model call.
	caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
	sess := newTestSession(t, caller, root)
	client := &stubLocker{}
	err := runAgentflowAuthorWithClient(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, nil, sess, flags{goal: "x", goalSet: true}, root, client, nil)
	if err == nil || !strings.Contains(err.Error(), `incompatible AgentFlow state (.agent/plan.lock.json schema_version "0.3.0")`) {
		t.Fatalf("err = %v", err)
	}
	if client.probes != 0 || caller.i != 0 || client.inits != 0 {
		t.Fatalf("guard refusal reached probe=%d model=%d init=%d", client.probes, caller.i, client.inits)
	}
	if after := snapshotAuditFixtureTree(t, filepath.Join(root, ".agent")); !reflect.DeepEqual(after, before) {
		t.Fatal("guard refusal changed .agent/")
	}
}

// pristineScaffoldLock is the unlocked plan.lock.json AgentFlow 1.0's init
// writes (artifacts.py initial_files); writePristineScaffold adds the rest of
// that file list. None of it is execution history (#643 F3).
const pristineScaffoldLock = `{"allowed_files":[],"blocked_files":[],"evidence_ids":[],"invariants":[],"locked":false,"locked_at":null,"non_goals":[],"objective":"","risk_level":"low","rollback_plan":"","schema_version":"1.0.0","scope":[],"steps":[],"validation_gates":[]}`

func writePristineScaffold(t *testing.T, root string) {
	t.Helper()
	writeAgentFile(t, root, "plan.lock.json", pristineScaffoldLock)
	writeAgentFile(t, root, "drift-report.json", `{"status":"not_run"}`)
	writeAgentFile(t, root, "proof-pack.md", "# Proof Pack\n")
	writeAgentFile(t, root, "assumptions.json", `{"assumptions":[],"schema_version":"0.2.0"}`)
	for _, name := range []string{"amendments.jsonl", "capability-receipts.jsonl", "context-receipts.jsonl", "evidence.jsonl", "failures.jsonl", "review-runs.jsonl", "runtime-snapshots.jsonl"} {
		writeAgentFile(t, root, name, "")
	}
}

// TestAgentflowExecutionHistory pins #643 S1: AgentFlow's canonical execution
// artifacts are history; the scaffold, a locked plan, the workflow contract,
// evidence, and empty or blank-only ledgers are not.
func TestAgentflowExecutionHistory(t *testing.T) {
	type file struct{ name, body string }
	for _, tt := range []struct {
		name     string
		scaffold bool
		files    []file
		want     string
	}{
		{name: "no .agent"},
		{name: "pristine scaffold", scaffold: true},
		{name: "goal handoff", files: []file{{"plan.lock.json", v1Lock}, {"workflow.contract.json", `{"workflow_profile":"standard"}`}, {"evidence.jsonl", ""}}},
		{name: "empty ledgers", files: []file{{"step-runs.jsonl", ""}, {"command-receipts.jsonl", ""}, {"file-receipts.jsonl", ""}, {"verification-runs.jsonl", ""}}},
		{name: "blank-only ledgers", files: []file{{"step-runs.jsonl", "\n \n\r\n"}, {"command-receipts.jsonl", "\n"}, {"file-receipts.jsonl", " "}, {"verification-runs.jsonl", "\r\n"}}},
		{name: "contract only", files: []file{{"execution.contract.json", v1Contract}}, want: ".agent/execution.contract.json"},
		{name: "step runs", files: []file{{"step-runs.jsonl", v1Row + "\n"}}, want: ".agent/step-runs.jsonl has 1 entries"},
		{name: "command receipts", files: []file{{"command-receipts.jsonl", v1Row + "\n"}}, want: ".agent/command-receipts.jsonl has 1 entries"},
		{name: "file receipts", files: []file{{"file-receipts.jsonl", v1Row + "\n"}}, want: ".agent/file-receipts.jsonl has 1 entries"},
		{name: "verification runs", files: []file{{"verification-runs.jsonl", v1Row + "\n"}}, want: ".agent/verification-runs.jsonl has 1 entries"},
		{name: "count skips blanks, CRLF and no final newline", files: []file{{"step-runs.jsonl", v1Row + "\r\n\n" + v1Row + "\n \n" + v1Row}}, want: ".agent/step-runs.jsonl has 3 entries"},
		{name: "contract before ledgers", files: []file{{"step-runs.jsonl", v1Row + "\n"}, {"execution.contract.json", v1Contract}}, want: ".agent/execution.contract.json"},
		{name: "first ledger in order", files: []file{{"verification-runs.jsonl", v1Row + "\n"}, {"command-receipts.jsonl", v1Row + "\n" + v1Row + "\n"}}, want: ".agent/command-receipts.jsonl has 2 entries"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.scaffold {
				writePristineScaffold(t, root)
			}
			for _, f := range tt.files {
				writeAgentFile(t, root, f.name, f.body)
			}
			got, err := agentflowExecutionHistory(root)
			if err != nil || got != tt.want {
				t.Fatalf("agentflowExecutionHistory = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// runFreshTaskOverFixture runs task mode against root with a fake AgentFlow on
// PATH and returns the error and the fake's call log path.
func runFreshTaskOverFixture(t *testing.T, root string, f flags) (calls string, err error) {
	t.Helper()
	calls = fakeAgentflowOnPath(t, "1.0.0")
	if f.planPath == "" {
		planBytes, err := json.Marshal(agentflow.Compile(validTraceableIR()))
		if err != nil {
			t.Fatal(err)
		}
		f.planPath = filepath.Join(t.TempDir(), "plan.json")
		if err := os.WriteFile(f.planPath, planBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f.approveEdits, f.approveGates = true, true
	sess := &replSession{orch: agent.New(&scriptCaller{}, agent.ContextManager{})}
	var stdout, stderr bytes.Buffer
	return calls, runAgentflowTask(context.Background(), &stdout, &stderr, nil, sess, f, root)
}

// #643 T2: a fresh -plan refuses retained execution history before any
// AgentFlow call and leaves .agent/ byte-identical; R5's incompatible-state
// refusal keeps precedence; a missing .agent/ and the pristine scaffold still
// reach AgentFlow.
func TestRunAgentflowTask_FreshPlanRefusesExecutionHistory(t *testing.T) {
	const r5 = "this workspace holds incompatible AgentFlow state (%s unreadable); finish that run, or build its proof, with the AgentFlow version that wrote it, then move .agent/ aside before starting an AgentFlow 1.x run"
	const history = "refusing a fresh -plan run: .agent/ holds AgentFlow execution history (%s); run -agentflow-status to see whether -agentflow-resume can continue it, or move .agent/ aside to start over"
	type file struct{ name, body string }
	for _, tt := range []struct {
		name     string
		scaffold bool
		files    []file
		dirs     []string
		want     string // "" means the run must reach AgentFlow
	}{
		{name: "history: contract", files: []file{{"plan.lock.json", v1Lock}, {"execution.contract.json", v1Contract}},
			want: fmt.Sprintf(history, ".agent/execution.contract.json")},
		{name: "history: step runs", files: []file{{"plan.lock.json", v1Lock}, {"step-runs.jsonl", v1Row + "\n"}},
			want: fmt.Sprintf(history, ".agent/step-runs.jsonl has 1 entries")},
		{name: "precedence: empty contract", files: []file{{"plan.lock.json", v1Lock}, {"execution.contract.json", ""}},
			want: fmt.Sprintf(r5, ".agent/execution.contract.json")},
		{name: "precedence: malformed row", files: []file{{"step-runs.jsonl", "{\n"}},
			want: fmt.Sprintf(r5, ".agent/step-runs.jsonl:1")},
		{name: "precedence: contract is a directory", dirs: []string{"execution.contract.json"},
			want: fmt.Sprintf(r5, ".agent/execution.contract.json")},
		{name: "proceeds: no .agent"},
		{name: "proceeds: pristine scaffold", scaffold: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.scaffold {
				writePristineScaffold(t, root)
			}
			for _, f := range tt.files {
				writeAgentFile(t, root, f.name, f.body)
			}
			for _, d := range tt.dirs {
				if err := os.MkdirAll(filepath.Join(root, ".agent", d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var before map[string]auditFixtureEntry
			if tt.scaffold || len(tt.files)+len(tt.dirs) > 0 {
				before = snapshotAuditFixtureTree(t, filepath.Join(root, ".agent"))
			}
			calls, err := runFreshTaskOverFixture(t, root, flags{})
			if tt.want == "" {
				if err != nil && strings.Contains(err.Error(), "execution history") {
					t.Fatalf("history guard refused a fresh workspace: %v", err)
				}
				if _, statErr := os.Stat(calls); statErr != nil {
					t.Fatalf("run never reached AgentFlow (err=%v)", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v\nwant %s", err, tt.want)
			}
			if _, statErr := os.Stat(calls); !os.IsNotExist(statErr) {
				got, _ := os.ReadFile(calls)
				t.Fatalf("AgentFlow ran before the guard: %q", got)
			}
			if after := snapshotAuditFixtureTree(t, filepath.Join(root, ".agent")); !reflect.DeepEqual(after, before) {
				t.Fatal("refusal changed .agent/")
			}
		})
	}
}

// #643 T3: the -goal handoff state (locked plan, materialized workflow
// contract, saved task brief, no execution contract) passes the handoff
// bindings and the history guard when run the way -goal's "execute
// separately" line prints it: -plan .agent/plan.lock.json with -task-brief
// and -workflow-handoff (agentflow_author.go, the lockedPath branch).
func TestRunAgentflowTask_HandoffStatePassesHistoryGuard(t *testing.T) {
	root := t.TempDir()
	plan := agentflow.Compile(validTraceableIR())
	brief := agentflow.TaskBriefFromPlan(plan, "feature")
	recommendation := defaultWorkflowRecommendation()
	handoffPath, err := saveApprovedWorkflowHandoff(root, plan, brief, recommendation)
	if err != nil {
		t.Fatal(err)
	}
	briefPath, err := saveApprovedTaskBrief(root, brief)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := json.Marshal(recommendation.Contract)
	if err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, root, "workflow.contract.json", string(contract))
	planBytes, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var locked map[string]any
	if err := json.Unmarshal(planBytes, &locked); err != nil {
		t.Fatal(err)
	}
	locked["locked"] = true
	locked["locked_at"] = "2026-10-10T00:00:00+00:00"
	lockBytes, err := json.Marshal(locked)
	if err != nil {
		t.Fatal(err)
	}
	writeAgentFile(t, root, "plan.lock.json", string(lockBytes))
	writeAgentFile(t, root, "evidence.jsonl", "")

	calls, err := runFreshTaskOverFixture(t, root, flags{
		planPath: filepath.Join(root, ".agent", "plan.lock.json"), taskBriefPath: briefPath, workflowHandoffPath: handoffPath,
	})
	if err != nil && (strings.Contains(err.Error(), "execution history") || strings.Contains(err.Error(), "handoff") || strings.Contains(err.Error(), "task brief")) {
		t.Fatalf("handoff state was refused before AgentFlow: %v", err)
	}
	if _, statErr := os.Stat(calls); statErr != nil {
		t.Fatalf("handoff run never reached AgentFlow (err=%v)", err)
	}
}

// #643 T4: resume is the recovery path, so the fresh-run history guard must
// not refuse it.
func TestRunAgentflowTask_ResumeIsNotRefusedByHistoryGuard(t *testing.T) {
	root := t.TempDir()
	writeAgentFile(t, root, "plan.lock.json", v1Lock)
	writeAgentFile(t, root, "execution.contract.json", v1Contract)
	writeAgentFile(t, root, "step-runs.jsonl", v1Row+"\n")
	calls, err := runFreshTaskOverFixture(t, root, flags{agentflowResume: true})
	if err != nil && strings.Contains(err.Error(), "execution history") {
		t.Fatalf("resume was refused by the fresh-run guard: %v", err)
	}
	got, readErr := os.ReadFile(calls)
	if readErr != nil || !strings.Contains(string(got), "--version") {
		t.Fatalf("resume never reached the AgentFlow version probe: calls=%q err=%v runErr=%v", got, readErr, err)
	}
}

// #643 T5: -goal refuses retained execution history before any probe, model
// call or .agent/ write, whether the plan lock is absent or the pristine
// scaffold (both pass guardExistingPlan today). A locked plan without history
// keeps guardExistingPlan's own refusal.
func TestRunAgentflowAuthor_RefusesExecutionHistory(t *testing.T) {
	const history = "refusing to plan: .agent/ holds AgentFlow execution history (%s); run -agentflow-status to see whether -agentflow-resume can continue it, or move .agent/ aside to start over"
	for _, tt := range []struct {
		name     string
		scaffold bool
		lock     string
		ledger   [2]string // file, body
		want     string
	}{
		{name: "no lock, contract", ledger: [2]string{"execution.contract.json", v1Contract}, want: fmt.Sprintf(history, ".agent/execution.contract.json")},
		{name: "no lock, step runs", ledger: [2]string{"step-runs.jsonl", v1Row + "\n"}, want: fmt.Sprintf(history, ".agent/step-runs.jsonl has 1 entries")},
		{name: "scaffold, contract", scaffold: true, ledger: [2]string{"execution.contract.json", v1Contract}, want: fmt.Sprintf(history, ".agent/execution.contract.json")},
		{name: "scaffold, step runs", scaffold: true, ledger: [2]string{"step-runs.jsonl", v1Row + "\n"}, want: fmt.Sprintf(history, ".agent/step-runs.jsonl has 1 entries")},
		{name: "locked plan, history", lock: v1Lock, ledger: [2]string{"step-runs.jsonl", v1Row + "\n"}, want: fmt.Sprintf(history, ".agent/step-runs.jsonl has 1 entries")},
		{name: "0.x contract keeps R5", ledger: [2]string{"execution.contract.json", `{"schema_version":"0.3.0"}`}, want: "r5"},
		{name: "locked plan, no history", lock: v1Lock, want: "already locked"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.scaffold {
				writePristineScaffold(t, root)
			}
			if tt.lock != "" {
				writeAgentFile(t, root, "plan.lock.json", tt.lock)
			}
			if tt.ledger[0] != "" {
				writeAgentFile(t, root, tt.ledger[0], tt.ledger[1])
			}
			before := snapshotAuditFixtureTree(t, filepath.Join(root, ".agent"))
			// One queued response: scriptCaller only advances i when it pops one.
			caller := &scriptCaller{responses: []agent.ModelResult{submitPlanCall(validIRJSON(t))}}
			client := &stubLocker{}
			err := runAgentflowAuthorWithClient(context.Background(), &bytes.Buffer{}, &bytes.Buffer{}, nil, newTestSession(t, caller, root), flags{goal: "x", goalSet: true}, root, client, nil)
			switch tt.want {
			case "already locked":
				if err == nil || !strings.Contains(err.Error(), "is already locked") {
					t.Fatalf("err = %v, want the locked-plan refusal", err)
				}
			case "r5":
				if err == nil || !strings.HasPrefix(err.Error(), `this workspace holds incompatible AgentFlow state (.agent/execution.contract.json schema_version "0.3.0")`) {
					t.Fatalf("err = %v, want R5's incompatible-state refusal", err)
				}
			default:
				if err == nil || err.Error() != tt.want {
					t.Fatalf("err = %v\nwant %s", err, tt.want)
				}
			}
			if client.probes+client.workflowProbes+client.recommendations+client.inits+client.locks+client.materializations != 0 || len(client.sequence) != 0 || caller.i != 0 {
				t.Fatalf("refusal reached AgentFlow or the model: client=%+v model=%d", client, caller.i)
			}
			if after := snapshotAuditFixtureTree(t, filepath.Join(root, ".agent")); !reflect.DeepEqual(after, before) {
				t.Fatal("refusal changed .agent/")
			}
		})
	}
}

// #643 T7: on a case-insensitive filesystem an .AGENT directory is the same
// directory, so its history must be refused too (the #209 case-fold lesson).
func TestAgentflowExecutionHistory_CaseFoldedAgentDir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "probe"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "PROBE")); err != nil {
		t.Skip("case-sensitive filesystem")
	}
	if err := os.MkdirAll(filepath.Join(root, ".AGENT"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".AGENT", "execution.contract.json"), []byte(v1Contract), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := agentflowExecutionHistory(root); err != nil || got != ".agent/execution.contract.json" {
		t.Fatalf("agentflowExecutionHistory = %q, %v; want the case-folded contract", got, err)
	}
}
