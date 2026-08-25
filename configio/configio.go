// Package configio implements the explicit I/O tier of the role-config
// stack (spec slice 4): RefreshInventory and ProbeToolCall. Both are
// user-invoked operations with values in and values out — nothing here
// runs implicitly on view or build, and a cancelled operation returns a
// zero value: the caller receives nothing.
//
// Error discipline follows the config/profiles house rules — bounded
// codes, message text passes through unchanged; CodeOf uses
// config.DiagnosticOf's two-value shape. The bounded code is the ONLY
// thing a projection boundary (e.g. Firn's Wails layer) may forward.
// Raw provider/transport text never crosses a consumer boundary via the
// code path. Caller cancellation is
// deliberately UNCLASSIFIED: it surfaces as the raw context error with
// no code, so consumers can distinguish "user cancelled" from "failed"
// (argument validation runs first: a structurally invalid request returns
// invalid_argument even under a cancelled context).
//
// Import graph: configio -> provider, configview, fingerprint. It must
// never be imported by config, configview, or provider.
package configio

import (
	"context"
	"errors"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// ErrorCode is the bounded vocabulary configio operations expose to
// consumers. Codes never carry paths, secrets, or raw transport text.
// configio errors carry no subject: the caller already knows which
// provider/model it addressed.
type ErrorCode string

// Exported code constants — consumers never duplicate string literals.
const (
	// CodeInvalidArgument reports a structurally invalid request (e.g.
	// an empty provider or model in a ModelKey).
	CodeInvalidArgument ErrorCode = "invalid_argument"
	// CodeProbeUnavailable reports that no resolution path could answer
	// for this model: no authoritative override or profile bit, and no
	// usable prober/store (globally unwired, or unavailable for this
	// specific provider/model). Distinct from a probe that RAN and
	// answered inconclusively.
	CodeProbeUnavailable ErrorCode = "probe_unavailable"
	// CodeProbeFailed reports a failed probe attempt: transient transport
	// trouble (network, auth) or a resolution failure (unknown provider
	// or model). Not necessarily retryable — the wrapped cause
	// distinguishes, for log/CLI surfaces only. Nothing was persisted;
	// the state is unknown.
	CodeProbeFailed ErrorCode = "probe_failed"
)

type opError struct {
	code ErrorCode
	err  error
}

func (e *opError) Error() string { return e.err.Error() }
func (e *opError) Unwrap() error { return e.err }

// wrapCode attaches a bounded code to err, preserving the chain.
func wrapCode(code ErrorCode, err error) error {
	return &opError{code: code, err: err}
}

// CodeOf extracts the bounded code from err's chain, if any.
func CodeOf(err error) (ErrorCode, bool) {
	if err == nil {
		return "", false
	}
	var oe *opError
	if errors.As(err, &oe) {
		return oe.code, true
	}
	return "", false
}

// ProbeOutcome is ProbeToolCall's result (D9 dual-outcome contract): the
// tri-state verdict, valid for this session immediately, and whether it
// is durable — persisted as a CURRENTLY-VALID probe row, or carried by
// explicit/catalog/profile knowledge that re-derives next session.
// Persisted=false means the verdict works now but may need a re-probe
// after restart; consumers render their own fixed message for it (no raw
// I/O text crosses here).
type ProbeOutcome struct {
	State     fingerprint.CapProbeState
	Persisted bool
}

// ProviderLister is the provider-set seam RefreshInventory reads.
// *provider.Registry satisfies it.
type ProviderLister interface {
	Names() []string
	Get(name string) (provider.Provider, bool)
}

// ListedProjector is the per-listing fact seam. *provider.ModelRegistry
// satisfies it via ProjectListedModels — read-only by contract: no
// probing (fingerprint or tool-call), no provider re-query, no cache
// writes; TOTAL over the listing (its only error is cancellation). The
// seam deliberately has no probe or lookup method, so refresh cannot
// trigger active I/O beyond the listing it already did.
type ListedProjector interface {
	ProjectListedModels(ctx context.Context, providerName string, infos []provider.ModelInfo) ([]provider.ListedModelFacts, error)
}

// ToolCallResolver is the probe seam for ProbeToolCall.
// *provider.ModelRegistry satisfies it.
type ToolCallResolver interface {
	ResolveToolCall(ctx context.Context, key provider.ModelKey) (fingerprint.CapProbeState, error)
	ExplainToolCall(ctx context.Context, key provider.ModelKey) (provider.ToolCallExplanation, error)
}

// Compile-time seam checks: the concrete registry types satisfy the seams.
var (
	_ ProviderLister   = (*provider.Registry)(nil)
	_ ListedProjector  = (*provider.ModelRegistry)(nil)
	_ ToolCallResolver = (*provider.ModelRegistry)(nil)
)
