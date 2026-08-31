// route_plan.go holds the RoutePlan type — the result of Router.Route().
// A RoutePlan binds an immutable request snapshot to the selected provider,
// model, scoring, and fallback chain. The Execute methods dispatch to the
// provider, walk the fallback chain on infrastructure errors, attach
// RouteOutcome metadata to responses, and handle cancellation without
// recording circuit-breaker failures.
package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"time"
)

// ---------------------------------------------------------------------------
// RouteRecorder
// ---------------------------------------------------------------------------

// RouteRecorder receives post-execution signals from RoutePlan.Execute
// methods. The Router implements this interface to feed the circuit breaker,
// warmth tracker, and sticky cache.
type RouteRecorder interface {
	RecordSuccess(key ModelKey, latency LatencyInfo)
	RecordFailure(key ModelKey, err error)
	RecordWarmthUse(key ModelKey)
}

// ---------------------------------------------------------------------------
// AdmittedEmbedder — optional provider extension (#400)
// ---------------------------------------------------------------------------

// AdmitFunc acquires an admission permit for the attempt's model key.
// release is non-nil on success and must be called exactly once (it is
// sync.Once-guarded, so a deferred call is safe).
type AdmitFunc func(ctx context.Context) (release func(), err error)

// AdmittedEmbedder is an optional Provider extension. Providers that
// dedupe embeds internally implement it so admission can be acquired
// inside the dedup leader (one permit per backend request, not per
// caller). admit is never nil; providers call it in the leader only.
//
// Error-chain contract: when admit returns an error, the provider MUST
// return it with the chain intact — wrap only via fmt.Errorf("...: %w",
// err). The route layer detects admission failures by unwrapping the
// returned error; breaking the chain converts an admission failure into
// a fake provider attempt with breaker/feedback side effects.
type AdmittedEmbedder interface {
	EmbedAdmitted(ctx context.Context, req EmbedRequest, admit AdmitFunc) (*EmbedResponse, error)
}

// admissionError marks an error as originating in the admission layer so
// ExecuteEmbed can distinguish it from a provider failure even after an
// AdmittedEmbedder provider wraps it with %w (admission failures surface
// through the provider's return path). Admission failures must not
// produce RouteAttempts or recorder signals. Only admitFuncFor
// constructs one, so providers cannot trip the detection with their own
// errors.
type admissionError struct{ err error }

func (e *admissionError) Error() string { return "admission: " + e.err.Error() }
func (e *admissionError) Unwrap() error { return e.err }

// unwrapAdmissionError strips the internal admission marker before an
// error is returned to the caller: callers see the bare cancellation or
// ErrRouterClosed, exactly like a primary admission failure (§4). The
// marker exists only so recordResult can tell an admission-gate exit
// apart from an interrupted provider call — the former must not warm
// the previous attempt's key.
func unwrapAdmissionError(err error) error {
	var admErr *admissionError
	if errors.As(err, &admErr) {
		return admErr.Unwrap()
	}
	return err
}

// ---------------------------------------------------------------------------
// RoutePlan
// ---------------------------------------------------------------------------

// RoutePlan holds the selected provider/model, scoring, and fallback chain.
// It is the result of Router.Route() and the entry point for executing
// routed requests against a provider.
type RoutePlan struct {
	Kind      RouteKind
	Provider  Provider
	Model     string
	Profile   *ModelProfile
	Request   RoutingRequest // immutable normalized request snapshot
	Score     float64
	Budget    BudgetResult
	Fallbacks []RoutePlan
	Reason    string
	Degraded  bool
	wasSticky bool             // internal: propagated to RouteOutcome
	recorder  RouteRecorder    // internal: set by Router
	feedback  *RoutingFeedback // internal: set by Router via SetFeedback; nil = no recording

	// admission is the slot-admission seam (#400). nil = ungoverned
	// Router (no slot source): every bracket is a no-op. Stamped by
	// buildPlan; deliberately NOT derived from recorder so an external
	// SetRecorder swap cannot silently disable admission.
	admission slotAdmitter

	// scoreBreakdown carries the winning candidate's unexported
	// scoreBreakdown; buildOutcome translates it into the public
	// ScoreBreakdown on the RouteOutcome. nil = no public breakdown.
	scoreBreakdown *scoreBreakdown
	// builtUnderMode records the FeedbackScoringMode that produced this
	// plan so buildOutcome can render the matching operator-facing label.
	builtUnderMode FeedbackScoringMode
	// feedbackStatus records the route-level feedback snapshot status so
	// operators can distinguish "off by design" from "off because the
	// store failed" without parsing logs.
	feedbackStatus feedbackSnapshotStatus

	// feedbackLogger is the optional once-logged-warning sink used by
	// newRouteIDWithWarn (RNG failures) and recordOutcomeFeedback (store
	// write failures). nil disables emission and the bare PR2 fallback
	// behavior (empty RouteID / silent swallow) applies. Set by buildPlan
	// via setFeedbackTelemetry.
	feedbackLogger feedbackLogger

	// feedbackWarn holds the sync.Once guards backing the above logger so
	// a persistently broken store cannot flood operator logs. Per-Router
	// instance; shared with every plan the Router constructs. nil
	// disables emission. Set by buildPlan via setFeedbackTelemetry.
	feedbackWarn *feedbackWarningState

	// destGate is the destination-admission seam (#477). nil = ungated
	// Router: every bind is a no-op and requests carry no capability, so
	// a guarded transport downstream still denies — absence fails closed
	// at the transport, never open here. Stamped by buildPlan; like
	// admission, deliberately not derived from recorder.
	destGate *DestinationGate
}

// String returns a human-readable summary of the route plan.
func (rp *RoutePlan) String() string {
	return fmt.Sprintf("%s/%s (score=%.2f, budget=%s, fallbacks=%d): %s",
		rp.Profile.Key.Provider, rp.Model, rp.Score,
		rp.Budget.Decision, len(rp.Fallbacks), rp.Reason)
}

// SetRecorder sets the internal route recorder. This is called by the Router
// after constructing the plan.
func (rp *RoutePlan) SetRecorder(r RouteRecorder) {
	rp.recorder = r
}

// SetFeedback sets the RoutingFeedback wrapper used by handleResult to
// record per-attempt outcomes. The Router calls this from buildPlan.
// nil disables feedback recording for this plan (default).
func (rp *RoutePlan) SetFeedback(rf *RoutingFeedback) {
	rp.feedback = rf
}

// SetWasSticky marks the plan as having been selected via sticky routing.
func (rp *RoutePlan) SetWasSticky(v bool) {
	rp.wasSticky = v
}

