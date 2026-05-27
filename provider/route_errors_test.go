package provider

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantClass  ErrorClass
		wantStatus AttemptStatus
	}{
		{"nil", nil, "", AttemptStatusSucceeded},
		{"canceled", context.Canceled, "", AttemptStatusUnknown},
		{"deadline-exceeded", context.DeadlineExceeded, ErrorClassTimeout, AttemptStatusFailed},
		{"net-op-error", &net.OpError{Op: "dial", Err: errors.New("refused")}, ErrorClassNetwork, AttemptStatusFailed},
		{"net-dns-error", &net.DNSError{Err: "no such host"}, ErrorClassNetwork, AttemptStatusFailed},
		{"http-429", &HTTPStatusError{StatusCode: 429, Status: "429 Too Many Requests"}, ErrorClassRateLimit, AttemptStatusFailed},
		{"http-500", &HTTPStatusError{StatusCode: 500, Status: "500 Internal Server Error"}, ErrorClass5xx, AttemptStatusFailed},
		{"http-503", &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}, ErrorClass5xx, AttemptStatusFailed},
		{"http-599", &HTTPStatusError{StatusCode: 599, Status: "599"}, ErrorClass5xx, AttemptStatusFailed},
		{"http-400", &HTTPStatusError{StatusCode: 400, Status: "400 Bad Request"}, ErrorClass4xx, AttemptStatusFailed},
		{"http-404", &HTTPStatusError{StatusCode: 404, Status: "404 Not Found"}, ErrorClass4xx, AttemptStatusFailed},
		{"http-499", &HTTPStatusError{StatusCode: 499, Status: "499"}, ErrorClass4xx, AttemptStatusFailed},
		{"unknown", errors.New("something else"), ErrorClassUnknown, AttemptStatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotClass, gotStatus := classifyError(tc.err)
			if gotClass != tc.wantClass {
				t.Errorf("class = %q, want %q", gotClass, tc.wantClass)
			}
			if gotStatus != tc.wantStatus {
				t.Errorf("status = %v, want %v", gotStatus, tc.wantStatus)
			}
		})
	}
}

func TestErrorClassModelUnloadedReservedNotProduced(t *testing.T) {
	// model_unloaded is reserved in the vocabulary but the PR2 classifier
	// never produces it — Ollama-specific detection is deferred. Lock the
	// invariant: no input to classifyError should currently return it.
	cases := []error{
		nil,
		context.Canceled,
		context.DeadlineExceeded,
		&net.OpError{Op: "dial", Err: errors.New("refused")},
		&net.DNSError{Err: "no such host"},
		&HTTPStatusError{StatusCode: 429},
		&HTTPStatusError{StatusCode: 500},
		&HTTPStatusError{StatusCode: 404},
		errors.New("something else"),
	}
	for _, err := range cases {
		class, _ := classifyError(err)
		if class == ErrorClassModelUnloaded {
			t.Fatalf("classifyError(%v) returned reserved %q", err, class)
		}
	}
}
