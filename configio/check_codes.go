package configio

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"

	"github.com/kstruzzieri/go-llm/internal/responsebound"
	"github.com/kstruzzieri/go-llm/provider"
)

// checkCode classifies a failed check, the caller's cancellation already ruled
// out. It reads status codes and error types, never text: a status first, from
// HTTPStatusCode or a bounded rejection whose body could not be read; then the
// byte limit, a destination denial, and the transport and decoding failures.
// Anything else is malformed_response.
func checkCode(err error) ErrorCode {
	switch status := checkStatus(err); {
	case status == 401:
		return CodeAuthFailed
	case status == 403 || status == 404:
		return CodeRefused
	case status == 429:
		return CodeRateLimited
	case status >= 400 && status < 500:
		return CodeRejected
	case status >= 500:
		return CodeServerError
	case status != 0:
		return CodeMalformedResponse // a redirect or another non-2xx
	}
	var denied *provider.DestinationDeniedError
	switch {
	case errors.Is(err, responsebound.ErrExceeded):
		return CodeResponseTooLarge
	case errors.As(err, &denied):
		return CodeDestinationNotGranted
	case errors.Is(err, provider.ErrDestinationDenied):
		return CodeMalformedResponse // the guard refused a redirect
	}
	if code, ok := transportCode(err); ok {
		return code
	}
	return CodeMalformedResponse
}

// checkStatus is a failed check's HTTP status, 0 when it has none: a bounded
// rejection whose body could not be read keeps its status; otherwise the
// chain's HTTPStatusCode. A 403 or 404 is a refusal whatever its body did.
func checkStatus(err error) int {
	var unreadable *responsebound.RejectionReadError
	if errors.As(err, &unreadable) {
		return unreadable.StatusCode
	}
	var hs interface{ HTTPStatusCode() int }
	if errors.As(err, &hs) {
		return hs.HTTPStatusCode()
	}
	return 0
}

// transportCode classifies a transport or decoding failure by type: timeout,
// unreachable or malformed_response, and false for anything else. A
// *url.Error is a transport failure even when it wraps io.EOF (a server that
// hangs up). Checks and listings share it.
func transportCode(err error) (ErrorCode, bool) {
	var urlErr *url.Error
	var syntax *json.SyntaxError
	var typed *json.UnmarshalTypeError
	var netErr net.Error
	switch {
	case errors.As(err, &urlErr):
		if urlErr.Timeout() {
			return CodeTimeout, true
		}
		return CodeUnreachable, true
	case errors.As(err, &syntax), errors.As(err, &typed),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return CodeMalformedResponse, true
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return CodeTimeout, true
	case errors.As(err, &netErr):
		return CodeUnreachable, true
	}
	return "", false
}
