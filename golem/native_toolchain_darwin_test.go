//go:build darwin

package golem

// Declared toolchains end to end (#722): a scripted model asks for
// run_command, a recording approver approves it and keeps the preview, and the
// real Seatbelt backend runs a real toolchain under the session's policy. Each
// leg also runs without the grant or setting it relies on, and fails there.
// The darwin-smoke job runs these with GO_LLM_REQUIRE_SEATBELT=1 and
// GO_LLM_REQUIRE_XCODE_LINK=1. Darwin-only: Linux CI also selects the
// TestNativeBackend prefix, and bwrap refuses these fields.

import (
	"context"
	"crypto/rand"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

// scriptedTimeout is every scripted run_command's timeout_seconds. GOCACHE
// is cold for each command, and a cold go build or go test on a small CI
// runner can outlast the 60 s default (A0 measured on a 16-core host).
const scriptedTimeout = 300

// scriptedCaller is the model: a turn's first call asks for run_command with
// argv, and the next ends the turn, keeping the tool result it was shown.
type scriptedCaller struct {
	argv   []string
	result string
}

func (c *scriptedCaller) Chat(_ context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "tool" {
		c.result = req.Messages[n-1].Content
		return agent.ModelResult{Response: provider.ChatResponse{Content: "done", Done: true}}, nil
	}
	args, err := json.Marshal(map[string]any{"argv": c.argv, "timeout_seconds": scriptedTimeout})
	if err != nil {
		return agent.ModelResult{}, err
	}
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: "run", Type: "function", Function: provider.ToolCallFunction{Name: "run_command", Arguments: args},
	}}}}, nil
}

// recordingApprover approves every call and keeps each preview it was shown.
type recordingApprover struct{ previews []string }

func (a *recordingApprover) Approve(_ context.Context, _ provider.ToolCall, preview string) (bool, error) {
	a.previews = append(a.previews, preview)
	return true, nil
}

// toolchainSession is a Trusted runtime over root whose tools are a real
// native run_command set for policy, driven one command per turn.
type toolchainSession struct {
	t        *testing.T
	rt       *Runtime
	caller   *scriptedCaller
	approver *recordingApprover
	turns    int
}

