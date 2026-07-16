package agentflow

import (
	"context"
	"strings"
	"testing"
)

func TestProbe_OKWhenAllPresent(t *testing.T) {
	help := "usage: agentflow {init,init-execution,lock-plan,record-file-change,run,finish-step,finish-run,next-step,next-action,doctor,status}"
	f := &fakeRunner{replies: probeReplies(help)}
	if err := NewClient(f, "/ws").Probe(context.Background()); err != nil {
		t.Fatalf("Probe = %v", err)
	}
}

func probeReplies(topHelp string) map[string]fakeReply {
	allFlags := []byte("--root --json --from-json --agent --step --attempt --path --gate --confirm-risk")
	replies := map[string]fakeReply{
		"--version": {stdout: []byte("agentflow 0.4.0\n")},
		"--help":    {stdout: []byte(topHelp)},
	}
	for _, sub := range []string{
		"init", "lock-plan", "init-execution", "doctor", "next-step", "claim-step",
		"record-file-change", "run", "finish-step", "finish-run", "next-action", "status",
	} {
		replies[sub] = fakeReply{stdout: allFlags}
	}
	return replies
}

func TestProbe_FailsOnMissingSubcommand(t *testing.T) {
	help := "usage: agentflow {init,doctor}" // lock-plan etc missing
	f := &fakeRunner{replies: map[string]fakeReply{
		"--version": {stdout: []byte("agentflow 0.4.0\n")},
		"--help":    {stdout: []byte(help)},
	}}
	err := NewClient(f, "/ws").Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lock-plan") {
		t.Fatalf("expected missing-subcommand error, got %v", err)
	}
}

func TestProbe_FailsOnMissingRequiredFlag(t *testing.T) {
	help := "usage: agentflow {init,init-execution,lock-plan,record-file-change,run,finish-step,finish-run,next-step,next-action,doctor,status}"
	replies := probeReplies(help)
	replies["lock-plan"] = fakeReply{stdout: []byte("usage: lock-plan [--json]\n")} // missing --from-json
	f := &fakeRunner{replies: replies}
	err := NewClient(f, "/ws").Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lock-plan --from-json") {
		t.Fatalf("expected missing flag error, got %v", err)
	}
}

func TestProbe_FailsOnMissingInitSubcommand(t *testing.T) {
	help := "usage: agentflow {init-execution,lock-plan,record-file-change,run,finish-step,finish-run,next-step,next-action,doctor,status}"
	replies := probeReplies(help)
	// Simulate standalone `init` removed: `init --help` errors (no --root in usage).
	replies["init"] = fakeReply{stdout: []byte("usage: agentflow [-h] ...\nagentflow: error: argument command: invalid choice: 'init'\n"), exit: 2}
	f := &fakeRunner{replies: replies}
	if err := NewClient(f, "/ws").Probe(context.Background()); err == nil {
		t.Fatal("expected probe to fail when standalone init is unavailable")
	}
}

func TestProbe_FailsOnOldVersion(t *testing.T) {
	f := &fakeRunner{replies: map[string]fakeReply{"--version": {stdout: []byte("agentflow 0.3.0\n")}}}
	if err := NewClient(f, "/ws").Probe(context.Background()); err == nil {
		t.Fatal("expected version-too-old error")
	}
}

func TestProbeReview_RequiresReviewCommandsWithoutChangingBaseProbe(t *testing.T) {
	baseHelp := "usage: agentflow {init,init-execution,lock-plan,record-file-change,run,finish-step,finish-run,next-step,next-action,doctor,status}"
	base := &fakeRunner{replies: probeReplies(baseHelp)}
	if err := NewClient(base, "/ws").Probe(context.Background()); err != nil {
		t.Fatalf("base Probe = %v", err)
	}
	if err := NewClient(base, "/ws").ProbeReview(context.Background()); err == nil || !strings.Contains(err.Error(), "record-review") {
		t.Fatalf("ProbeReview missing command error = %v", err)
	}

	reviewHelp := baseHelp[:len(baseHelp)-1] + ",record-review,amend-step}"
	replies := probeReplies(reviewHelp)
	replies["record-review"] = fakeReply{stdout: []byte("--root --manifest --json")}
	replies["amend-step"] = fakeReply{stdout: []byte("--root --agent --reason --reason-code --finding --json")}
	if err := NewClient(&fakeRunner{replies: replies}, "/ws").ProbeReview(context.Background()); err != nil {
		t.Fatalf("ProbeReview = %v", err)
	}
}

func TestProbeWorkflow_RequiresStableRecommendationAndMaterializationSurface(t *testing.T) {
	baseHelp := "usage: agentflow {init,workflow-contract}"
	missing := &fakeRunner{replies: map[string]fakeReply{"--help": {stdout: []byte(baseHelp)}}}
	if err := NewClient(missing, "/ws").ProbeWorkflow(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "recommend-workflow") || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("missing command error = %v", err)
	}

	help := "usage: agentflow {init,recommend-workflow,workflow-contract}"
	replies := map[string]fakeReply{
		"--help":             {stdout: []byte(help)},
		"recommend-workflow": {stdout: []byte("--stdin --json --selected-profile --reason")},
		"workflow-contract":  {stdout: []byte("--root --from-json")},
	}
	if err := NewClient(&fakeRunner{replies: replies}, "/ws").ProbeWorkflow(context.Background()); err != nil {
		t.Fatalf("ProbeWorkflow = %v", err)
	}

	replies["recommend-workflow"] = fakeReply{stdout: []byte("--stdin --json --selected-profile")}
	if err := NewClient(&fakeRunner{replies: replies}, "/ws").ProbeWorkflow(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "--reason") {
		t.Fatalf("missing flag error = %v", err)
	}
}