// setAdmission stamps the slot-admission seam (#400). The Router calls
// this from buildPlan; nil disables admission for this plan. Unexported:
// admission is Router-owned plumbing, not public plan API.
func (rp *RoutePlan) setAdmission(a slotAdmitter) {
	rp.admission = a
}

// setScoreBreakdown stamps the winning candidate's score breakdown onto
// the plan. The Router calls this from buildPlan; subsequent buildOutcome
// translates the unexported breakdown into the public ScoreBreakdown.
// nil disables the public ScoreBreakdown on this plan's outcomes.
// Unexported because it takes an unexported type; external callers
// cannot legally pass a *scoreBreakdown anyway.
// setDestinationGate stamps the destination-admission gate (#477). Package-
// internal: the Router wires it in buildPlan via WithDestinationGate.
func (rp *RoutePlan) setDestinationGate(g *DestinationGate) {
	rp.destGate = g
}

// bindDestination attaches the destination capability for this plan's use
// case and the attempt's provider. It runs once per provider attempt —
// primary and each fallback — BEFORE the admission bracket, so a denied
// attempt neither holds a slot nor records an attempt. With no gate the
// context passes through unchanged: the plan adds no authority, and any
// guarded transport downstream denies the bare context on its own.
func (rp *RoutePlan) bindDestination(ctx context.Context, key ModelKey) (context.Context, error) {
	if rp.destGate == nil {
		return ctx, nil
	}
	return rp.destGate.Bind(ctx, rp.Request.UseCase, key.Provider)
}

func (rp *RoutePlan) setScoreBreakdown(bd *scoreBreakdown) {
	rp.scoreBreakdown = bd
}

// setBuiltUnderMode records the FeedbackScoringMode in effect when the
// Router built this plan so buildOutcome can render the public
// ScoreBreakdown with the matching operator-facing label. Unexported
// to keep the RoutePlan public surface narrow; this is plumbing,
// not API.
func (rp *RoutePlan) setBuiltUnderMode(mode FeedbackScoringMode) {
	rp.builtUnderMode = mode
}

// setFeedbackStatus records the feedback snapshot status from the route's
// feedback snapshot so buildOutcome can render it on the public
// ScoreBreakdown for operator visibility into why feedback was active
// or inactive. Unexported because it takes an unexported type.
func (rp *RoutePlan) setFeedbackStatus(status feedbackSnapshotStatus) {
	rp.feedbackStatus = status
}

// setFeedbackTelemetry records the once-logged-warning state and logger
// the plan will use when newRouteIDWithWarn hits a crypto/rand failure or
// when recordOutcomeFeedback observes a store error. nil values disable
// warnings (silent fallback to PR2 behavior: empty RouteID, silent-swallow
// on write). Unexported because it takes an unexported *feedbackWarningState
// and feedbackLogger; the convention matches the other unexported setters
// (setScoreBreakdown / setBuiltUnderMode / setFeedbackStatus).
func (rp *RoutePlan) setFeedbackTelemetry(state *feedbackWarningState, logger feedbackLogger) {
	rp.feedbackWarn = state
	rp.feedbackLogger = logger
}

// WasSticky reports whether the plan was selected via sticky routing. This
// mirrors the RouteOutcome field of the same name but is available before
// execution, which is what dry-run callers want.
func (rp *RoutePlan) WasSticky() bool {
	return rp.wasSticky
}

// ---------------------------------------------------------------------------
// Execute — Chat (non-streaming)
// ---------------------------------------------------------------------------

// ExecuteChat dispatches a non-streaming chat request to the selected provider,
// walking the fallback chain on infrastructure errors.
func (rp *RoutePlan) ExecuteChat(ctx context.Context) (*ChatResponse, error) {
	if rp.Budget.Decision == BudgetTruncate {
		return nil, ErrBudgetAdaptationRequired
	}

	var attempts []RouteAttempt
	req := rp.buildChatRequest(false)

	// Destination bind (#477) precedes the admission bracket: a denied
	// destination must not consume a slot, and like admission failure it
	// means no attempt, no recorder signals, no outcome.
	bctx, bindErr := rp.bindDestination(ctx, rp.Profile.Key)
	if bindErr != nil {
		return nil, bindErr
	}
	// Admission bracket (#400): failure before any provider contact means
	// no attempt, no recorder signals, no outcome. The deferred release is
	// the panic/early-return backstop; the explicit release after the call
	// (Once-guarded) is what enforces release-before-fallback-acquire.
	release, admErr := rp.acquireFor(ctx, rp.Profile.Key)
	if admErr != nil {
		return nil, admErr
	}
	defer release()
	start := time.Now()
	resp, err := rp.Provider.Chat(bctx, req)
	release()
	attempts = append(attempts, makeAttempt(rp.Profile.Key, err, time.Since(start)))

	fallbacksUsed := 0
	if err != nil && IsInfrastructureError(err) {
		rp.recordFailure(rp.Profile.Key, err)

		for i, fb := range rp.Fallbacks {
			fbReq := fb.buildChatRequest(false)
			// Destination bind (#477): terminal like the admission errors
			// below — stop the walk without contacting the denied fallback.
			fbCtx, fbBindErr := rp.bindDestination(ctx, fb.Profile.Key)
			if fbBindErr != nil {
				err = &admissionError{err: fbBindErr}
				break
			}
			fbRelease, fbAdmErr := rp.acquireFor(ctx, fb.Profile.Key)
			if fbAdmErr != nil {
				// Terminal (cancel/closed): stop the walk; prior attempts
				// and their signals stand.
				err = &admissionError{err: fbAdmErr}
				break
			}
			defer fbRelease()
			fbStart := time.Now()
			resp, err = fb.Provider.Chat(fbCtx, fbReq)
			fbRelease()
			attempts = append(attempts, makeAttempt(fb.Profile.Key, err, time.Since(fbStart)))
			if err == nil {
				fallbacksUsed = i + 1
				break
			}
			if IsInfrastructureError(err) {
				rp.recordFailure(fb.Profile.Key, err)
				continue
			}
			// Non-infrastructure error (incl. cancellation) — stop trying.
			break
		}
	}

	outcome := rp.handleResult(err, fallbacksUsed, attempts)
	if resp != nil && outcome != nil {
		resp.RouteOutcome = outcome
	}

	return resp, unwrapAdmissionError(err)
}

// ---------------------------------------------------------------------------
// Execute — Chat stream
// ---------------------------------------------------------------------------

