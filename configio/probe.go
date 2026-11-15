package configio

import (
	"context"
	"errors"
	"fmt"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// ProbeToolCall performs the explicit, per-model tool_call probe (spec read
// tier 3). It wraps the registry's on-demand resolution: authoritative
// fast-path answers (explicit override, merged profile, valid cached row)
// return without probe I/O (the durability read may still perform provider
// metadata listings); otherwise the probe runs and the verdict is
// persisted via the capability-probe store.
//
// D9 dual-outcome contract: the returned State is valid for this session
// immediately; Persisted reports whether the verdict is durable — written
// as a CURRENTLY-VALID probe row, or carried by explicit/catalog/profile
// knowledge that re-derives next session (on a log-bound registry, catalog
// knowledge counts only when the explanation read the log and rests on no
// unsaved row; see probeDurable). Persisted=false means not confirmed
// durable: the write was lost, the evidence log could not be read, or the
// answer rests on an unsaved row. It is a warning, never an error: the
// verdict works now and a later session re-probes at most once. (The
// registry's save is best-effort by design.)
//
// Explicitness: never call this from a view, a build, or an inventory
// refresh — it is a user-invoked, consumer-consent-gated operation.
//
// Cancellation: the #456 promise is that a cancelled CALLER receives
// nothing — the zero outcome and the raw context error, UNCLASSIFIED (no
// CodeOf code), even when the underlying singleflight delivered a
// completed result. Store-side non-persistence is guaranteed when the
// probe itself is interrupted; a context-ignoring prober that completes
// after cancellation may still persist into the shared registry cache —
// shared knowledge, not this caller's published result.
//
// Errors carry bounded codes (CodeOf): invalid_argument; probe_unavailable
// when no resolution path could answer (unwired globally, or unavailable
// for this specific provider/model — distinct from a probe that ran and
// answered inconclusively), which includes provider.ErrEvidenceUnbound;
// probe_failed for transient failures, including
// provider.ErrEvidenceUnreadable and provider.ErrAutomaticProbeBudgetExhausted
// on a log-bound registry, with the cause retained in the chain.
func ProbeToolCall(ctx context.Context, resolver ToolCallResolver, key provider.ModelKey) (ProbeOutcome, error) {
	if key.Provider == "" || key.Model == "" {
		return ProbeOutcome{}, wrapCode(CodeInvalidArgument,
			fmt.Errorf("configio: probe tool_call: provider and model are required"))
	}
	if err := ctx.Err(); err != nil {
		return ProbeOutcome{}, err // cancellation: unclassified, publishes nothing
	}
	state, err := resolver.ResolveToolCall(ctx, key)
	if cerr := ctx.Err(); cerr != nil {
		// Post-barrier: a cancelled caller gets nothing, even a follower
		// handed the leader's completed flight result.
		return ProbeOutcome{}, cerr
	}
	if err != nil {
		code := CodeProbeFailed
		if errors.Is(err, provider.ErrEvidenceUnbound) {
			code = CodeProbeUnavailable // no path can answer for this provider
		}
		return ProbeOutcome{}, wrapCode(code,
			fmt.Errorf("configio: probe tool_call %s: %w", key, err))
	}
	switch state {
	case "":
		// ResolveToolCall's "" with nil error means no path could answer:
		// resolution unwired, per-key prober spec absent, or the prober
		// does not support tool-call probing.
		return ProbeOutcome{}, wrapCode(CodeProbeUnavailable,
			fmt.Errorf("configio: probe tool_call %s: resolution unavailable for this model", key))
	case fingerprint.CapProbeYes, fingerprint.CapProbeNo, fingerprint.CapProbeInconclusive:
	default:
		return ProbeOutcome{}, wrapCode(CodeProbeFailed,
			fmt.Errorf("configio: probe tool_call %s: resolver returned invalid state %q", key, state))
	}
	out := ProbeOutcome{State: state, Persisted: probeDurable(ctx, resolver, key, state)}
	if cerr := ctx.Err(); cerr != nil {
		return ProbeOutcome{}, cerr
	}
	return out, nil
}

// probeDurable reports whether a resolved verdict survives a restart, in this
// order. An explicit declaration is durable: the next session re-derives it
// from an unchanged config. Otherwise an explanation whose evidence-log read
// failed (EvidenceUnreadable) is not durable, since a stored answer it could
// not see may overturn it after recovery; then one resting on a row that was
// never stored (Unsaved: the log append failed, so only this registry holds
// it) is not durable. Only then is catalog knowledge durable, then a probe row
// that holds this verdict AND currently validates (a stale same-state row will
// not validate next session — Valid is required), then (for yes) a merged
// profile bit from a source the next session re-derives. Deliberately
// conservative: an unsaved or unreadable answer beside a catalog yes reports
// not durable even when the catalog would re-derive it. A failed explanation
// is conservative Persisted=false — never an error; the verdict stands.
func probeDurable(ctx context.Context, resolver ToolCallResolver, key provider.ModelKey, state fingerprint.CapProbeState) bool {
	exp, err := resolver.ExplainToolCall(ctx, key)
	if err != nil {
		return false
	}
	switch {
	// Source literals are provider.ToolCallExplanation's documented vocabulary
	// (model_registry.go: "explicit" | "catalog" | "runtime" | "probe" |
	// "unknown"); a provider-side rename degrades this to conservative
	// Persisted=false.
	case exp.Source == "explicit":
		return true // durable without a row
	case exp.EvidenceUnreadable:
		return false // a stored answer may be hidden behind this one
	case exp.Unsaved:
		return false // only this registry holds the row
	case exp.Source == "catalog":
		return true // durable without a row
	case exp.State == state && exp.Valid: // probe row holds this verdict and validates
		return true
	case state == fingerprint.CapProbeYes && exp.Has && exp.Source == "runtime":
		// ExplainToolCall labels this "runtime" only when its fresh,
		// read-only projection re-derives the cached profile bit.
		return true
	}
	return false
}
