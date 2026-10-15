package containmentinv

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestRunPatternQuotesEachComponent(t *testing.T) {
	got := RunPattern([]string{"TestHardeningContracts/Active/Workspace/public_confinement/nul_byte", "TestContainment/X2a", "TestX/a+b(c)"})
	// One alternative per leaf; Go matches each per slash component, and every
	// component is anchored and QuoteMeta'd.
	want := `^(TestHardeningContracts)$/^(Active)$/^(Workspace)$/^(public_confinement)$/^(nul_byte)$|^(TestContainment)$/^(X2a)$|^(TestX)$/^(a\+b\(c\))$`
	if got != want {
		t.Fatalf("pattern\n got %s\nwant %s", got, want)
	}
}

// passingStream is a realistic go test -json stream for two leaves: every
// event carries Package, as test2json emits it; the skip reason, when any,
// rides an "output" event.
func passingStream() []TestEvent {
	const p = "github.com/kstruzzieri/go-llm/cmd/golem"
	return []TestEvent{
		{Action: "start", Package: p},
		{Action: "run", Package: p, Test: "TestContainment"},
		{Action: "run", Package: p, Test: "TestContainment/A1"},
		{Action: "pass", Package: p, Test: "TestContainment/A1"},
		{Action: "run", Package: p, Test: "TestContainment/X2a"},
		{Action: "pass", Package: p, Test: "TestContainment/X2a"},
		{Action: "pass", Package: p, Test: "TestContainment"},
		{Action: "pass", Package: p},
	}
}

var twoLeaves = []string{"TestContainment/A1", "TestContainment/X2a"}

func without(ev []TestEvent, drop func(TestEvent) bool) []TestEvent {
	var out []TestEvent
	for _, e := range ev {
		if !drop(e) {
			out = append(out, e)
		}
	}
	return out
}

func TestCheckerAcceptsPassingStream(t *testing.T) {
	if errs := Evaluate(passingStream(), twoLeaves, nil); len(errs) != 0 {
		t.Fatalf("passing stream rejected: %v", errs)
	}
}

// Each guarantee is removed independently from a passing stream.
func TestCheckerRequiresEachEvent(t *testing.T) {
	for name, drop := range map[string]func(TestEvent) bool{
		"leaf run":     func(e TestEvent) bool { return e.Action == "run" && e.Test == "TestContainment/X2a" },
		"leaf pass":    func(e TestEvent) bool { return e.Action == "pass" && e.Test == "TestContainment/X2a" },
		"package pass": func(e TestEvent) bool { return e.Action == "pass" && e.Test == "" },
	} {
		t.Run(name, func(t *testing.T) {
			// containment: META-GATE
			if errs := Evaluate(without(passingStream(), drop), twoLeaves, nil); len(errs) == 0 {
				t.Fatalf("stream without %s accepted", name)
			}
		})
	}
}

func TestCheckerRejectsSkipOnLinuxHost(t *testing.T) {
	const p = "github.com/kstruzzieri/go-llm/cmd/golem"
	skipped := []TestEvent{
		{Action: "run", Package: p, Test: "TestContainment/W3"},
		{Action: "output", Package: p, Test: "TestContainment/W3", Output: "    containment_test.go:90: capability:symlink\n"},
		{Action: "skip", Package: p, Test: "TestContainment/W3"},
		{Action: "pass", Package: p},
	}
	// containment: META-GATE
	if errs := Evaluate(skipped, []string{"TestContainment/W3"}, nil); len(errs) == 0 {
		t.Fatal("skip accepted with an empty allow-list")
	}
	if errs := Evaluate(skipped, []string{"TestContainment/W3"}, []string{"TestContainment/W3=capability:symlink"}); len(errs) != 0 {
		t.Fatalf("allowed skip rejected: %v", errs)
	}
	if errs := Evaluate(skipped, []string{"TestContainment/W3"}, []string{"TestContainment/W3=capability:bash"}); len(errs) == 0 {
		t.Fatal("skip for a different capability accepted")
	}
}

func TestCheckerSourceIdentityFromEnv(t *testing.T) {
	ev, err := SourceIdentity(func(k string) string {
		return map[string]string{"GO_LLM_CI_TREE": "abc123", "GO_LLM_CI_COMMIT": "def456"}[k]
	}, t.TempDir())
	// containment: META-GATE
	if err != nil || ev.Tree != "abc123" || ev.Commit != "def456" || ev.Dirty || ev.SourceDigest == "" {
		t.Fatalf("evidence %+v err %v", ev, err)
	}
	ev, _ = SourceIdentity(func(string) string { return "" }, t.TempDir())
	if ev.Commit != "unknown" || !ev.Dirty {
		t.Fatalf("manual run must be commit unknown and dirty: %+v", ev)
	}
}

