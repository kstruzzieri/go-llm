// Package fingerprint profiles Ollama models with latency benchmarks,
// detects model kind (chat vs embedding), and persists backend-scoped
// profiles in SQLite for intelligent model selection.
package fingerprint

import (
	"errors"
	"fmt"
	"time"
)

// CurrentProfileVersion is the benchmark suite version written by this build.
// Increment when new benchmarks are added (e.g., Phase 2 full suite).
const CurrentProfileVersion = 1

// ErrNotFound is returned by Store.Get when no profile exists for the given key.
var ErrNotFound = errors.New("fingerprint: not found")

// ModelKind classifies a model's primary function.
type ModelKind string

const (
	ModelKindChat      ModelKind = "chat"
	ModelKindEmbedding ModelKind = "embedding"
	ModelKindUnknown   ModelKind = "unknown"
)

// KindDetection holds the result of model kind detection, including
// all detected capabilities and metadata from /api/show.
type KindDetection struct {
	Kind         ModelKind
	Capabilities []string // all detected capabilities (e.g. ["completion","tools","embedding"])
	Family       string   // /api/show family; passed through for think-mode gating
	Source       string   // "capabilities", "heuristic", or "probe"
}

// Profile holds a complete model fingerprint, including identity,
// capabilities, performance metrics, and resource observations.
//
// Capabilities is the authoritative field for what a model can do.
// ModelKind exists for backward compatibility and simple single-purpose
// routing; it does not reflect all model abilities. Callers that need
// full capability info must use Capabilities, not ModelKind.
type Profile struct {
	// Identity (composite key: backend_id + model_name)
	ModelName              string
	ModelDigest            string    // content-addressed version hash from /api/show
	BackendID              string    // normalized Ollama base URL
	ModelKind              ModelKind // primary kind for backward compat routing
	Capabilities           []string  // all model capabilities (e.g. ["completion","embedding"])
	IncompleteCapabilities []string  // capabilities still missing metrics; empty = complete
	KindSource             string    // how kind was determined: "capabilities", "heuristic", or "probe"
	ProfileVersion         int       // benchmark suite version (1 = latency-only, 2 = full suite)
	TestedAt               time.Time

	// Chat capabilities (-1 = not tested)
	EffectiveContext int     // 0 = not tested
	ToolCallingRate  float64 // 0.0-1.0, -1 = not tested
	InstructionScore float64 // 0.0-1.0, -1 = not tested

	// Chat performance (-1 / 0 = not tested)
	GenerationTokensPerSecond float64       // -1 = not tested
	PromptLatency             time.Duration // prompt_eval_duration; 0 = not tested
	ColdStartLatency          time.Duration // first-run latency including model load; 0 = model was warm

	// Embedding (-1 / 0 = not tested)
	EmbeddingDim       int           // 0 = not tested
	EmbeddingCoherence float64       // -1 = not tested
	EmbeddingLatency   time.Duration // 0 = not tested

	// Resource observations
	PeakMemoryMB  int64 // 0 = not measured (Phase 1)
	GPULayersUsed int   // 0 = not measured (Phase 1)
}

// ChatMetrics holds the output of a chat probe benchmark.
type ChatMetrics struct {
	TokensPerSecond  float64
	PromptLatency    time.Duration // prompt_eval_duration (warm model)
	ColdStartLatency time.Duration // total latency of cold-loaded first run; 0 if warm
}

// EmbeddingMetrics holds the output of an embedding probe benchmark.
type EmbeddingMetrics struct {
	Latency          time.Duration
	ColdStartLatency time.Duration // 0 if warm
	Dim              int           // embedding dimensions, captured from embed response
}

// FailureInfo records a failed fingerprint attempt for backoff tracking.
type FailureInfo struct {
	ModelDigest  string
	LastError    string
	AttemptedAt  time.Time
	AttemptCount int
	RetryAfter   time.Time
}

// BackoffError indicates that a model is in a fingerprint backoff window
// and should not be retried yet.
type BackoffError struct {
	RetryAfter time.Time
	LastError  string
}

func (e *BackoffError) Error() string {
	return fmt.Sprintf("fingerprint: in backoff until %s: %s", e.RetryAfter.Format(time.RFC3339), e.LastError)
}

// CapProbeState is the tri-state outcome of a capability probe.
// The empty string means "unknown" (never probed / cache miss) and is
// never persisted.
type CapProbeState string

const (
	CapProbeYes          CapProbeState = "yes"
	CapProbeNo           CapProbeState = "no"
	CapProbeInconclusive CapProbeState = "inconclusive"
)

// CurrentToolProbeVersion identifies the tool-call probe request shape.
// Bump when the probe protocol changes (tool definition, prompt,
// tool_choice escalation); cached rows from other versions are ignored.
const CurrentToolProbeVersion = 1

// CapProbeInconclusiveTTL bounds how long an inconclusive verdict is
// trusted before the next demand re-probes.
const CapProbeInconclusiveTTL = 24 * time.Hour

// CapProbeDigestlessNoTTL bounds a negative verdict for models with no
// content digest (openai-compat fallback keying): a wedged "no" silently
// blocks usage, so digestless negatives expire rather than sticking until
// a manual --reprobe.
const CapProbeDigestlessNoTTL = 7 * 24 * time.Hour

// CapProbe is one persisted capability-probe verdict, keyed by
// (backend_id, model_name, capability). Distinct from Profile so
// capability-only resolution can never masquerade as a complete
// fingerprint profile.
type CapProbe struct {
	BackendID    string
	ModelName    string
	Capability   string // canonical token, e.g. "tool_call"
	State        CapProbeState
	ModelDigest  string // runtime digest, or key fallback when digestless
	ProbeVersion int    // CurrentToolProbeVersion at probe time
	TestedAt     time.Time
	ExpiresAt    time.Time // zero = does not expire
}

// Valid reports whether the cached probe still applies for the given
// current digest at time now: digest and probe version must match and the
// row must not be expired.
//
// The version check is coupled to the tool_call probe protocol
// (CurrentToolProbeVersion); a second capability would need its own
// probe-version parameter here.
func (p CapProbe) Valid(currentDigest string, now time.Time) bool {
	if p.ModelDigest != currentDigest {
		return false
	}
	if p.ProbeVersion != CurrentToolProbeVersion {
		return false
	}
	if !p.ExpiresAt.IsZero() && now.After(p.ExpiresAt) {
		return false
	}
	return true
}
