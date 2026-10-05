package agentflow

import (
	"context"
	"errors"
	"reflect"
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
	allFlags := []byte("--root --json --from-json --agent --step --attempt --path --gate --confirm-risk --reason")
	replies := map[string]fakeReply{
		"--version": {stdout: []byte("agentflow 1.0.0\n")},
		"--help":    {stdout: []byte(topHelp)},
	}
	for _, sub := range []string{
		"init", "lock-plan", "init-execution", "doctor", "next-step", "claim-step",
		"record-file-change", "run", "finish-step", "block-step", "finish-run", "next-action", "status",
	} {
		replies[sub] = fakeReply{stdout: allFlags}
	}
	return replies
}

func TestProbe_FailsOnMissingSubcommand(t *testing.T) {
	help := "usage: agentflow {init,doctor}" // lock-plan etc missing
	f := &fakeRunner{replies: map[string]fakeReply{
		"--version": {stdout: []byte("agentflow 1.0.0\n")},
		"--help":    {stdout: []byte(help)},
	}}
	err := NewClient(f, "/ws").Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lock-plan") || !strings.HasSuffix(err.Error(), "(upgrade Agentflow)") {
		t.Fatalf("expected missing-subcommand error, got %v", err)
	}
}

func TestProbe_FailsOnMissingRequiredFlag(t *testing.T) {
	help := "usage: agentflow {init,init-execution,lock-plan,record-file-change,run,finish-step,finish-run,next-step,next-action,doctor,status}"
	replies := probeReplies(help)
	replies["lock-plan"] = fakeReply{stdout: []byte("usage: lock-plan [--json]\n")} // missing --from-json
	f := &fakeRunner{replies: replies}
	err := NewClient(f, "/ws").Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "lock-plan --from-json") || !strings.HasSuffix(err.Error(), "(upgrade Agentflow)") {
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

func TestProbeParallel_RequiresNextActionAgentWithoutChangingBaseProbe(t *testing.T) {
	help := "usage: agentflow {init,init-execution,lock-plan,record-file-change,run,finish-step,finish-run,next-step,next-action,doctor,status}"
	replies := probeReplies(help)
	replies["next-action"] = fakeReply{stdout: []byte("--root --json")}
	c := NewClient(&fakeRunner{replies: replies}, "/ws")
	if err := c.Probe(context.Background()); err != nil {
		t.Fatalf("base Probe = %v", err)
	}
	if err := c.ProbeParallel(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "next-action --agent unavailable (upgrade Agentflow)") ||
		strings.Contains(err.Error(), "0.4.0") {
		t.Fatalf("parallel probe diagnostic = %v", err)
	}
}

func TestProbeParallel_RequiresAggregateLedgersAndEveryUsedFlag(t *testing.T) {
	help := "usage: agentflow {aggregate-ledgers,next-action}"
	replies := map[string]fakeReply{
		"--help":            {stdout: []byte(help)},
		"next-action":       {stdout: []byte("--root --agent --json")},
		"aggregate-ledgers": {stdout: []byte("--input --source-id --output --base --dry-run --json")},
	}
	if err := NewClient(&fakeRunner{replies: replies}, "/ws").ProbeParallel(context.Background()); err != nil {
		t.Fatalf("ProbeParallel = %v", err)
	}

	for _, missing := range []string{"aggregate-ledgers", "--input", "--source-id", "--output", "--base", "--dry-run", "--json"} {
		t.Run(missing, func(t *testing.T) {
			copy := map[string]fakeReply{}
			for key, reply := range replies {
				copy[key] = reply
			}
			if missing == "aggregate-ledgers" {
				copy["--help"] = fakeReply{stdout: []byte("usage: agentflow {next-action}")}
			} else {
				copy["aggregate-ledgers"] = fakeReply{stdout: []byte(strings.ReplaceAll(string(replies["aggregate-ledgers"].stdout), missing, ""))}
			}
			err := NewClient(&fakeRunner{replies: copy}, "/ws").ProbeParallel(context.Background())
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("missing %s error = %v", missing, err)
			}
			if !strings.HasSuffix(err.Error(), "(upgrade Agentflow)") {
				t.Fatalf("missing %s error = %v, want the upgrade hint", missing, err)
			}
		})
	}
}

