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
// source, the platform and toolchain that ran it, the inventory and the go
// test flags. It is CI output, never committed. Executed lists
// "<package> <leaf>" for every leaf that passed.
type Evidence struct {
	SourceDigest    string   `json:"source_digest"`
	Tree            string   `json:"tree"`
	Commit          string   `json:"commit"`
	Dirty           bool     `json:"dirty"`
	GOOS            string   `json:"goos"`
	GOARCH          string   `json:"goarch"`
	GoVersion       string   `json:"go_version"`
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
	var errs []error
	for _, leaf := range leaves {
		switch {
		case !ran[leaf]:
			errs = append(errs, fmt.Errorf("%s: never ran", leaf))
			continue
		case final[leaf] == "":
			errs = append(errs, fmt.Errorf("%s: no terminal event", leaf))
			continue
		}
		for _, test := range tests {
			if test != leaf && !strings.HasPrefix(test, leaf+"/") {
				continue
			}
			switch final[test] {
			case "fail":
				errs = append(errs, fmt.Errorf("%s: fail", test))
			case "skip":
				if reason := skipReason(output[test]); !slices.Contains(allowedSkips, leaf+"="+reason) {
					errs = append(errs, fmt.Errorf("%s: skipped (%s), not an allowed skip", test, reason))
				}
			}
		}
	}
	if final[""] != "pass" {
		errs = append(errs, fmt.Errorf("package did not pass (terminal action %q)", final[""]))
	}
	return errs
}

// DecideRun decides one package run of the checker from `go test -json`'s
// complete output and exit error. The output must decode strictly as test
// events (DecodeEvents), go test must have exited 0, and Evaluate must accept
// the events. On success executed is the sorted leaves that passed; an allowed
// skip is not listed. On any error executed is nil.
func DecideRun(out []byte, runErr error, leaves, allowed []string) (executed []string, errs []error) {
	events, decodeErr := DecodeEvents(out)
	errs = Evaluate(events, leaves, allowed)
	if decodeErr != nil {
		errs = append(errs, decodeErr)
	}
	if runErr != nil {
		errs = append(errs, fmt.Errorf("go test: %w", runErr))
	}
	if len(errs) > 0 {
		return nil, errs
	}
	for _, leaf := range slices.Sorted(slices.Values(leaves)) {
		if slices.ContainsFunc(events, func(e TestEvent) bool { return e.Action == "pass" && e.Test == leaf }) {
			executed = append(executed, leaf)
		}
	}
	return executed, nil
}

// DecodeEvents strictly decodes a complete `go test -json` stream. Anything
// that is not a test event fails: a text line, an unknown field (the go
// command's build events carry ImportPath), an event without an Action, or a
// build-output or build-fail action. The events before the failure are
// returned with the error.
func DecodeEvents(out []byte) ([]TestEvent, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	var events []TestEvent
	for {
		var e TestEvent
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err == nil && (e.Action == "" || strings.HasPrefix(e.Action, "build-")) {
			err = fmt.Errorf("not a test event (action %q)", e.Action)
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
// pre-push hook or CI in GO_LLM_CI_TREE and GO_LLM_CI_COMMIT, and GOOS,
// GOARCH and the Go version. Without both variables the run is manual: commit
// "unknown", dirty, and never CI evidence. A manual run whose digest fails
// (git refusing a linked worktree's .git pointer inside Docker, say) records
// source_digest "unknown"; with a forwarded identity the error is fatal.
func SourceIdentity(getenv func(string) string, root string) (Evidence, error) {
	ev := Evidence{Tree: getenv("GO_LLM_CI_TREE"), Commit: getenv("GO_LLM_CI_COMMIT"),
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version()}
	if ev.Tree == "" || ev.Commit == "" {
		ev.Commit, ev.Dirty = "unknown", true
	}
	digest, err := SourceDigest(root)
	switch {
	case err == nil:
		ev.SourceDigest = digest
	case ev.Dirty:
		ev.SourceDigest = "unknown"
	default:
		return Evidence{}, err
	}
	return ev, nil
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
// except .git. A symlink digests its target string, as git stores it.
func SourceDigest(root string) (string, error) {
	var files []string
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
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
