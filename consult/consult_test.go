//go:build unix

package consult

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
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

const versionFixture = `if [ "$1" = --version ]; then printf '2.1.240 (Claude Code)\n'; exit 0; fi
`

// fakeClaude emits the given stream-json lines on stdout and exits with exit.
func fakeClaude(t *testing.T, stdoutLines string, exit int) Consultant {
	t.Helper()
	return consultantFor(t, versionFixture+fixtureBody(t, stdoutLines, exit))
}

// mustFail requires a fixed-code failure and, with it, a zero receipt: no
// field of a rejected consultation may reach the caller.
func mustFail(t *testing.T, ctx context.Context, c Consultant, prompt string) *Error {
	t.Helper()
	r, err := Run(ctx, c, prompt)
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatal("expected *consult.Error")
	}
	if r != (Receipt{}) {
		t.Fatal("expected zero receipt on failure")
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
		{"name", func(c *Consultant) { c.Name = "Not A Name" }},
		{"untrusted_egress", func(c *Consultant) { c.TrustedProcessEgress = false }},
		{"sha256", func(c *Consultant) { c.SHA256 = "malformed" }},
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
	notExec.Command = filepath.Join(realTempDir(t), "plain")
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

// TestRunReportsHostFailuresAsInternal covers the failures that are ours, not
// the consultant's: reporting a broken envelope or an unresolvable OS identity
// as process-exit would blame the child for a host defect.
func TestRunReportsHostFailuresAsInternal(t *testing.T) {
	t.Run("envelope", func(t *testing.T) {
		// Overrides package-level envelopeDirs, so no test here may t.Parallel.
		restore := envelopeDirs
		defer func() { envelopeDirs = restore }()
		envelopeDirs = []string{"cwd/unreachable"}
		c := fakeClaude(t, strings.Join([]string{validInit, assistantOK, goodResult}, "\n"), 0)
		if ce := mustFail(t, context.Background(), c, "hi\n"); ce.Code != "internal" || ce.Reason != "envelope" {
			t.Fatalf("err %v, want internal (envelope)", ce)
		}
	})
	// An identity failure needs a broken directory service, so the sentinel
	// contract is pinned at the classifier instead.
	t.Run("sentinels", func(t *testing.T) {
		for _, tc := range []struct {
			err          error
			code, reason string
		}{
			{fmt.Errorf("wrapped: %w", errEnvelope), "internal", "envelope"},
			{fmt.Errorf("wrapped: %w", errIdentity), "internal", "identity"},
			{fmt.Errorf("wrapped: %w", errTargetDrift), "target-drift", "target"},
			{fmt.Errorf("wrapped: %w", errTargetInvalid), "target-invalid", "target"},
			{fmt.Errorf("wrapped: %w", errStdinInvalid), "input-invalid", "prompt"},
			{fmt.Errorf("wrapped: %w", errors.ErrUnsupported), "unsupported-platform", "platform"},
			{errors.New("some start failure"), "process-exit", "start"},
		} {
			ce := classifyRunError(tc.err)
			if ce.Code != tc.code || ce.Reason != tc.reason {
				t.Errorf("classify %v = %v, want %s (%s)", tc.err, ce, tc.code, tc.reason)
			}
			if strings.Contains(ce.Error(), "wrapped") {
				t.Errorf("classifier leaked the wrapped text: %v", ce)
			}
		}
	})
}

// stubLookupUID installs a uid-lookup stub and clears the memoized identity,
// before and after. It touches package-level state, so no test in this package
// may call t.Parallel.
func stubLookupUID(t *testing.T, stub func(string) (*user.User, error)) {
	t.Helper()
	restore := lookupUID
	t.Cleanup(func() { lookupUID = restore; resetHostIdentityForTest() })
	lookupUID = stub
	resetHostIdentityForTest()
}

// realUID is the uid entry this host actually has, captured before any stub is
// installed so a stub can succeed with a usable identity.
func realUID(t *testing.T) *user.User {
	t.Helper()
	u, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestHostIdentityMemoizesOnlySuccess pins both halves of the caching rule: a
// resolved identity is looked up once for the whole process, and a failure is
// not cached, so one directory-service hiccup cannot disable /consult until
// the program is restarted.
func TestHostIdentityMemoizesOnlySuccess(t *testing.T) {
	valid := strings.Join([]string{validInit, assistantOK, goodResult}, "\n")

	t.Run("success_is_looked_up_once", func(t *testing.T) {
		real := realUID(t)
		calls := 0
		stubLookupUID(t, func(string) (*user.User, error) { calls++; return real, nil })
		c := fakeClaude(t, valid, 0)
		for i := 0; i < 2; i++ {
			if _, err := Run(context.Background(), c, "hi\n"); err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
		}
		if calls != 1 {
			t.Fatalf("uid lookup ran %d times across two consults, want 1", calls)
		}
	})

	t.Run("failure_is_not_cached", func(t *testing.T) {
		real := realUID(t)
		calls := 0
		stubLookupUID(t, func(string) (*user.User, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("directory service unavailable")
			}
			return real, nil
		})
		c := fakeClaude(t, valid, 0)
		if ce := mustFail(t, context.Background(), c, "hi\n"); ce.Code != "internal" || ce.Reason != "identity" {
			t.Fatalf("err %v, want internal (identity)", ce)
		}
		if _, err := Run(context.Background(), c, "hi\n"); err != nil {
			t.Fatalf("a transient lookup failure disabled consulting: %v", err)
		}
		// "No error" is not enough: a cache that admits the failed attempt
		// would hand every later consult a zero identity, and the child would
		// be launched with an empty HOME and USER rather than refused.
		got, err := hostIdentity()
		if err != nil || got.name != real.Username || got.home != real.HomeDir {
			t.Fatalf("recovered identity is %+v (%v), want %s at %s", got, err, real.Username, real.HomeDir)
		}
	})

	// An entry that resolves but is unusable is a rejection, not a cacheable
	// result: the same recovery must hold as for a failed lookup, so the
	// second half of this case is what makes the name true.
	t.Run("unusable_entry_is_not_cached", func(t *testing.T) {
		real := realUID(t)
		calls := 0
		stubLookupUID(t, func(string) (*user.User, error) {
			calls++
			if calls == 1 {
				return &user.User{Username: "x", HomeDir: "relative/home"}, nil
			}
			return real, nil
		})
		c := fakeClaude(t, valid, 0)
		if ce := mustFail(t, context.Background(), c, "hi\n"); ce.Code != "internal" || ce.Reason != "identity" {
			t.Fatalf("err %v, want internal (identity)", ce)
		}
		if _, err := Run(context.Background(), c, "hi\n"); err != nil {
			t.Fatalf("an unusable entry disabled consulting: %v", err)
		}
		got, err := hostIdentity()
		if err != nil || got.name != real.Username || got.home != real.HomeDir {
			t.Fatalf("recovered identity is %+v (%v), want %s at %s", got, err, real.Username, real.HomeDir)
		}
	})
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
	c.Command = script(t, versionFixture+"sleep 8 & "+fixtureBody(t, valid, 0))
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
		e.WaitStatus != "exited(0)", e.WaitErrorKind != "none",
		e.CodexTransport != "", e.CodexAppServerUsagePresent:
		t.Fatalf("evidence: %+v", e)
	}
}

