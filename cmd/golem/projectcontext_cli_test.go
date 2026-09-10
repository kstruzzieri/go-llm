package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/conversation"
)

const (
	projectContextOpen  = "<<<PROJECT_CONTEXT "
	projectContextClose = ">>>PROJECT_CONTEXT "
	gitContextOpen      = "<<<GIT_CONTEXT "
	gitContextClose     = ">>>GIT_CONTEXT "
)

func newTrustSession(t *testing.T, content string) (*replSession, *captureCaller) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	caller := &captureCaller{answer: "ok"}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess := newMountSession(t, caller, root)
	writeTrustDocument(t, sess.root, content)
	docs, err := loadProjectContextDocs(t.Context(), sess.root, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	state := newProjectContextState(sess.root, docs, false, nil)
	sess.projectContext = &state
	return sess, caller
}

func trustSlash(t *testing.T, sess *replSession, line string) string {
	t.Helper()
	var out strings.Builder
	dispatchSlash(t.Context(), &out, sess, line)
	return out.String()
}

func trustGoal(t *testing.T, sess *replSession, caller *captureCaller) string {
	t.Helper()
	caller.messages = nil
	caller.system = ""
	if _, err := runOnce(t.Context(), io.Discard, nil, sess, "inspect", nil); err != nil {
		t.Fatal(err)
	}
	return caller.system
}

func TestProjectTrustLiveConsentAndFreshness(t *testing.T) {
	sess, caller := newTrustSession(t, "original-guidance")
	digest := trustFixtureDigest(t, sess.root)
	for _, line := range []string{"/trust", "/trust bad", "/trust sha256:" + strings.Repeat("0", 64)} {
		trustSlash(t, sess, line)
		if system := trustGoal(t, sess, caller); strings.Contains(system, "original-guidance") {
			t.Fatalf("%s admitted docs: %s", line, system)
		}
	}
	trustSlash(t, sess, "/trust "+digest)
	if system := trustGoal(t, sess, caller); !strings.Contains(system, "original-guidance") || !strings.Contains(system, "<<<PROJECT_CONTEXT ") {
		t.Fatalf("matching consent not on wire: %s", system)
	}
	before, tools := sess.baseSystem, reflect.ValueOf(sess.tools).Pointer()
	trustSlash(t, sess, "/trust "+digest)
	trustSlash(t, sess, "/trust malformed")
	if system := trustGoal(t, sess, caller); system != before || reflect.ValueOf(sess.tools).Pointer() != tools {
		t.Fatal("unchanged consent/invalid argument replaced or revoked snapshot")
	}
	writeTrustDocument(t, sess.root, "modified-guidance")
	trustSlash(t, sess, "/trust malformed")
	if system := trustGoal(t, sess, caller); strings.Contains(system, "<<<PROJECT_CONTEXT ") || strings.Contains(system, "guidance") {
		t.Fatalf("changed snapshot survived bad consent: %s", system)
	}
	writeTrustDocument(t, sess.root, "original-guidance")
	if system := trustGoal(t, sess, caller); strings.Contains(system, "original-guidance") {
		t.Fatal("A-B-A restored revoked grant")
	}
}

func TestProjectTrustGoalRefreshChanges(t *testing.T) {
	for _, kind := range []string{"same-size", "tail", "add-global", "remove", "global-relocation", "omitted-global"} {
		t.Run(kind, func(t *testing.T) {
			content := "approved-A"
			if kind == "tail" || kind == "omitted-global" {
				content = strings.Repeat("P", projectContextMaxBytes) + "A"
			}
			sess, caller := newTrustSession(t, content)
			global := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "golem")
			if kind == "global-relocation" || kind == "omitted-global" {
				if err := os.MkdirAll(global, 0700); err != nil {
					t.Fatal(err)
				}
				writeTrustDocument(t, global, "global-A")
			}
			trustSlash(t, sess, "/trust "+trustFixtureDigest(t, sess.root))
			if !strings.Contains(trustGoal(t, sess, caller), "<<<PROJECT_CONTEXT ") {
				t.Fatal("fixture not approved")
			}
			switch kind {
			case "same-size":
				writeTrustDocument(t, sess.root, "approved-B")
			case "tail":
				writeTrustDocument(t, sess.root, strings.Repeat("P", projectContextMaxBytes)+"B")
			case "remove":
				if err := os.Remove(filepath.Join(sess.root, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
			case "add-global":
				if err := os.MkdirAll(global, 0700); err != nil {
					t.Fatal(err)
				}
				writeTrustDocument(t, global, "global-A")
			case "omitted-global":
				writeTrustDocument(t, global, "global-B")
			case "global-relocation":
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				newGlobal := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "golem")
				if err := os.MkdirAll(newGlobal, 0700); err != nil {
					t.Fatal(err)
				}
				writeTrustDocument(t, newGlobal, "global-A")
			}
			if system := trustGoal(t, sess, caller); strings.Contains(system, "<<<PROJECT_CONTEXT ") {
				t.Fatalf("%s retained context: %s", kind, system)
			}
		})
	}
}

