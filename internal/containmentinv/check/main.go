// Command check is the #452 execution checker. It runs every ref mandatory on
// one inventory profile, one `go test -count=1 -json` per package, decides
// each run from its complete event stream with containmentinv.Evaluate, and
// writes the source-bound evidence JSON. Run it from the module root, as
// scripts/ci-local and the CI workflows do.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/internal/containmentinv"
)

func main() {
	inventory := flag.String("inventory", "", "inventory JSON (docs/security/containment-inventory.json)")
	profile := flag.String("profile", "", "inventory profile to run")
	evidenceOut := flag.String("evidence-out", "", "evidence JSON path; stdout when empty")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	ev, err := check(ctx, *inventory, *profile)
	stop()
	if err == nil {
		err = writeEvidence(ev, *evidenceOut)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "containment check: %v\n", err)
		os.Exit(1)
	}
	_, _ = fmt.Fprintf(os.Stderr, "containment check: %s %d leaves passed\n", ev.Profile, len(ev.Executed))
}

func check(ctx context.Context, invPath, profile string) (containmentinv.Evidence, error) {
	var ev containmentinv.Evidence
	data, err := os.ReadFile(invPath)
	if err != nil {
		return ev, err
	}
	inv, err := containmentinv.Load(invPath)
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
	if ev, err = containmentinv.SourceIdentity(os.Getenv, "."); err != nil {
		return ev, err
	}
	sum := sha256.Sum256(data)
	ev.InventoryDigest, ev.Profile = hex.EncodeToString(sum[:]), profile
	ev.Flags = []string{"-count=1", "-json", "-timeout", p.Timeout}
	if p.Race {
		ev.Flags = append(ev.Flags, "-race")
	}
	if len(p.Tags) > 0 {
		ev.Flags = append(ev.Flags, "-tags", strings.Join(p.Tags, ","))
	}

	failed := 0
	for _, r := range runs {
		args := append(append([]string{"test"}, ev.Flags...), "-run", r.Pattern, r.Package)
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Stderr = os.Stderr
		out, runErr := cmd.Output()
		events, decodeErr := decode(out)
		errs := containmentinv.Evaluate(events, r.Leaves, p.AllowedSkips)
		if decodeErr != nil {
			errs = append(errs, decodeErr)
		}
		if runErr != nil {
			errs = append(errs, fmt.Errorf("go test: %w", runErr))
		}
		if len(errs) > 0 {
			failed++
			failureOutput(os.Stderr, events)
			for _, err := range errs {
				_, _ = fmt.Fprintf(os.Stderr, "containment check: %s: %v\n", r.Package, err)
			}
			continue
		}
		for _, leaf := range r.Leaves {
			if slices.ContainsFunc(events, func(e containmentinv.TestEvent) bool { return e.Action == "pass" && e.Test == leaf }) {
				ev.Executed = append(ev.Executed, r.Package+" "+leaf)
			}
		}
	}
	if failed > 0 {
		return ev, fmt.Errorf("profile %s: %d of %d package runs failed", profile, failed, len(runs))
	}
	slices.Sort(ev.Executed)
	return ev, nil
}

// decode reads a complete `go test -json` stream. Anything that is not a
// known event, such as text build output, fails the run.
func decode(out []byte) ([]containmentinv.TestEvent, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	var events []containmentinv.TestEvent
	for {
		var e containmentinv.TestEvent
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, fmt.Errorf("malformed go test -json stream after %d events: %w", len(events), err)
		}
		events = append(events, e)
	}
}

// failureOutput prints the output of the package and of every failed test.
func failureOutput(w io.Writer, events []containmentinv.TestEvent) {
	failed := map[string]bool{"": true}
	for _, e := range events {
		if e.Action == "fail" {
			failed[e.Test] = true
		}
	}
	for _, e := range events {
		if e.Action == "output" && failed[e.Test] {
			_, _ = io.WriteString(w, e.Output)
		}
	}
}

func writeEvidence(ev containmentinv.Evidence, path string) error {
	data, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if path == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
