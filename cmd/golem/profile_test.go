package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// trustedSelectors is spec section 4's legacy opt-in list, each with argv that
// sets only that one flag. The flag names are literals: the selection notice
// and the conflict error must name exactly these.
var trustedSelectors = []struct {
	flag string
	args []string
}{
	{"-allow-write", []string{"-allow-write"}},
	{"-allow-exec", []string{"-allow-exec"}},
	{"-scratch", []string{"-scratch"}},
	{"-allow-tool", []string{"-allow-tool", "run_command"}},
	{"-rag-db", []string{"-rag-db", "x.db"}},
	{"-agent-memory", []string{"-agent-memory"}},
	{"-delegate", []string{"-delegate"}},
	{"-dispatch", []string{"-dispatch"}},
	{"-mcp-stdio", []string{"-mcp-stdio", "srv=cmd"}},
	{"-mcp-http", []string{"-mcp-http", "srv=https://mcp.invalid"}},
	{"-progressive", []string{"-progressive"}},
	{"-grounding", []string{"-grounding"}},
}

// agentflowInvocations are the four AgentFlow modes, which keep their own tool
// policy and take no profile.
var agentflowInvocations = []struct {
	name string
	args []string
}{
	{"plan", []string{"-plan", "p.json", "-approve-plan-edits", "-approve-plan-gates"}},
	{"goal", []string{"-goal", "g"}},
	{"status", []string{"-agentflow-status"}},
	{"resume", []string{"-plan", "p.json", "-agentflow-resume", "-approve-plan-edits", "-approve-plan-gates"}},
}

func validateArgs(t *testing.T, args ...string) error {
	t.Helper()
	f, err := parseFlags(args)
	if err != nil {
		t.Fatalf("parseFlags(%q) = %v", args, err)
	}
	return validateFlags(f)
}

func TestValidateFlagsProfile(t *testing.T) {
	type row struct {
		name string
		args []string
		want string // "" means valid
	}
	rows := []row{
		{"inspect alone", []string{"-profile", "inspect"}, ""},
		{"trusted alone", []string{"-profile", "trusted"}, ""},
		{"trusted with writes", []string{"-profile", "trusted", "-allow-write"}, ""},
		{"inspect with feedback", []string{"-profile", "inspect", "-feedback"}, ""},
		// -no-session stops -agent-memory promoting a profile, not conflicting
		// with an explicit one (spec section 4: conflicts are errors).
		{"inspect with sessionless agent memory", []string{"-profile", "inspect", "-agent-memory", "-no-session"}, "golem: -profile inspect conflicts with -agent-memory"},
		{"preview", []string{"-profile", "preview"}, "golem: Preview unavailable: staged-workspace runtime requires #388/#389"},
		{"preview with tools", []string{"-profile", "preview", "-allow-write"}, "golem: Preview unavailable: staged-workspace runtime requires #388/#389"},
		// The conflict check runs before one-shot normalization drops -allow-write.
		{"inspect one-shot write", []string{"-profile", "inspect", "-p", "hi", "-allow-write"}, "golem: -profile inspect conflicts with -allow-write"},
	}
	for _, sel := range trustedSelectors {
		rows = append(rows, row{"inspect conflicts with " + sel.flag,
			append([]string{"-profile", "inspect"}, sel.args...),
			"golem: -profile inspect conflicts with " + sel.flag})
	}
	for _, af := range agentflowInvocations {
		for _, p := range []string{"trusted", "inspect"} {
			rows = append(rows, row{p + " with " + af.name,
				append([]string{"-profile", p}, af.args...),
				"golem: -profile does not apply to AgentFlow modes (-plan, -goal, -agentflow-status, -agentflow-resume)"})
		}
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			err := validateArgs(t, tc.args...)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateFlags(%q) = %v, want nil", tc.args, err)
				}
				return
			}
			if err == nil || err.Error() != tc.want {
				t.Fatalf("validateFlags(%q) = %v, want %q", tc.args, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"bogus", "Trusted", ""} {
		t.Run("bad value "+fmt.Sprintf("%q", bad), func(t *testing.T) {
			err := validateArgs(t, "-profile", bad)
			if err == nil || err.Error() != "golem: -profile must be inspect, preview, or trusted" {
				t.Fatalf("validateFlags(-profile %q) = %v", bad, err)
			}
		})
	}
}

// resolveArgs mirrors run: parse, then task/goal/one-shot normalization, then
// resolution. It skips validateFlags on purpose so a selector can be set alone
// (-scratch without -allow-exec), which isolates each trustedOptIn row.
func resolveArgs(t *testing.T, args ...string) (golemruntime.Profile, string) {
	t.Helper()
	f, err := parseFlags(args)
	if err != nil {
		t.Fatalf("parseFlags(%q) = %v", args, err)
	}
	f, _ = applyTaskMode(f)
	f, _ = applyGoalMode(f)
	f, _ = applyOneShotMode(f)
	return resolveProfile(f)
}

