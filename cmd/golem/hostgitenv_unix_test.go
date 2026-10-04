//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agentflow"
)

// The #623 canary. A record notes only whether the canary value appears in a
// process environment, never the value itself, so a failure cannot print it.
const (
	hostGitCanaryName  = "GO_LLM_623_CANARY"
	hostGitCanaryValue = "sk-canary-623-must-not-reach-git"
)

// isolateGitConfig keeps the developer's own git configuration out of a
// real-git fixture: an ambient core.hooksPath, core.excludesFile or filter
// driver would run or change results. HOME is an empty directory, system
// config is off and no global config file is selected. Call it before the
// fixture repository is built.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM", "XDG_CONFIG_HOME"} {
		unsetenvForTest(t, name)
	}
}

func writeExecutable(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// recordScript is a /bin/sh fragment that writes one fresh record for the
// current invocation into dir: its argv, then a "canary", "home" and "path"
// line for what the process environment holds. It never writes a value.
// Paths and the canary are baked into the script text because the policy
// under test strips unknown environment variables.
func recordScript(dir, kind string) string {
	return "rec=$(mktemp " + shellQuote(filepath.Join(dir, kind+".XXXXXX")) + ") || exit 97\n" +
		"{\n" +
		"  printf 'argv'; for a in \"$@\"; do printf ' %s' \"$a\"; done; printf '\\n'\n" +
		"  if /usr/bin/env | grep -qF " + shellQuote(hostGitCanaryValue) + "; then echo canary; fi\n" +
		"  if [ -n \"${HOME+set}\" ]; then echo home; fi\n" +
		"  if [ -n \"${PATH+set}\" ]; then echo path; fi\n" +
		"} > \"$rec\"\n"
}

// installGitRecorder puts a `git` wrapper first on the parent PATH. Every
// invocation writes a record (recordScript) into the returned directory and
// then execs the real git, resolved to an absolute path beforehand. Callers
// must pass "git", not an absolute path, so command construction finds the
// wrapper. Install it only after the fixture repository is built.
func installGitRecorder(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	if real, err = filepath.Abs(real); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	fakeGitOnPath(t, recordScript(dir, "git")+"exec "+shellQuote(real)+" \"$@\"\n")
	return dir
}

type gitRecord struct {
	kind               string
	argv               []string
	canary, home, path bool
}

func readGitRecords(t *testing.T, dir string) []gitRecord {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var records []gitRecord
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		kind, _, _ := strings.Cut(e.Name(), ".")
		r := gitRecord{kind: kind}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			switch {
			case line == "argv" || strings.HasPrefix(line, "argv "):
				r.argv = strings.Fields(strings.TrimPrefix(line, "argv"))
			case line == "canary":
				r.canary = true
			case line == "home":
				r.home = true
			case line == "path":
				r.path = true
			}
		}
		records = append(records, r)
	}
	return records
}

func clearRecords(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
}

// requireGitRecord fails unless some record of kind has words as a
// contiguous run of its argv. With no words, any record of kind matches.
func requireGitRecord(t *testing.T, records []gitRecord, kind string, words ...string) {
	t.Helper()
	for _, r := range records {
		if r.kind != kind {
			continue
		}
		for i := 0; i+len(words) <= len(r.argv); i++ {
			if slices.Equal(r.argv[i:i+len(words)], words) {
				return
			}
		}
	}
	t.Fatalf("no %s record with argv %q among %d records", kind, words, len(records))
}

// requireCleanRecords is the leak check. It first requires that records
// exist, so a fixture that never ran cannot pass as "no leak". Every bad
// record is reported, not just the first, so a leak shows which kinds of
// process (git, hook, smudge, fsmonitor, clean) each caught it.
func requireCleanRecords(t *testing.T, records []gitRecord) {
	t.Helper()
	if len(records) == 0 {
		t.Fatal("no records: the recorded processes never ran")
	}
	for _, r := range records {
		if r.canary {
			t.Errorf("the parent canary reached a %s process (argv %q)", r.kind, r.argv)
		}
		if !r.home || !r.path {
			t.Errorf("a %s process lacks the baseline: home=%v path=%v", r.kind, r.home, r.path)
		}
	}
	if t.Failed() {
		t.FailNow()
	}
}

