package main

import (
	"context"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// openCapProbeStore opens the shared fingerprint/capability store through
// fingerprint.OpenSharedStore (hardened path, in-memory fallback, sidecar
// re-chmod on every write) and returns it with its warning, empty when there is
// nothing to report. It returns a nil store when even the in-memory fallback
// failed. Never fatal. Golem still reads and writes the legacy probe table
// through Probes().
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