// ExecuteChatStream dispatches a streaming chat request to the selected
// provider. Fallback is only attempted when the primary fails with an
// infrastructure error AND no user-visible chunk has yet been delivered
// (a chunk is "visible" when Content, Thinking, or ToolCalls is non-empty).
// Each provider call produces exactly one RouteAttempt. A terminal Done chunk
// prepares the successful attempt and RouteOutcome, but feedback recording is
// deferred until the provider stream returns so post-Done provider errors and
// callback aborts are reconciled against the returned execution result. User-
// callback errors are captured separately and attributed as AttemptStatusUnknown.
//
// Stream callback invocation:
//   - fn is invoked once per chunk in the calling goroutine. Provider
//     implementations are required to honor this contract (see
//     Provider.ChatStream); custom providers that fan callbacks out to
//     goroutines would race against the closure-captured attempt state.
//   - If the primary emits chunks with no visible content (heartbeats,
//     synthetic partial-Done) and then fails with infra error, fn first
//     receives the primary's content-less chunks, then receives the
//     fallback's chunks. Once any visible content reaches fn, fallback is
//     suppressed for the rest of the request.
//   - On a Done chunk, fn receives the chunk with RouteOutcome attached.
//     Consumers that store the RouteOutcome (or its Attempts slice) should
//     do so synchronously inside fn — the slice is cloned per Done chunk,
//     so it remains valid after fn returns even if the callback errors and
//     post-stream logic appends additional attempts.
func (rp *RoutePlan) ExecuteChatStream(ctx context.Context, fn func(ChatResponse) error) error {
	if rp.Budget.Decision == BudgetTruncate {
		return ErrBudgetAdaptationRequired
	}

	var attempts []RouteAttempt
	req := rp.buildChatRequest(true)

	delivered := false
	fallbacksUsed := 0
	var outcome *RouteOutcome

	// Destination bind (#477) precedes the admission bracket; see
	// ExecuteChat for the contract.
	bctx, bindErr := rp.bindDestination(ctx, rp.Profile.Key)
	if bindErr != nil {
		return bindErr
	}
	// Admission bracket (#400): the permit spans the ENTIRE stream —
	// ChatStream is synchronous (callback on the caller's goroutine,
	// returns only when the stream completes, errors, or the caller's
	// ctx cancels), so release-after-return covers completion, mid-stream
	// error, and abandonment with one Once-guarded release point. The
	// clock starts after admission so queue wait stays out of attempt
	// latency.
	release, admErr := rp.acquireFor(ctx, rp.Profile.Key)
	if admErr != nil {
		return admErr
	}
	defer release()

	primaryStart := time.Now()
	streamDone := false
	var callbackErr error

	wrappedFn := func(chunk ChatResponse) error {
		if !delivered && hasVisibleContent(chunk.Content, chunk.Thinking, chunk.ToolCalls) {
			delivered = true
		}
		if chunk.Done && !chunk.Partial && !streamDone {
			// Clone attempts before appending so the chunk's outcome owns
			// an independent slice. Otherwise a callback error would let a
			// later iteration overwrite the success entry at index [n] of
			// the shared underlying array.
			pendingAttempts := append(slices.Clone(attempts),
				makeAttempt(rp.Profile.Key, nil, time.Since(primaryStart)))
			pendingOutcome := rp.buildOutcome(fallbacksUsed, pendingAttempts)
			chunk.RouteOutcome = pendingOutcome
			if e := fn(chunk); e != nil {
				callbackErr = e
				return e
			}
			attempts = pendingAttempts
			streamDone = true
			outcome = pendingOutcome
			return nil
		}
		if e := fn(chunk); e != nil {
			callbackErr = e
			return e
		}
		return nil
	}

	err := rp.Provider.ChatStream(bctx, req, wrappedFn)
	release()
	if streamDone && err != nil && (callbackErr == nil || !errors.Is(err, callbackErr)) {
		// A provider may still surface transport/read errors after it has
		// delivered a final Done chunk accepted by the callback. Treat the
		// accepted terminal chunk as authoritative so feedback and the returned
		// execution result agree.
		err = nil
	}

	if !streamDone {
		if callbackErr != nil {
			// User aborted via callback. Attribute as Unknown, not a
			// provider failure.
			attempts = append(attempts, makeUnknownAttempt(rp.Profile.Key, time.Since(primaryStart)))
		} else {
			attempts = append(attempts,
				makeAttempt(rp.Profile.Key, err, time.Since(primaryStart)))
		}
	}

	if err != nil && IsInfrastructureError(err) {
		rp.recordFailure(rp.Profile.Key, err)

		// Only attempt fallback if no user-visible content was delivered.
		if !delivered {
			for i, fb := range rp.Fallbacks {
				fbReq := fb.buildChatRequest(true)
				// Destination bind (#477): terminal, no contact (§4).
				fbCtx, fbBindErr := rp.bindDestination(ctx, fb.Profile.Key)
				if fbBindErr != nil {
					err = &admissionError{err: fbBindErr}
					break
				}
				fbRelease, fbAdmErr := rp.acquireFor(ctx, fb.Profile.Key)
				if fbAdmErr != nil {
					// Terminal (cancel/closed): stop the walk; prior
					// attempts and their signals stand (§4).
					err = &admissionError{err: fbAdmErr}
					break
				}
				defer fbRelease()
				delivered = false
				fbStart := time.Now()
				fbStreamDone := false
				var fbCallbackErr error

				wrappedFbFn := func(chunk ChatResponse) error {
					if !delivered && hasVisibleContent(chunk.Content, chunk.Thinking, chunk.ToolCalls) {
						delivered = true
					}
					if chunk.Done && !chunk.Partial && !fbStreamDone {
						// Clone attempts before appending so the chunk's
						// outcome owns an independent slice (see primary
						// Done handler for rationale).
						pendingAttempts := append(slices.Clone(attempts),
							makeAttempt(fb.Profile.Key, nil, time.Since(fbStart)))
						// Done implies this fallback served the request, so it
						// is the SELECTED fallback. Pending-only until the
						// callback returns nil — keeps the idempotent
						// finalization contract.
						pendingFallbacksUsed := i + 1
						pendingOutcome := rp.buildOutcome(pendingFallbacksUsed, pendingAttempts)
						chunk.RouteOutcome = pendingOutcome
						if e := fn(chunk); e != nil {
							fbCallbackErr = e
							return e
						}
						attempts = pendingAttempts
						fallbacksUsed = pendingFallbacksUsed
						fbStreamDone = true
						outcome = pendingOutcome
						return nil
					}
					if e := fn(chunk); e != nil {
						fbCallbackErr = e
						return e
					}
					return nil
				}

				err = fb.Provider.ChatStream(fbCtx, fbReq, wrappedFbFn)
				fbRelease()
				if fbStreamDone && err != nil && (fbCallbackErr == nil || !errors.Is(err, fbCallbackErr)) {
					// See primary streamDone handling above.
					err = nil
				}

				if !fbStreamDone {
					if fbCallbackErr != nil {
						attempts = append(attempts, makeUnknownAttempt(fb.Profile.Key, time.Since(fbStart)))
					} else {
						attempts = append(attempts,
							makeAttempt(fb.Profile.Key, err, time.Since(fbStart)))
					}
				}

				if err == nil {
					// Stream returned cleanly without a Done chunk; this
					// fallback still served the (incomplete) response.
					fallbacksUsed = i + 1
					break
				}
				if IsInfrastructureError(err) {
					rp.recordFailure(fb.Profile.Key, err)
					// Only continue to next fallback if this one did not deliver content.
					if delivered {
						break
					}
					continue
				}
				break
			}
		}
	}

	// Record signals for streams that completed without a Done chunk, or
	// that ended in a non-infrastructure error / cancellation.
	if outcome == nil {
		rp.handleResult(err, fallbacksUsed, attempts)
	} else if err == nil {
		rp.recordResult(nil, attempts, outcome)
	}

	return unwrapAdmissionError(err)
}

