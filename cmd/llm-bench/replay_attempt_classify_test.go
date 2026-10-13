package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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

	// error.message is one fixed sentence per code; the literals are the
	// published text, independent of attemptErrorMessage.
	wantMessage := map[string]string{
		attemptCodeInput:            "an input to the attempt was invalid",
		attemptCodeConnection:       "the connection to the endpoint failed",
		attemptCodeHTTPStatus:       "the server returned a non-2xx status",
		attemptCodeCanceled:         "the attempt was canceled",
		attemptCodeResponseDecode:   "the server's response could not be decoded",
		attemptCodeArgumentDecode:   "a tool call's arguments could not be decoded",
		attemptCodeEmptyResponse:    "the candidate returned no reply",
		attemptCodeReplayDivergence: "the candidate's tool calls did not match the scripted route",
		attemptCodeInternal:         "an internal error occurred",
	}
	// The transport returns context.Cause(ctx), wrapped by net/http in a
	// *url.Error: the real shape of a cancellation, unlike a bare
	// context.Canceled.
	timeoutErr := fmt.Errorf("provider: openaicompat: chat: %w", &url.Error{Op: "Post", URL: "u", Err: errAttemptTimeout})
	signalErr := fmt.Errorf("provider: openaicompat: chat: %w", &url.Error{Op: "Post", URL: "u", Err: signal})

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
		{"no rows: attempt timeout cause", timeoutErr, nil, errAttemptTimeout, attemptCodeCanceled, attemptStageRequest, attemptCancelTimeout, 0, 1},
		{"no rows: signal cause", signalErr, nil, signal, attemptCodeCanceled, attemptStageRequest, attemptCancelSignal, 0, 1},
		{"no rows: a cause that is not in the chain stays input", errors.New("tool_call 0 arguments: bad"), nil, signal, attemptCodeInput, attemptStageInput, "", 0, 0},
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
		{"frozen result name mismatch", fmt.Errorf("trace: %w", &frozenToolResultNameError{err: errToolCallMismatch}), []*recordedRequest{ok(1)}, nil, attemptCodeInput, attemptStageReplay, "", 0, 1},
		{"mid-replay fixture defect", fmt.Errorf("trace: %w", errMissingToolResult), []*recordedRequest{ok(1)}, nil, attemptCodeInput, attemptStageReplay, "", 0, 1},
		{"mid-replay missing scripted assistant", fmt.Errorf("trace: %w", errMissingScriptedAssistant), []*recordedRequest{ok(1)}, nil, attemptCodeInput, attemptStageReplay, "", 0, 1},
		{"mid-replay unsupported turns", fmt.Errorf("trace: %w", errUnsupportedTurns), []*recordedRequest{ok(1)}, nil, attemptCodeInput, attemptStageReplay, "", 0, 1},
		{"syntax-error response", fmt.Errorf("openaicompat: decode response: %w", badSyntax), []*recordedRequest{ok(1)}, nil, attemptCodeResponseDecode, attemptStageResponse, "", 200, 1},
		{"next request refused before the recorder", fmt.Errorf("w: %w", context.DeadlineExceeded), []*recordedRequest{ok(1)}, errAttemptTimeout, attemptCodeCanceled, attemptStageRequest, attemptCancelTimeout, 0, 2},
		{"next request refused: attempt timeout cause", timeoutErr, []*recordedRequest{ok(1)}, errAttemptTimeout, attemptCodeCanceled, attemptStageRequest, attemptCancelTimeout, 0, 2},
		{"next request refused: signal cause", signalErr, []*recordedRequest{ok(1)}, signal, attemptCodeCanceled, attemptStageRequest, attemptCancelSignal, 0, 2},
		{"unmatched", errors.New("marshal tool call arguments for /Users/x/y.json"), []*recordedRequest{ok(1)}, nil, attemptCodeInternal, attemptStageReplay, "", 0, 1},
		{"unmatched text mentioning a timeout", errors.New("upstream timeout while marshalling"), []*recordedRequest{ok(1)}, nil, attemptCodeInternal, attemptStageReplay, "", 0, 1},
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
			// A4: the message is the fixed sentence for the code, never the raw error.
			if want := wantMessage[tc.code]; got.Message != want || got.Message == tc.err.Error() {
				t.Fatalf("message %q, want %q (raw %q)", got.Message, want, tc.err.Error())
			}
			if strings.Contains(tc.err.Error(), "/Users/x/y.json") && strings.Contains(got.Message, "/Users/") {
				t.Fatalf("message %q leaks the raw path", got.Message)
			}
		})
	}
}

// error.message must not depend on the underlying error text, which holds
// paths and the server's own words. The legacy redactErrorMessage chose a
// stub by substring, so a server body that said "timeout" steered it.
func TestClassifyAttemptMessageIgnoresErrorText(t *testing.T) {
	now := time.Now()
	row500 := []*recordedRequest{{Ordinal: 1, StartedAt: now, HeadersAt: now, EndedAt: now, HTTPStatus: 500, ResponseComplete: true}}
	cases := []struct {
		name string
		err  error
		rows []*recordedRequest
		want string
	}{
		{"path in an input error", errors.New("open /Users/someone/secret-trace.json: denied"), nil, "an input to the attempt was invalid"},
		{"timeout in a server body", errors.New("provider: chat: status 500: upstream timeout"), row500, "the server returned a non-2xx status"},
		{"network words in a server body", errors.New("status 500: connection refused, no such host"), row500, "the server returned a non-2xx status"},
		{"parse words in a server body", errors.New("status 500: json: cannot unmarshal parse error"), row500, "the server returned a non-2xx status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAttempt(tc.err, tc.rows, nil); got == nil || got.Message != tc.want {
				t.Fatalf("got %+v, want message %q", got, tc.want)
			}
		})
	}
}
