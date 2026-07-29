package agent

import (
	"errors"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// Tunable defaults. All are overridable via Request fields where exposed.
const (
	defaultMaxSteps     = 16
	defaultToolTimeout  = 30 * time.Second
	defaultOutputCap    = 64 * 1024 // bytes of tool output fed back to the model
	defaultToolErrorCap = 3         // consecutive tool errors / repeats before stop
)

// ErrContextExhausted is returned when the pinned segment (system + goal +
// tool schemas) alone exceeds the input budget. The runtime never silently
// truncates the goal; selecting a bigger-context model is the provider's job.
var ErrContextExhausted = errors.New("agent: context exhausted (pinned segment exceeds budget)")

// StopReason explains why Run returned. A non-Completed reason is still a
// successful (non-error) return; the caller inspects Result.
type StopReason int

const (
	Completed StopReason = iota
	StepCapReached
	BudgetReached
	ToolErrorCapReached
	RepeatLimitReached
)

func (s StopReason) String() string {
	switch s {
	case Completed:
		return "completed"
	case StepCapReached:
		return "step_cap_reached"
	case BudgetReached:
		return "budget_reached"
	case ToolErrorCapReached:
		return "tool_error_cap_reached"
	case RepeatLimitReached:
		return "repeat_limit_reached"
	default:
		return "unknown"
	}
}

// Budget holds run-level token caps the consumer sets. InputCeiling is the
// authoritative per-turn input ceiling (the concrete model is unknown until the
// router selects one). Zero falls back to a conservative default.
type Budget struct {
	InputCeiling int
	// OutputReserve reserves room for the model's answer. It is forwarded to the
	// model request as Options.NumPredict when > 0 (capping generation) and is
	// also subtracted from the per-turn input ceiling during assembly.
	OutputReserve int
	TotalTokens   int // 0 = unbounded whole-run cap
	// Pressure tunes the warn/watch/critical bands classified during assembly.
	// The zero value normalizes to conservative defaults. #63.
	Pressure PressureThresholds
}

// Request is the unit of work handed to Run.
type Request struct {
	Goal           string
	System         string
	HistorySummary string
	History        []provider.ChatMessage // prior non-system turns; runtime marks every entry Elastic
	Tools          []Tool
	MaxSteps       int // 0 => defaultMaxSteps
	Budget         Budget
	Approver       Approver // nil => fail-safe (auto Read, deny Write/Exec)
	// Options carries per-run model options (think controls, temperature,
	// ...) applied to every model call in the run. Zero value preserves
	// prior behavior. Budget.OutputReserve still overrides NumPredict;
	// when OutputReserve is zero, a directly-set NumPredict also seeds the
	// router's ExpectedOutput hint.
	Options provider.ModelOptions
}

// Segment tags a message as always-present (Pinned) or compactable (Elastic).
type Segment int

const (
	Elastic Segment = iota
	Pinned
)

// Message wraps a provider chat message with runtime-only metadata.
type Message struct {
	provider.ChatMessage
	Segment Segment
	Attrib  *RetrievalAttribution
	// Context and OutputCap are runtime-only assembly metadata (like Segment):
	// excluded from JSON, never provider-visible, never persisted. Context is
	// the deep-copied structured payload of a tool-result anchor; OutputCap is
	// the normalized Effect.OutputCap the anchor was dispatched under (mixed
	// assembly enforces the same byte boundary capOutput enforces on fallback
	// Content).
	Context   *ContextSet `json:"-"`
	OutputCap int         `json:"-"`
}

// State is the canonical transcript the Orchestrator owns.
type State struct {
	System         string
	DurableSummary string
	Messages       []Message
}

// RetrievalAttribution credits the sources a retrieval-style tool returned.
type RetrievalAttribution struct {
	Sources []RetrievedSource
}

// RetrievedSource identifies one retrieved chunk for downstream feedback.
type RetrievedSource struct {
	StableKey string
	Source    string
	StartLine int
	EndLine   int
	Score     float64
}

// Pressure is the per-turn context-budget telemetry (#63 seam). UsedPct,
// Evicted, Compactions are the original v1 fields; the rest enrich them.
type Pressure struct {
	UsedPct     float64
	Evicted     int
	Compactions int
	InputTokens int
	InputBudget int
	Level       PressureLevel
	Cause       PressureCause
	Mitigation  PressureMitigation
}

// StepRecord is the durable per-turn truth. RouteOutcome is captured
// separately because provider.Collect does not copy it.
type StepRecord struct {
	Index        int
	Response     provider.ChatResponse
	RouteOutcome *provider.RouteOutcome
	Pressure     Pressure
	Latency      time.Duration // wall time of the ModelCaller.Chat call for this step
}

// EventRecord is a lightweight ordered log entry for replay/eval.
type EventRecord struct {
	Step int
	Kind string // "token" | "step" | "tool_call" | "tool_result" | "compaction" | "stop"
}

// ToolCallRecord captures one dispatched tool call and its outcome.
type ToolCallRecord struct {
	Step    int
	Name    string
	IsError bool
	Denied  bool
	Invoked bool          // false for synthetic pre-invoke outcomes (no Invoke ran)
	Latency time.Duration // wall time of Invoke only; zero when !Invoked
	// RouteOutcome names the model a delegating tool routed to; nil for ordinary
	// tools. Omitted from marshaled records when nil, so non-delegated run
	// traces are byte-identical to before.
	RouteOutcome *provider.RouteOutcome `json:"RouteOutcome,omitempty"`
}

// Result is the canonical final state of a run.
type Result struct {
	Answer     string
	Messages   []provider.ChatMessage
	Steps      []StepRecord
	Events     []EventRecord
	Usage      provider.Usage
	ToolCalls  []ToolCallRecord
	StopReason StopReason
}
