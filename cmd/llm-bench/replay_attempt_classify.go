package main

import (
	"context"
	"encoding/json"
	"errors"
)

// classifyAttempt maps one replay outcome to its stable code (spec §7) from
// facts frozen when the failure happened: the replay error, the recorder
// rows, and the attempt context's cause captured when replayWith returned.
// It never consults live context state, so a deadline that fires after an
// argument failure cannot relabel that failure. The message depends on the
// code alone (attemptErrorMessage), never on the error text.
func classifyAttempt(err error, rows []*recordedRequest, attemptCause error) *attemptError {
	if err == nil {
		return nil
	}
	f := classifyFailure(err, rows, attemptCause)
	f.Message = attemptErrorMessage(f.Code)
	return f
}

func classifyFailure(err error, rows []*recordedRequest, attemptCause error) *attemptError {
	if len(rows) == 0 {
		// Nothing reached the wire: fixture or config work failed. The
		// cancellation branch is defensive and unreachable through the CLI:
		// http.Client always invokes the recorder, so a request cancelled
		// before it is sent still records a row.
		if isCancellation(err, attemptCause) {
			return &attemptError{Code: attemptCodeCanceled, Stage: attemptStageRequest, CancelSource: attemptCancelSource(attemptCause), RequestOrdinal: 1}
		}
		return &attemptError{Code: attemptCodeInput, Stage: attemptStageInput}
	}
	// The replay stops at a failed request, so only the last row can fail.
	last := rows[len(rows)-1]
	if f := rowFailure(last); f != nil {
		return f
	}
	f := &attemptError{Code: attemptCodeInternal, Stage: attemptStageReplay, RequestOrdinal: last.Ordinal}
	var argErr *toolArgumentsError
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &argErr):
		f.Code = attemptCodeArgumentDecode
	case errors.As(err, &typeErr), errors.As(err, &syntaxErr):
		// After a good row only the provider still decodes: the recorder
		// proved the body is one JSON object, so a field has the wrong type.
		// Checked after argErr, which also wraps a JSON decode error.
		f.Code, f.Stage, f.HTTPStatus = attemptCodeResponseDecode, attemptStageResponse, last.HTTPStatus
	case errors.Is(err, errEmptyAssistantReply):
		f.Code = attemptCodeEmptyResponse
	case errors.Is(err, errToolCallMismatch):
		f.Code = attemptCodeReplayDivergence
	case errors.Is(err, errMissingToolResult), errors.Is(err, errMissingScriptedAssistant), errors.Is(err, errUnsupportedTurns):
		f.Code = attemptCodeInput
	case isCancellation(err, attemptCause):
		// Defensive and unreachable through the CLI: http.Client always
		// invokes the recorder, so a refused next request still records a
		// row and is classified by rowFailure above.
		f.Code, f.Stage = attemptCodeCanceled, attemptStageRequest
		f.CancelSource, f.RequestOrdinal = attemptCancelSource(attemptCause), last.Ordinal+1
	}
	return f
}

// rowFailure classifies one recorded request, or returns nil when it
// succeeded. Precedence follows spec §7: a cancellation that interrupted the
// request wins over the I/O error it caused; then a missing response; then a
// non-2xx status (a truncated or oversized 500 is still http_status, its
// incomplete body kept as evidence); then body and envelope failures.
func rowFailure(row *recordedRequest) *attemptError {
	f := &attemptError{HTTPStatus: row.HTTPStatus, RequestOrdinal: row.Ordinal}
	switch {
	case row.CancelCause != nil:
		f.Code, f.CancelSource, f.Stage = attemptCodeCanceled, attemptCancelSource(row.CancelCause), attemptStageRequest
		if !row.HeadersAt.IsZero() {
			f.Stage = attemptStageResponse
		}
	case row.TransportErr != nil:
		f.Code, f.Stage = attemptCodeConnection, attemptStageRequest
	case row.HTTPStatus/100 != 2:
		f.Code, f.Stage = attemptCodeHTTPStatus, attemptStageResponse
	case row.ReadErr != nil:
		f.Code, f.Stage = attemptCodeConnection, attemptStageResponse
	case row.Rejected != "":
		f.Code, f.Stage = row.Rejected, attemptStageResponse
	default:
		return nil
	}
	return f
}

func attemptCancelSource(cause error) string {
	if errors.Is(cause, errAttemptTimeout) {
		return attemptCancelTimeout
	}
	return attemptCancelSignal
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// isCancellation reports whether err is a cancellation of the attempt.
// isContextError alone matches only context.Canceled and DeadlineExceeded,
// but the transport returns context.Cause(ctx): errAttemptTimeout for the
// deadline, or the parent's signal error.
func isCancellation(err, attemptCause error) bool {
	return isContextError(err) || (attemptCause != nil && errors.Is(err, attemptCause))
}
