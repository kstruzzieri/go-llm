// Package opsbackend reads local inference backends' runtime surfaces for the
// golem ops console: llama-swap v235 (GET /api/version, /running,
// /api/metrics, /v1/models) and Ollama (GET /api/ps). It never calls a
// model-dispatched route: every request passes an exact method and path
// allowlist before it leaves the process, and only root base URLs on
// loopback destinations are ever contacted.
package opsbackend

import (
	"context"
	"errors"
	"net"

	"github.com/kstruzzieri/go-llm/provider"
)

// Code is the bounded vocabulary of observation failures. Messages carried
// beside a code never include response bodies, URLs or secrets.
type Code string

// Observation failure codes (spec §4.6).
const (
	CodeUnreachable          Code = "unreachable"
	CodeTimeout              Code = "timeout"
	CodeUnauthorized         Code = "unauthorized"
	CodeHTTPStatus           Code = "http_status"
	CodeMalformed            Code = "malformed_response"
	CodeTooLarge             Code = "body_too_large"
	CodeUnsupportedVersion   Code = "unsupported_version"
	CodeUnrecognizedRuntime  Code = "unrecognized_runtime"
	CodeInvalidConfiguration Code = "invalid_configuration"
	CodeRefused              Code = "refused"
	CodeDenied               Code = "denied"
)

// ErrRefused reports that the ops allowlist refused a request before it left
// the process. It always indicates a console bug.
var ErrRefused = errors.New("opsbackend: request refused by the ops allowlist")

type codedError struct {
	code Code
	msg  string
}

func (e *codedError) Error() string { return "opsbackend: " + string(e.code) + ": " + e.msg }

func newCoded(code Code, msg string) error { return &codedError{code: code, msg: msg} }

// CodeOf extracts the bounded code from err's chain, if any.
func CodeOf(err error) (Code, bool) {
	var ce *codedError
	if errors.As(err, &ce) {
		return ce.code, true
	}
	return "", false
}

// classify maps a transport failure on surface to a coded error. Caller
// cancellation stays unclassified so it never starts outage backoff, and the
// original error (which may name the URL) is dropped from coded results.
func classify(ctx context.Context, surface string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := CodeOf(err); ok {
		return err
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return err
	}
	switch {
	case errors.Is(err, ErrRefused):
		return newCoded(CodeRefused, surface)
	case errors.Is(err, provider.ErrDestinationDenied):
		return newCoded(CodeDenied, surface)
	case errors.Is(err, context.DeadlineExceeded):
		return newCoded(CodeTimeout, surface)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return newCoded(CodeTimeout, surface)
	}
	return newCoded(CodeUnreachable, surface)
}