func newToolchainSession(t *testing.T, root string, policy agenttools.SandboxConfig, scratch bool) *toolchainSession {
	t.Helper()
	var opts agenttools.ExecToolsOptions
	if scratch {
		opts.Scratch = agenttools.ScratchConfig{Enabled: true}
	}
	tools, err := agenttools.NewSandboxedExecToolsWithOptions(root, policy, opts)
	if err != nil {
		t.Fatalf("NewSandboxedExecToolsWithOptions: %v", err)
	}
	s := &toolchainSession{t: t, caller: &scriptedCaller{}, approver: &recordingApprover{}}
	if s.rt, err = newNative(t, root, policy, s.caller, tools); err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

var exitCodeLine = regexp.MustCompile(`(?m)^exit code: (-?\d+)$`)

// run runs argv in one turn and returns its exit code and the result the model
// saw. A result without an exit code (the command never ran) fails the test.
func (s *toolchainSession) run(argv ...string) (int, string) {
	s.t.Helper()
	s.turns++
	s.caller.argv, s.caller.result = argv, ""
	turn := Turn{RunID: fmt.Sprintf("run-%d", s.turns), Message: "run the command", Approver: s.approver}
	if _, err := s.rt.Run(context.Background(), turn, func(Event) error { return nil }); err != nil {
		s.t.Fatalf("turn running %q: %v", argv, err)
	}
	m := exitCodeLine.FindStringSubmatch(s.caller.result)
	if m == nil {
		s.t.Fatalf("%q did not run:\n%s", argv, s.caller.result)
	}
	code, err := strconv.Atoi(m[1])
	if err != nil {
		s.t.Fatal(err)
	}
	return code, s.caller.result
}

// ok runs argv and requires exit 0.
func (s *toolchainSession) ok(argv ...string) string {
	s.t.Helper()
	code, res := s.run(argv...)
	if code != 0 {
		s.t.Fatalf("%q exited %d:\n%s", argv, code, res)
	}
	return res
}

// fails runs argv and requires a non-zero exit whose result names reason, so
// a control cannot pass on an unrelated failure.
func (s *toolchainSession) fails(reason string, argv ...string) {
	s.t.Helper()
	code, res := s.run(argv...)
	if code == 0 {
		s.t.Fatalf("%q succeeded without what it relies on:\n%s", argv, res)
	}
	if !strings.Contains(res, reason) {
		s.t.Fatalf("%q failed (exit %d) without the expected reason %q:\n%s", argv, code, reason, res)
	}
}

// requirePreviews checks every recorded preview lists each want, quoted as
// the preview renders it, and the scripted timeout.
func (s *toolchainSession) requirePreviews(want ...string) {
	s.t.Helper()
	want = append(want, fmt.Sprintf("  timeout: %ds\n", scriptedTimeout))
	if len(s.approver.previews) == 0 {
		s.t.Fatal("no approval preview was recorded")
	}
	for _, p := range s.approver.previews {
		for _, w := range want {
			if !strings.Contains(p, w) {
				s.t.Errorf("approval preview lacks %s:\n%s", w, p)
			}
		}
	}
}

// hostGo returns this repository's toolchain root in its canonical spelling
// and the host module cache, from `go env` run where the tests run.
func hostGo(t *testing.T) (goroot, modcache string) {
	t.Helper()
	out, err := exec.Command("go", "env", "GOROOT", "GOMODCACHE").Output()
	if err != nil {
		t.Fatalf("go env: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("go env printed %q", out)
	}
	if goroot, err = agenttools.CanonicalToolchainRoot(lines[0]); err != nil {
		t.Fatal(err)
	}
	return goroot, lines[1]
}

// hostGoRun runs goroot's go on the host in dir. HOME and GOCACHE are fresh
// folders, so nothing is written under the real home.
func hostGoRun(t *testing.T, goroot, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(goroot, "bin", "go"), args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), "GOCACHE=" + t.TempDir(), "GOTOOLCHAIN=local"}, env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host go %q: %v\n%s", args, err, out)
	}
}

// hostGit runs the system git on the host in dir with home's config only.
func hostGit(t *testing.T, home, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("/usr/bin/git", args...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host git %q: %v\n%s", args, err, out)
	}
}

// writeTree writes each file under dir, creating parents.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// repoGoLine returns this repository's go directive, which the toolchain
// running these tests satisfies.
func repoGoLine(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "go "); ok {
			return strings.TrimSpace(v)
		}
	}
	t.Fatal("no go directive in ../go.mod")
	return ""
}

// syncRequirement returns the golang.org/x/sync version this repository
// requires and its go.sum lines, already verified there.
func syncRequirement(t *testing.T) (version, sum string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "golang.org/x/sync" {
			lines = append(lines, line)
			if !strings.HasSuffix(f[1], "/go.mod") {
				version = f[1]
			}
		}
	}
	for _, line := range lines {
		if v := strings.Fields(line)[1]; v == version || v == version+"/go.mod" {
			sum += line + "\n"
		}
	}
	if version == "" {
		t.Fatal("golang.org/x/sync is not in ../go.sum")
	}
	return version, sum
}

// reviewedSystemRoots restates the Seatbelt backend's fixed read roots, so
// the test can show a declared root is not already covered by one. The source
// of truth is seatbeltDefaultSystemRoots in agent/tools/exec_darwin.go; this
// copy must change with it.
var reviewedSystemRoots = []string{"/System/Library", "/System/Cryptexes/OS", "/usr/bin", "/usr/lib", "/usr/libexec", "/usr/share", "/bin", "/sbin"}

