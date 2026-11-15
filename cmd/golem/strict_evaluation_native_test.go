//go:build unix

package main

import (
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// strict_evaluation_native_test.go is row 4 of the strict-mode workflow
// evaluation (#581, spec section 10): exposure under a native sandbox with
// -sandbox-network allowed. Its outcome names the host's backend, so it stays
// out of the generated strict-mode-evaluation.md; with -v it logs one line per
// sub-test for the hand-written decision doc. It skips without a native
// backend, as the native wiring tests do, and never requires one.

// newNativeEvalRuntime is newNativeSession under a native policy that allows
// network, plus the exec tools on that policy, a session store and strict
// approval, wrapped as an evalRuntime so the 13a consult staging, resume
// seeding and counting turns run on it unchanged. golem.New rebuilds the
// runtime from those options, so it validates the network-denied variant at
// construction as the CLI's runtime does (#584).
func newNativeEvalRuntime(t *testing.T, policy agenttools.SandboxConfig) *evalRuntime {
	t.Helper()
	sess := newNativeSession(t, policy)
	model := &scriptCaller{}
	s, _, err := openSession(t.Context(), filepath.Join(t.TempDir(), "sessions.db"), "user:active")
	if err != nil {
		t.Fatalf("openSession: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mgr, execTools, err := buildExecMount(sess.root, policy, agenttools.ExecToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)
	sess.session, sess.tools = s, append(sess.tools, execTools...)
	sess.allowExec, sess.strictApproval = true, true
	sess.orch = agent.New(model, agent.ContextManager{}, agent.WithVerifier(sess.verifier))
	installCompactRuntime(t, sess, golemruntime.Options{Profile: golemruntime.ProfileTrusted, Sandbox: policy, Tools: execTools, StrictApproval: true})
	return &evalRuntime{sess: sess, model: model, mgr: mgr}
}

// tcpProbe scripts a foreground command that opens a TCP connection to p's
// listener. TCP only: a UDP send to loopback succeeds inside bwrap's private
// network namespace, so it proves nothing.
func tcpProbe(p *wiringProbe) agent.ModelResult {
	port := p.ln.Addr().(*net.TCPAddr).Port
	return call("run_command", map[string]any{"argv": []string{"bash", "-c", fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d", port)}})
}

// nativeTurn runs one turn of the TCP probe, answering its one prompt y, and
// returns the sandbox line's network field, the probe's exit code and its
// stderr, as observed. With why set the conversation is exposed: the prompt
// is fresh with that reason, the sandbox line shows label, and the probe must
// exit non-zero with the listener receiving nothing. With why empty it is
// clean: one grantable prompt, label, exit 0 and one connection. Neither
// prints a "network is not confined" line.
func nativeTurn(t *testing.T, r *evalRuntime, p *wiringProbe, label, why string) (string, int, string) {
	t.Helper()
	exposed := why != ""
	question, notices, connects := "Run this command? [y/N/a=always this command] ", 0, 1
	if exposed {
		question, notices, connects = "Run this command? [y/N] ", 1, 0
	}
	tr := r.turn(t, "y")
	if !slices.Equal(tr.questions, []string{question}) || tr.notices() != notices || noticeReasons(tr.out) != why || tr.networkLines() != 0 {
		t.Errorf("want one prompt %q, %d notice(s) %q and no \"not confined\" line; questions %q:\n%s", question, notices, why, tr.questions, tr.out)
	}
	_, field, _ := strings.Cut(tr.out, " network=")
	field, _, _ = strings.Cut(field, " ")
	if field = "network=" + field; strings.Count(tr.out, " network=") != 1 || field != label {
		t.Errorf("sandbox line shows %s, want %s:\n%s", field, label, tr.out)
	}
	rec := tr.res.ToolCalls
	if len(rec) != 1 || rec[0].Name != "run_command" || !rec[0].Invoked || rec[0].Denied || rec[0].AutoApproved {
		t.Fatalf("want the command run at its prompt; records %+v\n%s", rec, tr.out)
	}
	i := slices.IndexFunc(tr.res.Messages, func(m provider.ChatMessage) bool { return m.Role == "tool" })
	if i < 0 {
		t.Fatalf("no tool result in the turn: %+v", tr.res.Messages)
	}
	result := tr.res.Messages[i].Content
	code, err := strconv.Atoi(resultField(t, result, "exit code"))
	if err != nil {
		t.Fatalf("exit code: %v\n%s", err, result)
	}
	if exposed && code <= 0 || !exposed && code != 0 {
		t.Errorf("probe exit code = %d with exposed %t; want > 0 exposed, 0 clean\n%s", code, exposed, result)
	}
	if got := p.connects(t); got != connects {
		t.Errorf("listener received %d connection(s), want %d\n%s", got, connects, result)
	}
	_, stderr, _ := strings.Cut(result, "--- stderr ---\n")
	return field, code, strings.TrimSpace(stderr)
}

// TestStrictModeEvaluationNative is row 4: under the native sandbox with
// network allowed and strict on, a conversation exposed by accepted /consult
// advice or by resuming a foreign record asks once, fresh, for the TCP probe
// and runs it on the network-denied copy, so the connection fails and the
// listener receives nothing. A clean turn in the same session first runs the
// same probe on the allowed policy and connects. Under a native sandbox the
// fresh prompt has no "network is not confined" line.
func TestStrictModeEvaluationNative(t *testing.T) {
	policy := nativePolicyForTest(t, canonicalDir(t, t.TempDir()))
	policy.AllowNetwork = true
	denied := map[agenttools.SandboxRuntime]string{
		agenttools.SandboxRuntimeSeatbelt: "network=denied",
		agenttools.SandboxRuntimeBwrap:    "network=ip-denied",
	}[policy.Runtime]
	for _, tc := range []struct {
		name   string
		why    string
		expose func(t *testing.T, r *evalRuntime)
	}{
		{"consult", "outside content entered this conversation (consult)", func(t *testing.T, r *evalRuntime) { r.consult(t) }},
		{"exposed resume", "outside content entered this conversation (mcp__docs__fetch)", func(t *testing.T, r *evalRuntime) { r.resume(t, foreignExposure) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNativeEvalRuntime(t, policy)
			clean, exposed := newWiringProbe(t), newWiringProbe(t)
			r.script(tcpProbe(clean), answerStep("clean"), tcpProbe(exposed), answerStep("exposed"))

			cleanLabel, cleanCode, _ := nativeTurn(t, r, clean, "network=allowed", "")
			tc.expose(t, r)
			label, code, stderr := nativeTurn(t, r, exposed, denied, tc.why)
			t.Logf("%s: backend=%s; exposed: %s, exit %d, error %q; clean: %s, exit %d",
				tc.name, policy.Runtime, label, code, stderr, cleanLabel, cleanCode)
		})
	}
}
