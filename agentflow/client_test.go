package agentflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newTestClient(replies map[string]fakeReply) (*Client, *fakeRunner) {
	f := &fakeRunner{replies: replies}
	return NewClient(f, "/ws"), f
}

func TestClient_AggregateLedgers_UsesTypedVariantsAndExactArgv(t *testing.T) {
	inputs := []AggregationInput{{Root: "/worker-1", SourceID: "w1"}, {Root: "/worker-2", SourceID: "w2"}}
	tests := []struct {
		name          string
		dryRun        bool
		stdout        string
		exit          int
		wantCollision bool
		wantWritten   bool
	}{
		{"clean dry-run", true, `{"status":"ok","sources":[{"source_id":"w1"}],"collisions":[],"planned":{"step_runs":[]}}`, 0, false, false},
		{"dry-run collision", true, `{"status":"collision","sources":[],"collisions":[{"kind":"step_overlap"}],"planned":{}}`, 1, true, false},
		{"real collision", false, `{"status":"collision","sources":[],"collisions":[{"kind":"step_overlap"}],"planned":{}}`, 1, true, false},
		{"successful write", false, `{"status":"ok","sources":[{"source_id":"w1"}],"written":{"aggregation":".agent/aggregation.json"}}`, 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, f := newTestClient(map[string]fakeReply{
				"aggregate-ledgers": {stdout: []byte(tt.stdout), exit: tt.exit},
			})
			result, err := c.AggregateLedgers(context.Background(), inputs, "abc123", tt.dryRun)
			var collision *AggregationCollisionError
			if got := errors.As(err, &collision); got != tt.wantCollision {
				t.Fatalf("collision=%t err=%v, want %t", got, err, tt.wantCollision)
			}
			if !tt.wantCollision && err != nil {
				t.Fatal(err)
			}
			if tt.wantCollision {
				result = collision.Result
			}
			if len(result.Sources) == 0 && tt.name == "clean dry-run" {
				t.Fatalf("sources = %s", result.Sources)
			}
			if (len(result.Written) != 0) != tt.wantWritten {
				t.Fatalf("written = %s, want written=%t", result.Written, tt.wantWritten)
			}
			want := []string{"aggregate-ledgers",
				"--input", "/worker-1", "--source-id", "w1",
				"--input", "/worker-2", "--source-id", "w2",
				"--output", "/ws", "--base", "abc123"}
			if tt.dryRun {
				want = append(want, "--dry-run")
			}
			want = append(want, "--json")
			if !reflect.DeepEqual(f.calls[0], want) {
				t.Fatalf("argv = %v, want %v", f.calls[0], want)
			}
		})
	}
}

func TestClient_AggregateLedgers_FailsClosed(t *testing.T) {
	inputs := []AggregationInput{{Root: "/worker-1", SourceID: "w1"}}
	tests := []struct {
		name   string
		dryRun bool
		stdout string
		exit   int
		ce     bool
	}{
		{"malformed success", true, `{`, 0, false},
		{"unparseable nonzero", true, `{`, 1, true},
		{"exit two", true, `{"status":"ok","sources":[],"collisions":[],"planned":{}}`, 2, true},
		{"collision exit zero", true, `{"status":"collision","sources":[],"collisions":[],"planned":{}}`, 0, false},
		{"success exit one", false, `{"status":"ok","sources":[],"written":{}}`, 1, false},
		{"dry-run write variant", true, `{"status":"ok","sources":[],"written":{}}`, 0, false},
		{"write analysis variant", false, `{"status":"ok","sources":[],"collisions":[],"planned":{}}`, 0, false},
		{"collision write field", false, `{"status":"collision","sources":[],"collisions":[],"planned":{},"written":{}}`, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(map[string]fakeReply{
				"aggregate-ledgers": {stdout: []byte(tt.stdout), stderr: []byte("boom"), exit: tt.exit},
			})
			_, err := c.AggregateLedgers(context.Background(), inputs, "abc123", tt.dryRun)
			if err == nil {
				t.Fatal("AggregateLedgers unexpectedly succeeded")
			}
			var ce *CommandError
			if got := errors.As(err, &ce); got != tt.ce {
				t.Fatalf("CommandError=%t err=%v, want %t", got, err, tt.ce)
			}
			var collision *AggregationCollisionError
			if errors.As(err, &collision) {
				t.Fatalf("invalid aggregation result returned collision: %v", err)
			}
		})
	}

	for _, inputs := range [][]AggregationInput{
		nil,
		{{Root: "", SourceID: "w1"}},
		{{Root: "/worker-1", SourceID: ""}},
	} {
		c, f := newTestClient(nil)
		if _, err := c.AggregateLedgers(context.Background(), inputs, "abc123", true); err == nil {
			t.Fatalf("inputs=%+v unexpectedly succeeded", inputs)
		}
		if len(f.calls) != 0 {
			t.Fatalf("inputs=%+v invoked CLI: %v", inputs, f.calls)
		}
	}
}