// TestNativeBackendToolchainGoBuildAndTest: with GOROOT and an offline module
// cache declared as read-only roots, the four Env rows and GOCACHE in the
// private temp, go build and go test of a module using golang.org/x/sync
// succeed in scratch through Golem turns, network denied, and the approval
// preview shows every root and value. Without either root, or without
// GOCACHE, both fail.
func TestNativeBackendToolchainGoBuildAndTest(t *testing.T) {
	policy := nativePolicyOrSkip(t)
	goroot, hostModcache := hostGo(t)
	for _, sys := range reviewedSystemRoots {
		if goroot == sys || strings.HasPrefix(goroot, sys+"/") {
			t.Fatalf("GOROOT %s is inside the system root %s, so it cannot show the declared root is needed", goroot, sys)
		}
	}
	_, base := canonicalDir(t)
	ws := filepath.Join(base, "ws")
	version, sum := syncRequirement(t)
	writeTree(t, ws, map[string]string{
		"go.mod": "module example.com/hello\n\ngo " + repoGoLine(t) + "\n\nrequire golang.org/x/sync " + version + "\n",
		"go.sum": sum,
		"main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/hello/pool\"\n)\n\n" +
			"func main() {\n\tn, err := pool.Sum(1, 2)\n\tfmt.Println(n, err)\n}\n",
		"pool/pool.go": "package pool\n\nimport \"golang.org/x/sync/errgroup\"\n\n" +
			"// Sum adds xs concurrently.\nfunc Sum(xs ...int) (int, error) {\n\tvar g errgroup.Group\n\tout := make([]int, len(xs))\n" +
			"\tfor i, x := range xs {\n\t\tg.Go(func() error { out[i] = x; return nil })\n\t}\n\terr := g.Wait()\n" +
			"\ttotal := 0\n\tfor _, v := range out {\n\t\ttotal += v\n\t}\n\treturn total, err\n}\n",
		"pool/pool_test.go": "package pool\n\nimport \"testing\"\n\n" +
			"func TestSum(t *testing.T) {\n\tif n, err := Sum(1, 2, 3); n != 6 || err != nil {\n\t\tt.Fatal(n, err)\n\t}\n}\n",
	})
	// Seed the module cache offline from the host's, then prove it complete
	// with the proxy off. -modcacherw keeps it removable; the go.sum lines come
	// from this repository, so the checksum database is not needed.
	modRoot := filepath.Join(base, "modcache")
	seedEnv := []string{"GOMODCACHE=" + modRoot, "GOFLAGS=-mod=mod -modcacherw", "GOSUMDB=off"}
	hostGoRun(t, goroot, ws, append(seedEnv, "GOPROXY=file://"+filepath.Join(hostModcache, "cache", "download")), "mod", "download")
	hostGoRun(t, goroot, ws, append(seedEnv, "GOPROXY=off"), "list", "-deps", "-test", "./...")

	home := filepath.Join(base, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(goroot, "bin")+":/usr/bin:/bin")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	full := policy.Clone()
	full.ReadOnlyRoots = []string{goroot, modRoot}
	full.Env = []agenttools.SandboxEnv{
		{Name: "GOMODCACHE", Value: modRoot}, {Name: "GOPROXY", Value: "off"},
		{Name: "GOTOOLCHAIN", Value: "local"}, {Name: "GOFLAGS", Value: "-buildvcs=false"},
	}
	full.TempEnv = []string{"GOCACHE"}

	s := newToolchainSession(t, ws, full, true)
	s.ok("go", "build", "./...")
	s.ok("go", "test", "./...")
	s.requirePreviews("read_only_roots=", strconv.Quote(goroot), strconv.Quote(modRoot),
		strconv.Quote("GOMODCACHE="+modRoot), strconv.Quote("GOPROXY=off"), strconv.Quote("GOTOOLCHAIN=local"),
		strconv.Quote("GOFLAGS=-buildvcs=false"), strconv.Quote("GOCACHE=<private temp>/gocache"))

	without := map[string]struct {
		drop   func(*agenttools.SandboxConfig)
		reason string
	}{
		"without GOROOT": {func(c *agenttools.SandboxConfig) { c.ReadOnlyRoots = []string{modRoot} }, "cannot find GOROOT"},
		"without the module cache": {func(c *agenttools.SandboxConfig) {
			c.ReadOnlyRoots = []string{goroot}
			c.Env = slices.DeleteFunc(c.Env, func(e agenttools.SandboxEnv) bool { return e.Name == "GOMODCACHE" })
		}, "could not create module cache"},
		"without GOCACHE": {func(c *agenttools.SandboxConfig) { c.TempEnv = nil }, "failed to initialize build cache"},
	}
	for name, w := range without {
		t.Run(name, func(t *testing.T) {
			cfg := full.Clone()
			w.drop(&cfg)
			s := newToolchainSession(t, ws, cfg, true)
			s.fails(w.reason, "go", "build", "./...")
			s.fails(w.reason, "go", "test", "./...")
		})
	}
}

