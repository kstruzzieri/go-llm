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
}

// Request is the unit of work handed to Run.
type Request struct {
	Goal     string
	System   string
	Tools    []Tool
	MaxSteps int // 0 => defaultMaxSteps
	Budget   Budget
	Approver Approver // nil => fail-safe (auto Read, deny Write/Exec)
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
}

// State is the canonical transcript the Orchestrator owns.
type State struct {
	System   string
	Messages []Message
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

// Pressure is the per-turn context-budget telemetry (#63 seam).
type Pressure struct {
	UsedPct     float64
	Evicted     int
	Compactions int
}

// StepRecord is the durable per-turn truth. RouteOutcome is captured
// separately because provider.Collect does not copy it.
type StepRecord struct {
	Index        int
	Response     provider.ChatResponse
	RouteOutcome *provider.RouteOutcome
	Pressure     Pressure
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
}

// Result is the canonical final state of a run.
type Result struct {
	Answer     string
	Steps      []StepRecord
	Events     []EventRecord
	Usage      provider.Usage
	ToolCalls  []ToolCallRecord
	StopReason StopReason
}
