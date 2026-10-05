package main

import (
	"bytes"
	"context"
	"encoding/json"
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