// TestPlanRunsGroupsByPackage: one run per package, a duplicate ref cited
// once, rows mandatory on another profile left out, the profile's tags and
// race mode copied, packages and leaves sorted.
func TestPlanRunsGroupsByPackage(t *testing.T) {
	ref := func(pkg, test string) Ref { return Ref{Package: pkg, Test: test} }
	inv := Inventory{
		Profiles: []Profile{{Name: "p", Tags: []string{"tag"}, Race: true}, {Name: "other"}},
		Rows: []Row{
			{ID: "R1", Profiles: []string{"p"}, Refs: []Ref{ref("./b", "TestB/x"), ref("./a", "TestA")}},
			{ID: "R2", Profiles: []string{"p", "other"}, Refs: []Ref{ref("./a", "TestA"), ref("./a", "TestA/y/z")}},
			{ID: "R3", Profiles: []string{"other"}, Refs: []Ref{ref("./c", "TestC")}},
		},
	}
	got, err := PlanRuns(inv, "p")
	want := []PackageRun{
		{Package: "./a", Pattern: `^(TestA)$|^(TestA)$/^(y)$/^(z)$`, Tags: []string{"tag"}, Race: true, Leaves: []string{"TestA", "TestA/y/z"}},
		{Package: "./b", Pattern: `^(TestB)$/^(x)$`, Tags: []string{"tag"}, Race: true, Leaves: []string{"TestB/x"}},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("PlanRuns\n got %+v, %v\nwant %+v", got, err, want)
	}
	if _, err := PlanRuns(inv, "mars"); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

// TestCheckerSkipReasonIsTheSkipLine: the reason is the last logged line
// before the --- SKIP frame, not an earlier t.Log line.
func TestCheckerSkipReasonIsTheSkipLine(t *testing.T) {
	const p = "github.com/kstruzzieri/go-llm/cmd/golem"
	out := func(s string) TestEvent {
		return TestEvent{Action: "output", Package: p, Test: "TestContainment/W3", Output: s}
	}
	skipped := []TestEvent{
		{Action: "run", Package: p, Test: "TestContainment/W3"},
		out("=== RUN   TestContainment/W3\n"),
		out("    containment_test.go:88: capability:bash\n"),
		out("    containment_test.go:90: capability:symlink\n"),
		out("--- SKIP: TestContainment/W3 (0.00s)\n"),
		{Action: "skip", Package: p, Test: "TestContainment/W3"},
		{Action: "pass", Package: p},
	}
	leaf := []string{"TestContainment/W3"}
	if errs := Evaluate(skipped, leaf, []string{"TestContainment/W3=capability:symlink"}); len(errs) != 0 {
		t.Fatalf("skip with its own reason rejected: %v", errs)
	}
	if errs := Evaluate(skipped, leaf, []string{"TestContainment/W3=capability:bash"}); len(errs) == 0 {
		t.Fatal("an earlier log line accepted as the skip reason")
	}
}

// TestCheckerSourceDigestFailure: a checkout git cannot read (a linked
// worktree's .git pointer inside Docker) only downgrades a manual run, which
// is already dirty; with a forwarded identity it fails the run.
func TestCheckerSourceDigestFailure(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, ".git", "gitdir: /nonexistent/.git/worktrees/x\n")
	ev, err := SourceIdentity(func(string) string { return "" }, root)
	if err != nil || ev.SourceDigest != "unknown" || ev.Commit != "unknown" || !ev.Dirty {
		t.Fatalf("manual run: evidence %+v err %v, want source_digest and commit unknown, dirty", ev, err)
	}
	forwarded := func(k string) string {
		return map[string]string{"GO_LLM_CI_TREE": "abc123", "GO_LLM_CI_COMMIT": "def456"}[k]
	}
	if ev, err := SourceIdentity(forwarded, root); err == nil {
		t.Fatalf("forwarded identity with an unreadable checkout accepted: %+v", ev)
	}
}

// TestCheckerEvidenceBindsPlatform: evidence names the platform and Go that
// ran it, and a run on another GOOS than its profile's is dirty.
func TestCheckerEvidenceBindsPlatform(t *testing.T) {
	ev, err := SourceIdentity(func(k string) string {
		return map[string]string{"GO_LLM_CI_TREE": "abc123", "GO_LLM_CI_COMMIT": "def456"}[k]
	}, t.TempDir())
	if err != nil || ev.GOOS != runtime.GOOS || ev.GOARCH != runtime.GOARCH || ev.GoVersion != runtime.Version() {
		t.Fatalf("evidence %+v err %v, want the running platform", ev, err)
	}
	for _, tc := range []struct {
		name        string
		ev          Evidence
		profileGOOS string
		wantDirty   bool
	}{
		{"profile GOOS matches", Evidence{GOOS: "linux"}, "linux", false},
		{"profile has no GOOS", Evidence{GOOS: "darwin"}, "", false},
		{"linux-host on darwin", Evidence{GOOS: "darwin"}, "linux", true},
		{"manual run stays dirty", Evidence{GOOS: "linux", Dirty: true}, "linux", true},
	} {
		got := WithProfile(tc.ev, Profile{Name: "linux-host", GOOS: tc.profileGOOS})
		if got.Dirty != tc.wantDirty || got.Profile != "linux-host" {
			t.Errorf("%s: dirty %v profile %q, want dirty %v profile linux-host", tc.name, got.Dirty, got.Profile, tc.wantDirty)
		}
	}
}

// TestSourceDigestCheckoutMatchesExport: the pre-push hook gates a .git-free
// export while GitHub CI tests a checkout; both must digest the same files,
// hidden directories and symlinks included.
func TestSourceDigestCheckoutMatchesExport(t *testing.T) {
	repo, export := t.TempDir(), t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@localhost", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	writeGo(t, repo, ".github/x.yml", "on: push\n")
	writeGo(t, repo, "pkg/a.go", "package pkg\n")
	if err := os.Symlink("pkg/a.go", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "one")
	tarball := filepath.Join(t.TempDir(), "x.tar")
	git("archive", "-o", tarball, "HEAD")
	if out, err := exec.Command("tar", "-xf", tarball, "-C", export).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}

	checkout, err := SourceDigest(repo)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := SourceDigest(export)
	if err != nil {
		t.Fatal(err)
	}
	if checkout != exported {
		t.Fatalf("checkout digest %s != export digest %s", checkout, exported)
	}
	// The hidden directory is part of the digest.
	writeGo(t, export, ".github/x.yml", "on: pull_request\n")
	if changed, err := SourceDigest(export); err != nil || changed == exported {
		t.Fatalf("edit under .github did not change the digest (%s, %v)", changed, err)
	}
}