func TestProjectTrustResetAndFailedApproval(t *testing.T) {
	for _, line := range []string{"/new", "/clear", "/grants clear", "/resume missing"} {
		t.Run(line, func(t *testing.T) {
			sess, caller := newTrustSession(t, "reset-guidance")
			trustSlash(t, sess, "/trust "+trustFixtureDigest(t, sess.root))
			trustSlash(t, sess, line)
			want := line == "/resume missing"
			if system := trustGoal(t, sess, caller); strings.Contains(system, "reset-guidance") != want {
				t.Fatalf("%s retained=%v, want %v", line, !want, want)
			}
		})
	}
	t.Run("failed approval", func(t *testing.T) {
		sess, _ := newTrustSession(t, "reject-guidance")
		if err := sess.runtime.Close(); err != nil {
			t.Fatal(err)
		}
		trustSlash(t, sess, "/trust "+trustFixtureDigest(t, sess.root))
		if sess.projectContext.trusted(sess.grants) {
			t.Fatal("failed Replace granted trust")
		}
	})
}

func TestProjectTrustLaterFailureEnvelope(t *testing.T) {
	for _, format := range []string{"text", "json", "stream-json"} {
		t.Run(format, func(t *testing.T) {
			config, root, bodies := dispatchOneShotHarness(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			writeTrustDocument(t, root, "initial-trusted")
			in, out, diag := runTestFiles(t)
			err := run([]string{"-config", config, "-root", root, "-p", "hi", "-output-format", format, "-trust-project-context", trustFixtureDigest(t, root), "-no-probe", "-no-cap-probe", "-no-git-context", "-no-rag"}, in, out, diag, runHooks{afterSessionReady: func(s *replSession) error { writeTrustDocument(t, root, "changed-guidance"); return nil }})
			if err == nil || exitCodeFor(err) != 1 || len(bodies()) != 0 {
				t.Fatalf("late change: err=%v exit=%d provider=%d", err, exitCodeFor(err), len(bodies()))
			}
			assertTrustFailureOutput(t, format, readRunTestFile(t, out))
		})
	}
}

func TestProjectTrustCanceledRefresh(t *testing.T) {
	sess, caller := newTrustSession(t, "canceled-guidance")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runOnce(ctx, io.Discard, nil, sess, "hi", nil); err == nil || caller.messages != nil {
		t.Fatalf("canceled gate err=%v calls=%v", err, caller.messages)
	}
}

func TestProjectTrustGitRefreshFailureRevokes(t *testing.T) {
	sess, caller := newTrustSession(t, "git-approved-A")
	gitContextTestRun(t, sess.root, "-c", "init.defaultBranch=main", "init", "-q")
	gitContextTestCommit(t, sess.root, "tracked", "base", "base")
	trustSlash(t, sess, "/git-context refresh")
	trustSlash(t, sess, "/trust "+trustFixtureDigest(t, sess.root))
	system := trustGoal(t, sess, caller)
	if !strings.Contains(system, "git-approved-A") || !strings.Contains(system, "branch: main") {
		t.Fatal("fixture lacks project/Git")
	}
	writeTrustDocument(t, sess.root, "git-approved-B")
	// A configured content filter rejects capture deterministically.
	gitContextTestRun(t, sess.root, "config", "filter.broken.clean", "false")
	out := trustSlash(t, sess, "/git-context refresh")
	if !strings.Contains(out, "git context refresh failed") {
		t.Fatalf("capture=%s", out)
	}
	if sess.projectContext.trusted(sess.grants) || strings.Contains(sess.sysInputs.projectContext, "git-approved-A") {
		t.Fatal("failed Git capture retained project authority")
	}
	system = trustGoal(t, sess, caller)
	if strings.Contains(system, "<<<PROJECT_CONTEXT ") || !strings.Contains(system, "branch: main") {
		t.Fatalf("failed capture wire=%s", system)
	}
}

func TestProjectTrustAgentflowLateGate(t *testing.T) {
	for _, mode := range []string{"goal", "plan"} {
		t.Run(mode, func(t *testing.T) {
			config, root, bodies := dispatchOneShotHarness(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			writeTrustDocument(t, root, "agentflow-A")
			args := []string{"-config", config, "-root", root, "-no-probe", "-no-cap-probe", "-no-git-context", "-no-rag", "-trust-project-context", trustFixtureDigest(t, root)}
			if mode == "goal" {
				args = append(args, "-goal", "author", "-approve-plan-lock")
			} else {
				args = append(args, "-plan", filepath.Join(root, "missing-plan"), "-approve-plan-edits", "-approve-plan-gates")
			}
			in, out, diag := runTestFiles(t)
			err := run(args, in, out, diag, runHooks{afterSessionReady: func(s *replSession) error { writeTrustDocument(t, root, "agentflow-B"); return nil }})
			if err == nil || !strings.Contains(err.Error(), "project context untrusted") || len(bodies()) != 0 {
				t.Fatalf("%s late gate=%v provider=%d", mode, err, len(bodies()))
			}
		})
	}
}

func TestProjectTrustInitialScriptedMatrix(t *testing.T) {
	for _, mode := range []string{"prompt", "goal", "goal-lock-input", "plan"} {
		for _, kind := range []string{"mismatch", "empty", "strict-error", "deadline"} {
			t.Run(mode+"/"+kind, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("XDG_CONFIG_HOME", t.TempDir())
				switch kind {
				case "mismatch":
					writeTrustDocument(t, root, "scripted-guidance")
				case "strict-error":
					file := filepath.Join(t.TempDir(), "file")
					if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
						t.Fatal(err)
					}
					t.Setenv("XDG_CONFIG_HOME", file)
				case "deadline":
					file, err := os.Create(filepath.Join(root, "AGENTS.md"))
					if err != nil {
						t.Fatal(err)
					}
					if err := file.Truncate(1 << 36); err != nil {
						t.Fatal(err)
					}
					if err := file.Close(); err != nil {
						t.Fatal(err)
					}
				}
				args := []string{"-root", root, "-config", filepath.Join(root, "missing-config"), "-trust-project-context", "sha256:" + strings.Repeat("0", 64)}
				switch mode {
				case "prompt":
					args = append(args, "-p", "hi")
				case "goal":
					args = append(args, "-goal", "hi", "-approve-plan-lock")
				case "goal-lock-input":
					args = append(args, "-goal", "hi")
				case "plan":
					args = append(args, "-plan", "plan.json", "-approve-plan-edits", "-approve-plan-gates")
				}
				in, out, diag := runTestFiles(t)
				err := run(args, in, out, diag)
				var trustErr *projectContextTrustError
				if !errors.As(err, &trustErr) || exitCodeFor(err) != 1 {
					t.Fatalf("%s/%s = %v (exit=%d), want trust failure before config", mode, kind, err, exitCodeFor(err))
				}
				if readRunTestFile(t, out) != "" {
					t.Fatal("preflight wrote stdout")
				}
				stderr := readRunTestFile(t, diag)
				if kind == "strict-error" && !strings.Contains(stderr, "unavailable") || kind == "deadline" && !strings.Contains(stderr, "deadline exceeded") {
					t.Fatalf("missing cause: %s", stderr)
				}
			})
		}
	}
}

