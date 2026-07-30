package agenttrace

import (
	"context"
	"fmt"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

var (
	_ agent.Observer                = (*TelemetrySink)(nil)
	_ agent.ToolResultObserver      = (*TelemetrySink)(nil)
	_ agent.PressureObserver        = (*TelemetrySink)(nil)
	_ agent.ContextAssemblyObserver = (*TelemetrySink)(nil)
)

// TelemetrySink is a content-light agent.Observer (+ ToolResultObserver +
// PressureObserver). It emits a model_step span per OnStep, a tool_call span per
// OnToolResult, a runtime_stage span per OnPressure, then a root run span on
// Finish (callbacks do not know final status/duration). It is best-effort:
// write/encode errors are retained on the sink and every callback returns nil so
// telemetry never aborts a run or changes observer semantics.
//
// It serializes only content-light fields: identity, counts, sizes, durations,
// and the denied/error/truncated/invoked bools. It never reads or writes prompt,
// assistant, tool-argument, tool-output, or raw-error text.
type TelemetrySink struct {
	w       *jsonlWriter
	runID   string
	started time.Time
	now     func() time.Time

	lastErr  error
	lastStep int
	toolSeq  int

	prevUsedPct float64
	havePrev    bool
	maxUsedPct  float64
	maxLevel    agent.PressureLevel
}

// NewTelemetrySink opens path for append and returns a sink for one run. started
// is the run's start instant (for the run-span duration); now supplies the end.
func NewTelemetrySink(path, runID string, started time.Time, now func() time.Time) (*TelemetrySink, error) {
	w, err := openJSONL(path)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &TelemetrySink{w: w, runID: runID, started: started, now: now, lastStep: -1}, nil
}

func (s *TelemetrySink) record(v any) {
	if err := s.w.write(v); err != nil {
		s.lastErr = err
	}
}

func (s *TelemetrySink) OnStep(_ context.Context, e agent.StepEvent) error {
	span := modelStepSpan{
		SchemaVersion: SchemaVersion,
		RunID:         s.runID,
		SpanID:        fmt.Sprintf("%s-step-%d", s.runID, e.Index),
		ParentID:      s.runID + "-run",
		Kind:          "model_step",
		Step:          e.Index,
		DurationMS:    ms(e.Latency),
		Usage: usageLite{
			Prompt:     e.Response.Usage.PromptTokens,
			Completion: e.Response.Usage.CompletionTokens,
			Total:      e.Response.Usage.TotalTokens,
		},
		Pressure: pressureLite{
			UsedPct:         e.Pressure.UsedPct,
			Evicted:         e.Pressure.Evicted,
			Compactions:     e.Pressure.Compactions,
			AnchorOmissions: e.Pressure.AnchorOmissions,
			Level:           e.Pressure.Level.String(),
			Cause:           e.Pressure.Cause.String(),
			Mitigation:      e.Pressure.Mitigation.String(),
			InputTokens:     e.Pressure.InputTokens,
			InputBudget:     e.Pressure.InputBudget,
		},
	}
	if e.RouteOutcome != nil {
		span.Model = e.RouteOutcome.ActualModel.String()
		span.PlannedModel = e.RouteOutcome.PlannedModel.String()
		span.FallbacksUsed = e.RouteOutcome.FallbacksUsed
		span.WasSticky = e.RouteOutcome.WasSticky
	}
	s.record(span)
	return nil
}

func (s *TelemetrySink) OnToolResult(_ context.Context, e agent.ToolResultEvent) error {
	if e.Step != s.lastStep {
		s.lastStep, s.toolSeq = e.Step, 0
	}
	span := toolCallSpan{
		SchemaVersion: SchemaVersion,
		RunID:         s.runID,
		SpanID:        fmt.Sprintf("%s-tool-%d-%d", s.runID, e.Step, s.toolSeq),
		ParentID:      fmt.Sprintf("%s-step-%d", s.runID, e.Step),
		Kind:          "tool_call",
		Step:          e.Step,
		Name:          e.Call.Function.Name,
		Effect:        effectString(e.Effect.Class),
		Invoked:       e.Invoked,
		Denied:        e.Denied,
		IsError:       e.Result.IsError,
		Truncated:     e.Result.Truncated,
		ContentBytes:  len(e.Result.Content),
		DurationMS:    ms(e.Latency),
	}
	if ro := e.Result.RouteOutcome; ro != nil {
		span.DelegatedModel = ro.ActualModel.String()
		span.DelegatedPlannedModel = ro.PlannedModel.String()
		span.DelegatedFallbacksUsed = ro.FallbacksUsed
	}
	s.toolSeq++
	s.record(span)
	return nil
}

// OnPressure writes a content-light runtime_stage span for the assembly stage and
// updates the per-run pressure aggregates. Best-effort: write errors are retained
// and it returns nil so telemetry never aborts a run.
func (s *TelemetrySink) OnPressure(_ context.Context, e agent.PressureEvent) error {
	p := e.Pressure
	delta := 0.0
	if s.havePrev {
		delta = p.UsedPct - s.prevUsedPct
	}
	s.prevUsedPct, s.havePrev = p.UsedPct, true
	if p.UsedPct > s.maxUsedPct {
		s.maxUsedPct = p.UsedPct
	}
	if p.Level > s.maxLevel {
		s.maxLevel = p.Level
	}
	outcome := "ok"
	if p.Mitigation == agent.MitigationHalt {
		outcome = "exhausted"
	}
	s.record(runtimeStageSpan{
		SchemaVersion:   SchemaVersion,
		RunID:           s.runID,
		SpanID:          fmt.Sprintf("%s-stage-assemble-%d", s.runID, e.Step),
		ParentID:        s.runID + "-run",
		Kind:            "runtime_stage",
		Stage:           "assemble",
		Step:            e.Step,
		Level:           p.Level.String(),
		Cause:           p.Cause.String(),
		Mitigation:      p.Mitigation.String(),
		Outcome:         outcome,
		UsedPct:         p.UsedPct,
		UsedPctDelta:    delta,
		InputTokens:     p.InputTokens,
		InputBudget:     p.InputBudget,
		Evicted:         p.Evicted,
		Compactions:     p.Compactions,
		AnchorOmissions: p.AnchorOmissions,
	})
	return nil
}

// OnContextAssembly writes one aggregate context_assembly span per MIXED
// assembly. Legacy and no-anchor turns never fire the callback, so a run that
// never enables mixed assembly emits a byte-identical telemetry file to before.
//
// The per-subject rows are deliberately reduced to counts here; see
// contextAssemblySpan for why. Best-effort like every other callback: write
// errors are retained and nil is returned so telemetry never aborts a run.
func (s *TelemetrySink) OnContextAssembly(_ context.Context, e agent.ContextAssemblyEvent) error {
	tr := e.Trace
	span := contextAssemblySpan{
		SchemaVersion:      SchemaVersion,
		RunID:              s.runID,
		SpanID:             fmt.Sprintf("%s-stage-context-%d", s.runID, e.Step),
		ParentID:           s.runID + "-run",
		Kind:               "context_assembly",
		Step:               e.Step,
		MaxTokens:          tr.MaxTokens,
		UsedTokens:         tr.EstimatedTokensUsed,
		FreeTokens:         tr.EstimatedTokensFree,
		Subjects:           tr.SelectedSubjects,
		Rendered:           tr.RenderedSubjects,
		Omitted:            tr.OmittedSubjects,
		VerbatimShortfalls: tr.VerbatimShortfalls,
	}
	for _, sub := range tr.Subjects {
		if sub.Omitted {
			if span.ByOmissionReason == nil {
				span.ByOmissionReason = map[string]int{}
			}
			span.ByOmissionReason[sub.OmissionReason]++
			continue
		}
		span.RenderedBytes += sub.Bytes
		if span.ByDecision == nil {
			span.ByDecision = map[string]int{}
		}
		span.ByDecision[sub.Decision]++
	}
	s.record(span)
	return nil
}

// OnToolCall is a no-op: tool spans are emitted from OnToolResult, which carries
// effect/latency/invoked even for denied calls (where OnToolCall never fires).
func (s *TelemetrySink) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }

// OnToken is a no-op: token deltas carry assistant content and are not content-light.
func (s *TelemetrySink) OnToken(context.Context, agent.TokenEvent) error { return nil }

// Finish emits the root run span. Call after Run returns; status is computed by
// the caller. Returns the first retained write error, if any (advisory only).
func (s *TelemetrySink) Finish(res agent.Result, status string) error {
	stopReason := ""
	if status == "completed" {
		stopReason = res.StopReason.String()
	}
	s.record(runSpan{
		SchemaVersion:    SchemaVersion,
		RunID:            s.runID,
		SpanID:           s.runID + "-run",
		Kind:             "run",
		StartedAt:        s.started.UTC().Format(time.RFC3339Nano),
		DurationMS:       ms(s.now().Sub(s.started)),
		Steps:            len(res.Steps),
		Status:           status,
		StopReason:       stopReason,
		MaxUsedPct:       s.maxUsedPct,
		MaxPressureLevel: s.maxLevel.String(),
	})
	return s.lastErr
}

// Close releases the underlying file.
func (s *TelemetrySink) Close() error { return s.w.Close() }
