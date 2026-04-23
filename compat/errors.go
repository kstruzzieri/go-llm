package compat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kstruzzieri/go-llm/provider"
)

// errorEnvelope is the OpenAI-shape error body.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// errorBody mirrors OpenAI's error shape. "type" is OpenAI's vocabulary:
// invalid_request_error (4xx client), api_error (5xx/infra).
type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// writeError sends a JSON error using OpenAI's shape. The "type" field is
// derived from the status: 4xx -> invalid_request_error, else api_error.
func writeError(w http.ResponseWriter, status int, code, message string) {
	kind := "api_error"
	if status >= 400 && status < 500 {
		kind = "invalid_request_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{
		Error: errorBody{Message: message, Type: kind, Code: code},
	})
}

// writeCompatError classifies a router/provider error per amendment #4 of the
// plan and writes the corresponding OpenAI-shape error body. Never flattens
// everything to 502 — preserves retry semantics for 429/timeouts/budget.
func writeCompatError(w http.ResponseWriter, err error) {
	status, code, message := statusForCompatError(err)
	writeError(w, status, code, message)
}

// statusForCompatError maps a provider/router error to an HTTP status, a short
// machine-readable code, and a human-readable message. See the amendment #4
// table in docs/superpowers/plans/2026-04-20-openai-compat.md for the full
// rationale of each mapping.
func statusForCompatError(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, provider.ErrNoViableCandidate):
		return http.StatusBadRequest, "no_viable_candidate", err.Error()
	case errors.Is(err, provider.ErrBudgetExceeded):
		return http.StatusBadRequest, "budget_exceeded", err.Error()
	case errors.Is(err, provider.ErrBudgetAdaptationRequired):
		return http.StatusBadRequest, "budget_adaptation_required", err.Error()
	case errors.Is(err, provider.ErrAllBreakersOpen):
		return http.StatusServiceUnavailable, "all_breakers_open", err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout", "request timed out"
	case errors.Is(err, context.Canceled):
		// 499 follows nginx's "client closed request" convention — there is no
		// stdlib StatusCode constant for it.
		return 499, "canceled", "request canceled"
	}
	var httpErr *provider.HTTPStatusError
	if errors.As(err, &httpErr) && httpErr != nil {
		switch {
		case httpErr.StatusCode == http.StatusTooManyRequests:
			return http.StatusTooManyRequests, "rate_limited", httpErr.Error()
		case httpErr.StatusCode >= 500:
			return http.StatusBadGateway, "upstream_error", httpErr.Error()
		case httpErr.StatusCode >= 400:
			// Upstream 4xx (e.g. Ollama "model not found" 404, bad-request 400)
			// is almost always caused by a malformed client request. Pass the
			// status through so clients do not retry a deterministic failure
			// as if it were a transient upstream fault.
			return httpErr.StatusCode, "upstream_client_error", httpErr.Error()
		}
	}
	// Unknown infrastructure / unclassified failure: return a generic 502
	// without leaking provider-specific internals.
	return http.StatusBadGateway, "upstream_error", "upstream error"
}
