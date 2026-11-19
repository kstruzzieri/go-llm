package main

import (
	"context"
	"fmt"
	"io"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// openCapProbeStore opens the shared fingerprint/capability store through
// fingerprint.OpenSharedStore (hardened path, in-memory fallback, sidecar
// re-chmod on every write) and returns it with its warning, empty when there is
// nothing to report. It returns a nil store when even the in-memory fallback
// failed. Never fatal. With capability probing on, golem reads and writes
// capability answers through Log(); it no longer writes the legacy probe table.
func openCapProbeStore(ctx context.Context, getenv func(string) string, root string) (*fingerprint.SharedStore, string) {
	s, err := fingerprint.OpenSharedStore(ctx, getenv, root)
	if err != nil { // misuse only: a nil ctx or getenv
		return nil, "fingerprint cache disabled: " + err.Error()
	}
	if s.Status() == fingerprint.StoreDisabled {
		return nil, s.Warning()
	}
	return s, s.Warning()
}

// openModelsStore is how `golem models` opens the store: openCapProbeStore,
// replaced only by the test that needs a disabled store, which cannot be made
// from outside the fingerprint package.
var openModelsStore = openCapProbeStore

// automaticChecksUsedUp says why an automatic check was not sent: the run's
// allowance is spent. It holds for any chain, configured or recommended.
const automaticChecksUsedUp = "the automatic capability checks for this run are used up; the next golem run has a fresh allowance, and golem models -probe-all checks the configured agent chain"

// probeProgressPrinter renders a log-bound registry's automatic capability
// check progress on w, one line per event: before the check's first request,
// when it finishes or is cancelled, and when the run's allowance cannot fund
// it (once per model). Golem passes stderr, so none of it reaches stdout, a -p
// record or the headless stream. Output tokens are worded as a request to the
// server, not as a cost. The stage names are the provider's (unexported):
// each is rendered from the real registry by a test (started and finished by
// TestGolemAutomaticProgress_StderrOnlyAndBeforeHTTP, cancelled by
// TestGolemAutomaticCancel_StopsR2AndPreservesCommittedRows, budget_exhausted
// by TestGolemPreflight_SecondEntryRefusedThenNextRunSucceeds and
// TestGolemAutomaticBudget_SharedAcrossTurnsAndCandidates), so a rename fails
// one of them.
func probeProgressPrinter(w io.Writer) func(provider.CapabilityProbeProgress) {
	return func(p provider.CapabilityProbeProgress) {
		switch p.Stage {
		case "started":
			// The host comes from config: quoted, so no control rune reaches the terminal.
			_, _ = fmt.Fprintf(w, "capability check: %q at %q via %s: reserving %s, each requesting up to %d output tokens\n",
				p.Key.String(), p.EndpointHost, p.Method, plural(p.ReservedRequests, "request", "requests"), p.MaxOutputTokensPerRequest)
		case "finished", "cancelled":
			_, _ = fmt.Fprintf(w, "capability check: %q %s: %s sent\n", p.Key.String(), p.Stage, plural(p.Requests, "request", "requests"))
		case "budget_exhausted":
			_, _ = fmt.Fprintf(w, "capability check: %q not checked: %s\n", p.Key.String(), automaticChecksUsedUp)
		}
	}
}
