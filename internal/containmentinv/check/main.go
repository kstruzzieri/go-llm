// Command check is the #452 execution checker. It runs every ref mandatory on
// one inventory profile, one `go test -count=1 -json` per package, decides
// each run from its complete output and exit status with
// containmentinv.DecideRun, and writes the source-bound evidence JSON, on
// failure too, with a failures list. Its success line names the evidence's
// tree, commit and source digest, so a run whose evidence file is discarded
// (the pre-push container) still shows them. Run it from the module root, as
// scripts/ci-local and the CI workflows do.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/kstruzzieri/go-llm/internal/containmentinv"
)

func main() {
	inventory := flag.String("inventory", "", "inventory JSON (docs/security/containment-inventory.json)")
	profile := flag.String("profile", "", "inventory profile to run")
	evidenceOut := flag.String("evidence-out", "", "evidence JSON path; stdout when empty")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ev, err := check(ctx, *inventory, *profile)
	stop()
	if err != nil {
		ev.Failures = append(ev.Failures, err.Error())
	}
	if werr := writeEvidence(ev, *evidenceOut); err == nil {
		err = werr
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "containment check: %v\n", err)
		os.Exit(1)
	}
	_, _ = fmt.Fprintf(os.Stderr, "containment check: %s %d leaves passed (tree %s commit %s source_digest %s)\n",
		ev.Profile, len(ev.Executed), orNone(ev.Tree), orNone(ev.Commit), orNone(ev.SourceDigest))
}

// orNone prints an empty identity field, such as a manual run's tree, as none.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func check(ctx context.Context, invPath, profile string) (containmentinv.Evidence, error) {
	// Until the source identity is bound, failure evidence is a manual run's.
	ev := containmentinv.Evidence{Profile: profile, Commit: "unknown", Dirty: true}
	data, err := os.ReadFile(invPath)
	if err != nil {
		return ev, fmt.Errorf("containmentinv: read inventory: %w", err)
	}
	inv, err := containmentinv.Parse(data, invPath) // the bytes inventory_digest names
	if err != nil {
		return ev, err
	}
	runs, err := containmentinv.PlanRuns(inv, profile)
	if err != nil {
		return ev, err
	}
	p := inv.Profiles[slices.IndexFunc(inv.Profiles, func(p containmentinv.Profile) bool { return p.Name == profile })]
	if _, err := time.ParseDuration(p.Timeout); err != nil {
		return ev, fmt.Errorf("profile %s: timeout: %w", profile, err)
	}
	id, err := containmentinv.SourceIdentity(os.Getenv, ".")
	if err != nil {
		return ev, err
	}
	ev = containmentinv.WithProfile(id, p)
	sum := sha256.Sum256(data)
	ev.InventoryDigest = hex.EncodeToString(sum[:])
	ev.Flags = []string{"-count=1", "-json", "-timeout", p.Timeout}
	if p.Race {
		ev.Flags = append(ev.Flags, "-race")
	}
	if len(p.Tags) > 0 {
		ev.Flags = append(ev.Flags, "-tags", strings.Join(p.Tags, ","))
	}
	if err := containmentinv.RequireClean(ev); err != nil {
		return ev, err // CI and hook evidence is clean or absent; a manual run stays dirty
	}

	failed := 0
	for _, r := range runs {
		if err := ctx.Err(); err != nil {
			return ev, fmt.Errorf("profile %s: %w", profile, err) // interrupted: do not run the rest against a dead context
		}
		args := append(append([]string{"test"}, ev.Flags...), "-run", r.Pattern, r.Package)
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Stderr = os.Stderr
		cmd.WaitDelay = 10 * time.Second // a test binary holding stdout open past cancellation
		out, runErr := cmd.Output()
		executed, errs := containmentinv.DecideRun(out, runErr, r.Leaves, p.AllowedSkips)
		if len(errs) > 0 {
			failed++
			if events, decodeErr := containmentinv.DecodeEvents(out); decodeErr != nil {
				// Not a complete test stream (a build failure, a text
				// line): show it as captured.
				_, _ = os.Stderr.Write(out)
			} else {
				failureOutput(os.Stderr, events)
			}
			for _, err := range errs {
				_, _ = fmt.Fprintf(os.Stderr, "containment check: %s: %v\n", r.Package, err)
				ev.Failures = append(ev.Failures, r.Package+": "+err.Error())
			}
			continue
		}
		for _, leaf := range executed {
			ev.Executed = append(ev.Executed, r.Package+" "+leaf)
		}
	}
	if failed > 0 {
		return ev, fmt.Errorf("profile %s: %d of %d package runs failed", profile, failed, len(runs))
	}
	slices.Sort(ev.Executed)
	return ev, nil
}

// failureOutput prints the build output, and the output of the package and
// of every failed test.
func failureOutput(w io.Writer, events []containmentinv.TestEvent) {
	failed := map[string]bool{"": true}
	for _, e := range events {
		if e.Action == "fail" {
			failed[e.Test] = true
		}
	}
	for _, e := range events {
		if e.Action == "build-output" || e.Action == "output" && failed[e.Test] {
			_, _ = io.WriteString(w, e.Output)
		}
	}
}

func writeEvidence(ev containmentinv.Evidence, path string) error {
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return fmt.Errorf("containmentinv: encode evidence: %w", err)
	}
	data = append(data, '\n')
	if path == "" {
		_, err = os.Stdout.Write(data)
	} else {
		err = os.WriteFile(path, data, 0o644)
	}
	if err != nil {
		return fmt.Errorf("containmentinv: write evidence: %w", err)
	}
	return nil
}