func TestRunRejectsNonOpusAnswers(t *testing.T) {
	for _, tc := range []struct{ name, assistant, result, reason string }{
		{"sonnet_answer", strings.ReplaceAll(assistantOK, "claude-opus-4-8", "claude-sonnet-5"), goodResult, "response-model-invalid"},
		{"missing_answer_model", strings.ReplaceAll(assistantOK, "claude-opus-4-8", ""), goodResult, "response-model-invalid"},
		{"absent_answer_model", strings.ReplaceAll(assistantOK, `"model":"claude-opus-4-8",`, ""), goodResult, "response-model-invalid"},
		{"model_switch", stream(strings.ReplaceAll(assistantOK, "claude-opus-4-8", "claude-sonnet-5"), assistantOK), goodResult, "response-model-invalid"},
		{"sonnet_usage", assistantOK, withUsage(`{"claude-sonnet-5":{"inputTokens":10,"outputTokens":5,"webSearchRequests":0,"provider":"firstParty"}}`), "usage-model-invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fakeClaude(t, stream(validInit, tc.assistant, tc.result), 0)
			if ce := mustFail(t, context.Background(), c, "synthetic review prompt\n"); ce.Code != "protocol" || ce.Reason != tc.reason {
				t.Fatalf("model rejection: %v, want protocol (%s)", ce, tc.reason)
			}
		})
	}
}