func TestProbeParallel_RejectsLookalikeHelpTokens(t *testing.T) {
	tests := []struct {
		name    string
		topHelp string
		next    string
	}{
		{
			name:    "subcommand prefix",
			topHelp: "usage: agentflow {aggregate-ledgers-preview,next-action}",
			next:    "--root --agent --json",
		},
		{
			name:    "flag suffix",
			topHelp: "usage: agentflow {aggregate-ledgers,next-action}",
			next:    "--root --agent-id --json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replies := map[string]fakeReply{
				"--help":            {stdout: []byte(tt.topHelp)},
				"next-action":       {stdout: []byte(tt.next)},
				"aggregate-ledgers": {stdout: []byte("--input --source-id --output --base --dry-run --json")},
			}
			if err := NewClient(&fakeRunner{replies: replies}, "/ws").ProbeParallel(context.Background()); err == nil {
				t.Fatal("ProbeParallel unexpectedly accepted lookalike help token")
			}
		})
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

func TestProbe_RequiresBlockStepFlags(t *testing.T) {
	help := "usage: agentflow {init,init-execution,lock-plan,record-file-change,run,finish-step,finish-run,next-step,next-action,doctor,status}"
	required := []string{"--root", "--attempt", "--reason", "--agent", "--json"}
	for _, missing := range required {
		t.Run("missing "+missing, func(t *testing.T) {
			var present []string
			for _, flag := range required {
				if flag != missing {
					present = append(present, flag)
				}
			}
			replies := probeReplies(help)
			replies["block-step"] = fakeReply{stdout: []byte("usage: block-step " + strings.Join(present, " ") + "\n")}
			err := NewClient(&fakeRunner{replies: replies}, "/ws").Probe(context.Background())
			if err == nil || !strings.Contains(err.Error(), "block-step "+missing) {
				t.Fatalf("expected missing block-step %s error, got %v", missing, err)
			}
		})
	}
	t.Run("help fails", func(t *testing.T) {
		replies := probeReplies(help)
		replies["block-step"] = fakeReply{stdout: []byte("agentflow: error: invalid choice: 'block-step'\n"), exit: 2}
		err := NewClient(&fakeRunner{replies: replies}, "/ws").Probe(context.Background())
		if err == nil || !strings.Contains(err.Error(), "agentflow block-step --help failed") {
			t.Fatalf("expected block-step help failure, got %v", err)
		}
	})
}

const fullProbeHelp = "usage: agentflow {init,init-execution,lock-plan,record-file-change,run,finish-step,finish-run,next-step,next-action,doctor,status}"