// ---------------------------------------------------------------------------
// Execute — Generate (non-streaming)
// ---------------------------------------------------------------------------

// ExecuteGenerate dispatches a non-streaming generate request to the selected
// provider, walking the fallback chain on infrastructure errors.
func (rp *RoutePlan) ExecuteGenerate(ctx context.Context) (*GenerateResponse, error) {
	if rp.Budget.Decision == BudgetTruncate {
		return nil, ErrBudgetAdaptationRequired
	}

	var attempts []RouteAttempt
	req := rp.buildGenerateRequest(false)

	// Destination bind (#477), then the admission bracket (#400): see
	// ExecuteChat for the full contract.
	bctx, bindErr := rp.bindDestination(ctx, rp.Profile.Key)
	if bindErr != nil {
		return nil, bindErr
	}
	release, admErr := rp.acquireFor(ctx, rp.Profile.Key)
	if admErr != nil {
		return nil, admErr
	}
	defer release()
	start := time.Now()
	resp, err := rp.Provider.Generate(bctx, req)
	release()
	attempts = append(attempts, makeAttempt(rp.Profile.Key, err, time.Since(start)))

	fallbacksUsed := 0
	if err != nil && IsInfrastructureError(err) {
		rp.recordFailure(rp.Profile.Key, err)

		for i, fb := range rp.Fallbacks {
			fbReq := fb.buildGenerateRequest(false)
			// Destination bind (#477): terminal, no contact.
			fbCtx, fbBindErr := rp.bindDestination(ctx, fb.Profile.Key)
			if fbBindErr != nil {
				err = &admissionError{err: fbBindErr}
				break
			}
			fbRelease, fbAdmErr := rp.acquireFor(ctx, fb.Profile.Key)
			if fbAdmErr != nil {
				err = &admissionError{err: fbAdmErr}
				break
			}
			defer fbRelease()
			fbStart := time.Now()
			resp, err = fb.Provider.Generate(fbCtx, fbReq)
			fbRelease()
			attempts = append(attempts, makeAttempt(fb.Profile.Key, err, time.Since(fbStart)))
			if err == nil {
				fallbacksUsed = i + 1
				break
			}
			if IsInfrastructureError(err) {
				rp.recordFailure(fb.Profile.Key, err)
				continue
			}
			break
		}
	}

	outcome := rp.handleResult(err, fallbacksUsed, attempts)
	if resp != nil && outcome != nil {
		resp.RouteOutcome = outcome
	}

	return resp, unwrapAdmissionError(err)
}

// ---------------------------------------------------------------------------
// Execute — Generate stream
// ---------------------------------------------------------------------------

