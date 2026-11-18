// Package configio implements the explicit I/O tier of the role-config
// stack (spec slice 4): RefreshInventory, ProbeToolCall and PlanCheck. All are
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
// Import graph: configio -> config, configview, fingerprint, provider,
// internal/evidencesel, internal/providerbootstrap. It must never be imported
// by config, configview, provider or the internal packages it imports.
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

// Capability check codes. A check result carries at most one; PlanCheck and
// RunCheck return them on their errors. They are decided from status codes and
// error types, never from provider text.
const (
	// CodeAuthFailed reports a 401: the provider did not accept the key.
	CodeAuthFailed ErrorCode = "auth_failed"
	// CodeModelRefused reports a 403 or 404 for one model while another model
	// on the same provider answered in the same run: the key works, and this
	// model is refused to it.
	CodeModelRefused ErrorCode = "model_refused"
	// CodeRefused reports a 403 or 404 when no model answered in the same run,
	// so a refused key cannot be told from a refused model.
	CodeRefused ErrorCode = "refused"
	// CodeRateLimited reports a 429.
	CodeRateLimited ErrorCode = "rate_limited"
	// CodeTimeout reports a request that ran out of time.
	CodeTimeout ErrorCode = "timeout"
	// CodeUnreachable reports a request that could not reach the endpoint.
	CodeUnreachable ErrorCode = "unreachable"
	// CodeServerError reports a 5xx.
	CodeServerError ErrorCode = "server_error"
	// CodeRejected reports a 4xx not covered by another code.
	CodeRejected ErrorCode = "rejected"
	// CodeMalformedResponse reports a reply the check could not read.
	CodeMalformedResponse ErrorCode = "malformed_response"
	// CodeDestinationNotGranted reports a remote endpoint the caller's
	// destination policy does not grant; nothing was sent.
	CodeDestinationNotGranted ErrorCode = "destination_not_granted"
	// CodePlanStale reports a plan that no longer matches its provider, key,
	// endpoint or configuration, or that PlanCheck did not make; nothing was
	// sent.
	CodePlanStale ErrorCode = "plan_stale"
	// CodeListingRequired reports a model that is neither listed for the
	// key, named for the endpoint, nor authored in the config.
	CodeListingRequired ErrorCode = "listing_required"
	// CodeBatchTooLarge reports more than 25 distinct models in one plan.
	CodeBatchTooLarge ErrorCode = "batch_too_large"
	// CodeResponseTooLarge reports a reply larger than the check's response
	// limit.
	CodeResponseTooLarge ErrorCode = "response_too_large"
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
// explicit/catalog/profile knowledge that re-derives next session (on a
// registry with an evidence log, catalog knowledge counts only when the
// explanation read the log and rests on no unsaved row). Persisted=false
// means not confirmed durable (the write was lost, the evidence log could
// not be read, or the answer rests on an unsaved row): the verdict works
// now but may need a re-probe after restart; consumers render their own
// fixed message for it (no raw I/O text crosses here).
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
