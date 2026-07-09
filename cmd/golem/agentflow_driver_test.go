package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakeAF records the ordered sequence of driver->agentflow calls.
type fakeAF struct {
	seq       []string
	nextSteps []string // ids to hand out, then "" (done)
	i         int
}

func (f *fakeAF) Probe(context.Context) error { f.seq = append(f.seq, "probe"); return nil }
func (f *fakeAF) Init(context.Context) error  { f.seq = append(f.seq, "init"); return nil }
func (f *fakeAF) LockPlan(_ context.Context, p string) error {
	f.seq = append(f.seq, "lock:"+p)
	return nil
}
func (f *fakeAF) InitExecution(context.Context) error { f.seq = append(f.seq, "init-exec"); return nil }
func (f *fakeAF) Doctor(context.Context) error        { f.seq = append(f.seq, "doctor"); return nil }
func (f *fakeAF) NextStep(context.Context) (string, error) {
	f.seq = append(f.seq, "next-step")
	if f.i < len(f.nextSteps) {
		s := f.nextSteps[f.i]
		f.i++
		return s, nil
	}
	return "", nil
}
func (f *fakeAF) ClaimStep(_ context.Context, id string) (string, error) {
	f.seq = append(f.seq, "claim:"+id)
	return "A-" + id, nil
}
func (f *fakeAF) RunGate(_ context.Context, step, attempt, gate string, argv []string) error {
	f.seq = append(f.seq, "gate:"+step+":"+gate)
	return nil
}
func (f *fakeAF) FinishStep(_ context.Context, id, attempt string) error {
	f.seq = append(f.seq, "finish-step:"+id+":"+attempt)
	return nil
}
func (f *fakeAF) FinishRun(context.Context) (string, error) {
	f.seq = append(f.seq, "finish-run")
	return "proof-pack.md", nil
}
func (f *fakeAF) RecordFileChange(context.Context, string, string, string) error { return nil }
func (f *fakeAF) RecordEvidence(_ context.Context, e agentflow.EvidenceEntry) error {
	f.seq = append(f.seq, "evidence:"+e.ID)
	return nil
}
func (f *fakeAF) NextAction(context.Context) (agentflow.NextActionState, error) {
	return agentflow.NextActionState{}, nil
}

func TestDriver_HappyPathOrdering(t *testing.T) {
	af := &fakeAF{nextSteps: []string{"P1"}}
	plan := &agentflow.Plan{Steps: []agentflow.Step{{
		ID: "P1", Files: []string{"src/a.go"},
		Validation: []string{"go test"}, Gates: []agentflow.Gate{{Kind: "command", Run: []string{"go", "test"}}},
	}}}
	plan.AllowedFiles = []string{"src/*"}

	// runStep is scripted to "succeed" without a real model.
	runStep := func(ctx context.Context, step agentflow.Step, attempt string) error { return nil }

	d := &driver{
		af: af, plan: plan, planPath: "plan.json", runStep: runStep,
		evidence: []agentflow.EvidenceEntry{{ID: "E1", Claim: "fixture", Source: "evidence.json"}},
	}
	proof, err := d.run(context.Background())
	if err != nil || proof != "proof-pack.md" {
		t.Fatalf("proof=%q err=%v", proof, err)
	}
	want := []string{
		"probe", "init", "evidence:E1", "lock:plan.json", "init-exec", "doctor",
		"next-step", "claim:P1", "gate:P1:go test", "finish-step:P1:A-P1",
		"next-step", "finish-run",
	}
	if !equalSeq(af.seq, want) {
		t.Fatalf("seq =\n%v\nwant\n%v", af.seq, want)
	}
}

func TestTaskApprover_ApprovesWhenEnabled(t *testing.T) {
	if ok, err := taskApprover(true).Approve(context.Background(), provider.ToolCall{}, ""); !ok || err != nil {
		t.Fatalf("enabled: ok=%v err=%v", ok, err)
	}
	if ok, err := taskApprover(false).Approve(context.Background(), provider.ToolCall{}, ""); ok || err != nil {
		t.Fatalf("disabled: ok=%v err=%v", ok, err)
	}
}

func TestReadEvidenceSidecar(t *testing.T) {
	tests := []struct {
		name        string
		write       bool // false => empty-path case (no file)
		content     string
		wantLen     int
		wantFirstID string
		wantErr     bool
	}{
		{name: "empty path", write: false, wantLen: 0},
		{name: "single object", write: true, content: `{"id":"E1","claim":"c","source":"s"}`, wantLen: 1, wantFirstID: "E1"},
		{name: "array of two", write: true, content: `[{"id":"E1","claim":"c1","source":"s1"},{"id":"E2","claim":"c2","source":"s2"}]`, wantLen: 2, wantFirstID: "E1"},
		{name: "missing source", write: true, content: `{"id":"E1","claim":"c","source":""}`, wantErr: true},
		{name: "malformed json", write: true, content: `{not json`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := ""
			if tt.write {
				path = filepath.Join(t.TempDir(), "evidence.json")
				if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := readEvidenceSidecar(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got entries=%v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tt.wantLen {
				t.Fatalf("len=%d want %d (%v)", len(got), tt.wantLen, got)
			}
			if tt.wantFirstID != "" && got[0].ID != tt.wantFirstID {
				t.Fatalf("first id=%q want %q", got[0].ID, tt.wantFirstID)
			}
		})
	}
}

// TestRunAgentflowTask_RequiresApprovalFlags pins the fail-before-exec invariant:
// a headless run missing either approval class errors before building the runner
// or touching agentflow (no binary is on PATH here).
func TestRunAgentflowTask_RequiresApprovalFlags(t *testing.T) {
	planJSON := `{"steps":[{"id":"P1","files":["a.go"],"validation":["go test"],"gates":[{"kind":"command","run":["go","test"]}]}]}`
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planPath, []byte(planJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name         string
		approveEdits bool
		approveGates bool
		wantContains string
	}{
		{"missing edits", false, true, "approve-plan-edits"},
		{"missing gates", true, false, "approve-plan-gates"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			f := flags{planPath: planPath, approveEdits: tc.approveEdits, approveGates: tc.approveGates}
			err := runAgentflowTask(context.Background(), &stdout, &stderr, nil, &replSession{}, f, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.wantContains) {
				t.Fatalf("err=%v, want contains %q", err, tc.wantContains)
			}
		})
	}
}

func TestResolveTaskPlanPath(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	got, err := resolveTaskPlanPath(filepath.Join("plans", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "plans", "plan.json"); got != want {
		t.Fatalf("relative path = %q, want %q", got, want)
	}

	abs := filepath.Join(t.TempDir(), "plan.json")
	got, err = resolveTaskPlanPath(abs)
	if err != nil {
		t.Fatal(err)
	}
	if got != abs {
		t.Fatalf("absolute path = %q, want unchanged %q", got, abs)
	}
}

func equalSeq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