func TestResolveProfile(t *testing.T) {
	type row struct {
		name   string
		args   []string
		want   golemruntime.Profile
		notice string
	}
	rows := []row{
		{"no flags", nil, golemruntime.ProfileInspect, ""},
		{"one-shot", []string{"-p", "hi"}, golemruntime.ProfileInspect, ""},
		{"feedback", []string{"-feedback"}, golemruntime.ProfileInspect, ""},
		{"explicit trusted", []string{"-profile", "trusted"}, golemruntime.ProfileTrusted, ""},
		{"explicit trusted with a selector", []string{"-profile", "trusted", "-allow-write"}, golemruntime.ProfileTrusted, ""},
		{"explicit inspect", []string{"-profile", "inspect"}, golemruntime.ProfileInspect, ""},
	}
	for _, sel := range trustedSelectors {
		rows = append(rows, row{sel.flag, sel.args, golemruntime.ProfileTrusted, "profile: trusted (selected by " + sel.flag + ")"})
	}
	for _, af := range agentflowInvocations {
		rows = append(rows, row{"agentflow " + af.name, af.args, golemruntime.ProfileTrusted, ""})
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got, notice := resolveArgs(t, tc.args...)
			if got != tc.want || notice != tc.notice {
				t.Fatalf("resolveProfile(%q) = (%s, %q), want (%s, %q)", tc.args, got, notice, tc.want, tc.notice)
			}
		})
	}
}

// A flag that normalization drops must not promote the profile (Review Focus 2).
func TestResolveProfileIgnoresNormalizedFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-p", "hi", "-allow-write"},
		{"-p", "hi", "-allow-exec", "-scratch"},
		{"-agent-memory", "-no-session"},
		{"-p", "hi", "-agent-memory"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got, notice := resolveArgs(t, args...)
			if got != golemruntime.ProfileInspect || notice != "" {
				t.Fatalf("resolveProfile(%q) = (%s, %q), want (inspect, \"\")", args, got, notice)
			}
		})
	}
}

// The production gate itself, no hook override: Inspect never starts the
// auto-index job, Trusted keeps today's default.
func TestShouldStartAutoIndexSuppressed(t *testing.T) {
	var f flags
	if !shouldStartAutoIndex(applyProfileGates(f, golemruntime.ProfileTrusted)) {
		t.Fatal("Trusted with default flags must keep the auto-index default")
	}
	if shouldStartAutoIndex(applyProfileGates(f, golemruntime.ProfileInspect)) {
		t.Fatal("Inspect must not start the auto-index job")
	}
}

// isolateRunEnv keeps a run off the developer's real data and config dirs.
func isolateRunEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