func TestRunRevalidatesExecutable(t *testing.T) {
	for _, change := range []string{"group_writable", "world_writable", "parent_writable", "ancestor_writable", "parent_symlink"} {
		t.Run(change, func(t *testing.T) {
			c := fakeClaude(t, stream(validInit, assistantOK, goodResult), 0)
			var err error
			c.Command, err = filepath.EvalSymlinks(c.Command)
			if err != nil {
				t.Fatal(err)
			}
			if err := validate(&c); err != nil {
				t.Fatal(err)
			}
			if change != "parent_symlink" {
				mode := os.FileMode(0o770)
				target := c.Command
				if change == "world_writable" {
					mode = 0o707
				}
				if change == "parent_writable" || change == "ancestor_writable" {
					target = filepath.Dir(target)
				}
				if change == "ancestor_writable" {
					target = filepath.Dir(target)
				}
				if err := os.Chmod(target, mode); err != nil {
					t.Fatal(err)
				}
			} else {
				dir := filepath.Dir(c.Command)
				moved := dir + "-moved"
				if err := os.Rename(dir, moved); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(moved) })
				if err := os.Symlink(moved, dir); err != nil {
					t.Fatal(err)
				}
			}
			if err := validate(&c); err == nil {
				t.Fatal("fixture must now fail config validation")
			}
			if ce := mustFail(t, context.Background(), c, "synthetic review prompt\n"); ce.Code != "target-invalid" {
				t.Fatalf("changed target: %v, want target-invalid", ce)
			}
		})
	}
}

func TestCommandTrustIncludesOwnersAndStickyParents(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		leaf, foreign, sticky bool
	}{
		{name: "trusted_sticky_parent", sticky: true},
		{name: "foreign_leaf", leaf: true, foreign: true},
		{name: "foreign_parent", foreign: true},
		{name: "foreign_sticky_parent", foreign: true, sticky: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.foreign && os.Geteuid() != 0 {
				t.Skip("changing file ownership requires root; covered by Linux CI")
			}
			c := fakeClaude(t, stream(validInit, assistantOK, goodResult), 0)
			target := filepath.Dir(c.Command)
			if tc.leaf {
				target = c.Command
			}
			if tc.sticky {
				if err := os.Chmod(target, os.ModeSticky|0o777); err != nil {
					t.Fatal(err)
				}
			}
			if tc.foreign {
				if !tc.leaf && !tc.sticky {
					if err := os.Chmod(target, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Chown(target, 12345, -1); err != nil {
					if errors.Is(err, os.ErrPermission) {
						t.Skip("changing file ownership is unavailable in this environment")
					}
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chown(target, os.Geteuid(), -1); err != nil {
						t.Error(err)
					}
				})
				if err := validate(&c); err == nil || !strings.Contains(err.Error(), "owned by root or the current user") {
					t.Fatalf("foreign-owned executable path must fail ownership validation: %v", err)
				}
				if ce := mustFail(t, context.Background(), c, "hi\n"); ce.Code != "target-invalid" {
					t.Fatalf("foreign-owned path: %v", ce)
				}
				return
			}
			if _, err := Run(context.Background(), c, "hi\n"); err != nil {
				t.Fatalf("trusted sticky parent rejected: %v", err)
			}
		})
	}
}

func TestRunRejectsVersionBeforeSendingPrompt(t *testing.T) {
	for name, output := range map[string]string{
		"unsupported":  "2.1.241 (Claude Code)\n",
		"missing":      "",
		"malformed":    "2.1.240\n",
		"extra_output": "2.1.240 (Claude Code)\nunexpected\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := realTempDir(t)
			marker, probeStdin := filepath.Join(dir, "prompt"), filepath.Join(dir, "probe-stdin")
			versionFile := filepath.Join(dir, "version")
			if err := os.WriteFile(versionFile, []byte(output), 0o600); err != nil {
				t.Fatal(err)
			}
			transcript := stream(strings.ReplaceAll(validInit, "2.1.240", "2.1.241"), assistantOK, goodResult)
			body := "if [ \"$1\" = --version ]; then test \"$#\" -eq 1 || exit 9; cat > '" + probeStdin + "'; cat '" + versionFile + "'; exit 0; fi\ncat > '" + marker + "'\n" + fixtureBody(t, transcript, 0)
			c := consultantFor(t, body)
			if ce := mustFail(t, context.Background(), c, "synthetic review prompt\n"); ce.Code != "unsupported-version" {
				t.Fatalf("expected unsupported-version, got %v", ce)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsupported version received a prompt: %v", err)
			}
			if raw, err := os.ReadFile(probeStdin); err != nil || len(raw) != 0 {
				t.Fatalf("version probe stdin: %q, %v", raw, err)
			}
		})
	}
}

