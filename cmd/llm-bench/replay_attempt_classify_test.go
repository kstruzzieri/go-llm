package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestClassifyAttempt(t *testing.T) {
	now := time.Now()
	ok := func(ord int) *recordedRequest {
		return &recordedRequest{Ordinal: ord, StartedAt: now, HeadersAt: now, EndedAt: now, HTTPStatus: 200, ResponseComplete: true}
	}
	failed := func(mut func(*recordedRequest)) []*recordedRequest {
		r := &recordedRequest{Ordinal: 2, StartedAt: now, EndedAt: now}
		mut(r)
		return []*recordedRequest{ok(1), r}
	}
	signal := errors.New("interrupt signal received")
	var typed struct{ Content string }
	wrongTyped := json.Unmarshal([]byte(`{"Content":123}`), &typed) // *json.UnmarshalTypeError
	var anyVal any
	badSyntax := json.Unmarshal([]byte(`{"a":}`), &anyVal) // *json.SyntaxError
	var syn *json.SyntaxError
	if !errors.As(badSyntax, &syn) {
		t.Fatalf("fixture error %T is not a *json.SyntaxError", badSyntax)
	}
	// Like production, the argument error wraps a JSON decode error, so the
	// argument case must be checked before the generic decode case.
	argErr := &toolArgumentsError{name: "lookup", err: wrongTyped}

	cases := []struct {
		name                string
		err                 error
		rows                []*recordedRequest
		cause               error
		code, stage, source string
		status, ordinal     int
	}{
		{"success", nil, []*recordedRequest{ok(1)}, nil, "", "", "", 0, 0},
		{"no rows: fixture error", errors.New("tool_call 0 arguments: bad"), nil, nil, attemptCodeInput, attemptStageInput, "", 0, 0},
		{"no rows: cancelled before first request", fmt.Errorf("wrap: %w", context.Canceled), nil, signal, attemptCodeCanceled, attemptStageRequest, attemptCancelSignal, 0, 1},
		{"transport error", errors.New("dial"), failed(func(r *recordedRequest) { r.TransportErr = errors.New("dial") }), nil, attemptCodeConnection, attemptStageRequest, "", 0, 2},
		{"transport error under timeout", errors.New("x"), failed(func(r *recordedRequest) { r.TransportErr = errors.New("x"); r.CancelCause = errAttemptTimeout }), nil, attemptCodeCanceled, attemptStageRequest, attemptCancelTimeout, 0, 2},
		{"body read cancelled by signal", errors.New("x"), failed(func(r *recordedRequest) {
			r.HeadersAt, r.HTTPStatus, r.ReadErr, r.CancelCause = now, 200, errors.New("x"), signal
		}), nil, attemptCodeCanceled, attemptStageResponse, attemptCancelSignal, 200, 2},
		{"body read error", errors.New("read /Users/x/y.json: reset"), failed(func(r *recordedRequest) { r.HeadersAt, r.HTTPStatus, r.ReadErr = now, 200, errors.New("reset") }), nil, attemptCodeConnection, attemptStageResponse, "", 200, 2},
		{"recorder rejected decode", errors.New("x"), failed(func(r *recordedRequest) { r.HeadersAt, r.HTTPStatus, r.Rejected = now, 200, attemptCodeResponseDecode }), nil, attemptCodeResponseDecode, attemptStageResponse, "", 200, 2},
		{"recorder rejected empty", errors.New("x"), failed(func(r *recordedRequest) { r.HeadersAt, r.HTTPStatus, r.Rejected = now, 200, attemptCodeEmptyResponse }), nil, attemptCodeEmptyResponse, attemptStageResponse, "", 200, 2},
		{"http 500", errors.New("x"), failed(func(r *recordedRequest) { r.HeadersAt, r.HTTPStatus, r.ResponseComplete = now, 500, true }), nil, attemptCodeHTTPStatus, attemptStageResponse, "", 500, 2},
		{"http 307 not followed", errors.New("x"), failed(func(r *recordedRequest) { r.HeadersAt, r.HTTPStatus, r.ResponseComplete = now, 307, true }), nil, attemptCodeHTTPStatus, attemptStageResponse, "", 307, 2},
		{"truncated 500 is still http_status", errors.New("x"), failed(func(r *recordedRequest) { r.HeadersAt, r.HTTPStatus, r.ReadErr = now, 500, errors.New("reset") }), nil, attemptCodeHTTPStatus, attemptStageResponse, "", 500, 2},
		{"oversized 500 is still http_status", errors.New("x"), failed(func(r *recordedRequest) { r.HeadersAt, r.HTTPStatus, r.Rejected = now, 500, attemptCodeResponseDecode }), nil, attemptCodeHTTPStatus, attemptStageResponse, "", 500, 2},
		{"wrong-typed response field", fmt.Errorf("openaicompat: decode response: %w", wrongTyped), []*recordedRequest{ok(1)}, nil, attemptCodeResponseDecode, attemptStageResponse, "", 200, 1},
		{"argument decode survives a later timeout", fmt.Errorf("w: %w", argErr), []*recordedRequest{ok(1)}, errAttemptTimeout, attemptCodeArgumentDecode, attemptStageReplay, "", 0, 1},
		{"empty assistant reply", fmt.Errorf("trace: %w", errEmptyAssistantReply), []*recordedRequest{ok(1)}, nil, attemptCodeEmptyResponse, attemptStageReplay, "", 0, 1},
		{"tool call mismatch", fmt.Errorf("trace: %w", errToolCallMismatch), []*recordedRequest{ok(1)}, nil, attemptCodeReplayDivergence, attemptStageReplay, "", 0, 1},
		{"mid-replay fixture defect", fmt.Errorf("trace: %w", errMissingToolResult), []*recordedRequest{ok(1)}, nil, attemptCodeInput, attemptStageReplay, "", 0, 1},
		{"mid-replay missing scripted assistant", fmt.Errorf("trace: %w", errMissingScriptedAssistant), []*recordedRequest{ok(1)}, nil, attemptCodeInput, attemptStageReplay, "", 0, 1},
		{"mid-replay unsupported turns", fmt.Errorf("trace: %w", errUnsupportedTurns), []*recordedRequest{ok(1)}, nil, attemptCodeInput, attemptStageReplay, "", 0, 1},
		{"syntax-error response", fmt.Errorf("openaicompat: decode response: %w", badSyntax), []*recordedRequest{ok(1)}, nil, attemptCodeResponseDecode, attemptStageResponse, "", 200, 1},
		{"next request refused before the recorder", fmt.Errorf("w: %w", context.DeadlineExceeded), []*recordedRequest{ok(1)}, errAttemptTimeout, attemptCodeCanceled, attemptStageRequest, attemptCancelTimeout, 0, 2},
		{"unmatched", errors.New("marshal tool call arguments for /Users/x/y.json"), []*recordedRequest{ok(1)}, nil, attemptCodeInternal, attemptStageReplay, "", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyAttempt(tc.err, tc.rows, tc.cause)
			if tc.code == "" {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got == nil || got.Code != tc.code || got.Stage != tc.stage || got.CancelSource != tc.source || got.HTTPStatus != tc.status || got.RequestOrdinal != tc.ordinal {
				t.Fatalf("got %+v\nwant code=%s stage=%s source=%q status=%d ordinal=%d", got, tc.code, tc.stage, tc.source, tc.status, tc.ordinal)
			}
			// A4: every message is the redacted category stub, never the raw error.
			if want := redactErrorMessage(tc.err.Error()); got.Message != want || got.Message == tc.err.Error() {
				t.Fatalf("message %q, want the redacted stub %q (raw %q)", got.Message, want, tc.err.Error())
			}
			if strings.Contains(tc.err.Error(), "/Users/x/y.json") && strings.Contains(got.Message, "/Users/") {
				t.Fatalf("message %q leaks the raw path", got.Message)
			}
		})
	}
}

func TestClassifyAttemptMessageIsTheRedactedStub(t *testing.T) {
	got := classifyAttempt(errors.New("open /Users/someone/secret-trace.json: denied"), nil, nil)
	if strings.Contains(got.Message, "/Users/") || got.Message != redactErrorMessage("open /Users/someone/secret-trace.json: denied") {
		t.Fatalf("message %q leaks detail; attempt.json carries only the category stub (A4)", got.Message)
	}
}
