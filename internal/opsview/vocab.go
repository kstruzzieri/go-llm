package opsview

import "github.com/kstruzzieri/go-llm/internal/opsbackend"

// Residency, activity and reachability states. The residency words alias
// opsbackend's, which records transitions in them, so each has one source.
const (
	StateLoaded      = opsbackend.ResidencyLoaded
	StateLoading     = opsbackend.ResidencyLoading
	StateUnloading   = opsbackend.ResidencyUnloading
	StateUnloaded    = opsbackend.ResidencyUnloaded
	StateUnknown     = opsbackend.ResidencyUnknown
	StateNotObserved = "not_observed"
	ReachOK          = "ok"
	ReachUnreachable = "unreachable"
)

// Reason codes.
const (
	ReasonStale            = "stale"
	ReasonUnconfirmed      = "unconfirmed_model_id"
	ReasonRemote           = "remote_endpoint"
	ReasonNoInflight       = "no_inflight_source"
	ReasonOllamaNoInflight = "ollama_no_inflight"
	ReasonOllamaAbsent     = "absent_not_proof"
	ReasonNoSample         = "no_sample"
	ReasonUnknownProvider  = "unknown_provider"
	ReasonNotObserved      = "backend_not_observed"
	ReasonNoHistory        = "backend_no_history"
	ReasonTooFewSamples    = "too_few_samples"
	ReasonNoValues         = "no_available_values"
)

// Coverage states and reasons. There is deliberately no "complete" state in
// Phases 0-1 (spec §5.4).
const (
	CoverageIncomplete       = "incomplete"
	CoverageUnknown          = "unknown"
	CovEmpty                 = "empty_ring"
	CovNoncontiguous         = "noncontiguous_ids"
	CovEvicted               = "older_rows_evicted"
	CovRetentionStartUnknown = "retention_start_unknown"
	CovWithinRetained        = "window_within_retained"
)

// Scope and labels.
const (
	ScopeBackendAllClients = "backend_all_clients"
	LabelDuration          = "backend-reported request duration"
)

// Severities, most urgent first.
const (
	SevCritical = "critical"
	SevSerious  = "serious"
	SevWarning  = "warning"
	SevInfo     = "info"
)

// Attention reasons.
const (
	AttnBackendUnreachable   = "backend_unreachable"
	AttnRefused              = "refused_requests"
	AttnModelErrors          = "model_errors"
	AttnTelemetryUnavailable = "telemetry_unavailable"
	AttnTelemetryStale       = "telemetry_stale"
	AttnBackendUnsupported   = "backend_unsupported"
	AttnConfigProblem        = "config_problem"
	AttnCoverageNote         = "coverage_note"
)

var severityRank = map[string]int{SevCritical: 0, SevSerious: 1, SevWarning: 2, SevInfo: 3}

var labels = map[string]string{
	ReasonStale:              "no fresh reading",
	ReasonUnconfirmed:        "not confirmed as a llama-swap model ID",
	ReasonRemote:             "remote endpoint, not observed",
	ReasonNoInflight:         "llama-swap v235 publishes no reliable in-flight snapshot",
	ReasonOllamaNoInflight:   "Ollama publishes no in-flight count",
	ReasonOllamaAbsent:       "absent from Ollama /api/ps; absence is not proof",
	ReasonNoSample:           "no reading yet",
	ReasonUnknownProvider:    "provider not configured",
	ReasonNotObserved:        "backend not observed",
	ReasonNoHistory:          "backend publishes no per-request history",
	ReasonTooFewSamples:      "too few samples",
	ReasonNoValues:           "no reported values",
	CovEmpty:                 "no retained rows",
	CovNoncontiguous:         "retained rows have ID gaps",
	CovEvicted:               "older rows evicted; window not fully covered",
	CovRetentionStartUnknown: "retention start unknown (llama-swap may have restarted)",
	CovWithinRetained:        "window inside retained rows; completeness unproven",
}

// Label returns the human text for a code, or the code itself.
func Label(code string) string {
	if l, ok := labels[code]; ok {
		return l
	}
	return code
}
