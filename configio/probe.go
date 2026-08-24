package configio

import (
	"context"
	"fmt"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// ProbeToolCall performs the explicit, per-model tool_call probe (spec read
// tier 3). It wraps the registry's on-demand resolution: authoritative
// fast-path answers (explicit override, merged profile, valid cached row)
// return without network I/O; otherwise the probe runs and the verdict is
// persisted via the capability-probe store.
//
// D9 dual-outcome contract: the returned State is valid for this session
// immediately; Persisted reports whether the verdict is durable — written
// as a CURRENTLY-VALID probe row, or carried by explicit/catalog/profile
// knowledge that re-derives next session. Persisted=false is a warning,
// never an error: the verdict works now and a later session re-probes at
// most once. (The registry's save is best-effort by design; this outcome
// is how the caller learns a write was lost.)
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
// answered inconclusively); probe_failed for transient failures, with the
// cause retained in the chain.
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
		return ProbeOutcome{}, wrapCode(CodeProbeFailed,
			fmt.Errorf("configio: probe tool_call %s: %w", key, err))
	}
	if state == "" {
		// ResolveToolCall's "" with nil error means no path could answer:
		// resolution unwired, per-key prober spec absent, or the prober
		// does not support tool-call probing.
		return ProbeOutcome{}, wrapCode(CodeProbeUnavailable,
			fmt.Errorf("configio: probe tool_call %s: resolution unavailable for this model", key))
	}
	out := ProbeOutcome{State: state, Persisted: probeDurable(ctx, resolver, key, state)}
	if cerr := ctx.Err(); cerr != nil {
		return ProbeOutcome{}, cerr
	}
	return out, nil
}

// probeDurable reports whether a resolved verdict survives a restart: the
// probe row holds this verdict AND currently validates (a stale same-state
// row will not validate next session — Valid is required), or explicit/
// catalog knowledge declares it, or (for yes) the merged profile carries
// the bit from a source the next session re-derives. A failed read is
// conservative Persisted=false — never an error; the verdict stands.
func probeDurable(ctx context.Context, resolver ToolCallResolver, key provider.ModelKey, state fingerprint.CapProbeState) bool {
	exp, err := resolver.ExplainToolCall(ctx, key)
	if err != nil {
		return false
	}
	switch {
	case exp.State == state && exp.Valid: // probe row holds this verdict and validates
		return true
	case exp.Source == "explicit" || exp.Source == "catalog":
		return true // durable without a row
	case state == fingerprint.CapProbeYes && exp.Has:
		// Merged profile carries the bit. Reachable only with Source
		// "runtime" (incl. the floor merge layer): "explicit"/"catalog"
		// are caught by the case above, and a probe-sourced Has=true is
		// always State==yes && Valid, caught by the row-match case. If
		// ExplainToolCall ever gains a Source that can carry Has without
		// Valid, this case must learn to exclude it.
		return true
	}
	return false
}