// ExecuteGenerateStream dispatches a streaming generate request to the
// selected provider. Fallback is only attempted when the primary fails with
// an infrastructure error AND no user-visible Response chunk has yet been
// delivered. Each provider call produces exactly one RouteAttempt. A terminal
// Done chunk prepares the successful attempt and RouteOutcome, but feedback
// recording is deferred until the provider stream returns so post-Done
// provider errors and callback aborts are reconciled against the returned
// execution result. User-callback errors are captured separately and
// attributed as AttemptStatusUnknown.
//
// See ExecuteChatStream for the full stream callback contract — the
// invariants are identical (serial-callback contract, Done-chunk
// RouteOutcome ownership, attempts slice clone-per-Done).
func (rp *RoutePlan) ExecuteGenerateStream(ctx context.Context, fn func(GenerateResponse) error) error {
	if rp.Budget.Decision == BudgetTruncate {
		return ErrBudgetAdaptationRequired
	}

	var attempts []RouteAttempt
	req := rp.buildGenerateRequest(true)

	delivered := false
	fallbacksUsed := 0
	var outcome *RouteOutcome

	// Destination bind (#477), then the admission bracket (#400): see
	// ExecuteChatStream — the permit spans the entire stream and releases
	// exactly once on every return path.
	bctx, bindErr := rp.bindDestination(ctx, rp.Profile.Key)
	if bindErr != nil {
		return bindErr
	}
	release, admErr := rp.acquireFor(ctx, rp.Profile.Key)
	if admErr != nil {
		return admErr
	}
	defer release()

	primaryStart := time.Now()
	streamDone := false
	var callbackErr error

	wrappedFn := func(chunk GenerateResponse) error {
		if !delivered && chunk.Response != "" {
			delivered = true
		}
		if chunk.Done && !chunk.Partial && !streamDone {
			// Clone attempts before appending so the chunk's outcome owns
			// an independent slice. Otherwise a callback error would let a
			// later iteration overwrite the success entry at index [n] of
			// the shared underlying array.
			pendingAttempts := append(slices.Clone(attempts),
				makeAttempt(rp.Profile.Key, nil, time.Since(primaryStart)))
			pendingOutcome := rp.buildOutcome(fallbacksUsed, pendingAttempts)
			chunk.RouteOutcome = pendingOutcome
			if e := fn(chunk); e != nil {
				callbackErr = e
				return e
			}
			attempts = pendingAttempts
			streamDone = true
			outcome = pendingOutcome
			return nil
		}
		if e := fn(chunk); e != nil {
			callbackErr = e
			return e
		}
		return nil
	}

	err := rp.Provider.GenerateStream(bctx, req, wrappedFn)
	release()
	if streamDone && err != nil && (callbackErr == nil || !errors.Is(err, callbackErr)) {
		// A provider may still surface transport/read errors after it has
		// delivered a final Done chunk accepted by the callback. Treat the
		// accepted terminal chunk as authoritative so feedback and the returned
		// execution result agree.
		err = nil
	}

	if !streamDone {
		if callbackErr != nil {
			// User aborted via callback. Attribute as Unknown, not a
			// provider failure.
			attempts = append(attempts, makeUnknownAttempt(rp.Profile.Key, time.Since(primaryStart)))
		} else {
			attempts = append(attempts,
				makeAttempt(rp.Profile.Key, err, time.Since(primaryStart)))
		}
	}

	if err != nil && IsInfrastructureError(err) {
		rp.recordFailure(rp.Profile.Key, err)

		// Only attempt fallback if no user-visible content was delivered.
		if !delivered {
			for i, fb := range rp.Fallbacks {
				fbReq := fb.buildGenerateRequest(true)
				// Destination bind (#477): terminal, no contact (§4).
				fbCtx, fbBindErr := rp.bindDestination(ctx, fb.Profile.Key)
				if fbBindErr != nil {
					err = &admissionError{err: fbBindErr}
					break
				}
				fbRelease, fbAdmErr := rp.acquireFor(ctx, fb.Profile.Key)
				if fbAdmErr != nil {
					// Terminal (cancel/closed): stop the walk; prior
					// attempts and their signals stand (§4).
					err = &admissionError{err: fbAdmErr}
					break
				}
				defer fbRelease()
				delivered = false
				fbStart := time.Now()
				fbStreamDone := false
				var fbCallbackErr error

				wrappedFbFn := func(chunk GenerateResponse) error {
					if !delivered && chunk.Response != "" {
						delivered = true
					}
					if chunk.Done && !chunk.Partial && !fbStreamDone {
						// Clone attempts before appending so the chunk's
						// outcome owns an independent slice (see primary
						// Done handler for rationale).
						pendingAttempts := append(slices.Clone(attempts),
							makeAttempt(fb.Profile.Key, nil, time.Since(fbStart)))
						// Done implies this fallback served the request, so it
						// is the SELECTED fallback. Pending-only until the
						// callback returns nil — keeps the idempotent
						// finalization contract.
						pendingFallbacksUsed := i + 1
						pendingOutcome := rp.buildOutcome(pendingFallbacksUsed, pendingAttempts)
						chunk.RouteOutcome = pendingOutcome
						if e := fn(chunk); e != nil {
							fbCallbackErr = e
							return e
						}
						attempts = pendingAttempts
						fallbacksUsed = pendingFallbacksUsed
						fbStreamDone = true
						outcome = pendingOutcome
						return nil
					}
					if e := fn(chunk); e != nil {
						fbCallbackErr = e
						return e
					}
					return nil
				}

				err = fb.Provider.GenerateStream(fbCtx, fbReq, wrappedFbFn)
				fbRelease()
				if fbStreamDone && err != nil && (fbCallbackErr == nil || !errors.Is(err, fbCallbackErr)) {
					// See primary streamDone handling above.
					err = nil
				}

				if !fbStreamDone {
					if fbCallbackErr != nil {
						attempts = append(attempts, makeUnknownAttempt(fb.Profile.Key, time.Since(fbStart)))
					} else {
						attempts = append(attempts,
							makeAttempt(fb.Profile.Key, err, time.Since(fbStart)))
					}
				}

				if err == nil {
					// Stream returned cleanly without a Done chunk; this
					// fallback still served the (incomplete) response.
					fallbacksUsed = i + 1
					break
				}
				if IsInfrastructureError(err) {
					rp.recordFailure(fb.Profile.Key, err)
					// Only continue to next fallback if this one did not deliver content.
					if delivered {
						break
					}
					continue
				}
				break
			}
		}
	}

	// Record signals for streams that completed without a Done chunk, or
	// that ended in a non-infrastructure error / cancellation.
	if outcome == nil {
		rp.handleResult(err, fallbacksUsed, attempts)
	} else if err == nil {
		rp.recordResult(nil, attempts, outcome)
	}

	return unwrapAdmissionError(err)
}

// ---------------------------------------------------------------------------
// Execute — Embed
// ---------------------------------------------------------------------------

