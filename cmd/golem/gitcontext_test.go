package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// envValues returns every value carried for key in env, matched the way the
// process environment is consumed on every platform golem ships to:
// case-insensitively on the key.
func envValues(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, key) {
			out = append(out, v)
		}
	}
	return out
}

// hostGitLocationKeys are the repository-location overrides every host Git
// call (Agentflow's and #354's) must drop so cmd.Dir alone selects the
// repository. GIT_TERMINAL_PROMPT is listed because the helper owns its
// value: an inherited one must not survive beside the appended =0.
var hostGitLocationKeys = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX",
	"GIT_TERMINAL_PROMPT",
}

func seedEnv(t *testing.T, keys []string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "seeded-"+k)
		t.Setenv(strings.ToLower(k), "seeded-lower-"+k)
	}
	t.Setenv("GOLEM_UNRELATED_KEEP", "kept")
}

func TestHostGitEnvStripsLocationOverridesCaseInsensitively(t *testing.T) {
	seedEnv(t, hostGitLocationKeys)
	env := hostGitEnv()
	for _, k := range hostGitLocationKeys {
		if k == "GIT_TERMINAL_PROMPT" {
			continue
		}
		if got := envValues(env, k); len(got) != 0 {
			t.Fatalf("%s survived the host Git environment filter: %q", k, got)
		}
	}
	if got := envValues(env, "GIT_TERMINAL_PROMPT"); len(got) != 1 || got[0] != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT = %q, want exactly [0]", got)
	}
	if got := envValues(env, "GOLEM_UNRELATED_KEEP"); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("unrelated variable did not survive: %q", got)
	}
}

// gitContextEnv is the capture-only filter (#354 D5): on top of the host
// filter it removes config injection and discovery overrides and pins the C
// locale so the non-repository exit is classified from stable text.
func TestGitContextEnvStripsConfigAndDiscoveryOverrides(t *testing.T) {
	extra := []string{
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_KEY_17", "GIT_CONFIG_VALUE_17",
		"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM", "LC_ALL",
	}
	seedEnv(t, append(append([]string(nil), hostGitLocationKeys...), extra...))
	env := gitContextEnv()
	for _, k := range append(append([]string(nil), hostGitLocationKeys...), extra...) {
		if k == "GIT_TERMINAL_PROMPT" || k == "LC_ALL" {
			continue
		}
		if got := envValues(env, k); len(got) != 0 {
			t.Fatalf("%s survived the capture environment filter: %q", k, got)
		}
	}
	if got := envValues(env, "GIT_TERMINAL_PROMPT"); len(got) != 1 || got[0] != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT = %q, want exactly [0]", got)
	}
	if got := envValues(env, "LC_ALL"); len(got) != 1 || got[0] != "C" {
		t.Fatalf("LC_ALL = %q, want exactly [C]", got)
	}
	if got := envValues(env, "GOLEM_UNRELATED_KEEP"); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("unrelated variable did not survive: %q", got)
	}
	// The capture filter is strictly a superset of the host filter: everything
	// the host filter keeps survives unless it is one of the capture-only keys
	// seeded above. The predicate is spelled out here rather than borrowed from
	// production so the test cannot agree with a broken implementation.
	captureOnly := func(k string) bool {
		up := strings.ToUpper(k)
		if strings.HasPrefix(up, "GIT_CONFIG_KEY_") || strings.HasPrefix(up, "GIT_CONFIG_VALUE_") {
			return true
		}
		for _, x := range extra {
			if strings.EqualFold(k, x) {
				return true
			}
		}
		return false
	}
	for _, kv := range hostGitEnv() {
		k, _, _ := strings.Cut(kv, "=")
		if !captureOnly(k) && len(envValues(env, k)) == 0 {
			t.Fatalf("capture filter dropped %q, which the host filter keeps", k)
		}
	}
}

// gitContextTestRepo creates an empty repository whose root is canonical
// (symlinks resolved), exactly what main.go hands loadGitContext: git prints
// the real toplevel, so a /var -> /private/var temp dir would otherwise look
// like a different repository.
func gitContextTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitContextTestRun(t, root, "-c", "init.defaultBranch=main", "init", "-q")
	return root
}

func gitContextTestRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(hostGitEnv(),
		"GIT_AUTHOR_NAME=gitcontext-test", "GIT_AUTHOR_EMAIL=gitcontext-test@example.com",
		"GIT_COMMITTER_NAME=gitcontext-test", "GIT_COMMITTER_EMAIL=gitcontext-test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func gitContextTestCommit(t *testing.T, root, file, content, subject string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitContextTestRun(t, root, "add", "--", file)
	gitContextTestRun(t, root, "commit", "-q", "-m", subject)
}

func TestCapWriterRetainsPrefixAndCountsAllLines(t *testing.T) {
	w := &capWriter{max: 10}
	for _, chunk := range []string{"ab\ncd\n", "efgh\nij\n", "klmn\nop\nqr\n"} {
		n, err := w.Write([]byte(chunk))
		if err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil (Git must never see a short write)", chunk, n, err, len(chunk))
		}
	}
	if got := w.String(); got != "ab\ncd\nefgh" || len(got) > w.max {
		t.Fatalf("retained %q, want the first %d bytes", got, w.max)
	}
	if !w.Truncated {
		t.Fatal("Truncated not set after dropping bytes")
	}
	if w.Lines != 7 {
		t.Fatalf("Lines=%d, want 7 including the discarded tail", w.Lines)
	}
	small := &capWriter{max: 10}
	if _, _ = small.Write([]byte("one\ntwo\n")); small.Truncated || small.Lines != 2 || small.String() != "one\ntwo\n" {
		t.Fatalf("under-cap writer: truncated=%v lines=%d stored=%q", small.Truncated, small.Lines, small.String())
	}
}

var gitLogLine = regexp.MustCompile(`^[0-9a-f]{7,} \d{4}-\d{2}-\d{2} commit \d$`)

func TestLoadGitContextRealRepo(t *testing.T) {
	root := gitContextTestRepo(t)
	for i := 1; i <= 7; i++ {
		gitContextTestCommit(t, root, "tracked.go", "package x // v"+strconv.Itoa(i)+"\n", "commit "+strconv.Itoa(i))
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.go"), []byte("package x // dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("untracked\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil {
		t.Fatalf("loadGitContext: %v", err)
	}
	if snap.Absence != gitContextPresent {
		t.Fatalf("Absence=%v, want present", snap.Absence)
	}
	st := snap.State
	if st.Toplevel != root || st.Prefix != "" {
		t.Fatalf("toplevel=%q prefix=%q, want %q and no prefix at the repository root", st.Toplevel, st.Prefix, root)
	}
	if st.Branch != "main" {
		t.Fatalf("branch=%q, want main", st.Branch)
	}
	if st.Unborn {
		t.Fatal("Unborn set on a repository with commits")
	}
	if len(st.Commits) != gitContextCommits || st.TotalCommits != gitContextCommits {
		t.Fatalf("commits=%d total=%d, want %d newest", len(st.Commits), st.TotalCommits, gitContextCommits)
	}
	for i, c := range st.Commits {
		if !gitLogLine.MatchString(c) || !strings.HasSuffix(c, "commit "+strconv.Itoa(7-i)) {
			t.Fatalf("commit[%d]=%q, want %%h %%cs %%s form, newest first (commit %d)", i, c, 7-i)
		}
	}
	wantEntries := []string{" M tracked.go", "?? new.txt"}
	if strings.Join(st.Entries, "|") != strings.Join(wantEntries, "|") || st.TotalEntries != 2 {
		t.Fatalf("entries=%q total=%d, want %q", st.Entries, st.TotalEntries, wantEntries)
	}
	if snap.Block == "" || strings.Contains(snap.Block, root) || !strings.Contains(snap.Block, "branch: main\n") {
		t.Fatalf("block missing or leaks the absolute root:\n%s", snap.Block)
	}
	if body := gitBlockBody(t, snap.Block); snap.PayloadBytes != len(body) {
		t.Fatalf("PayloadBytes=%d, body=%d", snap.PayloadBytes, len(body))
	}
}

func TestLoadGitContextNonRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil {
		t.Fatalf("a non-repository must be a silent absence, got error: %v", err)
	}
	if snap.Absence != gitContextNotRepository || snap.Block != "" || snap.PayloadBytes != 0 {
		t.Fatalf("snapshot=%+v, want NotRepository with no block", snap)
	}
}

func TestLoadGitContextMissingGit(t *testing.T) {
	root := t.TempDir()
	snap, err := loadGitContext(context.Background(), "golem-test-no-such-git-binary-354", root)
	if err != nil {
		t.Fatalf("a missing git binary must be a silent absence, got error: %v", err)
	}
	if snap.Absence != gitContextGitUnavailable || snap.Block != "" {
		t.Fatalf("snapshot=%+v, want GitUnavailable with no block", snap)
	}
}

func TestLoadGitContextUnbornBranch(t *testing.T) {
	root := gitContextTestRepo(t)
	if err := os.WriteFile(filepath.Join(root, "first.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil {
		t.Fatalf("loadGitContext on an unborn branch: %v", err)
	}
	st := snap.State
	if !st.Unborn || st.TotalCommits != 0 || len(st.Commits) != 0 {
		t.Fatalf("unborn=%v commits=%d total=%d, want unborn with no commits and no log call", st.Unborn, len(st.Commits), st.TotalCommits)
	}
	if st.Branch != "No commits yet on main" {
		t.Fatalf("branch=%q, want Git's unborn header text", st.Branch)
	}
	if !strings.Contains(snap.Block, "recent commits (newest first): (none)\n") || !strings.Contains(snap.Block, "?? first.txt\n") {
		t.Fatalf("block:\n%s", snap.Block)
	}
}

// Repository identity is never inferred from a partial path: a toplevel cut by
// the raw cap is an error, not a shorter repository.
func TestLoadGitContextRejectsTruncatedToplevel(t *testing.T) {
	root := gitContextTestRepo(t)
	saved := gitContextRawCap
	gitContextRawCap = 4
	t.Cleanup(func() { gitContextRawCap = saved })
	snap, err := loadGitContext(context.Background(), "git", root)
	if err == nil || !strings.Contains(err.Error(), "malformed toplevel") {
		t.Fatalf("err=%v snapshot=%+v, want a malformed-toplevel error", err, snap)
	}
}

// An embedded newline is a legal path byte: it must not be read as a record
// terminator when the output is complete, and it is exactly the case where a
// truncated capture still ends in a newline, so the Truncated flag, not the
// terminator check, is what refuses the partial identity.
func TestLoadGitContextToplevelWithEmbeddedNewline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("newline is not a legal path byte on Windows")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "line1\nline2")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitContextTestRun(t, root, "-c", "init.defaultBranch=main", "init", "-q")

	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil {
		t.Fatalf("complete output with an embedded newline must parse: %v", err)
	}
	if snap.State.Toplevel != root || snap.State.Prefix != "" {
		t.Fatalf("toplevel=%q prefix=%q, want %q at the root", snap.State.Toplevel, snap.State.Prefix, root)
	}

	saved := gitContextRawCap
	gitContextRawCap = strings.IndexByte(root, '\n') + 1 // retained bytes end in the embedded newline
	t.Cleanup(func() { gitContextRawCap = saved })
	snap, err = loadGitContext(context.Background(), "git", root)
	if err == nil || !strings.Contains(err.Error(), "malformed toplevel") {
		t.Fatalf("err=%v snapshot=%+v, want a malformed-toplevel error for a newline-terminated partial path", err, snap)
	}
}

// A workspace that no longer exists is a genuine error (refresh must retain
// the previous block and say so), not the silent "git unavailable" absence:
// both surface as fs.ErrNotExist, and only the executable's own path may be
// classified as a missing Git.
func TestLoadGitContextMissingRootIsAnError(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := filepath.Join(t.TempDir(), "gone")
	snap, err := loadGitContext(context.Background(), "git", root)
	if err == nil {
		t.Fatalf("missing workspace classified as absence %v; want an error", snap.Absence)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err=%v, want the underlying not-exist cause preserved", err)
	}
}