// requireLinkedXcodeIfAsked logs which developer selection links exist. With
// GO_LLM_REQUIRE_XCODE_LINK=1, the test fails unless one exists and dev, the
// active developer directory, is an Xcode bundle: that job must exercise an
// Xcode a link selects, and a link to the Command Line Tools cannot stand in.
func requireLinkedXcodeIfAsked(t *testing.T, dev string) {
	t.Helper()
	// The source of truth is developerSelectionLinks in
	// agent/tools/sandbox_policy.go; this copy must change with it.
	links := []string{"/private/var/select/developer_dir", "/private/var/db/xcode_select_link"}
	var present []string
	for _, link := range links {
		if _, err := os.Lstat(link); err == nil {
			present = append(present, link)
		}
	}
	t.Logf("developer selection links checked %q; present %q", links, present)
	if os.Getenv("GO_LLM_REQUIRE_XCODE_LINK") != "1" {
		return
	}
	if len(present) == 0 {
		t.Fatalf("GO_LLM_REQUIRE_XCODE_LINK=1 but none of %q exists", links)
	}
	if !strings.HasSuffix(dev, ".app") {
		t.Fatalf("GO_LLM_REQUIRE_XCODE_LINK=1 but the active developer directory %s is not an Xcode bundle", dev)
	}
}

// vcsSettings counts the vcs build settings stamped into bin.
func vcsSettings(t *testing.T, bin string) int {
	t.Helper()
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range info.Settings {
		if strings.HasPrefix(s.Key, "vcs") {
			n++
		}
	}
	return n
}