// ExecuteEmbed dispatches an embedding request to the selected provider,
// walking the fallback chain on infrastructure errors.
func (rp *RoutePlan) ExecuteEmbed(ctx context.Context) (*EmbedResponse, error) {
	if rp.Budget.Decision == BudgetTruncate {
		return nil, ErrBudgetAdaptationRequired
	}

	var attempts []RouteAttempt
	req := rp.buildEmbedRequest()

	var resp *EmbedResponse
	var err error
	// Destination bind (#477) precedes both admission shapes below: a
	// denied destination must neither start a shared flight nor hold a
	// per-caller slot.
	bctx, bindErr := rp.bindDestination(ctx, rp.Profile.Key)
	if bindErr != nil {
		return nil, bindErr
	}
	if ae, ok := rp.Provider.(AdmittedEmbedder); ok {
		// Dead-caller pre-check (§6 amended contract): a caller whose
		// ctx is already done must not start or join a shared flight on
		// a GOVERNED key. No separate governance probe: a dead ctx is
		// routed through acquireFor itself — the gate's governance-first
		// entry ordering means ungoverned keys (and nil admission)
		// return a no-op success and the call proceeds bit-identically
		// to today, while governed keys return the context error before
		// any flight exists. Healthy callers (ctx.Err() == nil) skip
		// this entirely: zero extra work, no Capacity read. An already-
		// cancelled UNGOVERNED caller pays one extra governance read
		// here on top of the one inside the flight's admit — two
		// lock-free map reads on a dead-caller edge path, accepted over
		// plumbing governance knowledge into the admit func.
		if ctx.Err() != nil {
			rel, admErr := rp.acquireFor(ctx, rp.Profile.Key)
			if admErr != nil {
				return nil, admErr // no flight, no attempt, no signals
			}
			rel() // no-op release: ungoverned pass-through
		}
		// Admission happens inside the provider's dedup leader (§6): no
		// route-level bracket, or M identical callers would hold M
		// permits and the gate would break the dedup itself.
		start := time.Now()
		resp, err = ae.EmbedAdmitted(bctx, req, rp.admitFuncFor(rp.Profile.Key))
		var aErr *admissionError
		if errors.As(err, &aErr) {
			// Admission failure surfaced through the provider (§4): no
			// attempt, no signals; return the original error.
			return nil, aErr.Unwrap()
		}
		attempts = append(attempts, makeAttempt(rp.Profile.Key, err, time.Since(start)))
	} else {
		// Conservative per-caller bracket (§4) for providers without
		// internal dedup; see ExecuteChat for the full contract.
		release, admErr := rp.acquireFor(ctx, rp.Profile.Key)
		if admErr != nil {
			return nil, admErr
		}
		defer release()
		start := time.Now()
		resp, err = rp.Provider.Embed(bctx, req)
		release()
		attempts = append(attempts, makeAttempt(rp.Profile.Key, err, time.Since(start)))
	}

	fallbacksUsed := 0
	if err != nil && IsInfrastructureError(err) {
		rp.recordFailure(rp.Profile.Key, err)

		for i, fb := range rp.Fallbacks {
			fbReq := fb.buildEmbedRequest()
			// Destination bind (#477): terminal, no contact, either shape.
			fbCtx, fbBindErr := rp.bindDestination(ctx, fb.Profile.Key)
			if fbBindErr != nil {
				err = &admissionError{err: fbBindErr}
				break
			}
			if ae, ok := fb.Provider.(AdmittedEmbedder); ok {
				if ctx.Err() != nil {
					rel, fbAdmErr := rp.acquireFor(ctx, fb.Profile.Key)
					if fbAdmErr != nil {
						err = &admissionError{err: fbAdmErr}
						break
					}
					rel()
				}
				fbStart := time.Now()
				resp, err = ae.EmbedAdmitted(fbCtx, fbReq, rp.admitFuncFor(fb.Profile.Key))
				var aErr *admissionError
				if errors.As(err, &aErr) {
					// Terminal (§4): stop the walk; prior attempts and
					// their signals stand, no attempt for this one. The
					// marker is kept so recordResult suppresses cancel-
					// warmth; the return site unwraps it for the caller.
					err = aErr
					break
				}
				attempts = append(attempts, makeAttempt(fb.Profile.Key, err, time.Since(fbStart)))
			} else {
				fbRelease, fbAdmErr := rp.acquireFor(ctx, fb.Profile.Key)
				if fbAdmErr != nil {
					err = &admissionError{err: fbAdmErr}
					break
				}
				defer fbRelease()
				fbStart := time.Now()
				resp, err = fb.Provider.Embed(fbCtx, fbReq)
				fbRelease()
				attempts = append(attempts, makeAttempt(fb.Profile.Key, err, time.Since(fbStart)))
			}
			if err == nil {
				fallbacksUsed = i + 1
				break
			}
			if IsInfrastructureError(err) {
				rp.recordFailure(fb.Profile.Key, err)
				continue
			}
			break
		}
	}

	outcome := rp.handleResult(err, fallbacksUsed, attempts)
	if resp != nil && outcome != nil {
		resp.RouteOutcome = outcome
	}

	return resp, unwrapAdmissionError(err)
}

// ---------------------------------------------------------------------------
// handleResult — post-execution recorder + outcome builder
// ---------------------------------------------------------------------------

// handleResult records side-channel signals (breaker, warmth, feedback) and
// returns a *RouteOutcome carrying the per-attempt trace. The outcome is
// ALWAYS built, regardless of err — failure-path callers have no response
// to attach the outcome to, but the feedback seam consumes it via
// recordOutcomeFeedback. Pre-PR2 returned nil on cancellation/failure;
// PR2 always returns non-nil.
func (rp *RoutePlan) handleResult(err error, fallbacksUsed int, attempts []RouteAttempt) *RouteOutcome {
	outcome := rp.buildOutcome(fallbacksUsed, attempts)
	rp.recordResult(err, attempts, outcome)
	return outcome
}

// recordResult attributes warmth and (on success) success signals against the
// last-touched model, then forwards the prebuilt outcome to the feedback seam.
// Warmth derives from attempts[len(attempts)-1].Key when available — the last
// attempt is the most recently touched model regardless of outcome, so the OS
// page cache is hot for that model whether it succeeded, failed, or was
// cancelled. Falls back to rp.Profile.Key when attempts is nil/empty (only
// the unit tests for handleResult directly hit this branch; production
// Execute* methods always populate attempts).
func (rp *RoutePlan) recordResult(err error, attempts []RouteAttempt, outcome *RouteOutcome) {
	actualKey := rp.Profile.Key
	if len(attempts) > 0 {
		actualKey = attempts[len(attempts)-1].Key
	}

	var admErr *admissionError
	if err == nil {
		rp.recordSuccess(actualKey, LatencyInfo{})
		rp.recordWarmthUse(actualKey)
		rp.recordSlotUse(actualKey)
	} else if errors.As(err, &admErr) {
		// The walk ended at a fallback's ADMISSION gate (#400): no
		// provider call was interrupted, so the cancel-warmth branch
		// below must not fire — actualKey is the previous (failed)
		// attempt's key, whose signals already ran inline, and an
		// infra-failed attempt never earned warmth pre-admission
		// either. Outcome feedback below still records the turn.
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		rp.recordWarmthUse(actualKey) // model IS warm even if caller bailed — but no slot signal
	}
	// Infrastructure failures continue to be recorded inline by Execute methods.

	rp.recordOutcomeFeedback(outcome)
}

// buildOutcome constructs a RouteOutcome from the plan, the count of
// fallbacks selected, and the per-attempt trace.
//
// ActualModel reflects the model whose response served the request — the
// last attempt's key when it succeeded, otherwise the planned (primary)
// model. ActualModel is derived from attempts alone, not from
// fallbacksUsed, which keeps it consistent across non-streaming and
// streaming Execute methods regardless of how mid-stream counter updates
// were ordered.
//
// fallbacksUsed populates the RouteOutcome.FallbacksUsed output field only;
// callers must pass the count of fallbacks that actually served (zero if
// the primary served or if every attempt failed).
func (rp *RoutePlan) buildOutcome(fallbacksUsed int, attempts []RouteAttempt) *RouteOutcome {
	actualKey := rp.Profile.Key
	if n := len(attempts); n > 0 && attempts[n-1].Status == AttemptStatusSucceeded {
		actualKey = attempts[n-1].Key
	}
	out := &RouteOutcome{
		PlannedModel:  rp.Profile.Key,
		ActualModel:   actualKey,
		FallbacksUsed: fallbacksUsed,
		WasSticky:     rp.wasSticky,
		Score:         rp.Score,
		Reason:        rp.Reason,
		RouteID:       newRouteIDWithWarn(rp.feedbackWarn, rp.feedbackLogger),
		Attempts:      attempts,
	}
	if rp.scoreBreakdown != nil {
		out.ScoreBreakdown = publicScoreBreakdown(rp.scoreBreakdown, rp.builtUnderMode, rp.feedbackStatus)
	}
	return out
}

