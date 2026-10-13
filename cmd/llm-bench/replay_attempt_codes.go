package main

import "errors"

// Stable -replay-attempt error codes (#687), one per golem-lab foundation
// spec §11.1 class. They are a published contract: never rename one.
const (
	attemptCodeInput            = "input"
	attemptCodeConnection       = "connection"
	attemptCodeHTTPStatus       = "http_status"
	attemptCodeCanceled         = "canceled"
	attemptCodeResponseDecode   = "response_decode"
	attemptCodeArgumentDecode   = "argument_decode"
	attemptCodeEmptyResponse    = "empty_response"
	attemptCodeReplayDivergence = "replay_divergence"
	attemptCodeInternal         = "internal"
	attemptCodeArtifactWrite    = "artifact_write"
)

// Stages name where an attempt failed.
const (
	attemptStageInput    = "input"
	attemptStageRequest  = "request"
	attemptStageResponse = "response"
	attemptStageReplay   = "replay"
	attemptStagePublish  = "publish"
)

// Cancellation sources. The CLI's only parent canceller is main's
// SIGINT/SIGTERM context, so every cancellation that is not the attempt
// timeout is a signal (spec amendment A1).
const (
	attemptCancelTimeout = "attempt_timeout"
	attemptCancelSignal  = "signal"
)

// errAttemptTimeout is the cause the attempt deadline cancels with.
var errAttemptTimeout = errors.New("replay-attempt: attempt timeout")

// attemptError is one classified failure in attempt.json.
type attemptError struct {
	Code           string `json:"code"`
	Stage          string `json:"stage"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	CancelSource   string `json:"cancel_source,omitempty"`
	RequestOrdinal int    `json:"request_ordinal,omitempty"`
	Message        string `json:"message"`
}