// writeRemoteEmbeddingConfig serves a local agent model and configures the
// embedding default on a remote destination that nothing admits: a
// noninteractive run that plans the embedding route fails closed.
func writeRemoteEmbeddingConfig(t *testing.T) (configPath, root string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveCompat(w, r, "agent-model", nil)
	}))
	t.Cleanup(server.Close)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configPath = filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "local":  {"base_url": %q, "api_format": "openai-compat", "timeout": "2s"},
    "remote": {"base_url": "https://embeddings.invalid", "api_format": "openai-compat", "timeout": "2s"}
  },
  "models": {
    "agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "stream", "tool_call"]},
    "embedding": {"name": "qwen3-embedding:8b", "provider": "remote", "type": "embedding", "dimensions": 768,
      "capabilities": ["embed"]}
  },
  "defaults": {"agent": "agent", "embedding": "embedding"}
}`, server.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, root
}

func stderrLines(t *testing.T, file *os.File) []string {
	t.Helper()
	return strings.Split(strings.TrimSpace(readRunTestFile(t, file)), "\n")
}

func TestInspectStartupCapabilities(t *testing.T) {
	isolateRunEnv(t)
	configPath, root := writeRemoteEmbeddingConfig(t)
	// The workspace already has an auto index from an earlier Trusted run;
	// Inspect must not infer Trusted from it.
	dbPath, workspaceID, err := indexDBPathForWorkspace(os.Getenv, root)
	if err != nil {
		t.Fatal(err)
	}
	seedIndex(t, dbPath, workspaceID, "remote/qwen3-embedding:8b")
	args := []string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
		"-no-session", "-no-compress", "-no-project-context", "-no-git-context"}

	// Positive control: the same fixture under Trusted plans the remote
	// embedding route and is denied, so the Inspect run below proves the
	// route was never planned rather than that nothing was remote.
	t.Run("trusted control is denied", func(t *testing.T) {
		stdin, stdout, stderr := runTestFiles(t)
		err := run(append(append([]string{}, args...), "-profile", "trusted"), stdin, stdout, stderr)
		if !errors.Is(err, provider.ErrDestinationDenied) {
			t.Fatalf("trusted run = %v, want ErrDestinationDenied\nstderr:\n%s", err, readRunTestFile(t, stderr))
		}
	})

	stdin, stdout, stderr := stdinFileWith(t, "/remember x\n/tools\n")
	var sess *replSession
	var perms golemruntime.EffectivePermissions
	err = run(args, stdin, stdout, stderr, runHooks{
		afterSessionReady: func(s *replSession) error {
			sess = s
			perms = s.runtime.EffectivePermissions()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("inspect run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	if sess == nil {
		t.Fatal("session was not captured")
	}
	errOut := readRunTestFile(t, stderr)
	if strings.Contains(errOut, "Allow this session") || strings.Contains(errOut, "embeddings.invalid") {
		t.Fatalf("inspect startup asked about the embedding destination:\n%s", errOut)
	}
	if perms.Profile != golemruntime.ProfileInspect || sess.profile != golemruntime.ProfileInspect {
		t.Fatalf("profile = %s (session %s), want inspect", perms.Profile, sess.profile)
	}
	var mounted []string
	for _, tool := range perms.Tools {
		mounted = append(mounted, tool.Name)
	}
	if want := []string{"read_file", "search", "glob", "list"}; !slices.Equal(mounted, want) {
		t.Fatalf("mounted tools = %q, want %q", mounted, want)
	}
	fragment := memorySystemFragment(true)
	if !strings.Contains(fragment, "memory_search") {
		t.Fatalf("fixture: memory fragment %q does not name memory_search", fragment)
	}
	if strings.Contains(sess.baseSystem, fragment) || sess.sysInputs.memory {
		t.Fatalf("inspect system prompt advertises memory_search:\n%s", sess.baseSystem)
	}
	for _, unwanted := range []string{"retrieve unavailable", "retrieve:", "retrieve auto-index", "retrieve disabled"} {
		if strings.Contains(errOut, unwanted) {
			t.Errorf("stderr carries %q under inspect:\n%s", unwanted, errOut)
		}
	}
	// Human memory stays a host operation (spec section 4, REV 3).
	if sess.memory == nil || !strings.Contains(errOut, "memory: enabled") {
		t.Fatalf("human memory store not opened under inspect:\n%s", errOut)
	}
	out := readRunTestFile(t, stdout)
	if strings.Contains(out, "memory disabled") || !strings.Contains(out, "remembered ") {
		t.Fatalf("/remember under inspect:\n%s", out)
	}
	if strings.Contains(out, "retrieve") || strings.Contains(out, "memory_search") {
		t.Fatalf("/tools under inspect names retrieval or memory tools:\n%s", out)
	}
}

// An unusable auto-index path is a retrieval warning; Inspect plans no
// retrieval, so it stays silent there while Trusted still reports it.
func TestInspectSilencesAutoIndexPathWarning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		want    bool
	}{
		{"inspect", "inspect", false},
		{"trusted control", "trusted", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunEnv(t)
			configPath, root := writeRunLifecycleConfig(t)
			// A data dir inside the workspace makes the auto-index path invalid.
			t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop at session ready")
			err := run([]string{"-config", configPath, "-root", root, "-profile", tc.profile, "-no-probe", "-no-cap-probe",
				"-no-session", "-no-memory", "-no-project-context"}, stdin, stdout, stderr,
				runHooks{afterSessionReady: func(*replSession) error { return errStop }})
			if !errors.Is(err, errStop) {
				t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
			}
			if got := strings.Contains(readRunTestFile(t, stderr), "retrieve auto-index disabled"); got != tc.want {
				t.Fatalf("auto-index path warning = %t, want %t\nstderr:\n%s", got, tc.want, readRunTestFile(t, stderr))
			}
		})
	}
}

func TestStartupNoticeInspectNamesTrustedRestore(t *testing.T) {
	isolateRunEnv(t)
	configPath, root := writeRunLifecycleConfig(t)
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop at session ready")
	err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
		"-no-session", "-no-memory", "-no-project-context"}, stdin, stdout, stderr,
		runHooks{afterSessionReady: func(*replSession) error { return errStop }})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	lines := stderrLines(t, stderr)
	want := "permissions: profile=inspect tools=read_file,search,glob,list commands=n/a; restart with -profile trusted to enable retrieval, memory tools, writes or commands"
	if !slices.Contains(lines, want) {
		t.Fatalf("stderr lines = %q, want %q", lines, want)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "profile:") {
			t.Fatalf("default inspect printed a selection notice %q", line)
		}
	}
	if strings.Contains(readRunTestFile(t, stdout), "permissions") {
		t.Fatalf("stdout carries the notice: %q", readRunTestFile(t, stdout))
	}
}

func TestStartupNoticeTrustedHost(t *testing.T) {
	const host = "commands=host (unsandboxed; runs with the host user's filesystem and network authority)"
	t.Run("selected by -allow-exec", func(t *testing.T) {
		isolateRunEnv(t)
		configPath, root := writeRunLifecycleConfig(t)
		stdin, stdout, stderr := runTestFiles(t)
		errStop := errors.New("stop at session ready")
		var got golemruntime.Profile
		err := run([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
			"-no-session", "-no-memory", "-no-rag", "-no-project-context", "-allow-exec"}, stdin, stdout, stderr,
			runHooks{afterSessionReady: func(s *replSession) error { got = s.profile; return errStop }})
		if !errors.Is(err, errStop) {
			t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
		}
		if got != golemruntime.ProfileTrusted {
			t.Fatalf("session profile = %s, want trusted", got)
		}
		lines := stderrLines(t, stderr)
		for _, want := range []string{
			"profile: trusted (selected by -allow-exec)",
			"permissions: profile=trusted tools=read_file,search,glob,list,run_command,start_command,command_status,command_tail,stop_command " + host,
		} {
			if !slices.Contains(lines, want) {
				t.Errorf("stderr lines = %q, want %q", lines, want)
			}
		}
	})
	t.Run("one-shot json keeps the record clean", func(t *testing.T) {
		isolateRunEnv(t)
		configPath, root := writeRunLifecycleConfig(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serveCompat(w, r, "agent-model", nil)
		}))
		t.Cleanup(server.Close)
		stdin, stdout, stderr := runTestFiles(t)
		err := run([]string{"-config", configPath, "-root", root, "-base-url", server.URL, "-no-probe", "-no-cap-probe",
			"-no-rag", "-no-project-context", "-profile", "trusted", "-p", "hi", "-output-format", "json"},
			stdin, stdout, stderr)
		if err != nil {
			t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
		}
		out := readRunTestFile(t, stdout)
		recLines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(recLines) != 1 {
			t.Fatalf("stdout = %d lines, want one result record:\n%s", len(recLines), out)
		}
		if m := decodeResult(t, recLines[0]); string(m["status"]) != `"completed"` {
			t.Fatalf("status = %s, want completed: %s", m["status"], recLines[0])
		}
		if strings.Contains(out, "permissions") {
			t.Fatalf("result record carries the notice: %s", out)
		}
		lines := stderrLines(t, stderr)
		if want := "permissions: profile=trusted tools=read_file,search,glob,list " + host; !slices.Contains(lines, want) {
			t.Fatalf("stderr lines = %q, want %q", lines, want)
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "profile:") {
				t.Fatalf("explicit -profile printed a selection notice %q", line)
			}
		}
	})
}

// AgentFlow modes keep their own tool policy: Trusted, silently.
func TestAgentflowModesPrintNoProfileNotice(t *testing.T) {
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"plan", []string{"-plan", plan, "-approve-plan-edits", "-approve-plan-gates"}},
		{"goal", []string{"-goal", "plan it"}},
		{"resume", []string{"-agentflow-resume", "-plan", plan, "-approve-plan-edits", "-approve-plan-gates"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateRunEnv(t)
			configPath, root := writeRunLifecycleConfig(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serveCompat(w, r, "agent-model", nil)
			}))
			t.Cleanup(server.Close)
			stdin, stdout, stderr := runTestFiles(t)
			errStop := errors.New("stop at session ready")
			var got golemruntime.Profile
			args := append([]string{"-config", configPath, "-root", root, "-base-url", server.URL,
				"-no-probe", "-no-cap-probe", "-no-project-context"}, tc.args...)
			err := run(args, stdin, stdout, stderr, runHooks{afterSessionReady: func(s *replSession) error {
				got = s.runtime.EffectivePermissions().Profile
				return errStop
			}})
			if !errors.Is(err, errStop) {
				t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
			}
			if got != golemruntime.ProfileTrusted {
				t.Fatalf("agentflow runtime profile = %s, want trusted", got)
			}
			for _, line := range stderrLines(t, stderr) {
				if strings.HasPrefix(line, "permissions:") || strings.HasPrefix(line, "profile:") {
					t.Fatalf("agentflow mode printed %q", line)
				}
			}
		})
	}
}
