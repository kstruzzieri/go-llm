package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/projectcontext"
)

// gitContextSystemFromChatBody extracts the system message golem sent to the
// fake openai-compat backend.
func gitContextSystemFromChatBody(t *testing.T, body string) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("chat body: %v", err)
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	t.Fatalf("no system message in %s", body)
	return ""
}

// startupSession runs golem to the point where the session exists and
// returns what it composed. extra flags follow the isolation set.
func startupSession(t *testing.T, root string, extra ...string) (system string, sess replSession, stderr string) {
	t.Helper()
	configPath, _ := writeRunLifecycleConfig(t)
	stdin, stdout, stderrFile := runTestFiles(t)
	errStop := errors.New("stop after startup")
	args := append([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe", "-no-session",
		"-no-memory", "-no-auto-index"}, extra...)
	err := run(args, stdin, stdout, stderrFile, runHooks{
		startAutoIndex: func() func() { return func() {} },
		afterSessionReady: func(s *replSession) error {
			system, sess = s.baseSystem, *s
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v (stderr: %s)", err, readRunTestFile(t, stderrFile))
	}
	return system, sess, readRunTestFile(t, stderrFile)
}

// Startup captures the repository, composes project context THEN Git context
// into the one prompt the runtime and every request surface receive, and
// retains the project documents and the snapshot for refresh. -no-git-context
// is byte-identical to the same run in a non-repository.
func TestStartupComposesGitContextIntoSystem(t *testing.T) {
	repo := gitContextTestRepo(t)
	gitContextTestCommit(t, repo, "tracked.go", "package x\n", "base")
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("repo rule: keep it short\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	system, sess, stderr := startupSession(t, repo)
	if sess.sysInputs.gitContext == "" || !strings.Contains(system, gitContextOpen) || !strings.Contains(system, "branch: main\n") {
		t.Fatalf("Git context missing from the composed prompt:\n%s", system)
	}
	if p, g := strings.Index(system, projectContextOpen), strings.Index(system, gitContextOpen); p < 0 || g < p {
		t.Fatalf("project context must precede Git context: project=%d git=%d", p, g)
	}
	if system != composeSystem(sess.sysInputs) {
		t.Fatal("baseSystem != composeSystem(sysInputs) after startup Git capture")
	}
	if sess.gitSnapshot.Block != sess.sysInputs.gitContext || sess.gitSnapshot.State.Toplevel != sess.root {
		t.Fatalf("snapshot not retained on the session: block match=%v toplevel=%q root=%q", sess.gitSnapshot.Block == sess.sysInputs.gitContext, sess.gitSnapshot.State.Toplevel, sess.root)
	}
	if len(sess.projectDocs) != 1 || !strings.Contains(sess.projectDocs[0].Content, "repo rule") {
		t.Fatalf("project documents not retained for refresh: %+v", sess.projectDocs)
	}
	if !strings.Contains(stderr, "git context: main, 1 status entry, 1 recent commit") {
		t.Fatalf("startup notice missing on stderr:\n%s", stderr)
	}

	// Opt-out and non-repository: identical prompt bytes, no Git block or notice.
	noRepo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	optOut, optOutSess, optOutErr := startupSession(t, repo, "-no-git-context", "-no-project-context")
	plain, plainSess, plainErr := startupSession(t, noRepo, "-no-project-context")
	if optOut != plain {
		t.Fatalf("-no-git-context prompt differs from the non-repository prompt:\n opt-out=%q\n plain=%q", optOut, plain)
	}
	if optOutSess.gitSnapshot.Block != "" || plainSess.gitSnapshot.Block != "" || !optOutSess.noGitContext || plainSess.noGitContext {
		t.Fatalf("snapshot/flag state: opt-out=%+v plain=%+v", optOutSess.gitSnapshot, plainSess.gitSnapshot)
	}
	if plainSess.gitSnapshot.Absence != gitContextNotRepository {
		t.Errorf("startupSession(non-repository).gitSnapshot.Absence = %v, want %v", plainSess.gitSnapshot.Absence, gitContextNotRepository)
	}
	if strings.Contains(optOutErr, "git context") || strings.Contains(plainErr, "git context") {
		t.Fatalf("absent Git context must print no notice:\n%s\n%s", optOutErr, plainErr)
	}
}

// The default runtime and planner budgets must carry a prompt with both blocks
// at the aggregate ceiling to a model caller instead of exhausting context.
func TestInjectedContextBudgetReachesTheModel(t *testing.T) {
	st := gitState{Branch: "main", TotalCommits: 5}
	for i := 0; i < 5; i++ {
		st.Commits = append(st.Commits, "abc123"+string(rune('0'+i))+" 2026-09-01 commit subject "+strings.Repeat("x", 40))
	}
	for i := 0; i < 5000; i++ {
		st.Entries = append(st.Entries, " M dir/some/longer/path/file-"+strings.Repeat("y", i%9)+".go")
	}
	st.TotalEntries = 5000
	block, payload := gitContextBlock(st, gitContextMaxBytes)
	if payload > gitContextMaxBytes || payload < gitContextMaxBytes-96 {
		t.Fatalf("fixture Git payload %d must sit at the %d component cap", payload, gitContextMaxBytes)
	}
	docs := []projectcontext.Document{{Source: "workspace", Path: "/ws/AGENTS.md", Content: strings.Repeat("rule line\n", 3000)}}
	project := projectContextBlock(docs, projectContextBudget(payload))
	body := strings.TrimPrefix(project, projectContextOpen+"\n")
	if i := strings.Index(body, "\n[project context truncated"); i >= 0 {
		body = body[:i]
	}
	if len(body)+payload > projectContextMaxBytes {
		t.Fatalf("project body %d + Git payload %d exceeds the shared %d budget", len(body), payload, projectContextMaxBytes)
	}
	if full := projectContextBlock(docs, projectContextBudget(0)); full != projectContextBlock(docs, projectContextMaxBytes) {
		t.Fatal("with no Git payload, project context must keep its prior 16 KiB cap byte-for-byte")
	}
	in := systemInputs{projectContext: project, gitContext: block}
	composed := composeSystem(in)
	if strings.Count(composed, projectContextOpen) != 1 || strings.Count(composed, gitContextOpen) != 1 || strings.Index(composed, projectContextOpen) > strings.Index(composed, gitContextOpen) {
		t.Fatalf("composed prompt must carry each block once, project first:\n%s", composed[:300])
	}

	root := t.TempDir()
	caller := &captureCaller{answer: "ok"}
	sess := newTestSession(t, caller, root)
	sess.sysInputs, sess.baseSystem = in, composed
	if err := sess.runtime.Replace(composed, nil); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if _, err := runOnce(context.Background(), &out, nil, sess, "hello", nil); err != nil {
		t.Fatalf("runtime request under the default budget: %v", err)
	}
	if caller.system != composed {
		t.Fatal("runtime sent a different System than the session composed")
	}

	planner := &captureCaller{answer: "no submission"}
	psess := newTestSession(t, planner, root)
	psess.sysInputs = in
	err := runAgentflowAuthorWithClient(context.Background(), &out, &out, nil, psess, flags{goal: "x", goalSet: true}, root, &stubLocker{}, nil)
	if !errors.Is(err, errPlannerNoSubmission) {
		t.Fatalf("planner under the default budget: %v", err)
	}
	if !strings.Contains(planner.system, gitContextOpen) || !strings.Contains(planner.system, "rule line") {
		t.Fatal("planner request lost an injected block")
	}
}

// One-shot wire contract: the repository's block reaches the model in every
// output format, -no-git-context removes it, and the startup notice stays on
// stderr so machine stdout is untouched.
func TestRunOneShotGitContextInjection(t *testing.T) {
	configPath, root, chatBodies := dispatchOneShotHarness(t)
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available")
	}
	gitContextTestRun(t, root, "-c", "init.defaultBranch=main", "init", "-q")
	gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
	base := []string{"-config", configPath, "-root", root, "-p", "say done", "-no-probe", "-no-cap-probe",
		"-no-session", "-no-memory", "-no-auto-index", "-no-project-context"}
	for _, tc := range []struct {
		name  string
		extra []string
		want  bool
	}{
		{"text", nil, true},
		{"json", []string{"-output-format", "json"}, true},
		{"stream-json", []string{"-output-format", "stream-json"}, true},
		{"text opt-out", []string{"-no-git-context"}, false},
		{"json opt-out", []string{"-output-format", "json", "-no-git-context"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(chatBodies())
			stdin, stdout, stderr := runTestFiles(t)
			if err := run(append(append([]string(nil), base...), tc.extra...), stdin, stdout, stderr, runHooks{
				startAutoIndex: func() func() { return func() {} },
			}); err != nil {
				t.Fatalf("run: %v\nstderr: %s", err, readRunTestFile(t, stderr))
			}
			bodies := chatBodies()
			if len(bodies) <= before {
				t.Fatal("no chat request reached the backend")
			}
			system := gitContextSystemFromChatBody(t, bodies[before])
			if got := strings.Contains(system, gitContextOpen) && strings.Contains(system, "branch: main\n"); got != tc.want {
				t.Fatalf("Git block on the wire = %v, want %v:\n%s", got, tc.want, system)
			}
			out, errOut := readRunTestFile(t, stdout), readRunTestFile(t, stderr)
			if strings.Contains(out, "git context") {
				t.Fatalf("Git notice leaked to stdout:\n%s", out)
			}
			if strings.Contains(errOut, "git context: main") != tc.want {
				t.Fatalf("stderr notice presence = %v, want %v:\n%s", !tc.want, tc.want, errOut)
			}
		})
	}
}

// The budget is shared on the real startup path, not only in the pure helper:
// a repository whose Git payload fills its 4 KiB component leaves project
// context at most the remainder of 16 KiB.
func TestStartupSharesInjectedContextBudget(t *testing.T) {
	repo := gitContextTestRepo(t)
	gitContextTestCommit(t, repo, "tracked.go", "package x\n", "base")
	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("untracked-%s-%03d.txt", strings.Repeat("n", 40), i)
		if err := os.WriteFile(filepath.Join(repo, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte(strings.Repeat("repo rule line\n", 1500)), 0o644); err != nil {
		t.Fatal(err)
	}
	system, sess, _ := startupSession(t, repo)
	payload := sess.gitSnapshot.PayloadBytes
	if payload > gitContextMaxBytes || payload < gitContextMaxBytes-128 {
		t.Fatalf("fixture Git payload %d must fill the %d component cap", payload, gitContextMaxBytes)
	}
	body := strings.TrimPrefix(sess.sysInputs.projectContext, projectContextOpen+"\n")
	if i := strings.Index(body, "\n[project context truncated"); i >= 0 {
		body = body[:i]
	}
	if len(body)+payload > projectContextMaxBytes {
		t.Fatalf("startup rendered project body %d + Git payload %d > shared %d budget", len(body), payload, projectContextMaxBytes)
	}
	if len(body) < projectContextMaxBytes-gitContextMaxBytes-256 {
		t.Fatalf("project body %d is far below its %d remainder; the fixture is not exercising the cap", len(body), projectContextMaxBytes-payload)
	}
	if strings.Count(system, projectContextOpen) != 1 || strings.Count(system, gitContextOpen) != 1 {
		t.Fatalf("each block must appear once in the composed prompt")
	}
}
