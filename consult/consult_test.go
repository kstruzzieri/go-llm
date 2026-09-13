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

// fakeClaude emits the given stream-json lines on stdout and exits with exit.
// Every "/private/synthetic" in the fixture is substituted with the child's
// real private cwd, so the init's cwd check sees the envelope it ran in.
func fakeClaude(t *testing.T, stdoutLines string, exit int) Consultant {
	t.Helper()
	body := `cat >/dev/null; printf '%s\n' "$(printf '` +
		strings.ReplaceAll(stdoutLines, "/private/synthetic", "%s") +
		`' "$PWD")"; exit ` + strconv.Itoa(exit)
	return consultantFor(t, body)
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
		code         string
	}{
		{"version", strings.Join([]string{strings.Replace(validInit, "2.1.240", "2.1.241", 1), assistantOK, goodResult}, "\n"), 0, "unsupported-version"},
		{"tool", strings.Join([]string{validInit, `{"type":"tool_use","name":"SENSITIVE"}`, assistantOK, goodResult}, "\n"), 0, "tool-activity"},
		{"quota", strings.Join([]string{validInit, rate(`{"status":"rejected"}`), assistantOK, goodResult}, "\n"), 0, "quota"},
		{"billing", strings.Join([]string{validInit, rate(`{"status":"allowed","isUsingOverage":true}`), assistantOK, goodResult}, "\n"), 0, "billing"},
		{"auth", strings.Join([]string{strings.Replace(validInit, `"apiKeySource":"none"`, `"apiKeySource":"ANTHROPIC_API_KEY"`, 1), assistantOK, goodResult}, "\n"), 0, "auth"},
		{"exit", strings.Join([]string{validInit, assistantOK, goodResult}, "\n"), 3, "process-exit"},
		{"protocol", strings.Join([]string{validInit, `{"type":"SENSITIVE"}`, assistantOK, goodResult}, "\n"), 0, "protocol"},
		{"no_terminal", strings.Join([]string{validInit, assistantOK}, "\n"), 0, "protocol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Run(context.Background(), fakeClaude(t, tc.stdout, tc.exit), "hi\n")
			var ce *Error
			if !errors.As(err, &ce) || ce.Code != tc.code || strings.Contains(err.Error(), "SENSITIVE") {
				t.Fatalf("err %v, want code %s", err, tc.code)
			}
			if r != (Receipt{}) {
				t.Fatalf("receipt on failure: %+v", r)
			}
		})
	}
}

func TestRunRejectsBadPromptBeforeExec(t *testing.T) {
	c := fakeClaude(t, "", 0)
	c.Command = "/nonexistent/never-run"
	for _, p := range []string{"", strings.Repeat("x", 65537), "bad\xff"} {
		var ce *Error
		if _, err := Run(context.Background(), c, p); !errors.As(err, &ce) || ce.Code != "input-invalid" {
			t.Fatalf("prompt %d bytes: %v", len(p), err)
		}
	}
}

func TestRunRejectsUnsupportedModelBeforeExec(t *testing.T) {
	for _, tc := range []struct{ name, model, adapter string }{
		{"model", "sonnet", "claude"},
		{"adapter", "opus", "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeClaude(t, "", 0)
			c.Command, c.Model, c.Adapter = "/nonexistent/never-run", tc.model, tc.adapter
			var ce *Error
			if _, err := Run(context.Background(), c, "hi\n"); !errors.As(err, &ce) || ce.Code != "input-invalid" {
				t.Fatalf("err %v, want input-invalid", err)
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
			_, err := Run(context.Background(), tc.c, "hi\n")
			var ce *Error
			if !errors.As(err, &ce) || ce.Code != tc.code || ce.Reason != tc.reason {
				t.Fatalf("err %v, want %s (%s)", err, tc.code, tc.reason)
			}
			if strings.Contains(err.Error(), tc.c.Command) {
				t.Fatalf("error leaked the command path: %v", err)
			}
		})
	}
}

func TestRunTimeoutAndCancelCodes(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		c := consultantFor(t, "sleep 30")
		c.TimeoutSeconds = 1
		var ce *Error
		if _, err := Run(context.Background(), c, "hi\n"); !errors.As(err, &ce) || ce.Code != "timeout" {
			t.Fatalf("err %v, want timeout", err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		c := consultantFor(t, "sleep 30")
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(300*time.Millisecond, cancel)
		defer cancel()
		var ce *Error
		if _, err := Run(ctx, c, "hi\n"); !errors.As(err, &ce) || ce.Code != "canceled" {
			t.Fatalf("err %v, want canceled", err)
		}
	})
}

func TestRunOutputLimitCode(t *testing.T) {
	line := strings.Repeat("x", 100)
	c := consultantFor(t, "i=0; while [ $i -lt 100 ]; do printf '"+line+"\\n'; i=$((i+1)); done")
	c.MaxOutputBytes = 1024
	var ce *Error
	if _, err := Run(context.Background(), c, "hi\n"); !errors.As(err, &ce) || ce.Code != "output-limit" {
		t.Fatalf("err %v, want output-limit", err)
	}
}

func TestRunReceiptEvidenceIsFixedFieldsOnly(t *testing.T) {
	c := fakeClaude(t, strings.Join([]string{validInit, assistantOK, goodResult}, "\n"), 0)
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
	if r.Evidence.InitAPIKeySource != "none" || !r.Evidence.UsageComplete && r.Evidence.OpusInputTokens != 0 {
		t.Fatalf("evidence: %+v", r.Evidence)
	}
}
