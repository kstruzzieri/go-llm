//go:build unix

package consult

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sha256Hex recomputes the receipt digest independently of the implementation.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// consultantFor wraps one shell-script body as an otherwise valid consultant.
func consultantFor(t *testing.T, body string) Consultant {
	t.Helper()
	return Consultant{Name: "claude", Adapter: "claude", Command: script(t, body), Model: "opus",
		TimeoutSeconds: 5, MaxOutputBytes: 1 << 20, TrustedProcessEgress: true}
}

// fixtureBody is the shell body that replays one stream-json fixture. The
// fixture is a file rather than an argument so no byte of it is parsed as a
// format string or a shell word; sed substitutes the child's real private cwd
// for the fixture placeholder, so the init's cwd check sees the envelope it
// actually ran in.
func fixtureBody(t *testing.T, stdoutLines string, exit int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(path, []byte(stdoutLines+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return `cat >/dev/null; sed "s|/private/synthetic|$PWD|g" ` + path + `; exit ` + strconv.Itoa(exit)
}

// fakeClaude emits the given stream-json lines on stdout and exits with exit.
func fakeClaude(t *testing.T, stdoutLines string, exit int) Consultant {
	t.Helper()
	return consultantFor(t, fixtureBody(t, stdoutLines, exit))
}

// mustFail requires a fixed-code failure and, with it, a zero receipt: no
// field of a rejected consultation may reach the caller.
func mustFail(t *testing.T, ctx context.Context, c Consultant, prompt string) *Error {
	t.Helper()
	r, err := Run(ctx, c, prompt)
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("err %v, want *consult.Error", err)
	}
	if r != (Receipt{}) {
		t.Fatalf("receipt on failure: %+v", r)
	}
	return ce
}

func TestRunReturnsReceiptForAdmittedStream(t *testing.T) {
	c := fakeClaude(t, strings.Join([]string{validInit, assistantOK, goodResult}, "\n"), 0)
	r, err := Run(context.Background(), c, "Reply with exactly OK.\n")
	if err != nil {
		t.Fatal(err)
	}
	if r.Answer != "OK" || r.Version != "2.1.240" || r.Model != "opus" || r.ExitCode != 0 ||
		r.ContentForm != "consult-result/v1" || r.ContentSHA256 != sha256Hex("OK") ||
		r.Consultant != "claude" || r.Adapter != "claude" || r.Duration <= 0 {
		t.Fatalf("receipt: %+v", r)
	}
}

func TestRunMapsFailuresToFixedCodes(t *testing.T) {
	rate := func(info string) string {
		return `{"type":"rate_limit_event","uuid":"u","session_id":"synthetic-session","rate_limit_info":` + info + `}`
	}
	for _, tc := range []struct {
		name, stdout string
		exit         int
		code, reason string
	}{
		{"version", strings.Join([]string{strings.Replace(validInit, "2.1.240", "2.1.241", 1), assistantOK, goodResult}, "\n"), 0, "unsupported-version", "version-mismatch"},
		{"tool", strings.Join([]string{validInit, `{"type":"tool_use","name":"SENSITIVE"}`, assistantOK, goodResult}, "\n"), 0, "tool-activity", "tool-activity"},
		{"quota", strings.Join([]string{validInit, rate(`{"status":"rejected"}`), assistantOK, goodResult}, "\n"), 0, "quota", "quota-rejected"},
		{"billing", strings.Join([]string{validInit, rate(`{"status":"allowed","isUsingOverage":true}`), assistantOK, goodResult}, "\n"), 0, "billing", "overage-in-use"},
		{"auth", strings.Join([]string{strings.Replace(validInit, `"apiKeySource":"none"`, `"apiKeySource":"ANTHROPIC_API_KEY"`, 1), assistantOK, goodResult}, "\n"), 0, "auth", "auth-source-invalid"},
		// The reason carries the leader's exact wait status, not a constant:
		// a caller distinguishing exit 3 from a signal depends on it.
		{"exit", strings.Join([]string{validInit, assistantOK, goodResult}, "\n"), 3, "process-exit", "exited(3)"},
		{"protocol", strings.Join([]string{validInit, `{"type":"SENSITIVE"}`, assistantOK, goodResult}, "\n"), 0, "protocol", "unknown-event"},
		{"no_terminal", strings.Join([]string{validInit, assistantOK}, "\n"), 0, "protocol", "terminal-invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ce := mustFail(t, context.Background(), fakeClaude(t, tc.stdout, tc.exit), "hi\n")
			if ce.Code != tc.code || ce.Reason != tc.reason || strings.Contains(ce.Error(), "SENSITIVE") {
				t.Fatalf("err %v, want %s (%s)", ce, tc.code, tc.reason)
			}
		})
	}
}