// A successful version check must not authorize bytes installed while it ran.
func TestRunBindsVersionToExecutable(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(strconv.FormatBool(pinned), func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "prompt-received")
			valid := stream(validInit, assistantOK, goodResult)
			replacement := script(t, "cat > '"+marker+"'\n"+fixtureBody(t, valid, 0))
			body := "if [ \"$1\" = --version ]; then mv '" + replacement + "' \"$0\"; printf '2.1.240 (Claude Code)\\n'; exit 0; fi\n" + fixtureBody(t, valid, 0)
			c := consultantFor(t, body)
			if pinned {
				c.SHA256 = sha256File(t, c.Command)
			}
			if ce := mustFail(t, context.Background(), c, "private prompt\n"); ce.Code != "target-drift" {
				t.Fatalf("replacement after version check: %v", ce)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("replacement received the prompt: %v", err)
			}
		})
	}
}

func TestRunBoundsVersionPreflight(t *testing.T) {
	for _, tc := range []struct{ name, probe, code string }{
		{"output", "head -c 8192 /dev/zero; sleep 30", "output-limit"},
		{"timeout", "sleep 30", "timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "prompt")
			body := "if [ \"$1\" = --version ]; then " + tc.probe + "; exit 0; fi\ncat > '" + marker + "'\n" + fixtureBody(t, stream(validInit, assistantOK, goodResult), 0)
			c := consultantFor(t, body)
			if tc.code == "timeout" {
				c.TimeoutSeconds = 1
			}
			if ce := mustFail(t, context.Background(), c, "private prompt\n"); ce.Code != tc.code {
				t.Fatalf("preflight: %v, want %s", ce, tc.code)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prompt sent after failed preflight: %v", err)
			}
		})
	}
}

func TestRunSharesDeadlineWithVersionPreflight(t *testing.T) {
	body := "if [ \"$1\" = --version ]; then sleep 0.9; printf '2.1.240 (Claude Code)\\n'; exit 0; fi\nsleep 1.2\n" + fixtureBody(t, stream(validInit, assistantOK, goodResult), 0)
	c := consultantFor(t, body)
	c.TimeoutSeconds = 2
	if ce := mustFail(t, context.Background(), c, "hi\n"); ce.Code != "timeout" {
		t.Fatalf("shared deadline: %v", ce)
	}
}

func TestRunReportsNonzeroIncompleteDrain(t *testing.T) {
	c := consultantFor(t, versionFixture+"sleep 30 & exit 3")
	old := runWaitDelay
	t.Cleanup(func() { runWaitDelay = old })
	runWaitDelay = 100 * time.Millisecond
	ce := mustFail(t, context.Background(), c, "hi\n")
	if ce.Code != "drain-incomplete" || ce.Reason != "wait-delay" {
		t.Fatalf("Run(held pipe, exit 3) = %v, want drain-incomplete/wait-delay", ce)
	}
}

func TestHostOutcomePrecedence(t *testing.T) {
	out := runOutcome{CapExceeded: true, Canceled: true, TimedOut: true, WaitErrorKind: "wait-delay", ExitCode: 7, WaitStatus: "exited(7)"}
	for _, tc := range []struct {
		clear func()
		want  Error
	}{
		{func() {}, Error{Code: "output-limit", Reason: "cap"}},
		{func() { out.CapExceeded = false }, Error{Code: "canceled", Reason: "caller"}},
		{func() { out.Canceled = false }, Error{Code: "timeout", Reason: "deadline"}},
		{func() { out.TimedOut = false }, Error{Code: "process-exit", Reason: "cleanup"}},
		{func() { out.GroupCleanupOK = true }, Error{Code: "process-exit", Reason: "cleanup"}},
		{func() { out.CleanupOK = true }, Error{Code: "drain-incomplete", Reason: "wait-delay"}},
		{func() { out.WaitErrorKind = "other" }, Error{Code: "drain-incomplete", Reason: "other"}},
		{func() { out.WaitErrorKind = "exit" }, Error{Code: "process-exit", Reason: "exited(7)"}},
	} {
		tc.clear()
		if got := classifyRunOutcome(out); got == nil || *got != tc.want {
			t.Fatalf("classifyRunOutcome(%+v) = %v, want %v", out, got, tc.want)
		}
	}
	out.ExitCode, out.WaitStatus, out.WaitErrorKind = 0, "exited(0)", "none"
	if got := classifyRunOutcome(out); got != nil {
		t.Fatalf("successful host outcome = %v", got)
	}
}

