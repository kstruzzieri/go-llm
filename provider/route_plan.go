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

	start := time.Now()
	resp, err := rp.Provider.Chat(ctx, req)
	attempts = append(attempts, makeAttempt(rp.Profile.Key, err, time.Since(start)))

	fallbacksUsed := 0
	if err != nil && IsInfrastructureError(err) {
		rp.recordFailure(rp.Profile.Key, err)

		for i, fb := range rp.Fallbacks {
			fbReq := fb.buildChatRequest(false)
			fbStart := time.Now()
			resp, err = fb.Provider.Chat(ctx, fbReq)
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

	return resp, err
}

// ---------------------------------------------------------------------------
// Execute — Chat stream
// ---------------------------------------------------------------------------

// ExecuteChatStream dispatches a streaming chat request to the selected
// provider. Fallback is only attempted before the first user-visible chunk
// has been delivered. Each provider call produces exactly one RouteAttempt,
// finalized either by the Done-chunk handler (when chunk.Partial == false)
// or by the post-stream code (using the real stream-method error). User-
// callback errors are captured separately and attributed as
// AttemptStatusUnknown.
func (rp *RoutePlan) ExecuteChatStream(ctx context.Context, fn func(ChatResponse) error) error {
	if rp.Budget.Decision == BudgetTruncate {
		return ErrBudgetAdaptationRequired
	}

	var attempts []RouteAttempt
	req := rp.buildChatRequest(true)

	delivered := false
	fallbacksUsed := 0
	var outcome *RouteOutcome

	primaryStart := time.Now()
	streamDone := false
	var callbackErr error

	wrappedFn := func(chunk ChatResponse) error {
		if !delivered && hasVisibleContent(chunk.Content, chunk.Thinking, chunk.ToolCalls) {
			delivered = true
		}
		if chunk.Done && !chunk.Partial && !streamDone {
			pendingAttempts := append(attempts,
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
			rp.recordResult(nil, fallbacksUsed, attempts, outcome)
			return nil
		}
		if e := fn(chunk); e != nil {
			callbackErr = e
			return e
		}
		return nil
	}

	err := rp.Provider.ChatStream(ctx, req, wrappedFn)

	if !streamDone {
		if callbackErr != nil {
			// User aborted via callback. Attribute as Unknown, not a
			// provider failure.
			attempts = append(attempts, RouteAttempt{
				Key:       rp.Profile.Key,
				Status:    AttemptStatusUnknown,
				LatencyMs: time.Since(primaryStart).Milliseconds(),
			})
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
				delivered = false
				fallbacksUsed = i + 1
				fbStart := time.Now()
				fbStreamDone := false
				var fbCallbackErr error

				wrappedFbFn := func(chunk ChatResponse) error {
					if !delivered && hasVisibleContent(chunk.Content, chunk.Thinking, chunk.ToolCalls) {
						delivered = true
					}
					if chunk.Done && !chunk.Partial && !fbStreamDone {
						pendingAttempts := append(attempts,
							makeAttempt(fb.Profile.Key, nil, time.Since(fbStart)))
						pendingOutcome := rp.buildOutcome(fallbacksUsed, pendingAttempts)
						chunk.RouteOutcome = pendingOutcome
						if e := fn(chunk); e != nil {
							fbCallbackErr = e
							return e
						}
						attempts = pendingAttempts
						fbStreamDone = true
						outcome = pendingOutcome
						rp.recordResult(nil, fallbacksUsed, attempts, outcome)
						return nil
					}
					if e := fn(chunk); e != nil {
						fbCallbackErr = e
						return e
					}
					return nil
				}

				err = fb.Provider.ChatStream(ctx, fbReq, wrappedFbFn)

				if !fbStreamDone {
					if fbCallbackErr != nil {
						attempts = append(attempts, RouteAttempt{
							Key:       fb.Profile.Key,
							Status:    AttemptStatusUnknown,
							LatencyMs: time.Since(fbStart).Milliseconds(),
						})
					} else {
						attempts = append(attempts,
							makeAttempt(fb.Profile.Key, err, time.Since(fbStart)))
					}
				}

				if err == nil {
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
	}

	return err
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

	start := time.Now()
	resp, err := rp.Provider.Generate(ctx, req)
	attempts = append(attempts, makeAttempt(rp.Profile.Key, err, time.Since(start)))

	fallbacksUsed := 0
	if err != nil && IsInfrastructureError(err) {
		rp.recordFailure(rp.Profile.Key, err)

		for i, fb := range rp.Fallbacks {
			fbReq := fb.buildGenerateRequest(false)
			fbStart := time.Now()
			resp, err = fb.Provider.Generate(ctx, fbReq)
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

	return resp, err
}

// ---------------------------------------------------------------------------
// Execute — Generate stream
// ---------------------------------------------------------------------------

// ExecuteGenerateStream dispatches a streaming generate request to the selected
// provider. Fallback is only attempted before the first user-visible chunk
// has been delivered. Each provider call produces exactly one RouteAttempt,
// finalized either by the Done-chunk handler (when chunk.Partial == false)
// or by the post-stream code (using the real stream-method error). User-
// callback errors are captured separately and attributed as
// AttemptStatusUnknown.
func (rp *RoutePlan) ExecuteGenerateStream(ctx context.Context, fn func(GenerateResponse) error) error {
	if rp.Budget.Decision == BudgetTruncate {
		return ErrBudgetAdaptationRequired
	}

	var attempts []RouteAttempt
	req := rp.buildGenerateRequest(true)

	delivered := false
	fallbacksUsed := 0
	var outcome *RouteOutcome

	primaryStart := time.Now()
	streamDone := false
	var callbackErr error

	wrappedFn := func(chunk GenerateResponse) error {
		if !delivered && chunk.Response != "" {
			delivered = true
		}
		if chunk.Done && !chunk.Partial && !streamDone {
			pendingAttempts := append(attempts,
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
			rp.recordResult(nil, fallbacksUsed, attempts, outcome)
			return nil
		}
		if e := fn(chunk); e != nil {
			callbackErr = e
			return e
		}
		return nil
	}

	err := rp.Provider.GenerateStream(ctx, req, wrappedFn)

	if !streamDone {
		if callbackErr != nil {
			// User aborted via callback. Attribute as Unknown, not a
			// provider failure.
			attempts = append(attempts, RouteAttempt{
				Key:       rp.Profile.Key,
				Status:    AttemptStatusUnknown,
				LatencyMs: time.Since(primaryStart).Milliseconds(),
			})
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
				delivered = false
				fallbacksUsed = i + 1
				fbStart := time.Now()
				fbStreamDone := false
				var fbCallbackErr error

				wrappedFbFn := func(chunk GenerateResponse) error {
					if !delivered && chunk.Response != "" {
						delivered = true
					}
					if chunk.Done && !chunk.Partial && !fbStreamDone {
						pendingAttempts := append(attempts,
							makeAttempt(fb.Profile.Key, nil, time.Since(fbStart)))
						pendingOutcome := rp.buildOutcome(fallbacksUsed, pendingAttempts)
						chunk.RouteOutcome = pendingOutcome
						if e := fn(chunk); e != nil {
							fbCallbackErr = e
							return e
						}
						attempts = pendingAttempts
						fbStreamDone = true
						outcome = pendingOutcome
						rp.recordResult(nil, fallbacksUsed, attempts, outcome)
						return nil
					}
					if e := fn(chunk); e != nil {
						fbCallbackErr = e
						return e
					}
					return nil
				}

				err = fb.Provider.GenerateStream(ctx, fbReq, wrappedFbFn)

				if !fbStreamDone {
					if fbCallbackErr != nil {
						attempts = append(attempts, RouteAttempt{
							Key:       fb.Profile.Key,
							Status:    AttemptStatusUnknown,
							LatencyMs: time.Since(fbStart).Milliseconds(),
						})
					} else {
						attempts = append(attempts,
							makeAttempt(fb.Profile.Key, err, time.Since(fbStart)))
					}
				}

				if err == nil {
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
	}

	return err
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

	start := time.Now()
	resp, err := rp.Provider.Embed(ctx, req)
	attempts = append(attempts, makeAttempt(rp.Profile.Key, err, time.Since(start)))

	fallbacksUsed := 0
	if err != nil && IsInfrastructureError(err) {
		rp.recordFailure(rp.Profile.Key, err)

		for i, fb := range rp.Fallbacks {
			fbReq := fb.buildEmbedRequest()
			fbStart := time.Now()
			resp, err = fb.Provider.Embed(ctx, fbReq)
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

	return resp, err
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
	rp.recordResult(err, fallbacksUsed, attempts, outcome)
	return outcome
}

func (rp *RoutePlan) recordResult(err error, fallbacksUsed int, attempts []RouteAttempt, outcome *RouteOutcome) {
	// Derive the most recently attempted key for warmth/success attribution.
	// attempts[last].Key is correct in every case Execute* produces: success,
	// infra failure, cancellation after a fallback was tried. The previous
	// fallbacksUsed-arithmetic mis-attributed warmth to the primary when a
	// fallback was attempted but never successfully selected.
	//
	// While the Execute* call sites still pass attempts=nil (Tasks 5-9 fill
	// these in), fall back to the legacy fallbacksUsed-based derivation so
	// existing tests and consumer behavior continue to observe RecordSuccess
	// against the actually-selected fallback.
	actualKey := rp.Profile.Key
	if len(attempts) > 0 {
		actualKey = attempts[len(attempts)-1].Key
	} else if fallbacksUsed > 0 && fallbacksUsed <= len(rp.Fallbacks) {
		actualKey = rp.Fallbacks[fallbacksUsed-1].Profile.Key
	}

	if err == nil {
		rp.recordSuccess(actualKey, LatencyInfo{})
		rp.recordWarmthUse(actualKey)
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		rp.recordWarmthUse(actualKey) // model IS warm even if caller bailed
	}
	// Infrastructure failures continue to be recorded inline by Execute methods.

	rp.recordOutcomeFeedback(outcome)
}

// buildOutcome constructs a RouteOutcome from the plan, fallback index, and
// per-attempt trace. ActualModel reflects the *selected* fallback (the model
// whose response is in the user's hand). Attempts records every provider
// call that happened, in order.
func (rp *RoutePlan) buildOutcome(fallbacksUsed int, attempts []RouteAttempt) *RouteOutcome {
	actualKey := rp.Profile.Key
	if fallbacksUsed > 0 && fallbacksUsed <= len(rp.Fallbacks) {
		actualKey = rp.Fallbacks[fallbacksUsed-1].Profile.Key
	}
	return &RouteOutcome{
		PlannedModel:  rp.Profile.Key,
		ActualModel:   actualKey,
		FallbacksUsed: fallbacksUsed,
		WasSticky:     rp.wasSticky,
		Score:         rp.Score,
		Reason:        rp.Reason,
		RouteID:       newRouteID(),
		Attempts:      attempts,
	}
}

// ---------------------------------------------------------------------------
// Request builders
// ---------------------------------------------------------------------------

// buildChatRequest constructs a ChatRequest from the immutable request snapshot.
func (rp *RoutePlan) buildChatRequest(stream bool) ChatRequest {
	return ChatRequest{
		Model:    rp.Model,
		Messages: rp.Request.Messages,
		Options:  rp.Request.Options,
		Tools:    rp.Request.Tools,
		Stream:   stream,
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

// newRouteID returns a 16-byte random hex string (32 chars) suitable as an
// opaque correlation ID on RouteOutcome.RouteID. crypto/rand failures are
// silently coerced to an empty string — RouteID is informational; we do
// not want routing paths to fail because the OS RNG returned an error.
func newRouteID() string {
	var b [16]byte
	if _, err := io.ReadFull(routeIDRand, b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
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
	ms := duration.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	return RouteAttempt{
		Key:        key,
		Status:     status,
		LatencyMs:  ms,
		ErrorClass: string(class),
	}
}

// recordOutcomeFeedback delegates to RoutingFeedback.RecordOutcome when a
// feedback wrapper is configured AND the routing request has a non-empty
// UseCase. Errors are swallowed — the seam is observational, never
// load-bearing for routing. Uses context.Background() so cancellation of
// the request's ctx does not cut short the feedback write.
func (rp *RoutePlan) recordOutcomeFeedback(outcome *RouteOutcome) {
	if rp.feedback == nil || outcome == nil || rp.Request.UseCase == "" {
		return
	}
	_ = rp.feedback.RecordOutcome(context.Background(), rp.Request.UseCase, *outcome)
}