func TestRunRejectsBadPromptBeforeExec(t *testing.T) {
	c := fakeClaude(t, "", 0)
	c.Command = "/nonexistent/never-run"
	for _, p := range []string{"", strings.Repeat("x", 65537), "bad\xff"} {
		if ce := mustFail(t, context.Background(), c, p); ce.Code != "input-invalid" || ce.Reason != "prompt" {
			t.Fatalf("prompt %d bytes: %v, want input-invalid (prompt)", len(p), ce)
		}
	}
}

func TestRunRejectsUnsupportedModelBeforeExec(t *testing.T) {
	// A hand-built Consultant never passed Load, so it carries neither its
	// bounds nor its defaults. The command is missing on purpose: a re-check
	// that did not reject would exec and fail with target-invalid instead.
	for _, tc := range []struct {
		name   string
		mutate func(*Consultant)
	}{
		{"model", func(c *Consultant) { c.Model = "sonnet" }},
		{"adapter", func(c *Consultant) { c.Adapter = "codex" }},
		{"timeout_zero", func(c *Consultant) { c.TimeoutSeconds = 0 }},
		{"timeout_over", func(c *Consultant) { c.TimeoutSeconds = maxTimeoutSeconds + 1 }},
		{"output_zero", func(c *Consultant) { c.MaxOutputBytes = 0 }},
		{"output_over", func(c *Consultant) { c.MaxOutputBytes = maxOutputBytes + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeClaude(t, "", 0)
			c.Command = "/nonexistent/never-run"
			tc.mutate(&c)
			if ce := mustFail(t, context.Background(), c, "hi\n"); ce.Code != "input-invalid" || ce.Reason != "consultant" {
				t.Fatalf("err %v, want input-invalid (consultant)", ce)
			}
		})
	}
}

func TestRunClassifiesRunnerErrors(t *testing.T) {
	valid := strings.Join([]string{validInit, assistantOK, goodResult}, "\n")

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(fakeClaude(t, valid, 0).Command, link); err != nil {
		t.Fatal(err)
	}
	symlinked := fakeClaude(t, valid, 0)
	symlinked.Command = link

	drifted := fakeClaude(t, valid, 0)
	drifted.SHA256 = strings.Repeat("a", 64)

	missing := fakeClaude(t, valid, 0)
	missing.Command = filepath.Join(t.TempDir(), "absent")

	notExec := fakeClaude(t, valid, 0)
	notExec.Command = filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(notExec.Command, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name         string
		c            Consultant
		code, reason string
	}{
		{"symlink", symlinked, "target-invalid", "target"},
		{"digest", drifted, "target-drift", "target"},
		{"missing", missing, "target-invalid", "target"},
		{"not_executable", notExec, "process-exit", "start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ce := mustFail(t, context.Background(), tc.c, "hi\n")
			if ce.Code != tc.code || ce.Reason != tc.reason {
				t.Fatalf("err %v, want %s (%s)", ce, tc.code, tc.reason)
			}
			if strings.Contains(ce.Error(), tc.c.Command) {
				t.Fatalf("error leaked the command path: %v", ce)
			}
		})
	}
}

