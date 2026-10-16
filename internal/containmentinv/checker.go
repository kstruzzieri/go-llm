package containmentinv

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// TestEvent is one event of a `go test -json` stream: a cmd/test2json event
// (test actions, plus the attr and artifacts actions newer toolchains emit,
// carrying Key/Value or Path, which the checker ignores) or one of the go command's build events
// (build-output, build-fail), which carry ImportPath instead of Package. Package events (start, the
// package's own pass/fail) have an empty Test. DecodeEvents ignores fields it
// does not know, so a newer toolchain's additions never fail a run.
type TestEvent struct {
	Time        time.Time
	Action      string
	Package     string
	ImportPath  string
	Test        string
	Elapsed     float64
	Output      string
	OutputType  string
	FailedBuild string
	Key         string
	Value       string
	Path        string
}

// eventActions are the actions DecodeEvents accepts: test2json's actions and
// the go command's build actions.
var eventActions = []string{"start", "run", "pause", "cont", "pass", "fail", "skip", "output", "bench", "attr", "artifacts", "build-output", "build-fail"}

// PackageRun is one `go test` invocation of the execution checker: every
// mandatory leaf a profile cites in one package.
type PackageRun struct {
	Package string
	Pattern string
	Tags    []string
	Race    bool
	Leaves  []string
}

// Evidence is the observed record of one checker run, bound to the tested
// source, the platform and toolchain that ran it, the inventory and the go
// test flags. It is CI output, never committed. Executed lists
// "<package> <leaf>" for every leaf that passed; a failed run lists why in
// Failures. Tree and Commit are the forwarded identity (stamped, not
// verified); MeasuredTree is the checkout's own HEAD tree when it has .git.
type Evidence struct {
	SourceDigest    string   `json:"source_digest"`
	Tree            string   `json:"tree"`
	Commit          string   `json:"commit"`
	MeasuredTree    string   `json:"measured_tree,omitempty"`
	PRHead          string   `json:"pr_head,omitempty"`
	Dirty           bool     `json:"dirty"`
	GOOS            string   `json:"goos"`
	GOARCH          string   `json:"goarch"`
	GoVersion       string   `json:"go_version"`
	InventoryDigest string   `json:"inventory_digest"`
	Profile         string   `json:"profile"`
	Flags           []string `json:"flags"`
	Executed        []string `json:"executed"`
	Failures        []string `json:"failures,omitempty"`
}

// PlanRuns groups the refs of every row mandatory on profile, and every
// skip-allowed leaf the profile's allowed skips name (run there, so a skip
// must carry its allowed reason, but no row's mandatory claim), by package:
// one PackageRun per package, packages and leaves sorted.
func PlanRuns(inv Inventory, profile string) ([]PackageRun, error) {
	i := slices.IndexFunc(inv.Profiles, func(p Profile) bool { return p.Name == profile })
	if i < 0 {
		return nil, fmt.Errorf("containmentinv: unknown profile %q", profile)
	}
	p := inv.Profiles[i]
	leaves := map[string][]string{}
	for _, r := range inv.Rows {
		mandatory := slices.Contains(r.Profiles, profile)
		for _, ref := range r.Refs {
			if (mandatory || skipAllowed(p, ref.Test)) && !slices.Contains(leaves[ref.Package], ref.Test) {
				leaves[ref.Package] = append(leaves[ref.Package], ref.Test)
			}
		}
	}
	if len(leaves) == 0 {
		return nil, fmt.Errorf("containmentinv: profile %s has no mandatory or skip-allowed refs", profile)
	}
	var runs []PackageRun
	for _, pkg := range slices.Sorted(maps.Keys(leaves)) {
		l := slices.Sorted(slices.Values(leaves[pkg]))
		runs = append(runs, PackageRun{Package: pkg, Pattern: RunPattern(l), Tags: p.Tags, Race: p.Race, Leaves: l})
	}
	return runs, nil
}

// RunPattern builds the -run pattern that selects exactly leaves, in one run.
// Go's testing package splits -run on unbracketed `|` into alternatives and
// each alternative on unbracketed `/` into per-level patterns; a test runs
// when any alternative matches its name level by level, and levels past an
// alternative's end match everything. So each leaf is its own alternative of
// anchored, QuoteMeta'd components: a cited parent runs all its subtests, and
// a deeper leaf elsewhere cannot filter them away, as one pattern per level
// would.
func RunPattern(leaves []string) string {
	alts := make([]string, len(leaves))
	for i, leaf := range leaves {
		comps := strings.Split(leaf, "/")
		for j, c := range comps {
			comps[j] = "^(" + regexp.QuoteMeta(c) + ")$"
		}
		alts[i] = strings.Join(comps, "/")
	}
	return strings.Join(alts, "|")
}

