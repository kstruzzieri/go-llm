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

// --- Task 4: correct repository, exact counts ---

func TestLoadGitContextSubdirReportsPrefix(t *testing.T) {
	root := gitContextTestRepo(t)
	gitContextTestCommit(t, root, "top.go", "package x\n", "base")
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inner.go"), []byte("package sub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "top.go"), []byte("package y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := loadGitContext(context.Background(), "git", sub)
	if err != nil {
		t.Fatal(err)
	}
	st := snap.State
	if st.Toplevel != root || st.Prefix != "sub/" {
		t.Fatalf("toplevel=%q prefix=%q, want %q and sub/", st.Toplevel, st.Prefix, root)
	}
	// Status stays repository-wide and repository-root-relative; the prefix
	// line tells the model how tool-root paths map.
	if got := strings.Join(st.Entries, "|"); got != " M top.go|?? sub/" {
		t.Fatalf("entries=%q, want repository-root-relative paths", got)
	}
	if !strings.Contains(snap.Block, "prefix: sub/ (workspace root; strip this prefix for file-tool paths)\n") {
		t.Fatalf("block lacks the prefix line:\n%s", snap.Block)
	}
}

func TestLoadGitContextLinkedWorktreeReportsItself(t *testing.T) {
	main := gitContextTestRepo(t)
	gitContextTestCommit(t, main, "tracked.go", "package x\n", "base")
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(parent, "wt")
	gitContextTestRun(t, main, "worktree", "add", "-q", "-b", "feature", wt)
	t.Cleanup(func() { _ = exec.Command("git", "-C", main, "worktree", "remove", "--force", wt).Run() })
	if err := os.WriteFile(filepath.Join(wt, "tracked.go"), []byte("package y\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := loadGitContext(context.Background(), "git", wt)
	if err != nil {
		t.Fatal(err)
	}
	if got.State.Toplevel != wt || got.State.Branch != "feature" || strings.Join(got.State.Entries, "|") != " M tracked.go" {
		t.Fatalf("linked worktree reported %+v, want its own toplevel %q, branch feature, and its own dirty file", got.State, wt)
	}
	base, err := loadGitContext(context.Background(), "git", main)
	if err != nil {
		t.Fatal(err)
	}
	if base.State.Toplevel != main || base.State.Branch != "main" || base.State.TotalEntries != 0 {
		t.Fatalf("main checkout reported %+v, want %q on main and clean", base.State, main)
	}
}

func TestLoadGitContextSubmoduleReportsItself(t *testing.T) {
	module := gitContextTestRepo(t)
	gitContextTestCommit(t, module, "tracked.go", "package m\n", "module base")
	super := gitContextTestRepo(t)
	gitContextTestCommit(t, super, "README", "super\n", "super base")
	gitContextTestRun(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", module, "mod")
	gitContextTestRun(t, super, "commit", "-q", "-m", "add submodule")
	modRoot := filepath.Join(super, "mod")
	if err := os.WriteFile(filepath.Join(modRoot, "tracked.go"), []byte("package changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	inner, err := loadGitContext(context.Background(), "git", modRoot)
	if err != nil {
		t.Fatal(err)
	}
	if inner.State.Toplevel != modRoot || inner.State.Prefix != "" || strings.Join(inner.State.Entries, "|") != " M tracked.go" {
		t.Fatalf("submodule root reported %+v, want its own toplevel %q and its own dirty file", inner.State, modRoot)
	}
	// --ignore-submodules=dirty: the superproject never spawns a status inside
	// the submodule (whose config is as untrusted as its own), so modified
	// content there is not reported; a changed submodule HEAD is, because the
	// gitlink comparison needs no child process.
	outer, err := loadGitContext(context.Background(), "git", super)
	if err != nil {
		t.Fatal(err)
	}
	if outer.State.Toplevel != super || outer.State.TotalEntries != 0 {
		t.Fatalf("superproject reported %+v, want no entry for dirty submodule content", outer.State)
	}
	gitContextTestRun(t, modRoot, "add", "--", "tracked.go")
	gitContextTestRun(t, modRoot, "commit", "-q", "-m", "module change")
	outer, err = loadGitContext(context.Background(), "git", super)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(outer.State.Entries, "|") != " M mod" || outer.State.TotalEntries != 1 {
		t.Fatalf("superproject reported %+v, want exactly one changed-submodule (HEAD) entry", outer.State)
	}
}

func TestLoadGitContextIgnoresInheritedLocationOverrides(t *testing.T) {
	a := gitContextTestRepo(t)
	gitContextTestCommit(t, a, "a.go", "package a\n", "a base")
	sub := filepath.Join(a, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitContextTestRun(t, other, "-c", "init.defaultBranch=elsewhere", "init", "-q")
	gitContextTestCommit(t, other, "b.go", "package b\n", "b base")

	// Every inherited location override points at the OTHER repository, and
	// the ceiling forbids discovery from a/sub up into a. Capture must still
	// report the opened workspace.
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	t.Setenv("GIT_COMMON_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_CEILING_DIRECTORIES", a)
	t.Setenv("GIT_DISCOVERY_ACROSS_FILESYSTEM", "0")

	snap, err := loadGitContext(context.Background(), "git", sub)
	if err != nil {
		t.Fatalf("capture under inherited overrides: %v", err)
	}
	if snap.Absence != gitContextPresent || snap.State.Toplevel != a || snap.State.Branch != "main" || snap.State.Prefix != "sub/" {
		t.Fatalf("reported %+v (absence %v), want repository %q on main with prefix sub/", snap.State, snap.Absence, a)
	}
}

func TestLoadGitContextDetachedHead(t *testing.T) {
	root := gitContextTestRepo(t)
	for i := 1; i <= 6; i++ {
		gitContextTestCommit(t, root, "f.go", "package x // "+strconv.Itoa(i)+"\n", "commit "+strconv.Itoa(i))
	}
	gitContextTestRun(t, root, "checkout", "-q", "--detach")
	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State.Branch != "HEAD (no branch)" {
		t.Fatalf("branch=%q, want Git's stable detached header", snap.State.Branch)
	}
	if len(snap.State.Commits) != 5 || snap.State.TotalCommits != 5 || !strings.HasSuffix(snap.State.Commits[0], "commit 6") {
		t.Fatalf("commits=%q total=%d, want the five newest", snap.State.Commits, snap.State.TotalCommits)
	}
}

func TestLoadGitContextBareRepoWarns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(parent, "bare.git")
	gitContextTestRun(t, parent, "init", "-q", "--bare", bare)
	snap, err := loadGitContext(context.Background(), "git", bare)
	if err == nil {
		t.Fatalf("bare repository classified as absence %v; want an error the startup path warns about", snap.Absence)
	}
	var exit *gitExitError
	if !errors.As(err, &exit) || !strings.Contains(err.Error(), "work tree") {
		t.Fatalf("err=%v, want Git's bare-repository refusal surfaced", err)
	}
}

// The raw cap bounds retained bytes, never the counts: hundreds of entries past
// a small cap still report their exact total, only complete lines are kept,
// and a commit subject longer than the whole cap is dropped and counted as an
// omitted commit rather than shown as a complete one.
func TestLoadGitContextRawCapKeepsExactCounts(t *testing.T) {
	root := gitContextTestRepo(t)
	gitContextTestCommit(t, root, "base.go", "package x\n", strings.Repeat("s", 3000))
	const untracked = 300
	for i := 0; i < untracked; i++ {
		if err := os.WriteFile(filepath.Join(root, "u-"+strconv.Itoa(1000+i)+".txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	saved := gitContextRawCap
	gitContextRawCap = 2048
	t.Cleanup(func() { gitContextRawCap = saved })

	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil {
		t.Fatal(err)
	}
	st := snap.State
	if st.TotalEntries != untracked {
		t.Fatalf("TotalEntries=%d, want %d exactly despite the raw cap", st.TotalEntries, untracked)
	}
	if len(st.Entries) == 0 || len(st.Entries) >= untracked {
		t.Fatalf("retained %d entries; the fixture must actually exceed the raw cap", len(st.Entries))
	}
	for _, e := range st.Entries {
		if !strings.HasPrefix(e, "?? u-") || !strings.HasSuffix(e, ".txt") {
			t.Fatalf("retained a cut or foreign line %q", e)
		}
	}
	if st.TotalCommits != 1 || len(st.Commits) != 0 {
		t.Fatalf("commits=%q total=%d, want the oversized subject dropped and counted", st.Commits, st.TotalCommits)
	}
	if !strings.Contains(snap.Block, "recent commits (newest first):\n[... 1 more commit omitted]\n") {
		t.Fatalf("block does not report the dropped commit:\n%s", snap.Block[:300])
	}
	rendered, omitted := 0, -1
	for _, l := range strings.Split(snap.Block, "\n") {
		switch {
		case strings.HasPrefix(l, "?? u-"):
			rendered++
		case strings.HasPrefix(l, "[... ") && strings.HasSuffix(l, " more status entries omitted]"):
			omitted, _ = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(l, "[... "), " more status entries omitted]"))
		}
	}
	if omitted < 0 || rendered+omitted != untracked {
		t.Fatalf("rendered %d + omitted %d != %d", rendered, omitted, untracked)
	}
}

// core.worktree in the repository's own config relocates the work tree Git
// reports; an ancestor of root still "contains" root, so the containment check
// alone would render the intermediate path in the prefix line and let status
// enumerate the ancestor. The toplevel must be the repository discovered from
// root, nothing else.
func TestLoadGitContextRejectsRedirectedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "sibling-secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "evil")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	gitContextTestRun(t, root, "-c", "init.defaultBranch=main", "init", "-q")
	gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
	gitContextTestRun(t, root, "config", "core.worktree", "../..") // relative to the .git directory
	if got := strings.TrimSpace(gitContextTestRun(t, root, "rev-parse", "--show-toplevel")); got != parent {
		t.Fatalf("fixture: Git did not relocate the work tree (toplevel=%q, want %q)", got, parent)
	}

	snap, err := loadGitContext(context.Background(), "git", root)
	if err == nil {
		t.Fatalf("redirected work tree accepted: prefix=%q block=\n%s", snap.State.Prefix, snap.Block)
	}
	if !strings.Contains(err.Error(), "discovered") {
		t.Fatalf("err=%v, want a toplevel/discovery mismatch", err)
	}
	if strings.Contains(snap.Block, "sibling-secret") {
		t.Fatalf("ancestor content reached the block:\n%s", snap.Block)
	}
}

// With no filter driver anywhere (isolated HOME, no system config),
// `git config --get-regexp` exits 1; that is "none", not a capture error.
// Every other real-Git test here inherits the developer's global config, which
// may define git-lfs filters, so this is the only case that reaches that exit.
func TestLoadGitContextWithoutAnyFilterDrivers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := gitContextTestRepo(t)
	gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
	if out, err := exec.Command("git", "-C", root, "config", "--show-scope", "--get-regexp", `^filter\.`).CombinedOutput(); err == nil {
		t.Fatalf("fixture: a filter driver is still visible: %s", out)
	}
	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil || snap.Absence != gitContextPresent || snap.State.Branch != "main" {
		t.Fatalf("no filter drivers must be a plain success: err=%v snapshot=%+v", err, snap.State)
	}
}