// Every git command a parallel cohort runs goes through runParallelGit or one
// of the two direct execs (symbolic-ref, check-ignore); the recorder sees all
// three construction sites, so a site that bypasses hostGitEnv is caught.
func TestParallelCohortGitDoesNotSeeParentEnvironment(t *testing.T) {
	isolateGitConfig(t)
	root := newParallelTestRepo(t)
	worker := func(_ context.Context, w parallelWorker) error {
		switch w.sourceID {
		case "w1":
			return os.WriteFile(filepath.Join(w.root, "created.go"), []byte("created\n"), 0o600)
		case "w2":
			return os.WriteFile(filepath.Join(w.root, "b.go"), []byte("b changed\n"), 0o600)
		default:
			return fmt.Errorf("unexpected worker %s", w.sourceID)
		}
	}
	c := newParallelCoordinator(root, parallelTestPlan("created.go", "b.go"), 2, worker)
	c.aggregate = func(context.Context, []agentflow.AggregationInput, string, string, bool) (agentflow.AggregationResult, error) {
		return agentflow.AggregationResult{Status: "ok"}, nil
	}
	t.Cleanup(func() { _ = c.cleanup(context.Background()) })
	records := installGitRecorder(t)
	t.Setenv(hostGitCanaryName, hostGitCanaryValue)

	if ran, err := c.runCohort(context.Background()); err != nil || !ran {
		t.Fatalf("runCohort() = %v, %v", ran, err)
	}
	if err := c.cleanup(context.Background()); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	got := readGitRecords(t, records)
	for _, words := range [][]string{
		{"worktree", "add"}, {"worktree", "remove"}, {"rev-parse"}, {"status"},
		{"ls-files"}, {"ls-tree"}, {"symbolic-ref"}, {"check-ignore"},
	} {
		requireGitRecord(t, got, "git", words...)
	}
	requireCleanRecords(t, got)
}

func TestGitContextCaptureDoesNotSeeParentEnvironment(t *testing.T) {
	isolateGitConfig(t)
	root := gitContextTestRepo(t)
	gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
	records := installGitRecorder(t)
	t.Setenv(hostGitCanaryName, hostGitCanaryValue)

	snap, err := loadGitContext(context.Background(), "git", root)
	if err != nil || snap.Absence != gitContextPresent {
		t.Fatalf("capture: err=%v absence=%v", err, snap.Absence)
	}
	got := readGitRecords(t, records)
	for _, words := range [][]string{{"rev-parse", "--show-toplevel"}, {"config"}, {"status"}, {"log"}} {
		requireGitRecord(t, got, "git", words...)
	}
	requireCleanRecords(t, got)
}

// Repository-controlled children: a post-checkout hook and a smudge filter run
// by worktree add, and a core.fsmonitor helper run by status.
func TestParallelGitChildrenDoNotSeeParentEnvironment(t *testing.T) {
	isolateGitConfig(t)
	root := newParallelTestRepo(t)
	children := t.TempDir()
	scripts := t.TempDir()
	hooks := filepath.Join(scripts, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(hooks, "post-checkout"), recordScript(children, "hook"))
	smudge := filepath.Join(scripts, "smudge")
	writeExecutable(t, smudge, recordScript(children, "smudge")+"exec cat\n")
	fsmonitor := filepath.Join(scripts, "fsmonitor")
	writeExecutable(t, fsmonitor, recordScript(children, "fsmonitor")+"printf 'token\\000/\\000'\n")
	writeTestFile(t, filepath.Join(root, ".gitattributes"), "*.go filter=probe\n", 0o600)
	runTestGit(t, root, "add", ".gitattributes")
	runTestGit(t, root, "commit", "-m", "attributes")
	runTestGit(t, root, "config", "core.hooksPath", hooks)
	runTestGit(t, root, "config", "filter.probe.smudge", smudge)
	runTestGit(t, root, "config", "filter.probe.clean", "cat")
	runTestGit(t, root, "config", "core.fsmonitor", fsmonitor)
	// Control: an ordinary status runs the fsmonitor helper on this git.
	runTestGit(t, root, "status", "--porcelain")
	fsmonitorRuns := slices.ContainsFunc(readGitRecords(t, children), func(r gitRecord) bool { return r.kind == "fsmonitor" })
	clearRecords(t, children)
	t.Setenv(hostGitCanaryName, hostGitCanaryValue)

	ctx := context.Background()
	worktree := filepath.Join(t.TempDir(), "wt")
	if _, err := runParallelGit(ctx, root, "worktree", "add", "--detach", worktree, "HEAD"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = runParallelGit(context.Background(), root, "worktree", "remove", "--force", worktree) })
	if _, err := runParallelGit(ctx, root, "status", "--porcelain=v1", "-z"); err != nil {
		t.Fatal(err)
	}
	got := readGitRecords(t, children)
	requireGitRecord(t, got, "hook")
	requireGitRecord(t, got, "smudge")
	if fsmonitorRuns {
		requireGitRecord(t, got, "fsmonitor")
	} else {
		t.Log("this git does not run core.fsmonitor helpers from status; hook and filter observed only")
	}
	requireCleanRecords(t, got)
}

