// Package opsbackend reads local inference backends' runtime surfaces for the
// golem ops console. Every unidentified openai-compat root receives
// GET /api/version to identify it; a llama-swap v235 root is then read through
// GET /api/version, /running, /api/metrics and /v1/models, and an Ollama root
// through GET /api/ps. It never calls a model-dispatched route: every request
// passes an exact method and path allowlist before it leaves the process, and
// only root base URLs on loopback destinations are ever contacted.
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

// Observation failure codes (spec §4.6). Consumers switch on these constants
// and never duplicate the string literals.
const (
	// CodeUnreachable reports that no complete HTTP response arrived: dial
	// failure, reset, EOF or short body, TLS failure, or a non-HTTP peer.
	CodeUnreachable Code = "unreachable"
	// CodeTimeout reports that a per-request or per-tick deadline expired.
	CodeTimeout Code = "timeout"
	// CodeUnauthorized reports an HTTP 401 or 403 from the backend.
	CodeUnauthorized Code = "unauthorized"
	// CodeHTTPStatus reports any other non-2xx status from the backend.
	CodeHTTPStatus Code = "http_status"
	// CodeMalformed reports a response that failed decoding or semantic
	// validation.
	CodeMalformed Code = "malformed_response"
	// CodeTooLarge reports a response body over its surface's byte cap; the
	// surface is recorded beside the code.
	CodeTooLarge Code = "body_too_large"
	// CodeUnsupportedVersion reports a llama-swap whose version is not v235.
	CodeUnsupportedVersion Code = "unsupported_version"
	// CodeUnrecognizedRuntime reports an openai-compat root that is not
	// llama-swap (llama-server direct, vLLM, LM Studio, Ollama).
	CodeUnrecognizedRuntime Code = "unrecognized_runtime"
	// CodeInvalidConfiguration reports an unusable base URL: a path prefix, a
	// bad scheme, user info, or an unparsable URL.
	CodeInvalidConfiguration Code = "invalid_configuration"
	// CodeRefused reports that the ops allowlist refused the request before it
	// left the process. It always indicates a console bug.
	CodeRefused Code = "refused"
	// CodeDenied reports that the destination gate refused the request,
	// including a redirect.
	CodeDenied Code = "denied"
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
// cancellation returns ctx.Err() unclassified so it never starts outage
// backoff. No path returns the transport error itself: it may name the URL,
// a redirect target, or bytes from a non-HTTP peer.
func classify(ctx context.Context, surface string, err error) error {
	if err == nil {
		return nil
	}
	if _, ok := CodeOf(err); ok {
		return err
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
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