func TestProjectTrustReplaceFailureRetriesRemoval(t *testing.T) {
	sess, caller := newTrustSession(t, "stale-guidance")
	trustSlash(t, sess, "/trust "+trustFixtureDigest(t, sess.root))
	stale := sess.baseSystem
	if err := sess.runtime.Close(); err != nil {
		t.Fatal(err)
	}
	writeTrustDocument(t, sess.root, "fresh-guidance")
	for range 2 {
		if _, err := runOnce(t.Context(), io.Discard, nil, sess, "blocked", nil); err == nil {
			t.Fatal("closed Replace permitted goal")
		}
		if caller.messages != nil || sess.projectContext.trusted(sess.grants) || sess.baseSystem != stale {
			t.Fatal("failed removal either invoked provider, retained grant, or pretended publication")
		}
	}
	sess.runtime = newTestRuntime(t, sess.root, stale, sess.orch, nil)
	if system := trustGoal(t, sess, caller); strings.Contains(system, "<<<PROJECT_CONTEXT ") {
		t.Fatalf("retry retained stale context: %s", system)
	}
}

func TestProjectTrustPersistentLifecycle(t *testing.T) {
	for _, line := range []string{"/new", "/clear", "/resume user:other", "/resume missing", "failed-clear"} {
		t.Run(line, func(t *testing.T) {
			sess, caller := newTrustSession(t, "session-guidance")
			saved := newSessionedTestSession(t, caller, sess.root, "workspace:current")
			sess.session = saved.session
			if err := sess.session.store.Save(t.Context(), conversation.Conversation{ID: "user:other", Title: "other", Messages: []conversation.Message{{Role: "user", Content: "hi"}}}); err != nil {
				t.Fatal(err)
			}
			trustSlash(t, sess, "/trust "+trustFixtureDigest(t, sess.root))
			if line == "failed-clear" {
				if err := sess.session.Close(); err != nil {
					t.Fatal(err)
				}
				trustSlash(t, sess, "/clear")
				if sess.projectContext.trusted(sess.grants) {
					t.Fatal("failed clear retained grant")
				}
				sess.session = nil
			} else {
				trustSlash(t, sess, line)
			}
			want := line == "/resume missing"
			if system := trustGoal(t, sess, caller); strings.Contains(system, "session-guidance") != want {
				t.Fatalf("%s retained=%v, want %v", line, !want, want)
			}
		})
	}
}

