//go:build agentflow_integration

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// Real Agentflow does not yet implement verify-proof --integrity-only (see
// docs/golem.md), so it answers with an argparse error, exit 2. Reaching that
// answer proves the audit runner launched Agentflow under the policy in this
// mode; a launch failure would report a different diagnostic.
func TestAgentflowEnv_RealCLI_AuditLaunchesUnderPolicy(t *testing.T) {
	_ = agentflowRunnerOrSkip(t, t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	src := os.Getenv("AGENTFLOW_SRC")
	// Audit's runner must reach real Agentflow first: an interpreter too old for
	// `python3 -P` also exits 2 and would impersonate the argparse answer below.
	probe := mustAgentflowRunner(root, src, nil)
	probe.DisablePythonBytecodeWrites()
	if _, stderr, exit, err := probe.Run(t.Context(), []string{"--version"}, nil); err != nil || exit != 0 {
		t.Fatalf("audit runner cannot launch Agentflow: exit=%d err=%v stderr=%q", exit, err, stderr)
	}
	args := []string{"-root", root, "-scope", "proofs"}
	if src != "" {
		args = append(args, "-agentflow-src", src)
	}
	var out, errOut bytes.Buffer
	err = runAudit(t.Context(), args, &out, &errOut)
	var exit *auditExitError
	want := "proofs: outcome=incomplete assurance=\"structural/checksum; unsigned\" checked=0 sources=0\n" +
		"  diagnostic code=agentflow_unavailable target=\"\" message=\"agentflow proof verification unavailable: requires agentflow with --integrity-only support\"\n" +
		"overall: outcome=incomplete\n"
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || out.String() != want || errOut.Len() != 0 {
		t.Fatalf("audit: err=%v stdout=%q stderr=%q", err, out.String(), errOut.String())
	}
}

// The production task entry with two plan workers must give every Agentflow
// launch it wires (canonical root, each worker root, the aggregate) the approved
// names. A guard shadowing the Agentflow executable on PATH exits 97 unless the
// approved name reached it, so any construction site that drops the names fails
// its launch. The barrier caller deadlocks a serial run, so the parallel cohort
// must really run for the test to pass.
func TestAgentflowEnv_RealCLI_ParallelDriverForwardsApprovedNames(t *testing.T) {
	dir, _, base := writeParallelSmokeFixture(t)
	_ = agentflowRunnerOrSkip(t, dir)
	src := os.Getenv("AGENTFLOW_SRC")
	bin := "agentflow"
	if src != "" {
		bin = "python3"
	}
	real, err := exec.LookPath(bin)
	if err != nil {
		t.Fatal(err)
	}
	if real, err = filepath.Abs(real); err != nil {
		t.Fatal(err)
	}
	const guardName = "GOLEM_577_GUARD"
	guardDir := t.TempDir()
	guard := "#!/bin/sh\n[ \"$" + guardName + "\" = on ] || { echo 'golem577: approved name missing' >&2; exit 97; }\nexec " + shellQuote(real) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(guardDir, bin), []byte(guard), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", guardDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(guardName, "on")

	barrier := &parallelSmokeBarrier{ready: make(chan struct{})}
	newOrchestrator := func() *agent.Orchestrator {
		return agent.New(parallelSmokeCaller{barrier: barrier}, agent.ContextManager{})
	}
	sess := &replSession{orch: newOrchestrator(), newOrchestrator: newOrchestrator, maxSteps: 4, clock: time.Now}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err = runAgentflowTask(ctx, &stdout, &stderr, nil, sess, flags{
		planPath: filepath.Join(dir, "plan.json"), planWorkers: 2, approveEdits: true, approveGates: true,
		agentflowSrc: src, agentflowEnv: stringSliceFlag{guardName},
	}, dir)
	// The version probe reports only the exit code, not the guard's stderr.
	if strings.Contains(stderr.String(), "golem577: approved name missing") || strings.Contains(stderr.String(), "exit 97") {
		t.Fatalf("an Agentflow launch ran without approved name %s (guard exit 97); err=%v\nstderr:\n%s", guardName, err, stderr.String())
	}
	if err != nil {
		t.Fatalf("runAgentflowTask: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "no safe parallel cohort; continuing serially") {
		t.Fatalf("driver fell back to serial:\n%s", stdout.String())
	}
	if got := barrier.count(); got != 2 {
		t.Fatalf("parallel worker barrier arrivals = %d, want 2", got)
	}
	_, proof, ok := strings.Cut(stdout.String(), "proof pack: ")
	if !ok {
		t.Fatalf("no proof pack in stdout:\n%s", stdout.String())
	}
	proof, _, _ = strings.Cut(proof, "\n")
	assertParallelSmokeProof(t, dir, strings.TrimSpace(proof), base)
}