// treeOf lists every path under root.
func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		out = append(out, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNativeBackendDeveloperTools: with the active developer directory
// declared, python3 runs and leaves the workspace unchanged (HOME a missing
// folder under /Users/Shared, the trigger for CLT python's stray tree), git
// runs with a .gitconfig in HOME it must not read, and go build stamps VCS
// information in a committed repository; metadata outside the grant stays
// denied; and the preview lists the fixed settings. Without the developer
// directory the first three fail. In a linked worktree the build is
// unstamped unless the main repository's .git is declared too (#722 F9).
func TestNativeBackendDeveloperTools(t *testing.T) {
	policy := nativePolicyOrSkip(t)
	dev, err := agenttools.ActiveDeveloperDirectory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("developer directory %s", dev)
	requireLinkedXcodeIfAsked(t, dev)
	goroot, _ := hostGo(t)
	_, base := canonicalDir(t)
	home := filepath.Join(base, "home")
	writeTree(t, home, map[string]string{".gitconfig": "[user]\n\tname = Golem Test\n\temail = golem-test@example.invalid\n"})
	noHome := "/Users/Shared/go-llm-no-such-home-" + rand.Text()
	if _, err := os.Lstat(noHome); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s: %v; want it missing", noHome, err)
	}
	t.Setenv("PATH", filepath.Join(goroot, "bin")+":/usr/bin:/bin")
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	repo := filepath.Join(base, "repo")
	writeTree(t, repo, map[string]string{
		"go.mod":  "module example.com/stamp\n\ngo " + repoGoLine(t) + "\n",
		"main.go": "package main\n\nfunc main() {}\n",
	})
	hostGit(t, home, repo, "init", "-q")
	hostGit(t, home, repo, "add", ".")
	hostGit(t, home, repo, "commit", "-q", "-m", "fixture")

	// The python and git legs declare only the developer directory. Python's
	// stray tree needs /Users outside every metadata spine; a GOROOT under a
	// home in /Users would put /Users on its spine and hide the trigger.
	devOnly := policy.Clone()
	devOnly.DeveloperDirectory = dev
	for _, p := range []string{dev, repo} {
		if strings.HasPrefix(p, "/Users/") {
			t.Fatalf("%s lies under /Users, which would hide python's stray tree", p)
		}
	}
	withGo := devOnly.Clone()
	withGo.ReadOnlyRoots = []string{goroot}
	withGo.TempEnv = []string{"GOCACHE"}
	python := []string{"python3", "-c", "print(1)"}

	s := newToolchainSession(t, repo, devOnly, false)
	before := treeOf(t, repo)
	t.Setenv("HOME", noHome)
	if res := s.ok(python...); !strings.Contains(res, "--- stdout ---\n1\n") {
		t.Errorf("python3 did not print 1:\n%s", res)
	}
	t.Setenv("HOME", home)
	if after := treeOf(t, repo); !slices.Equal(after, before) {
		t.Errorf("python3 changed the workspace: %q, was %q", after, before)
	}
	s.ok("git", "--version")
	s.ok("/usr/bin/stat", filepath.Join(repo, "go.mod"))
	const receipts = "/private/var/db/receipts"
	if _, err := os.Stat(receipts); err != nil {
		t.Fatalf("host stat of %s: %v; the denial below needs it to exist", receipts, err)
	}
	s.fails("Operation not permitted", "/usr/bin/stat", receipts)
	fixed := []string{strconv.Quote("GIT_CONFIG_GLOBAL=/dev/null"), strconv.Quote("GIT_CONFIG_NOSYSTEM=1"), strconv.Quote("PYTHONDONTWRITEBYTECODE=1")}
	s.requirePreviews(append(fixed, "developer_directory="+strconv.Quote(dev))...)
	if _, err := os.Lstat(noHome); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s: %v; want it still missing", noHome, err)
	}

	s = newToolchainSession(t, repo, withGo, false)
	s.ok("go", "build", "-o", "stamped", ".")
	if n := vcsSettings(t, filepath.Join(repo, "stamped")); n == 0 {
		t.Error("go build in a committed repository stamped no vcs settings")
	}
	s.requirePreviews(fixed...)

	t.Run("without the developer directory", func(t *testing.T) {
		// The xcode-select shim's own error: which link it names differs
		// between a linked Xcode and the implicit Command Line Tools.
		const shim = "xcode-select: error"
		s := newToolchainSession(t, repo, policy, false)
		t.Setenv("HOME", noHome)
		s.fails(shim, python...)
		t.Setenv("HOME", home)
		s.fails(shim, "git", "--version")
		noDev := withGo.Clone()
		noDev.DeveloperDirectory = ""
		s = newToolchainSession(t, repo, noDev, false)
		s.fails("error obtaining VCS status", "go", "build", "-o", "unstamped", ".")
	})

	t.Run("worktree", func(t *testing.T) {
		wt := filepath.Join(base, "wt")
		hostGit(t, home, repo, "worktree", "add", "-q", wt)
		s := newToolchainSession(t, wt, withGo, false)
		s.ok("go", "build", "-o", "unstamped", ".")
		if n := vcsSettings(t, filepath.Join(wt, "unstamped")); n != 0 {
			t.Errorf("go build in a linked worktree stamped %d vcs settings without the main .git declared", n)
		}
		withGit := withGo.Clone()
		withGit.ReadOnlyRoots = append(withGit.ReadOnlyRoots, filepath.Join(repo, ".git"))
		s = newToolchainSession(t, wt, withGit, false)
		s.ok("go", "build", "-o", "stamped", ".")
		if n := vcsSettings(t, filepath.Join(wt, "stamped")); n == 0 {
			t.Error("go build in a linked worktree stamped no vcs settings with the main .git declared")
		}
	})
}
