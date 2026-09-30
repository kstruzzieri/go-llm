package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/kstruzzieri/go-llm/provider"
)

// parallelToolCallLimit bounds concurrent read-only tool invocations within a
// single assistant turn.
const parallelToolCallLimit = 8 // ponytail: hardcoded MVP cap; make it configurable only if real read fan-out queues.

// parallelSafe reports whether a tool's normalized static effect is exactly
// read-only and needs no approval — the conservative set safe to run concurrently.
func parallelSafe(e Effect) bool {
	e = normalizeEffect(e)
	return e.Class == Read && !needsApproval(e.Approval, e.Class)
}

// canRunParallel reports whether EVERY call in the batch may run concurrently. It
// is a cheap, static, side-effect-free pre-check (no ctx, observer, or Plan). Any
// miss routes the whole batch to the serial path. PlanningTools are excluded so we
// never run Plan twice/concurrently; duplicate tool names are excluded to avoid a
// same-instance Invoke race and to keep governor repeat-detection on the serial path.
func canRunParallel(reg *toolRegistry, calls []provider.ToolCall) bool {
	seen := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		name := call.Function.Name
		if _, dup := seen[name]; dup {
			return false
		}
		seen[name] = struct{}{}
		tool, ok := reg.lookup(name)
		if !ok {
			return false
		}
		if !json.Valid(call.Function.Arguments) {
			return false
		}
		if _, isPlanning := tool.(PlanningTool); isPlanning {
			return false
		}
		if !parallelSafe(tool.Effect()) {
			return false
		}
	}
	return true
}

// runToolCallsParallel dispatches a batch that canRunParallel has vetted as all
// read-only and independent. Three phases keep all observer/governor work on the
// main goroutine in model order; only Invoke runs concurrently:
//
//	Phase 1 (serial): prepareCall per call -> OnToolCall, in model order.
//	Phase 2 (parallel): invokeCall per call via a bounded errgroup. Workers never
//	  fail the group, so the group ctx cancels only when the PARENT ctx cancels.
//	Phase 3 (serial): record, OnToolResult, append observation, governor, in model
//	  order. A short-circuit discards trailing content while retaining audit
//	  metadata for every synthetic or invoked outcome already completed.
func (o *Orchestrator) runToolCallsParallel(ctx context.Context, res *Result, state *State,
	reg *toolRegistry, calls []provider.ToolCall, approver Approver, obs Observer, step int,
	gov *restraintGovernor, b *batch, ic *interceptorRun) error {

	// Phase 1: prepare serially, in model order.
	eventStart := len(res.Events)
	prepared := make([]preparedCall, len(calls))
	for i, call := range calls {
		res.Events = append(res.Events, EventRecord{Step: step, Kind: "tool_call"})
		p, err := o.prepareCall(ctx, reg, call, approver, obs, step, gov, ic)
		if err != nil {
			if errors.Is(err, errRunBudgetExhausted) {
				// No workers started; only prior synthetic outcomes are audited.
				retainToolCallEvents(res, eventStart, func(j int) bool { return prepared[j].result != nil })
			}
			for _, earlier := range prepared[:i] {
				if earlier.result != nil {
					appendToolCallRecord(res, step, earlier.rec, earlier.result.RouteOutcome)
				}
			}
			return err // hard abort: no invokes launched, nothing to cancel
		}
		prepared[i] = p
	}

	// Phase 2: invoke concurrently, bounded; time each invoke in isolation. A
	// prepared call carrying a synthetic result (#436 interceptor block) is
	// never invoked: its result is copied through. Invocation uses the
	// canonical prepared call, never the model's slice.
	results := make([]ToolResult, len(prepared))
	latencies := make([]time.Duration, len(prepared))
	invoked := make([]bool, len(prepared))
	invokeErrors := make([]error, len(prepared))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(parallelToolCallLimit)
	for i := range prepared {
		if prepared[i].result != nil {
			results[i] = *prepared[i].result
			continue
		}
		g.Go(func() error {
			start := o.now()
			results[i], invokeErrors[i] = o.invokeCall(gctx, prepared[i].tool, prepared[i].effect, prepared[i].call.Function.Arguments)
			invoked[i] = invokeErrors[i] == nil
			latencies[i] = o.now().Sub(start)
			return nil // never fail the group; tool errors are model-visible results
		})
	}
	_ = g.Wait() // joins every worker -> no goroutine leak
	// Prune budget refusals before any cancellation or observer-error exit.
	retainToolCallEvents(res, eventStart, func(i int) bool {
		return !errors.Is(invokeErrors[i], errRunBudgetExhausted)
	})
	if err := ctx.Err(); err != nil {
		appendTrailingToolCallRecords(res, step, prepared, results, latencies, invoked, 0)
		return err // parent cancelled -> hard abort
	}

	// Phase 3: observe serially, in model order, via the shared recordResult tail
	// so governor/observer semantics are identical to the serial path.
	for i := range prepared {
		if invokeErrors[i] != nil {
			continue // no invocation or observation; shared batch tail handles the stop
		}
		rec := prepared[i].rec
		rec.IsError = results[i].IsError
		if invoked[i] {
			rec.Invoked = true
			rec.Latency = latencies[i]
		}
		stop, err := o.recordResult(ctx, res, state, obs, gov, step, prepared[i].call, prepared[i].effect, rec, results[i], prepared[i].inspectResult, b, ic)
		if err != nil {
			appendTrailingToolCallRecords(res, step, prepared, results, latencies, invoked, i+1)
			return err
		}
		b.completed = append(b.completed, i)
		if stop {
			appendTrailingToolCallRecords(res, step, prepared, results, latencies, invoked, i+1)
			return nil // governor tripped: discard later already-computed read-only results
		}
	}
	return nil
}

// retainToolCallEvents filters the current batch's contiguous call events before
// any results are appended. Earlier events and retained call order stay intact.
func retainToolCallEvents(res *Result, start int, keep func(int) bool) {
	events := res.Events[start:]
	res.Events = res.Events[:start]
	for i, event := range events {
		if keep(i) {
			res.Events = append(res.Events, event)
		}
	}
}

func appendTrailingToolCallRecords(res *Result, step int, prepared []preparedCall, results []ToolResult,
	latencies []time.Duration, invoked []bool, start int) {

	for i := start; i < len(prepared); i++ {
		if !invoked[i] && prepared[i].result == nil {
			continue
		}
		rec := prepared[i].rec
		rec.IsError = results[i].IsError
		if invoked[i] {
			rec.Invoked = true
			rec.Latency = latencies[i]
		}
		appendToolCallRecord(res, step, rec, results[i].RouteOutcome)
	}
}