func TestRunTimeoutAndCancelCodes(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		c := consultantFor(t, "sleep 30")
		c.TimeoutSeconds = 1
		if ce := mustFail(t, context.Background(), c, "hi\n"); ce.Code != "timeout" || ce.Reason != "deadline" {
			t.Fatalf("err %v, want timeout (deadline)", ce)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		c := consultantFor(t, "sleep 30")
		ctx, cancel := context.WithCancel(context.Background())
		// 1s, not 300ms: a freshly written script costs ~200ms to exec on
		// macOS, and cancelling inside that window would surface as
		// process-exit/start and read as a regression.
		time.AfterFunc(time.Second, cancel)
		defer cancel()
		if ce := mustFail(t, ctx, c, "hi\n"); ce.Code != "canceled" || ce.Reason != "caller" {
			t.Fatalf("err %v, want canceled (caller)", ce)
		}
	})
	// A context already dead on entry never reaches Wait: CommandContext
	// refuses to start, so the outcome arrives as a start error and must not
	// be reported to the user as a process failure of their own Ctrl-C.
	t.Run("canceled_before_exec", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c := fakeClaude(t, strings.Join([]string{validInit, assistantOK, goodResult}, "\n"), 0)
		if ce := mustFail(t, ctx, c, "hi\n"); ce.Code != "canceled" || ce.Reason != "caller" {
			t.Fatalf("err %v, want canceled (caller)", ce)
		}
	})
	t.Run("expired_before_exec", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		c := fakeClaude(t, strings.Join([]string{validInit, assistantOK, goodResult}, "\n"), 0)
		if ce := mustFail(t, ctx, c, "hi\n"); ce.Code != "timeout" || ce.Reason != "deadline" {
			t.Fatalf("err %v, want timeout (deadline)", ce)
		}
	})
}

func TestRunOutputLimitCode(t *testing.T) {
	line := strings.Repeat("x", 100)
	c := consultantFor(t, "i=0; while [ $i -lt 100 ]; do printf '"+line+"\\n'; i=$((i+1)); done")
	c.MaxOutputBytes = 1024
	if ce := mustFail(t, context.Background(), c, "hi\n"); ce.Code != "output-limit" || ce.Reason != "cap" {
		t.Fatalf("err %v, want output-limit (cap)", ce)
	}
}

// TestRunReportsIncompleteDrain covers the fail-closed path where the leader
// exits cleanly but a descendant keeps stdout open: Wait abandons the copy at
// WaitDelay, so the transcript is a prefix and must never be admitted.
func TestRunReportsIncompleteDrain(t *testing.T) {
	valid := strings.Join([]string{validInit, assistantOK, goodResult}, "\n")
	c := fakeClaude(t, valid, 0)
	// The background sleep inherits stdout and outlives the leader. Shortening
	// the runner's grace period is the only way to reach this branch from Run:
	// waitDelay is not part of the Consultant contract.
	c.Command = script(t, "sleep 8 & "+fixtureBody(t, valid, 0))
	restore := runWaitDelay
	defer func() { runWaitDelay = restore }()
	runWaitDelay = 200 * time.Millisecond
	if ce := mustFail(t, context.Background(), c, "hi\n"); ce.Code != "drain-incomplete" || ce.Reason != "wait-delay" {
		t.Fatalf("err %v, want drain-incomplete (wait-delay)", ce)
	}
}

func TestRunReceiptEvidenceIsFixedFieldsOnly(t *testing.T) {
	usage := `{"claude-opus-4-8":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":2,"cacheCreationInputTokens":1,"webSearchRequests":0,"provider":"firstParty"}}`
	c := fakeClaude(t, strings.Join([]string{validInit, assistantOK, withUsage(usage)}, "\n"), 0)
	r, err := Run(context.Background(), c, "hi\n")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"synthetic-session", "synthetic-uuid", "claude-opus-4-8", "/private", "SENSITIVE"} {
		if strings.Contains(string(b), needle) {
			t.Fatalf("evidence leaked %q: %s", needle, b)
		}
	}
	// Each condition stands alone: a single combined expression lets a zeroed
	// evidence block satisfy the assertion through one surviving disjunct.
	e := r.Evidence
	switch {
	case e.InitAPIKeySource != "none", !e.UsageComplete, !e.WebSearchZero,
		e.OpusInputTokens != 10, e.OpusOutputTokens != 5,
		e.WaitStatus != "exited(0)", e.WaitErrorKind != "none":
		t.Fatalf("evidence: %+v", e)
	}
}