func TestProjectTrustScannerAndStartupGrantConsumed(t *testing.T) {
	config, root, bodies := dispatchOneShotHarness(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	writeTrustDocument(t, root, "scanner-guidance")
	digest := trustFixtureDigest(t, root)
	for _, flag := range []string{"", digest, "sha256:" + strings.Repeat("0", 64)} {
		t.Run(flag, func(t *testing.T) {
			before := len(bodies())
			in, out, diag := runTestFiles(t)
			script := "/trust\nfirst\n/trust " + digest + "\nsecond\n/new\nthird\n"
			if _, err := in.WriteString(script); err != nil {
				t.Fatal(err)
			}
			if _, err := in.Seek(0, 0); err != nil {
				t.Fatal(err)
			}
			args := []string{"-config", config, "-root", root, "-no-probe", "-no-cap-probe", "-no-git-context", "-no-memory", "-no-session", "-no-rag", "-no-auto-index"}
			if flag != "" {
				args = append(args, "-trust-project-context", flag)
			}
			if err := run(args, in, out, diag); err != nil {
				t.Fatalf("scanner run=%v", err)
			}
			sent := bodies()[before:]
			if len(sent) != 3 {
				t.Fatalf("scanner calls=%d, want 3", len(sent))
			}
			for i, body := range sent {
				want := i == 1 || i == 0 && flag == digest
				system := gitContextSystemFromChatBody(t, body)
				if strings.Contains(system, "scanner-guidance") != want {
					t.Fatalf("flag=%s goal=%d project=%v, want %v", flag, i, !want, want)
				}
			}
		})
	}
}

func TestProjectTrustFlagSyntaxAndDisabled(t *testing.T) {
	for _, mode := range []string{"-p", "-goal"} {
		for _, value := range []string{"", "sha256:abc", "sha256:" + strings.Repeat("A", 64)} {
			t.Run(mode+value, func(t *testing.T) {
				in, out, diag := runTestFiles(t)
				err := run([]string{mode, "hi", "-trust-project-context", value}, in, out, diag)
				want := 1
				if mode == "-p" {
					want = 2
				}
				if exitCodeFor(err) != want {
					t.Fatalf("syntax %q exit=%d, want %d", value, exitCodeFor(err), want)
				}
			})
		}
	}
	config, root, bodies := dispatchOneShotHarness(t)
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	writeTrustDocument(t, root, "disabled-guidance")
	in, out, diag := runTestFiles(t)
	if err := run([]string{"-config", config, "-root", root, "-p", "/trust sha256:" + strings.Repeat("0", 64), "-no-project-context", "-no-git-context", "-no-probe", "-no-cap-probe", "-no-rag"}, in, out, diag); err != nil {
		t.Fatal(err)
	}
	if len(bodies()) != 1 || strings.Contains(gitContextSystemFromChatBody(t, bodies()[0]), "disabled-guidance") || strings.Contains(readRunTestFile(t, diag), "cannot locate config dir") {
		t.Fatal("disabled project context performed discovery or injection")
	}
}

func TestProjectTrustInspectionReportsPublishedRetention(t *testing.T) {
	sess, _ := newTrustSession(t, "manifest-guidance")
	out := trustSlash(t, sess, "/trust "+trustFixtureDigest(t, sess.root))
	if !strings.Contains(out, ", retained\n") || !strings.Contains(out, "project context approved") {
		t.Fatalf("approval manifest=%q, want actual retained status", out)
	}
	out = trustSlash(t, sess, "/trust")
	if !strings.Contains(out, ", retained\n") || !strings.Contains(out, "approved") || strings.Contains(out, "manifest-guidance") {
		t.Fatalf("inspection=%q, want evidence and approved state without body", out)
	}
}

func writeTrustDocument(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProjectTrustStartupWire(t *testing.T) {
	for _, consent := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "approved"}[consent], func(t *testing.T) {
			config, root, bodies := dispatchOneShotHarness(t)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			writeTrustDocument(t, root, "unique-project-guidance")
			args := []string{"-config", config, "-root", root, "-p", "answer", "-no-probe", "-no-cap-probe", "-no-git-context", "-no-memory", "-no-rag"}
			if consent {
				args = append(args, "-trust-project-context", trustFixtureDigest(t, root))
			}
			in, out, diag := runTestFiles(t)
			if err := run(args, in, out, diag, runHooks{afterSessionReady: func(sess *replSession) error {
				if strings.Contains(sess.sysInputs.projectContext, "unique-project-guidance") != consent {
					t.Fatalf("startup composition consent=%v contains project=%v", consent, !consent)
				}
				return nil
			}}); err != nil {
				t.Fatalf("run consent=%v: %v; %s", consent, err, readRunTestFile(t, diag))
			}
			if len(bodies()) != 1 {
				t.Fatalf("requests=%d, want 1", len(bodies()))
			}
			system := gitContextSystemFromChatBody(t, bodies()[0])
			if strings.Contains(system, "unique-project-guidance") != consent {
				t.Fatalf("wire consent=%v: %s", consent, system)
			}
			if strings.Contains(readRunTestFile(t, out), "project context") {
				t.Fatal("manifest leaked to stdout")
			}
			if !strings.Contains(readRunTestFile(t, diag), "sha256:") {
				t.Fatal("missing full digest diagnostic")
			}
		})
	}
}

