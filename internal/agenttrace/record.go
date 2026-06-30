package agenttrace

import (
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// SchemaVersion is bumped on any breaking change to the trace/telemetry records.
// v2 adds the runtime_stage span and pressure enrichment (#63).
const SchemaVersion = 2

// TraceMeta is the replay-relevant request context the caller supplies (the
// agent.Request is not embedded in agent.Result, and history is excluded from
// Result.Messages, so a trace needs it to reconstruct the model input).
type TraceMeta struct {
	Goal           string
	System         string
	HistorySummary string
	History        []provider.ChatMessage
	ToolSchemaHash string
	ModelHint      string
	MaxSteps       int
	Budget         agent.Budget
}

// TraceRequest is the serialized request side of a trace.
type TraceRequest struct {
	Goal           string                 `json:"goal"`
	System         string                 `json:"system"`
	HistorySummary string                 `json:"history_summary,omitempty"`
	History        []provider.ChatMessage `json:"history,omitempty"`
	ToolSchemaHash string                 `json:"tool_schema_hash"`
	ModelHint      string                 `json:"model_hint,omitempty"`
	MaxSteps       int                    `json:"max_steps"`
	Budget         agent.Budget           `json:"budget"`
}

// TraceRecord is one content-full run trace (#238). It embeds agent.Result; the
// embedded record uses Go's default JSON shape (e.g. Latency as integer
// nanoseconds) - the schema documents that rather than forking a trace-only copy.
type TraceRecord struct {
	SchemaVersion int          `json:"schema_version"`
	RunID         string       `json:"run_id"`
	StartedAt     string       `json:"started_at"`
	EndedAt       string       `json:"ended_at"`
	Status        string       `json:"status"`  // completed | error | canceled
	Partial       bool         `json:"partial"` // derived: status != completed
	Request       TraceRequest `json:"request"`
	Result        agent.Result `json:"result"`
	Error         string       `json:"error,omitempty"`
}

// --- telemetry spans (#239, content-light) ---

type usageLite struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Total      int `json:"total"`
}

type pressureLite struct {
	UsedPct     float64 `json:"used_pct"`
	Evicted     int     `json:"evicted"`
	Compactions int     `json:"compactions"`
	Level       string  `json:"level"`
	Cause       string  `json:"cause"`
	Mitigation  string  `json:"mitigation"`
	InputTokens int     `json:"input_tokens"`
	InputBudget int     `json:"input_budget"`
}

type runSpan struct {
	SchemaVersion    int     `json:"schema_version"`
	RunID            string  `json:"run_id"`
	SpanID           string  `json:"span_id"`
	Kind             string  `json:"kind"` // "run"
	StartedAt        string  `json:"started_at"`
	DurationMS       float64 `json:"duration_ms"`
	Steps            int     `json:"steps"`
	Status           string  `json:"status"`
	StopReason       string  `json:"stop_reason,omitempty"`
	MaxUsedPct       float64 `json:"max_used_pct"`
	MaxPressureLevel string  `json:"max_pressure_level"`
}

type modelStepSpan struct {
	SchemaVersion int          `json:"schema_version"`
	RunID         string       `json:"run_id"`
	SpanID        string       `json:"span_id"`
	ParentID      string       `json:"parent_id"`
	Kind          string       `json:"kind"` // "model_step"
	Step          int          `json:"step"`
	DurationMS    float64      `json:"duration_ms"`
	Model         string       `json:"model,omitempty"`
	PlannedModel  string       `json:"planned_model,omitempty"`
	FallbacksUsed int          `json:"fallbacks_used"`
	WasSticky     bool         `json:"was_sticky"`
	Usage         usageLite    `json:"usage"`
	Pressure      pressureLite `json:"pressure"`
}

type toolCallSpan struct {
	SchemaVersion int     `json:"schema_version"`
	RunID         string  `json:"run_id"`
	SpanID        string  `json:"span_id"`
	ParentID      string  `json:"parent_id"`
	Kind          string  `json:"kind"` // "tool_call"
	Step          int     `json:"step"`
	Name          string  `json:"name"`
	Effect        string  `json:"effect,omitempty"`
	Invoked       bool    `json:"invoked"`
	Denied        bool    `json:"denied"`
	IsError       bool    `json:"is_error"`
	Truncated     bool    `json:"truncated"`
	ContentBytes  int     `json:"content_bytes"`
	DurationMS    float64 `json:"duration_ms"`
}

// runtimeStageSpan is one content-light runtime-stage record (#63). Today the
// only emitted stage is "assemble" (the condense/compact boundary); future
// stages reuse this shape with a different Stage value.
type runtimeStageSpan struct {
	SchemaVersion int     `json:"schema_version"`
	RunID         string  `json:"run_id"`
	SpanID        string  `json:"span_id"`
	ParentID      string  `json:"parent_id"`
	Kind          string  `json:"kind"`
	Stage         string  `json:"stage"`
	Step          int     `json:"step"`
	Level         string  `json:"level"`
	Cause         string  `json:"cause"`
	Mitigation    string  `json:"mitigation"`
	Outcome       string  `json:"outcome"`
	UsedPct       float64 `json:"used_pct"`
	UsedPctDelta  float64 `json:"used_pct_delta"`
	InputTokens   int     `json:"input_tokens"`
	InputBudget   int     `json:"input_budget"`
	Evicted       int     `json:"evicted"`
	Compactions   int     `json:"compactions"`
}

// effectString renders an EffectClass bitset as a stable, content-light label.
// There is no agent.EffectClass.String(), so agenttrace owns this projection.
func effectString(c agent.EffectClass) string {
	var parts []string
	if c.Has(agent.Read) {
		parts = append(parts, "read")
	}
	if c.Has(agent.Write) {
		parts = append(parts, "write")
	}
	if c.Has(agent.Exec) {
		parts = append(parts, "exec")
	}
	if c.Has(agent.Network) {
		parts = append(parts, "network")
	}
	return strings.Join(parts, "|")
}

// ms converts a duration to fractional milliseconds for telemetry spans.
func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