// Each user-config trust root is exercised alone: a global clean filter
// assigned to a tracked file runs during capture's status when the file's
// stat is stale. If the root were not forwarded, git would not see the
// filter and no record would appear.
func TestGitContextForwardsUserConfigTrustRoots(t *testing.T) {
	for _, tc := range []struct {
		name   string
		choose func(t *testing.T, config string)
	}{
		{name: "XDG_CONFIG_HOME", choose: func(t *testing.T, config string) {
			xdg := t.TempDir()
			if err := os.Mkdir(filepath.Join(xdg, "git"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(xdg, "git", "config"), config, 0o600)
			t.Setenv("XDG_CONFIG_HOME", xdg)
		}},
		{name: "GIT_CONFIG_GLOBAL", choose: func(t *testing.T, config string) {
			path := filepath.Join(t.TempDir(), "config")
			writeTestFile(t, path, config, 0o600)
			t.Setenv("GIT_CONFIG_GLOBAL", path)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateGitConfig(t)
			children := t.TempDir()
			clean := filepath.Join(t.TempDir(), "clean")
			writeExecutable(t, clean, recordScript(children, "clean")+"exec cat\n")
			root := gitContextTestRepo(t)
			writeTestFile(t, filepath.Join(root, ".gitattributes"), "tracked.go filter=probe\n", 0o600)
			gitContextTestRun(t, root, "add", "--", ".gitattributes")
			gitContextTestCommit(t, root, "tracked.go", "package x\n", "base")
			tc.choose(t, "[filter \"probe\"]\n\tclean = "+clean+"\n")
			future := time.Now().Add(2 * time.Hour)
			if err := os.Chtimes(filepath.Join(root, "tracked.go"), future, future); err != nil {
				t.Fatal(err)
			}
			clearRecords(t, children)
			t.Setenv(hostGitCanaryName, hostGitCanaryValue)

			snap, err := loadGitContext(context.Background(), "git", root)
			if err != nil || snap.Absence != gitContextPresent {
				t.Fatalf("capture: err=%v absence=%v", err, snap.Absence)
			}
			got := readGitRecords(t, children)
			requireGitRecord(t, got, "clean")
			requireCleanRecords(t, got)
		})
	}
}

// A path ignored only through $XDG_CONFIG_HOME/git/ignore must not be a
// parallel candidate; the control case shows the same plan selects it.
func TestParallelEligibilityHonorsXDGIgnore(t *testing.T) {
	for _, tc := range []struct {
		name, ignore string
		want         []string
	}{
		{name: "not ignored", ignore: "", want: []string{"P1", "P2"}},
		{name: "ignored only through XDG_CONFIG_HOME", ignore: "created.go\n", want: []string{"P2", "P3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateGitConfig(t)
			xdg := t.TempDir()
			if err := os.Mkdir(filepath.Join(xdg, "git"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(xdg, "git", "ignore"), tc.ignore, 0o600)
			t.Setenv("XDG_CONFIG_HOME", xdg)
			root := newParallelTestRepo(t)
			c := newParallelCoordinator(root, parallelTestPlan("created.go", "a.go", "b.go"), 2, nil)
			selected, err := c.selectWorkers(context.Background())
			defer func() { _ = c.releaseWorkspaceLock() }()
			if err != nil || !selected || !reflect.DeepEqual(workerStepIDs(c.workers), tc.want) {
				t.Fatalf("selected=%v err=%v workers=%v, want %v", selected, err, workerStepIDs(c.workers), tc.want)
			}
		})
	}
}