// publicScoreBreakdown translates the unexported per-candidate
// scoreBreakdown into the public ScoreBreakdown for RouteOutcome. The
// mode argument is the FeedbackScoringMode under which the plan was
// built (Off mode skips the stamp at the Router call site, so this
// function is unreachable under Off in production); the status argument
// is the snapshot's feedbackSnapshotStatus, stamped by the Router from
// feedbackSnapshot.status at plan-build.
func publicScoreBreakdown(bd *scoreBreakdown, mode FeedbackScoringMode, status feedbackSnapshotStatus) *ScoreBreakdown {
	if bd == nil {
		return nil
	}
	// Convert the zero-value time.Time to a nil *time.Time so the JSON
	// boundary omits the field entirely (rather than emitting "0001-01-01T..."
	// which an operator might confuse with a real timestamp).
	var updatedAt *time.Time
	if !bd.feedbackUpdatedAt.IsZero() {
		t := bd.feedbackUpdatedAt
		updatedAt = &t
	}
	return &ScoreBreakdown{
		FeedbackMode:           mode.String(),
		FeedbackSnapshotStatus: string(status),
		FeedbackApplied:        bd.feedbackActive && mode == FeedbackScoringEnforce,
		FeedbackScore:          sanitizeScoreForJSON(bd.feedbackRaw),
		FeedbackAdjustedScore:  bd.feedbackAdjusted,
		FeedbackSampleCount:    bd.feedbackSampleCount,
		FeedbackScoredCount:    bd.feedbackScoredCount,
		FeedbackUpdatedAt:      updatedAt,
		ScoreWithoutFeedback:   bd.scoreWithoutFeedback,
		ScoreWithFeedback:      bd.scoreWithFeedback,
	}
}

func sanitizeScoreForJSON(score float64) float64 {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return DefaultNeutralScore
	}
	return clip(score, 0, 1)
}

// ---------------------------------------------------------------------------
// Request builders
// ---------------------------------------------------------------------------

// buildChatRequest constructs a ChatRequest from the immutable request snapshot.
//
// The selected profile's think mode/tags ride the outgoing request as parse
// controls (ParseThinkMode/ParseThinkTags), and a ThinkNone profile clears
// the wire think options — a routed fallback to a non-thinking model must
// not receive controls negotiated for a thinking one (#220). The options
// copy is by value, which keeps rp.Request immutable: ModelOptions' only
// reference field is Stop []string, and we reassign scalar/pointer fields
// on the copy without ever mutating through them.
func (rp *RoutePlan) buildChatRequest(stream bool) ChatRequest {
	opts := rp.Request.Options
	var parseMode *ThinkMode
	var parseTags *ThinkTags
	if rp.Profile != nil {
		mode := rp.Profile.ThinkMode
		// A toggle-family profile with no caller think intent must parse as
		// ThinkAuto, not ThinkToggle: the wire request is untouched, so the
		// model may still emit inline think tags, and a toggle parser with
		// no activating signal runs INACTIVE (passthrough) — leaking raw
		// tags into Content. Auto-sniff is the safety net for the silent
		// default; toggle semantics apply only when the caller expressed
		// intent (Think set either way, or an effort hint) (#220).
		if mode == ThinkToggle && opts.Think == nil && opts.ThinkEffort == "" {
			mode = ThinkAuto
		}
		parseMode = &mode
		if rp.Profile.ThinkTags != nil {
			tags := *rp.Profile.ThinkTags
			parseTags = &tags
		}
		// ThinkNone is the ThinkMode zero value, but here it is always a
		// DECLARED value: every catalog family sets think_mode and
		// inferProfile defaults unknown models to ThinkAuto, so a
		// zero-value profile cannot reach a routed plan. The clear is
		// intentional policy, not a zero-value accident.
		if rp.Profile.ThinkMode == ThinkNone {
			opts.Think = nil
			opts.ThinkEffort = ""
		}
	}
	return ChatRequest{
		Model:          rp.Model,
		Messages:       rp.Request.Messages,
		Options:        opts,
		Tools:          rp.Request.Tools,
		Stream:         stream,
		ParseThinkMode: parseMode,
		ParseThinkTags: parseTags,
	}
}

// buildGenerateRequest constructs a GenerateRequest from the request snapshot.
func (rp *RoutePlan) buildGenerateRequest(stream bool) GenerateRequest {
	return GenerateRequest{
		Model:   rp.Model,
		Prompt:  rp.Request.Prompt,
		System:  rp.Request.System,
		Suffix:  rp.Request.Suffix,
		Options: rp.Request.Options,
		Stream:  stream,
	}
}

// buildEmbedRequest constructs an EmbedRequest from the request snapshot.
func (rp *RoutePlan) buildEmbedRequest() EmbedRequest {
	return EmbedRequest{
		Model: rp.Model,
		Input: rp.Request.Input,
	}
}

// ---------------------------------------------------------------------------
// Recorder helpers (nil-safe)
// ---------------------------------------------------------------------------

// admitFuncFor binds an AdmitFunc to one attempt's model key. Never
// returns nil; on an ungoverned plan the returned func admits with a
// no-op release, so AdmittedEmbedder providers take one uniform path.
// Failures are wrapped in admissionError so ExecuteEmbed can tell them
// apart from provider failures after the provider re-wraps them.
func (rp *RoutePlan) admitFuncFor(key ModelKey) AdmitFunc {
	return func(ctx context.Context) (func(), error) {
		release, err := rp.acquireFor(ctx, key)
		if err != nil {
			return nil, &admissionError{err: err}
		}
		return release, nil
	}
}

// acquireFor brackets one provider attempt with slot admission (#400).
// Returns a no-op release when the plan has no admission seam (Router
// without a slot source) — the ungoverned path allocates nothing and
// serializes nothing. On error no permit is held and no provider call
// may be made.
func (rp *RoutePlan) acquireFor(ctx context.Context, key ModelKey) (func(), error) {
	if rp.admission == nil {
		return noopRelease, nil
	}
	release, err := rp.admission.acquireSlot(ctx, key)
	if err != nil {
		return nil, err
	}
	if release == nil {
		release = noopRelease
	}
	return release, nil
}

func (rp *RoutePlan) recordSuccess(key ModelKey, latency LatencyInfo) {
	if rp.recorder != nil {
		rp.recorder.RecordSuccess(key, latency)
	}
}