func TestAppServerFixedErrorMappings(t *testing.T) {
	for _, tc := range []struct {
		err          error
		code, reason string
	}{
		{errAppServerProtocol, "protocol", "codex-app-server-invalid-protocol"},
		{errAppServerProtocolInitialize, "protocol", "codex-app-server-invalid-protocol-initialize"},
		{errAppServerProtocolDiscovery, "protocol", "codex-app-server-invalid-protocol-discovery"},
		{errAppServerProtocolThread, "protocol", "codex-app-server-invalid-protocol-thread"},
		{errAppServerProtocolTurn, "protocol", "codex-app-server-invalid-protocol-turn"},
		{errAppServerProtocolShutdown, "protocol", "codex-app-server-invalid-protocol-shutdown"},
		{errAppServerConfigWarning, "protocol", "codex-app-server-config-warning-rejected"},
		{errAppServerWarning, "protocol", "codex-app-server-warning-rejected"},
		{errAppServerDeprecationNotice, "protocol", "codex-app-server-deprecation-notice-rejected"},
		{errAppServerAccountUpdated, "protocol", "codex-app-server-account-updated-rejected"},

		{errAppServerRequest, "tool-activity", "codex-app-server-request"},
		{errAppServerVendor, "protocol", "codex-app-server-vendor-error"},
		{errAppServerUnavailable, "protocol", "codex-app-server-model-unavailable"},
		{errAppServerIncomplete, "protocol", "codex-app-server-incomplete"},
		{errAppServerAnswer, "protocol", "codex-app-server-invalid-answer"},
		{errDuplexEOF, "protocol", "codex-app-server-incomplete"},
		{errDuplexWrite, "protocol", "codex-app-server-write"},
		{errDuplexRecords, "protocol", "codex-app-server-record-limit"},
		{errDuplexBackpressure, "protocol", "codex-app-server-backpressure"},
		{errors.New("SECRET /private/path RPC-ID"), "protocol", "codex-app-server-invalid-protocol"},
	} {
		got := classifyAppServerError(fmt.Errorf("SECRET: %w", tc.err))
		if got.Code != tc.code || got.Reason != tc.reason {
			t.Fatalf("classifyAppServerError(%v) = %v, want %s/%s", tc.err, got, tc.code, tc.reason)
		}
	}
}

func TestAppServerFaultMappingIsClosed(t *testing.T) {
	for _, tc := range []struct {
		phase         error
		point, reason string
	}{
		{errAppServerProtocolTurn, "record-decode", "codex-app-server-invalid-protocol-turn-record-decode"},
		{errAppServerProtocolTurn, "PRIVATE-CANARY /private/path", "codex-app-server-invalid-protocol-turn"},
		{errAppServerProtocolTurn, "", "codex-app-server-invalid-protocol-turn"},
		{errAppServerWarning, "record-decode", "codex-app-server-warning-rejected"},
		{errAppServerVendor, "record-decode", "codex-app-server-vendor-error"},
		{errAppServerRequest, "record-decode", "codex-app-server-request"},
		{errAppServerIncomplete, "record-decode", "codex-app-server-incomplete"},
		{errAppServerAnswer, "record-decode", "codex-app-server-invalid-answer"},
	} {
		got := classifyAppServerError(fmt.Errorf("PRIVATE-CANARY: %w", &appServerFault{phase: tc.phase, point: tc.point}))
		if got.Reason != tc.reason {
			t.Errorf("classifyAppServerError fault = %s, want %s", got.Reason, tc.reason)
		}
	}
}

// TestAppServerPointsMatchSource pins appServerPoints to the literals the
// protocol file can actually emit, in both directions: a point added without an
// allowlist entry would otherwise silently degrade to the phase-only reason,
// and a removed point would leave a dead entry.
func TestAppServerPointsMatchSource(t *testing.T) {
	src, err := os.ReadFile("codex_app_server_protocol.go")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\b(?:rejectPoint|invalid)\("([a-z0-9-]+)"\)|point :?= (?:[^\n]*, )?"([a-z0-9-]+)"|return (?:result, )?"([a-z0-9-]+)"`)
	inSource := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		for _, g := range m[1:] {
			if g != "" {
				inSource[g] = true
			}
		}
	}
	if len(inSource) == 0 {
		t.Fatal("no rejection points found in source")
	}
	for p := range inSource {
		if !appServerPoints[p] {
			t.Errorf("point %q emitted by codex_app_server_protocol.go is missing from appServerPoints", p)
		}
		got := classifyAppServerError(&appServerFault{phase: errAppServerProtocolTurn, point: p})
		if want := "codex-app-server-invalid-protocol-turn-" + p; got.Reason != want {
			t.Errorf("point %q reason = %s, want %s", p, got.Reason, want)
		}
	}
	for p := range appServerPoints {
		if !inSource[p] {
			t.Errorf("appServerPoints entry %q is not emitted by codex_app_server_protocol.go", p)
		}
	}
}
