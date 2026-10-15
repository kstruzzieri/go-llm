package containmentinv

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// TestEvent is one `go test -json` event, with every field cmd/test2json
// emits (Go 1.25 added OutputType), so a strict decoder accepts any test event
// and rejects anything else, such as the go command's build events. Package
// events (start, the package's own pass/fail) have an empty Test.
type TestEvent struct {
	Time        time.Time
	Action      string
	Package     string
	Test        string
	Elapsed     float64
	Output      string
	OutputType  string
	FailedBuild string
	Key         string
	Value       string
	Path        string
}

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
// source, the inventory and the go test flags. It is CI output, never
// committed. Executed lists "<package> <leaf>" for every leaf that passed.
type Evidence struct {
	SourceDigest    string   `json:"source_digest"`
	Tree            string   `json:"tree"`
	Commit          string   `json:"commit"`
	Dirty           bool     `json:"dirty"`
	InventoryDigest string   `json:"inventory_digest"`
	Profile         string   `json:"profile"`
	Flags           []string `json:"flags"`
	Executed        []string `json:"executed"`
}

// PlanRuns groups the refs of every row mandatory on profile by package:
// one PackageRun per package, packages and leaves sorted.
func PlanRuns(inv Inventory, profile string) ([]PackageRun, error) {
	i := slices.IndexFunc(inv.Profiles, func(p Profile) bool { return p.Name == profile })
	if i < 0 {
		return nil, fmt.Errorf("containmentinv: unknown profile %q", profile)
	}
	p := inv.Profiles[i]
	leaves := map[string][]string{}
	for _, r := range inv.Rows {
		if !slices.Contains(r.Profiles, profile) {
			continue
		}
		for _, ref := range r.Refs {
			if !slices.Contains(leaves[ref.Package], ref.Test) {
				leaves[ref.Package] = append(leaves[ref.Package], ref.Test)
			}
		}
	}
	if len(leaves) == 0 {
		return nil, fmt.Errorf("containmentinv: profile %s has no mandatory refs", profile)
	}
	var runs []PackageRun
	for _, pkg := range slices.Sorted(maps.Keys(leaves)) {
		l := slices.Sorted(slices.Values(leaves[pkg]))
		runs = append(runs, PackageRun{Package: pkg, Pattern: RunPattern(l), Tags: p.Tags, Race: p.Race, Leaves: l})
	}
	return runs, nil
}

// RunPattern builds the -run pattern for leaves. Go splits -run on unbracketed
// `/` and matches each level of a test name independently, so level i is the
// anchored alternation of every leaf's i-th component, each QuoteMeta'd. That
// cross product can select tests the leaves do not name; Evaluate's exact-leaf
// check is what makes the run precise.
//
// Ceiling: the deeper levels also filter the subtests of a leaf shorter than
// the deepest one, so a cited parent whose assertions live in its subtests
// would pass without running them. Cite the subtest leaves; per-leaf
// top-level `|` alternatives, which Go also accepts, are the upgrade path.
func RunPattern(leaves []string) string {
	var levels [][]string
	for _, leaf := range leaves {
		for i, c := range strings.Split(leaf, "/") {
			if i == len(levels) {
				levels = append(levels, nil)
			}
			if !slices.Contains(levels[i], c) {
				levels[i] = append(levels[i], c)
			}
		}
	}
	parts := make([]string, len(levels))
	for i, comps := range levels {
		slices.Sort(comps)
		for j, c := range comps {
			comps[j] = regexp.QuoteMeta(c)
		}
		parts[i] = "^(" + strings.Join(comps, "|") + ")$"
	}
	return strings.Join(parts, "/")
}

// skipLine is the line t.Skip(reason) prints: "    file.go:NN: reason".
var skipLine = regexp.MustCompile(`^\s+\S+\.go:\d+: (.*)$`)

// Evaluate decides one package run from its complete event stream: every leaf
// needs a run event and a terminal pass, or a terminal skip whose reason
// matches an allowedSkips entry "<leaf>=<reason>"; the package itself must
// pass. A truncated stream has no package pass and fails.
func Evaluate(events []TestEvent, leaves []string, allowedSkips []string) []error {
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
	var errs []error
	for _, leaf := range leaves {
		switch {
		case !ran[leaf]:
			errs = append(errs, fmt.Errorf("%s: never ran", leaf))
		case final[leaf] == "pass":
		case final[leaf] == "skip":
			if reason := skipReason(output[leaf]); !slices.Contains(allowedSkips, leaf+"="+reason) {
				errs = append(errs, fmt.Errorf("%s: skipped (%s), not an allowed skip", leaf, reason))
			}
		case final[leaf] == "":
			errs = append(errs, fmt.Errorf("%s: no terminal event", leaf))
		default:
			errs = append(errs, fmt.Errorf("%s: %s", leaf, final[leaf]))
		}
	}
	if final[""] != "pass" {
		errs = append(errs, fmt.Errorf("package did not pass (terminal action %q)", final[""]))
	}
	return errs
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

// SourceIdentity binds evidence to the tested source: its content digest plus
// the tree and commit forwarded by the pre-push hook or CI in GO_LLM_CI_TREE
// and GO_LLM_CI_COMMIT. Without both, the run is manual: commit "unknown",
// dirty, and never CI evidence.
func SourceIdentity(getenv func(string) string, root string) (Evidence, error) {
	digest, err := SourceDigest(root)
	if err != nil {
		return Evidence{}, err
	}
	ev := Evidence{SourceDigest: digest, Tree: getenv("GO_LLM_CI_TREE"), Commit: getenv("GO_LLM_CI_COMMIT")}
	if ev.Tree == "" || ev.Commit == "" {
		ev.Commit, ev.Dirty = "unknown", true
	}
	return ev, nil
}

// SourceDigest is the SHA-256 over sorted "path|sha256(content)" lines for
// every tracked file under root, hidden directories included: `git ls-files`
// when root has .git, otherwise (the hook's .git-free export) every file
// except .git. A symlink digests its target string, as git stores it.
func SourceDigest(root string) (string, error) {
	var files []string
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
		if err != nil {
			return "", fmt.Errorf("containmentinv: git ls-files: %w", err)
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
		if errors.Is(err, fs.ErrNotExist) {
			continue // tracked but deleted in a dirty checkout: not in the tested tree
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
