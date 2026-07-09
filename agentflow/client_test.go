package agentflow

import (
	"context"
	"reflect"
	"testing"
)

func newTestClient(replies map[string]fakeReply) (*Client, *fakeRunner) {
	f := &fakeRunner{replies: replies}
	return NewClient(f, "/ws"), f
}

func TestClient_LockPlan_ArgvAndInvalid(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{
		"lock-plan": {stdout: []byte(`{"status":"invalid","errors":[{"code":"x","message":"m"}]}`), exit: 1},
	})
	err := c.LockPlan(context.Background(), "plan.json")
	ce, ok := err.(*CommandError)
	if !ok || ce.Exit != 1 || len(ce.Errors) != 1 || ce.Errors[0].Code != "x" {
		t.Fatalf("err = %v", err)
	}
	// lock-plan takes NO --root (Cmd.Dir handles it); it does take --from-json + --json.
	want := []string{"lock-plan", "--from-json", "plan.json", "--json"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestClient_ClaimStep_ArgvAndAttempt(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{
		"claim-step": {stdout: []byte(`{"attempt_id":"A1"}`), exit: 0},
	})
	att, err := c.ClaimStep(context.Background(), "P1")
	if err != nil || att != "A1" {
		t.Fatalf("att=%q err=%v", att, err)
	}
	want := []string{"claim-step", "P1", "--root", "/ws", "--agent", "golem", "--json"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestClient_NextStep_ParsesStepOrNull(t *testing.T) {
	c, _ := newTestClient(map[string]fakeReply{
		"next-step": {stdout: []byte(`{"id":"P1","action":"edit"}`), exit: 0},
	})
	id, err := c.NextStep(context.Background())
	if err != nil || id != "P1" {
		t.Fatalf("id=%q err=%v", id, err)
	}

	c, _ = newTestClient(map[string]fakeReply{
		"next-step": {stdout: []byte(`null`), exit: 0},
	})
	id, err = c.NextStep(context.Background())
	if err != nil || id != "" {
		t.Fatalf("done id=%q err=%v", id, err)
	}
}

func TestClient_RunGate_ArgvArgvAfterDashDash(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{"run": {exit: 0}})
	err := c.RunGate(context.Background(), "P1", "A1", "go test ./src", []string{"go", "test", "./src"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--root", "/ws", "--step", "P1", "--attempt", "A1",
		"--gate", "go test ./src", "--agent", "golem", "--confirm-risk", "--", "go", "test", "./src"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestClient_RecordFileChange_Argv(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{"record-file-change": {exit: 0}})
	if err := c.RecordFileChange(context.Background(), "P1", "A1", "src/a.go"); err != nil {
		t.Fatal(err)
	}
	want := []string{"record-file-change", "--root", "/ws", "--step", "P1",
		"--attempt", "A1", "--path", "src/a.go", "--agent", "golem", "--json"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestClient_FinishStep_ArgvIncludesAttempt(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{"finish-step": {exit: 0}})
	if err := c.FinishStep(context.Background(), "P1", "A1"); err != nil {
		t.Fatal(err)
	}
	want := []string{"finish-step", "P1", "--root", "/ws", "--attempt", "A1", "--agent", "golem", "--json"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestClient_FinishRun_StopReportsStoppedAt(t *testing.T) {
	c, _ := newTestClient(map[string]fakeReply{
		"finish-run": {stdout: []byte(`{"ok":false,"stopped_at":"verify-proof","diagnostics":["bad proof"]}`), exit: 1},
	})
	_, err := c.FinishRun(context.Background())
	fr, ok := err.(*FinishRunError)
	if !ok || fr.StoppedAt != "verify-proof" || len(fr.Diagnostics) != 1 {
		t.Fatalf("err = %#v", err)
	}
}

func TestClient_RecordEvidence_Argv(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{"record-evidence": {exit: 0}})
	e := EvidenceEntry{
		ID: "E1", Claim: "fixture evidence", Source: "evidence.json",
		Confidence: "high", Kind: "test", Supports: []string{"P1"},
	}
	if err := c.RecordEvidence(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	want := []string{"record-evidence", "--root", "/ws", "--id", "E1", "--claim", "fixture evidence",
		"--source", "evidence.json", "--confidence", "high", "--kind", "test", "--supports", "P1"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("argv = %v, want %v", f.calls[0], want)
	}
}