func trustFixtureDigest(t *testing.T, root string) string {
	t.Helper()
	docs, err := loadProjectContextDocs(t.Context(), root, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	return formatProjectContextDigest(projectContextDigest(docs))
}

func TestProjectTrustEarlyFailureEnvelope(t *testing.T) {
	for _, format := range []string{"text", "json", "stream-json"} {
		t.Run(format, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			root := t.TempDir()
			writeTrustDocument(t, root, "unapproved")
			in, out, diag := runTestFiles(t)
			required := "sha256:" + strings.Repeat("0", 64)
			err := run([]string{"-root", root, "-config", filepath.Join(root, "does-not-exist"), "-p", "hello", "-output-format", format, "-trust-project-context", required}, in, out, diag)
			if exitCodeFor(err) != 1 || err == nil || !strings.Contains(err.Error(), "project context") {
				t.Fatalf("preflight = %v (exit %d), want trust exit 1 before config", err, exitCodeFor(err))
			}
			assertTrustFailureOutput(t, format, readRunTestFile(t, out))
			stderr := readRunTestFile(t, diag)
			for _, want := range []string{required, trustFixtureDigest(t, root), "not injected", "-trust-project-context"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr=%q, want %q", stderr, want)
				}
			}
		})
	}
}

func assertTrustFailureOutput(t *testing.T, format, output string) {
	t.Helper()
	if format == "text" {
		if output != "" {
			t.Fatalf("stdout=%q, want empty", output)
		}
		return
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("one result required: %q: %v", output, err)
	}
	if len(result) != 7 || result["schema"] != "golem.result.v1" || result["status"] != "error" {
		t.Fatalf("result=%v", result)
	}
	for _, key := range []string{"answer", "model", "stopReason", "grounding"} {
		if value, present := result[key]; !present || value != nil {
			t.Errorf("%s=%v, want null", key, result[key])
		}
	}
	failure, ok := result["error"].(map[string]any)
	if !ok || failure["code"] != "project_context_untrusted" {
		t.Fatalf("error=%v", result["error"])
	}
}