// TestCheckVersion pins the #612 range: AgentFlow 1.x only. Every rejection is
// a *VersionError, so status may show it where it hides runner errors.
func TestCheckVersion(t *testing.T) {
	for _, tt := range []struct{ out, want string }{
		{"agentflow 1.0.0\n", ""},
		{"agentflow 1.7.3\n", ""},
		{"agentflow 1.0.0rc1\n", ""},
		{"agentflow 1.0.0.dev1+gabc\n", ""},
		{"agentflow 0.3.0\n", "agentflow 0.3 is too old; need >= 1.0"},
		{"agentflow 0.4.0\n", "agentflow 0.4 is too old; need >= 1.0"},
		{"agentflow 2.0.0\n", "agentflow 2.0 is newer than this Golem supports; need 1.x"},
		{"agentflow x.0.0\n", `cannot parse agentflow version "x.0.0"`},
		{"agentflow 1.x\n", `cannot parse agentflow version "1.x"`},
		{"agentflow 1.bad.0\n", `cannot parse agentflow version "1.bad.0"`},
		{"agentflow 1\n", `cannot parse agentflow version "1"`},
		{"agentflow 1.0rc1\n", `cannot parse agentflow version "1.0rc1"`},
		{"agentflow +1.0.0\n", `cannot parse agentflow version "+1.0.0"`},
		{"agentflow 1.-0.0\n", `cannot parse agentflow version "1.-0.0"`},
		// %q is the only sanitizer for text that reaches stderr raw later (#612).
		{"agentflow 1.\x1b[2J\n", `cannot parse agentflow version "1.\x1b[2J"`},
		{"agentflow\n", `cannot parse agentflow version from "agentflow"`},
		// Only the first line is the version; later lines (warnings, paths) are
		// neither parsed nor echoed (#612).
		{"agentflow 1.0.0\nwarning: deprecated /Users/x/y\n", ""},
		{"\n  agentflow 1.0.0 (python 3.12)\n", ""},
		{"agentflow 2.0.0\nwarning: /Users/x/y\n", "agentflow 2.0 is newer than this Golem supports; need 1.x"},
		{"agentflow\nwarning: /Users/x/y\n", `cannot parse agentflow version from "agentflow"`},
		// A real AgentFlow prints about 16 bytes; anything echoed is clipped.
		{"agentflow 1." + strings.Repeat("9", 100) + "x\n", `cannot parse agentflow version "1.` + strings.Repeat("9", 62) + `..."`},
	} {
		t.Run(strings.TrimSpace(tt.out), func(t *testing.T) {
			c, f := newTestClient(map[string]fakeReply{"--version": {stdout: []byte(tt.out)}})
			err := c.CheckVersion(context.Background())
			if tt.want == "" {
				if err != nil {
					t.Fatalf("CheckVersion(%q) = %v", tt.out, err)
				}
			} else {
				var versionErr *VersionError
				if !errors.As(err, &versionErr) || err.Error() != tt.want {
					t.Fatalf("CheckVersion(%q) = %v, want *VersionError %q", tt.out, err, tt.want)
				}
			}
			if !reflect.DeepEqual(f.calls, [][]string{{"--version"}}) {
				t.Fatalf("calls = %v, want only --version", f.calls)
			}
		})
	}
}

// A launch failure keeps Probe's existing wrapping and is not a VersionError,
// so JSON status keeps it silent (#612 G17).
func TestCheckVersion_LaunchFailureIsNotAVersionError(t *testing.T) {
	launch := errors.New("exec: agentflow: not found")
	for _, tt := range []struct {
		reply fakeReply
		want  string
	}{
		{fakeReply{err: launch}, "agentflow unavailable (--version failed): exec: agentflow: not found"},
		{fakeReply{exit: 2}, "agentflow unavailable (--version failed): agentflow --version: exit 2"},
		// A broken install's own error is the diagnosis; keep it (#612).
		{fakeReply{exit: 1, stderr: []byte("boom: missing module\n")}, "agentflow unavailable (--version failed): agentflow --version: exit 1: boom: missing module"},
	} {
		c, _ := newTestClient(map[string]fakeReply{"--version": tt.reply})
		err := c.CheckVersion(context.Background())
		var versionErr *VersionError
		if err == nil || errors.As(err, &versionErr) || err.Error() != tt.want {
			t.Fatalf("CheckVersion = %v, want plain %q", err, tt.want)
		}
	}
}

// Probe stops at a rejected version: no --help or subcommand probe runs.
func TestProbe_RejectsUnsupportedVersionBeforeHelp(t *testing.T) {
	for _, version := range []string{"agentflow 0.4.0\n", "agentflow 2.0.0\n"} {
		f := &fakeRunner{replies: probeReplies(fullProbeHelp)}
		f.replies["--version"] = fakeReply{stdout: []byte(version)}
		err := NewClient(f, "/ws").Probe(context.Background())
		var versionErr *VersionError
		if !errors.As(err, &versionErr) {
			t.Fatalf("Probe(%q) = %v, want *VersionError", version, err)
		}
		if len(f.calls) != 1 {
			t.Fatalf("Probe(%q) ran %v after rejecting the version", version, f.calls)
		}
	}
}
