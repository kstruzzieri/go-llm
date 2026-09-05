package agenttrace

import (
	"encoding/json"
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
// System is the caller's application prompt: agent.Run appends
// agent.ToolTrustContract and interceptor addenda, and frames every tool
// observation under a per-render key (#430), so replaying through Run
// reproduces the effective prompt and framing rather than reading them here.
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

// TraceRecord is one content-full run trace (#238). It embeds agent.Result but
// does not serialize structured ContextAssemblyTrace rows or row fields; those
// are available live to ContextAssemblyObserver. Its model-visible messages may
// independently contain tool-call, source, or record identifiers. The embedded
// record uses Go's default JSON shape (e.g. Latency as integer nanoseconds) -
// the schema documents that rather than forking a trace-only copy.
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
	// Grounding is the #348 grounding-verification report, embedded verbatim as
	// a JSON object. Raw rather than typed so agenttrace keeps no dependency on
	// the analysis package for a payload it only stores. Additive within
	// SchemaVersion 2 (same rule as the spans below): omitempty leaves every
	// pre-#348 trace byte-identical.
	Grounding json.RawMessage `json:"grounding,omitempty"`
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
	// Additive within SchemaVersion 2: omitempty keeps every span a legacy or
	// lossless mixed turn emits byte-identical to what v2 already emitted.
	AnchorOmissions int    `json:"anchor_omissions,omitempty"`
	Level           string `json:"level"`
	Cause           string `json:"cause"`
	Mitigation      string `json:"mitigation"`
	InputTokens     int    `json:"input_tokens"`
	InputBudget     int    `json:"input_budget"`
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
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	SpanID        string `json:"span_id"`
	ParentID      string `json:"parent_id"`
	Kind          string `json:"kind"` // "tool_call"
	Step          int    `json:"step"`
	Name          string `json:"name"`
	Effect        string `json:"effect,omitempty"`
	Invoked       bool   `json:"invoked"`
	Denied        bool   `json:"denied"`
	// AutoApproved records that the approval decision came from a session
	// grant (#341). Additive within SchemaVersion 2 (same rule as
	// pressureLite's AnchorOmissions): omitempty keeps every pre-#341 span
	// byte-identical.
	AutoApproved bool    `json:"auto_approved,omitempty"`
	IsError      bool    `json:"is_error"`
	Truncated    bool    `json:"truncated"`
	ContentBytes int     `json:"content_bytes"`
	DurationMS   float64 `json:"duration_ms"`
	// Delegated* record the model a delegating tool (e.g. delegate_code) routed
	// to. Identity + fallback count only; omitempty so non-delegated spans are
	// byte-identical to before.
	DelegatedModel         string `json:"delegated_model,omitempty"`
	DelegatedPlannedModel  string `json:"delegated_planned_model,omitempty"`
	DelegatedFallbacksUsed int    `json:"delegated_fallbacks_used,omitempty"`
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
	// Additive within SchemaVersion 2: see pressureLite.AnchorOmissions.
	AnchorOmissions int `json:"anchor_omissions,omitempty"`
}

// contextAssemblySpan is one mixed-assembly outcome (#331). A NEW span kind, so
// it is additive within SchemaVersion 2 by construction: no record any existing
// run emits changes a byte, and only a mixed assembly emits this one at all.
//
// It is AGGREGATE on purpose. ContextAssemblyTrace is content-free but not
// privacy-free — its rows carry source paths, memory record IDs and tool call
// IDs — and this sink persists no such identifier today (tool spans carry the
// tool NAME and a content byte count, never arguments, output or call IDs).
// Emitting the rows would widen what telemetry retains about a workspace, so the
// span keeps the counts and drops the names: ByDecision says whether the upgrade
// pass is doing anything, ByOmissionReason is what makes Pressure.AnchorOmissions
// actionable (byte_cap vs token_budget vs chain_evicted), and neither can
// identify a file. Telemetry retains only these aggregates; -trace does not
// serialize structured ContextAssemblyTrace rows or row fields, though its
// model-visible messages can independently contain the same identifiers. The
// rows themselves are available only to the live ContextAssemblyObserver.
type contextAssemblySpan struct {
	SchemaVersion      int    `json:"schema_version"`
	RunID              string `json:"run_id"`
	SpanID             string `json:"span_id"`
	ParentID           string `json:"parent_id"`
	Kind               string `json:"kind"` // "context_assembly"
	Step               int    `json:"step"`
	MaxTokens          int    `json:"max_tokens"`
	UsedTokens         int    `json:"used_tokens"`
	FreeTokens         int    `json:"free_tokens"`
	Subjects           int    `json:"subjects"`
	Rendered           int    `json:"rendered"`
	Omitted            int    `json:"omitted"`
	VerbatimShortfalls int    `json:"verbatim_shortfalls"`
	RenderedBytes      int    `json:"rendered_bytes"`
	// Keyed by agent's fixed Decision/Omit vocabulary, so the keys are a closed
	// set and never payload-derived. Omitted when empty rather than written as
	// null, which keeps a zero-subject assembly's span readable.
	ByDecision       map[string]int `json:"by_decision,omitempty"`
	ByOmissionReason map[string]int `json:"by_omission_reason,omitempty"`
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