func (rp *RoutePlan) recordFailure(key ModelKey, err error) {
	if rp.recorder != nil {
		rp.recorder.RecordFailure(key, err)
	}
}

func (rp *RoutePlan) recordWarmthUse(key ModelKey) {
	if rp.recorder != nil {
		rp.recorder.RecordWarmthUse(key)
	}
}

// slotUseRecorder is the optional RouteRecorder extension consumed by
// slot-capacity discovery (#399). RouteRecorder itself is unchanged so
// external recorders installed via SetRecorder keep compiling; the Router
// implements RecordSlotUse and is picked up here by type assertion.
type slotUseRecorder interface {
	RecordSlotUse(key ModelKey)
}

// recordSlotUse forwards a slot use signal when the recorder supports it.
// Called ONLY from the success branch of recordResult: warmth deliberately
// also fires on cancellation (the model is page-warm either way), but a
// slot probe on behalf of a cancelled request could make llama-swap load a
// model nobody is using.
func (rp *RoutePlan) recordSlotUse(key ModelKey) {
	if sr, ok := rp.recorder.(slotUseRecorder); ok {
		sr.RecordSlotUse(key)
	}
}

// ---------------------------------------------------------------------------
// hasVisibleContent
// ---------------------------------------------------------------------------

// hasVisibleContent reports whether a streaming chunk contains content
// that has been "delivered" to the user. Once any visible content has been
// emitted, provider fallback is no longer safe.
func hasVisibleContent(content, thinking string, toolCalls []ToolCall) bool {
	return content != "" || thinking != "" || len(toolCalls) > 0
}

// routeIDRand is the entropy source for newRouteID. It is a package-level
// variable so tests can substitute a failing reader and exercise the
// empty-string-on-error branch.
var routeIDRand io.Reader = rand.Reader

// newRouteIDWithWarn is the warning-emitting variant of newRouteID.
// state and logger may both be nil; nil disables the once-logged warning
// emission but preserves the empty-string-on-error fallback (so callers
// that don't carry a Router-owned warn state — tests, helpers — keep
// PR2's silent-empty-string behavior).
//
// Reuses the package-level routeIDRand io.Reader so tests can inject
// failure modes without going through a constructor seam.
func newRouteIDWithWarn(state *feedbackWarningState, logger feedbackLogger) string {
	var b [16]byte
	if _, err := io.ReadFull(routeIDRand, b[:]); err != nil {
		if state != nil {
			state.warnRouteIDRandOnce(logger, err)
		}
		return ""
	}
	return hex.EncodeToString(b[:])
}

// newRouteID returns a 16-byte random hex string (32 chars) suitable as an
// opaque correlation ID on RouteOutcome.RouteID. crypto/rand failures are
// silently coerced to an empty string — RouteID is informational; we do
// not want routing paths to fail because the OS RNG returned an error.
//
// **Test-only entry point.** The production path in `buildOutcome` calls
// `newRouteIDWithWarn(rp.feedbackWarn, rp.feedbackLogger)` so RNG failures
// fire the once-logged warning via the Router-owned `feedbackWarn`. This
// bare variant is equivalent to `newRouteIDWithWarn(nil, nil)` and exists
// only for tests and helpers that don't carry a Router-owned warn state;
// it does NOT emit warnings. Do NOT call from production code paths or
// you will lose visibility on the first RNG failure.
func newRouteID() string {
	return newRouteIDWithWarn(nil, nil)
}

// makeAttempt builds a RouteAttempt for one provider call.
//
//   - err == nil: Status=Succeeded, no ErrorClass, LatencyMs from duration.
//   - err != nil: Status and ErrorClass from classifyError(err); LatencyMs
//     from duration regardless (latency-to-failure is informative for
//     failure-mode pivots in PR3+).
//   - Negative duration clamps to LatencyMs=0.
func makeAttempt(key ModelKey, err error, duration time.Duration) RouteAttempt {
	class, status := classifyError(err)
	return RouteAttempt{
		Key:        key,
		Status:     status,
		LatencyMs:  clampMs(duration),
		ErrorClass: string(class),
	}
}

// makeUnknownAttempt builds a RouteAttempt with AttemptStatusUnknown — used
// by streaming Execute methods when a user-callback error aborts the stream
// before a Done chunk. The attribution must NOT classify as a provider
// failure (the provider may have been healthy), and Unknown signals are
// no-ops in RoutingFeedback. Shares clampMs with makeAttempt so negative
// monotonic-clock excursions can't produce negative LatencyMs.
func makeUnknownAttempt(key ModelKey, duration time.Duration) RouteAttempt {
	return RouteAttempt{
		Key:       key,
		Status:    AttemptStatusUnknown,
		LatencyMs: clampMs(duration),
	}
}

// clampMs converts a duration to milliseconds, clamping negative values to 0
// to guard against non-monotonic clock excursions.
func clampMs(duration time.Duration) int64 {
	ms := duration.Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

// feedbackWriteTimeout bounds how long recordOutcomeFeedback will wait for
// the store before giving up. Picked to be generous for an in-memory store
// and survivable for a SQLite store under contention. Without this bound a
// stuck store (locked WAL, frozen filesystem) would block every routed
// request indefinitely.
//
// Declared as a package-level var (not const) so tests can override it to
// exercise the timeout path without burning real wall-clock seconds.
var feedbackWriteTimeout = 1 * time.Second

// recordOutcomeFeedback delegates to RoutingFeedback.RecordOutcome when a
// feedback wrapper is configured AND the routing request has a non-empty
// UseCase. The seam is observational, never load-bearing for routing;
// errors do not bubble up to the caller. Uses a fresh context with a
// bounded timeout (not the request ctx) so cancellation of the caller's
// ctx does not cut short the feedback write, and so a slow/stuck store
// does not block the routing path indefinitely.
//
// On non-nil store error: emits a once-logged warning via feedbackWarn /
// feedbackLogger (set by buildPlan through setFeedbackTelemetry). Replaces
// PR2's silent-swallow; nil warn state preserves the silent fallback for
// plans constructed without telemetry wiring.
func (rp *RoutePlan) recordOutcomeFeedback(outcome *RouteOutcome) {
	if rp.feedback == nil || outcome == nil || rp.Request.UseCase == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedbackWriteTimeout)
	defer cancel()
	if err := rp.feedback.RecordOutcome(ctx, rp.Request.UseCase, *outcome); err != nil {
		if rp.feedbackWarn != nil {
			rp.feedbackWarn.warnFeedbackWriteOnce(rp.feedbackLogger, err)
		}
	}
}
