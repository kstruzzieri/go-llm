// Package opsview is the pure projection behind golem ops. Build performs no
// I/O and reads no clocks. The JSON field names on these types are the v1
// wire contract; the golden test pins them, and changing one is a contract
// break that needs a v2.
package opsview

import "time"

// Mode is how the snapshot was produced.
type Mode string

// Modes.
const (
	ModeOnce  Mode = "once"
	ModeWatch Mode = "watch"
	ModeServe Mode = "serve"
)

// Snapshot is the v1 document.
type Snapshot struct {
	Version         int           `json:"version"`
	Mode            Mode          `json:"mode"`
	IntervalMs      int64         `json:"interval_ms"`
	GeneratedAt     string        `json:"generated_at"`
	TimingUncertain bool          `json:"timing_uncertain"`
	Config          ConfigView    `json:"config"`
	Backends        []Backend     `json:"backends"`
	Models          []Model       `json:"models"`
	Attention       []Attention   `json:"attention"`
	GeneratedMono   time.Duration `json:"-"` // renderers advance ages from here
}

// ConfigView carries the configuration origin and problems, never paths.
type ConfigView struct {
	Source string `json:"source"`
	// Ready is configview's flag, which it sets before chain validation, so
	// it can read true alongside diagnostics. Judge problems by Diagnostics.
	Ready       bool         `json:"ready"`
	Revision    *string      `json:"revision"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Diagnostic mirrors configview's fixed-code diagnostics.
type Diagnostic struct {
	Code    string `json:"code"`
	Subject string `json:"subject"`
}

// Envelope is the provenance every observed value carries.
type Envelope struct {
	Source     string  `json:"source"`
	ObservedAt *string `json:"observed_at"`
	Stale      bool    `json:"stale"`
	AgeMs      *int64  `json:"age_ms"`
}

// Backend is one configured provider.
type Backend struct {
	ID           string        `json:"id"`
	Endpoint     *string       `json:"endpoint"`
	Hosting      string        `json:"hosting"`
	Runtime      Runtime       `json:"runtime"`
	Reachability Reachability  `json:"reachability"`
	Observation  Observation   `json:"observation"`
	Surfaces     []SurfaceView `json:"surfaces"`
}

// Runtime is what the backend was identified as.
type Runtime struct {
	Kind    string  `json:"kind"`
	Version *string `json:"version"`
	Support string  `json:"support"`
}

// Reachability is whether the backend answered. A failure carries the time
// until the next scheduled retry, except in once mode, which exits before it.
type Reachability struct {
	State     string  `json:"state"`
	Code      *string `json:"code"`
	RetryInMs *int64  `json:"retry_in_ms"`
	Envelope
}

// Observation summarizes observation continuity.
type Observation struct {
	Since               *string `json:"since"`
	Periods             int     `json:"periods"`
	Gaps                int     `json:"gaps"`
	Refused             int64   `json:"refused"`
	ModelsOverflow      int     `json:"models_overflow"`
	BackendOnlyOverflow int     `json:"backend_only_overflow"`
}

// SurfaceView is one endpoint's last outcome.
type SurfaceView struct {
	Name        string  `json:"name"`
	LastSuccess *string `json:"last_success"`
	LastError   *string `json:"last_error"`
}

// Model is one configured or backend-only model.
type Model struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	BackendModel string    `json:"backend_model"`
	Configured   bool      `json:"configured"`
	UsedBy       []UsedBy  `json:"used_by"`
	Residency    Residency `json:"residency"`
	Activity     Activity  `json:"activity"`
	Loads        *Loads    `json:"loads,omitempty"` // omitted in once mode and for unobserved backends
	Stats        []Stats   `json:"stats"`
}

// UsedBy is one use case whose chain includes the model.
type UsedBy struct {
	UseCase    string `json:"use_case"`
	Role       string `json:"role"`
	IsFallback bool   `json:"is_fallback"`
}

// Residency is whether the model is in memory.
type Residency struct {
	State         string  `json:"state"`
	Reason        *string `json:"reason"`
	LastState     *string `json:"last_state"`
	FirstObserved *string `json:"first_observed"`
	ExpiresAt     *string `json:"expires_at"`
	Envelope
}

// Activity is always unknown or not observed in Phases 0-1.
type Activity struct {
	State           string  `json:"state"`
	Reason          *string `json:"reason"`
	LastCompletedAt *string `json:"last_completed_at"`
	Envelope
}

// Loads is observed load history (watch and serve only).
type Loads struct {
	Count       int              `json:"count"`
	Since       *string          `json:"since"`
	Gaps        int              `json:"gaps"`
	SampleMs    int64            `json:"sample_ms"`
	Complete    bool             `json:"complete"`
	Transitions []TransitionView `json:"transitions"`
}

// TransitionView is one observed residency change.
type TransitionView struct {
	To string `json:"to"`
	At string `json:"at"`
}

// Stats are one window of retained backend history.
type Stats struct {
	Window string `json:"window"`
	Scope  string `json:"scope"`
	Envelope
	Coverage              Coverage `json:"coverage"`
	Calls                 *int     `json:"calls"`
	Errors                *int     `json:"errors"`
	PromptTokensProcessed TokenSum `json:"prompt_tokens_processed"`
	PromptTokensUsage     TokenSum `json:"prompt_tokens_usage"`
	OutputTokens          TokenSum `json:"output_tokens"`
	PrefillTPS            Dist     `json:"prefill_tps"`
	DecodeTPS             Dist     `json:"decode_tps"`
	DurationMs            Dist     `json:"duration_ms"`
}

// Coverage says how much of a window the retained history can speak for.
type Coverage struct {
	RetainedRows     int     `json:"retained_rows"`
	OldestRetainedAt *string `json:"oldest_retained_at"`
	NewestRetainedAt *string `json:"newest_retained_at"`
	Evicted          bool    `json:"evicted"`
	Contiguous       bool    `json:"contiguous"`
	State            string  `json:"state"`
	Reason           string  `json:"reason"`
}

// TokenSum is a sum over available values only; a null sum carries a reason.
type TokenSum struct {
	Sum         *int64  `json:"sum"`
	N           int     `json:"n"`
	Unavailable int     `json:"unavailable"`
	Reason      *string `json:"reason"`
}

// Dist is a nearest-rank distribution summary; null percentiles carry a reason.
type Dist struct {
	P50         *float64 `json:"p50"`
	P95         *float64 `json:"p95"`
	N           int      `json:"n"`
	Provisional bool     `json:"provisional"`
	Label       string   `json:"label"`
	Reason      *string  `json:"reason"`
}

// Attention is one item that needs the operator. Since is null in Phases
// 0-1: no rule has a reading of when its condition began.
type Attention struct {
	Severity string  `json:"severity"`
	Reason   string  `json:"reason"`
	Subject  string  `json:"subject"`
	Since    *string `json:"since"`
	Text     string  `json:"text"`
}