// skipLine is the line t.Skip(reason) prints: "    file.go:NN: reason".
var skipLine = regexp.MustCompile(`^\s+\S+\.go:\d+: (.*)$`)

// Evaluate decides one package run from its complete event stream: every leaf
// needs a run event and a terminal pass, or a terminal skip whose reason
// matches an allowedSkips entry "<leaf>=<reason>"; the package itself must
// pass. A leaf's outcome covers its subtests: Go reports a parent whose
// subtest skipped as pass, so a skip or fail of any test under the leaf counts
// as the leaf's own, with skip reasons checked against the leaf's entries. A
// truncated stream has no package pass and fails.
func Evaluate(events []TestEvent, leaves []string, allowedSkips []string) []error {
	_, errs := evaluate(events, leaves, allowedSkips)
	return errs
}

// evaluate derives passing leaves and errors from the same descendant-aware
// outcome: an allowed skip accepts the run but is not passing evidence.
func evaluate(events []TestEvent, leaves []string, allowedSkips []string) (executed []string, errs []error) {
	ran := map[string]bool{}
	final := map[string]string{}
	output := map[string][]string{}
	for _, e := range events {
		switch e.Action {
		case "run":
			ran[e.Test] = true
		case "output":
			output[e.Test] = append(output[e.Test], strings.TrimSuffix(e.Output, "\n"))
		case "pass", "fail", "skip":
			final[e.Test] = e.Action
		}
	}
	tests := slices.Sorted(maps.Keys(final))
	for _, leaf := range leaves {
		switch {
		case !ran[leaf]:
			errs = append(errs, fmt.Errorf("%s: never ran", leaf))
			continue
		case final[leaf] == "":
			errs = append(errs, fmt.Errorf("%s: no terminal event", leaf))
			continue
		}
		passed := true
		for _, test := range tests {
			if test != leaf && !strings.HasPrefix(test, leaf+"/") {
				continue
			}
			switch final[test] {
			case "fail":
				passed = false
				errs = append(errs, fmt.Errorf("%s: fail", test))
			case "skip":
				passed = false
				if reason := skipReason(output[test]); !slices.Contains(allowedSkips, leaf+"="+reason) {
					errs = append(errs, fmt.Errorf("%s: skipped (%s), not an allowed skip", test, reason))
				}
			}
		}
		if passed {
			executed = append(executed, leaf)
		}
	}
	if final[""] != "pass" {
		errs = append(errs, fmt.Errorf("package did not pass (terminal action %q)", final[""]))
	}
	slices.Sort(executed)
	return executed, errs
}