func TestClient_NextAction_Argv(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{"next-action": {stdout: []byte(`{"state":"steps_pending"}`), exit: 0}})
	st, err := c.NextAction(context.Background())
	if err != nil || st.State != "steps_pending" {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	want := []string{"next-action", "--root", "/ws", "--json"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("argv=%v", f.calls[0])
	}
}

func TestClient_OwnedAgent_ArgvAndResumabilityProjection(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{
		"claim-step":         {stdout: []byte(`{"attempt_id":"A1"}`), exit: 0},
		"amend-step":         {stdout: []byte(`{"event":"amendment_started","step_id":"P1","attempt_id":"A2"}`), exit: 0},
		"record-file-change": {exit: 0},
		"run":                {exit: 0},
		"finish-step":        {exit: 0},
		"next-action": {stdout: []byte(`{
			"state":"steps_pending",
			"resumability":{
				"contract":{"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},
				"agent_id":"golem-w1",
				"attempt":null,
				"diagnostics":[],
				"step":{"id":"P1","state":"pending","completed":false},
				"lease":{"state":"not_applicable"}
			}
		}`), exit: 0},
	}}
	c := NewOwnedClient(f, "/ws", "golem-w1")
	if _, err := c.ClaimStep(context.Background(), "P1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AmendStep(context.Background(), "P1", []string{"RR#RF-1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.RecordFileChange(context.Background(), "P1", "A2", "a.go"); err != nil {
		t.Fatal(err)
	}
	if err := c.RunGate(context.Background(), "P1", "A2", "go test", []string{"go", "test"}); err != nil {
		t.Fatal(err)
	}
	if err := c.FinishStep(context.Background(), "P1", "A2"); err != nil {
		t.Fatal(err)
	}
	state, err := c.NextAction(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Resumability == nil || state.Resumability.Contract == nil ||
		state.Resumability.Contract.PlanSHA256 != "plan" ||
		state.Resumability.Contract.ExecutionContractSHA256 != "execution" ||
		!state.Resumability.Contract.Locked || state.Resumability.AgentID != "golem-w1" ||
		state.Resumability.Attempt != nil || len(state.Resumability.Diagnostics) != 0 {
		t.Fatalf("resumability = %+v", state.Resumability)
	}
	want := [][]string{
		{"claim-step", "P1", "--root", "/ws", "--agent", "golem-w1", "--json"},
		{"amend-step", "P1", "--root", "/ws", "--agent", "golem-w1", "--reason", "address review findings RR#RF-1", "--reason-code", "review_feedback", "--finding", "RR#RF-1", "--json"},
		{"record-file-change", "--root", "/ws", "--step", "P1", "--attempt", "A2", "--path", "a.go", "--agent", "golem-w1", "--json"},
		{"run", "--root", "/ws", "--step", "P1", "--attempt", "A2", "--gate", "go test", "--agent", "golem-w1", "--confirm-risk", "--", "go", "test"},
		{"finish-step", "P1", "--root", "/ws", "--attempt", "A2", "--agent", "golem-w1", "--json"},
		{"next-action", "--root", "/ws", "--json", "--agent", "golem-w1"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("argv = %v, want %v", f.calls, want)
	}
}

func TestClient_NextAction_ParsesAttemptAndNestedDiagnostics(t *testing.T) {
	c, _ := newTestClient(map[string]fakeReply{"next-action": {stdout: []byte(`{
		"resumability": {
			"contract": {"plan_sha256":"plan","locked":true,"execution_contract_sha256":"execution"},
			"agent_id":"golem-w1",
			"attempt":{"id":"A1","open":true},
			"diagnostics":[{"code":"state_invalid","message":"bad ledger","artifact":".agent/step-runs.jsonl"}]
		}
	}`), exit: 0}})
	state, err := c.NextAction(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	projection := state.Resumability
	if projection == nil || projection.Attempt == nil || projection.Attempt.ID != "A1" || !projection.Attempt.Open ||
		len(projection.Diagnostics) != 1 || projection.Diagnostics[0] != (ResumabilityDiagnostic{
		Code: "state_invalid", Message: "bad ledger", Artifact: ".agent/step-runs.jsonl",
	}) {
		t.Fatalf("resumability = %+v", projection)
	}
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

func TestClient_Doctor_SurfacesFindings(t *testing.T) {
	c, _ := newTestClient(map[string]fakeReply{
		"doctor": {stdout: []byte(`{"contract":null,"findings":[{"message":".agent/execution.contract.json is missing","severity":"error"}],"status":"failed"}`), exit: 1},
	})
	err := c.Doctor(context.Background())
	if err == nil || !strings.Contains(err.Error(), "execution.contract.json is missing") {
		t.Fatalf("doctor error must surface findings message, got %v", err)
	}
}

func TestClient_FinishStep_SurfacesDiagnostics(t *testing.T) {
	c, _ := newTestClient(map[string]fakeReply{
		"finish-step": {stdout: []byte(`{"completed":false,"verified":false,"diagnostics":["gate go test failed"]}`), exit: 1},
	})
	err := c.FinishStep(context.Background(), "P1", "A1")
	if err == nil || !strings.Contains(err.Error(), "gate go test failed") {
		t.Fatalf("finish-step error must surface diagnostics, got %v", err)
	}
}

func TestClient_Init_Argv(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{"init": {exit: 0}})
	if err := c.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"init", "--root", "/ws"}
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

func TestClient_NextStep_MalformedSuccessNamesSubcommand(t *testing.T) {
	c, _ := newTestClient(map[string]fakeReply{
		"next-step": {stdout: []byte(`{"id":`), exit: 0}, // truncated JSON, exit 0
	})
	_, err := c.NextStep(context.Background())
	if err == nil || !strings.Contains(err.Error(), "next-step") {
		t.Fatalf("parse error must name the subcommand, got %v", err)
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

func TestClient_FinishRun_SurfacesAuthoritativeFailedProofCheck(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	proof := `{"checks":[{"id":"required_review_satisfied","status":"failed","message":"review_depth=deep requires a review run at depth deep"},{"id":"other","status":"passed","message":"ok"}]}`
	if err := os.WriteFile(filepath.Join(root, ".agent", "proof-pack.json"), []byte(proof), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{replies: map[string]fakeReply{
		"finish-run": {stdout: []byte(`{"ok":false,"stopped_at":"build-proof","diagnostics":["created .agent/proof-pack.json"]}`), exit: 1},
	}}
	c := NewClient(f, root)
	_, err := c.FinishRun(context.Background())
	if err == nil || !strings.Contains(err.Error(), "required_review_satisfied: review_depth=deep requires a review run at depth deep") {
		t.Fatalf("finish-run error omitted authoritative proof requirement: %v", err)
	}
}

func TestClient_FinishRun_DoesNotSurfaceStaleProofWhenBuildStopsBeforeWrite(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	proof := `{"checks":[{"id":"stale","status":"failed","message":"old run failure"}]}`
	if err := os.WriteFile(filepath.Join(root, ".agent", "proof-pack.json"), []byte(proof), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{replies: map[string]fakeReply{
		"finish-run": {stdout: []byte(`{"ok":false,"stopped_at":"build-proof","diagnostics":["invalid requirement traceability"]}`), exit: 1},
	}}

	_, err := NewClient(f, root).FinishRun(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid requirement traceability") || strings.Contains(err.Error(), "old run failure") {
		t.Fatalf("finish-run mixed a stale proof pack into a pre-write build failure: %v", err)
	}
}

func TestClient_FinishRun_DoesNotSurfaceStaleProofBeforeProofStage(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	proof := `{"checks":[{"id":"stale","status":"failed","message":"old run failure"}]}`
	if err := os.WriteFile(filepath.Join(root, ".agent", "proof-pack.json"), []byte(proof), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{replies: map[string]fakeReply{
		"finish-run": {stdout: []byte(`{"ok":false,"stopped_at":"gate:tests","diagnostics":["current gate failed"]}`), exit: 1},
	}}
	_, err := NewClient(f, root).FinishRun(context.Background())
	if err == nil || !strings.Contains(err.Error(), "current gate failed") || strings.Contains(err.Error(), "old run failure") {
		t.Fatalf("finish-run mixed stale proof diagnostics into current stage: %v", err)
	}
}

func TestClient_FinishRun_SuccessReturnsProofPath(t *testing.T) {
	c, _ := newTestClient(map[string]fakeReply{
		"finish-run": {stdout: []byte(`{"ok":true,"gates":[]}`), exit: 0},
	})
	path, err := c.FinishRun(context.Background())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if want := filepath.Join("/ws", ".agent", "proof-pack.json"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestClient_FinishRun_OKFalseZeroExitStillStops(t *testing.T) {
	// The ok=false half of `exit != 0 || !r.OK`, exercised with exit 0.
	c, _ := newTestClient(map[string]fakeReply{
		"finish-run": {stdout: []byte(`{"ok":false,"stopped_at":"gate:x","diagnostics":["d"]}`), exit: 0},
	})
	_, err := c.FinishRun(context.Background())
	var fr *FinishRunError
	if !errors.As(err, &fr) || fr.StoppedAt != "gate:x" {
		t.Fatalf("err = %#v, want *FinishRunError stopped at gate:x", err)
	}
}

func TestClient_FinishRun_UnparseableNonzeroExitIsCommandError(t *testing.T) {
	c, _ := newTestClient(map[string]fakeReply{
		"finish-run": {stdout: []byte(`not json`), stderr: []byte("boom"), exit: 2},
	})
	_, err := c.FinishRun(context.Background())
	var ce *CommandError
	if !errors.As(err, &ce) || ce.Cmd != "finish-run" || ce.Exit != 2 {
		t.Fatalf("err = %#v, want *CommandError finish-run exit 2", err)
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

func TestClient_RecordReview_ParsesProjectionAndUsesJSON(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{
		"record-review": {stdout: []byte(`{
			"review_run_id":"RR-20260714T120000Z-1234abcd",
			"gate_status":"fail",
			"active_blocking":["RF-1"],
			"amendment_ready":true,
			"findings":{"index":[{
				"finding_id":"RF-1","severity":"high","status":"accepted",
				"owning_step":"P1","claim":"proof omits a receipt",
				"location":{"path":"src/a.go","line":7,"line_end":9},
				"suggested_fix":"record the receipt"
			}]}
		}`), exit: 0},
	})

	run, err := c.RecordReview(context.Background(), "docs/review-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if run.ReviewRunID != "RR-20260714T120000Z-1234abcd" || !run.AmendmentReady || len(run.Findings.Index) != 1 {
		t.Fatalf("run = %+v", run)
	}
	finding := run.Findings.Index[0]
	if finding.OwningStep != "P1" || finding.Location == nil || finding.Location.LineEnd != 9 {
		t.Fatalf("finding = %+v", finding)
	}
	want := []string{"record-review", "--root", "/ws", "--manifest", "docs/review-manifest.json", "--json"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestClient_RecordReview_RejectsMalformedRequiredProjectionFields(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"review run id", `{"gate_status":"pass","active_blocking":[],"amendment_ready":false,"findings":{"index":[]}}`, "review_run_id"},
		{"amendment readiness", `{"review_run_id":"RR-20260714T120000Z-1234abcd","gate_status":"pass","active_blocking":[],"findings":{"index":[]}}`, "amendment_ready"},
		{"gate status", `{"review_run_id":"RR-20260714T120000Z-1234abcd","active_blocking":[],"amendment_ready":false,"findings":{"index":[]}}`, "gate_status"},
		{"invalid gate status", `{"review_run_id":"RR-20260714T120000Z-1234abcd","gate_status":"unknown","active_blocking":[],"amendment_ready":false,"findings":{"index":[]}}`, "gate_status"},
		{"active blockers", `{"review_run_id":"RR-20260714T120000Z-1234abcd","gate_status":"pass","amendment_ready":false,"findings":{"index":[]}}`, "active_blocking"},
		{"findings", `{"review_run_id":"RR-20260714T120000Z-1234abcd","gate_status":"pass","active_blocking":[],"amendment_ready":false}`, "findings"},
		{"ready finding index", `{"review_run_id":"RR-20260714T120000Z-1234abcd","gate_status":"pass","active_blocking":[],"amendment_ready":true,"findings":{}}`, "findings.index"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(map[string]fakeReply{
				"record-review": {stdout: []byte(tt.json), exit: 0},
			})
			if _, err := c.RecordReview(context.Background(), "review.json"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want missing %s", err, tt.want)
			}
		})
	}
}

func TestClient_RecordReview_AllowsLegacyProjectionWithoutFindingIndex(t *testing.T) {
	c, _ := newTestClient(map[string]fakeReply{
		"record-review": {stdout: []byte(`{
			"review_run_id":"RR-20260714T120000Z-1234abcd",
			"gate_status":"pass",
			"active_blocking":[],
			"amendment_ready":false,
			"findings":{}
		}`), exit: 0},
	})
	run, err := c.RecordReview(context.Background(), "legacy-review.json")
	if err != nil {
		t.Fatal(err)
	}
	if run.AmendmentReady || len(run.Findings.Index) != 0 {
		t.Fatalf("run = %+v, want empty display-only legacy projection", run)
	}
}

func TestClient_AmendStep_PreservesEveryFindingReference(t *testing.T) {
	c, f := newTestClient(map[string]fakeReply{
		"amend-step": {stdout: []byte(`{"event":"amendment_started","step_id":"P1","attempt_id":"A2"}`), exit: 0},
	})
	refs := []string{"RR-20260714T120000Z-1234abcd#RF-1", "RR-20260714T120000Z-1234abcd#RF-2"}

	attempt, err := c.AmendStep(context.Background(), "P1", refs)
	if err != nil || attempt != "A2" {
		t.Fatalf("attempt=%q err=%v", attempt, err)
	}
	want := []string{
		"amend-step", "P1", "--root", "/ws", "--agent", "golem",
		"--reason", "address review findings " + strings.Join(refs, ", "),
		"--reason-code", "review_feedback",
		"--finding", refs[0], "--finding", refs[1], "--json",
	}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("argv = %v, want %v", f.calls[0], want)
	}
}

func TestClient_AmendStep_RejectsMalformedSuccessProjection(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"empty object", `{}`},
		{"wrong event", `{"event":"claimed","step_id":"P1","attempt_id":"A2"}`},
		{"wrong step", `{"event":"amendment_started","step_id":"P2","attempt_id":"A2"}`},
		{"empty attempt", `{"event":"amendment_started","step_id":"P1","attempt_id":""}`},
		{"malformed attempt", `{"event":"amendment_started","step_id":"P1","attempt_id":"not-an-attempt"}`},
		{"padded attempt", `{"event":"amendment_started","step_id":"P1","attempt_id":" A2 "}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(map[string]fakeReply{
				"amend-step": {stdout: []byte(tt.json), exit: 0},
			})
			if attempt, err := c.AmendStep(context.Background(), "P1", []string{"RR-20260714T120000Z-1234abcd#RF-1"}); err == nil {
				t.Fatalf("attempt = %q, want malformed success error", attempt)
			}
		})
	}
}
