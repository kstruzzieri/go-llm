//go:build agentflow_integration

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	args := []string{"-root", root, "-scope", "proofs"}
	if src := os.Getenv("AGENTFLOW_SRC"); src != "" {
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