// DecideRun decides one package run of the checker from `go test -json`'s
// complete output and exit error. The output must decode as a go test -json
// stream with no build failure (DecodeEvents), go test must have exited 0, and Evaluate must accept
// the events. On success executed is the sorted leaves that passed; an allowed
// skip is not listed. On any error executed is nil.
func DecideRun(out []byte, runErr error, leaves, allowed []string) (executed []string, errs []error) {
	events, decodeErr := DecodeEvents(out)
	executed, errs = evaluate(events, leaves, allowed)
	if decodeErr != nil {
		errs = append(errs, decodeErr)
	}
	if runErr != nil {
		errs = append(errs, fmt.Errorf("go test: %w", runErr))
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return executed, nil
}

// DecodeEvents decodes a complete `go test -json` stream, ignoring unknown
// fields. It fails only on a line that is not a JSON event, an event whose
// Action is not one of eventActions (an empty one included), or a build-fail
// event. build-output events are kept: their text joins a failed run's
// output, and alone they never fail a run (a linker warning, say). The events
// before the failure are returned with the error.
func DecodeEvents(out []byte) ([]TestEvent, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	var events []TestEvent
	for {
		var e TestEvent
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		switch {
		case err != nil:
		case e.Action == "build-fail":
			err = fmt.Errorf("build failed (%s)", e.ImportPath)
		case !slices.Contains(eventActions, e.Action):
			err = fmt.Errorf("unknown action %q", e.Action)
		}
		if err != nil {
			return events, fmt.Errorf("malformed go test -json stream after %d events: %w", len(events), err)
		}
		events = append(events, e)
	}
}

// skipReason is the last "file.go:NN: message" line a leaf printed: the
// message t.Skip logs just before the --- SKIP frame.
func skipReason(lines []string) string {
	reason := ""
	for _, line := range lines {
		if m := skipLine.FindStringSubmatch(line); m != nil {
			reason = m[1]
		}
	}
	return reason
}

// SourceIdentity binds evidence to the tested source and the running
// platform: the source's content digest, the tree and commit forwarded by the
// pre-push hook or CI in GO_LLM_CI_TREE and GO_LLM_CI_COMMIT, the PR head
// (GO_LLM_CI_PR_HEAD, when set), and GOOS, GOARCH and the Go version. Without
// both variables the run is manual: commit "unknown", dirty, and never CI
// evidence. In a checkout (root has .git) cleanliness is measured: the run is
// dirty unless HEAD's tree equals the forwarded tree and `git status` lists no
// tracked change. The hook's .git-free export cannot be measured; it is clean
// by construction, `git archive` of the forwarded tree. A manual run whose
// digest or measurement fails (git refusing a linked worktree's .git pointer
// inside Docker, say) records source_digest "unknown"; with a forwarded
// identity either error is fatal, as is a tracked file missing from the
// checkout.
func SourceIdentity(getenv func(string) string, root string) (Evidence, error) {
	ev := Evidence{Tree: getenv("GO_LLM_CI_TREE"), Commit: getenv("GO_LLM_CI_COMMIT"), PRHead: getenv("GO_LLM_CI_PR_HEAD"),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version()}
	forwarded := ev.Tree != "" && ev.Commit != ""
	if !forwarded {
		ev.Commit, ev.Dirty = "unknown", true
	}
	digest, err := SourceDigest(root, forwarded)
	if err == nil && hasGit(root) {
		var clean bool
		if ev.MeasuredTree, clean, err = measureCheckout(root); err == nil && (!clean || ev.MeasuredTree != ev.Tree) {
			ev.Dirty = true
		}
	}
	switch {
	case err == nil:
		ev.SourceDigest = digest
	case !forwarded:
		ev.SourceDigest = "unknown"
	default:
		return Evidence{}, err
	}
	return ev, nil
}

func hasGit(root string) bool {
	_, err := os.Lstat(filepath.Join(root, ".git"))
	return err == nil
}

// measureCheckout returns the checkout's HEAD tree and whether `git status`
// lists no tracked change (untracked files do not count: they are not in the
// digest either).
func measureCheckout(root string) (tree string, clean bool, err error) {
	git := func(args ...string) (string, error) {
		var stderr bytes.Buffer
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("containmentinv: git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
		}
		return string(out), nil
	}
	tree, err = git("rev-parse", "HEAD^{tree}")
	if err != nil {
		return "", false, err
	}
	status, err := git("status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(tree), status == "", nil
}

// WithProfile binds ev to profile p. A run on another GOOS than the profile
// declares, such as linux-host on a darwin host, is a manual run: dirty.
func WithProfile(ev Evidence, p Profile) Evidence {
	ev.Profile = p.Name
	if p.GOOS != "" && p.GOOS != ev.GOOS {
		ev.Dirty = true
	}
	return ev
}

// SourceDigest is the SHA-256 over sorted "path|sha256(content)" lines for
// every tracked file under root, hidden directories included: `git ls-files`
// when root has .git, otherwise (the hook's .git-free export) every file
// except .git. A symlink digests its target string, as git stores it. A
// tracked file missing from the checkout is left out, unless strict (a
// forwarded identity), when it is an error.
func SourceDigest(root string, strict bool) (string, error) {
	var files []string
	if hasGit(root) {
		var stderr bytes.Buffer
		cmd := exec.Command("git", "-C", root, "ls-files", "-z")
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("containmentinv: git ls-files: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		files = strings.FieldsFunc(string(out), func(r rune) bool { return r == 0 })
	} else {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.Name() == ".git":
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			case d.IsDir():
				return nil
			}
			rel, err := filepath.Rel(root, p)
			files = append(files, filepath.ToSlash(rel))
			return err
		})
		if err != nil {
			return "", fmt.Errorf("containmentinv: %w", err)
		}
	}
	slices.Sort(files)
	h := sha256.New()
	for _, f := range files {
		sum, err := fileSum(filepath.Join(root, filepath.FromSlash(f)))
		if errors.Is(err, fs.ErrNotExist) && !strict {
			continue // tracked but deleted in a manual run's checkout: not in the tested tree
		}
		if err != nil {
			return "", fmt.Errorf("containmentinv: %w", err)
		}
		_, _ = fmt.Fprintf(h, "%s|%x\n", f, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileSum(path string) ([sha256.Size]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		return sha256.Sum256([]byte(target)), err
	}
	data, err := os.ReadFile(path)
	return sha256.Sum256(data), err
}

// RequireClean rejects evidence that claims a forwarded identity but measured
// dirty: a CI or hook run whose checkout, tree or GOOS does not match the
// identity it forwards has tested something else, so the run fails instead
// of publishing evidence stamped as that commit's. A manual run (commit
// "unknown") is dirty by definition and never fails for it.
func RequireClean(ev Evidence) error {
	if ev.Commit == "unknown" || !ev.Dirty {
		return nil
	}
	return fmt.Errorf("containmentinv: forwarded identity (tree %s, commit %s) measured dirty: measured tree %q, GOOS %s, profile %s", ev.Tree, ev.Commit, ev.MeasuredTree, ev.GOOS, ev.Profile)
}
