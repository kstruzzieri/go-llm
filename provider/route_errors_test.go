package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
)

type testStatusCodeError int

func (e testStatusCodeError) Error() string {
	return fmt.Sprintf("HTTP %d", int(e))
}

func (e testStatusCodeError) HTTPStatusCode() int {
	return int(e)
}

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
		{"status-code-interface-429", testStatusCodeError(429), ErrorClassRateLimit, AttemptStatusFailed},
		{"wrapped-status-code-interface-503", fmt.Errorf("wrapped: %w", testStatusCodeError(503)), ErrorClass5xx, AttemptStatusFailed},
		{"status-code-interface-404", testStatusCodeError(404), ErrorClass4xx, AttemptStatusFailed},
		{"wrapped-ollama-api-error-429", fmt.Errorf("provider: ollama: chat: %w", &ollama.APIError{StatusCode: 429}), ErrorClassRateLimit, AttemptStatusFailed},
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

func TestMakeAttemptHappyPath(t *testing.T) {
	key := ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}
	got := makeAttempt(key, nil, 820*time.Millisecond)
	if got.Key != key {
		t.Errorf("Key = %v, want %v", got.Key, key)
	}
	if got.Status != AttemptStatusSucceeded {
		t.Errorf("Status = %v, want Succeeded", got.Status)
	}
	if got.LatencyMs != 820 {
		t.Errorf("LatencyMs = %d, want 820", got.LatencyMs)
	}
	if got.ErrorClass != "" {
		t.Errorf("ErrorClass = %q, want empty", got.ErrorClass)
	}
}

func TestMakeAttemptFailedNetwork(t *testing.T) {
	key := ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}
	err := &net.OpError{Op: "dial", Err: errors.New("refused")}
	got := makeAttempt(key, err, 200*time.Millisecond)
	if got.Status != AttemptStatusFailed {
		t.Errorf("Status = %v, want Failed", got.Status)
	}
	if got.LatencyMs != 200 {
		t.Errorf("LatencyMs = %d, want 200", got.LatencyMs)
	}
	if got.ErrorClass != string(ErrorClassNetwork) {
		t.Errorf("ErrorClass = %q, want %q", got.ErrorClass, ErrorClassNetwork)
	}
}

func TestMakeAttemptCanceled(t *testing.T) {
	key := ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}
	got := makeAttempt(key, context.Canceled, 100*time.Millisecond)
	if got.Status != AttemptStatusUnknown {
		t.Errorf("Status = %v, want Unknown", got.Status)
	}
	if got.LatencyMs != 100 {
		t.Errorf("LatencyMs = %d, want 100", got.LatencyMs)
	}
	if got.ErrorClass != "" {
		t.Errorf("ErrorClass = %q, want empty (Canceled gets no class)", got.ErrorClass)
	}
}

func TestMakeAttemptClampsNegativeDuration(t *testing.T) {
	key := ModelKey{Provider: "ollama-a", Model: "qwen3:8b"}
	got := makeAttempt(key, nil, -1*time.Second)
	if got.LatencyMs != 0 {
		t.Errorf("LatencyMs = %d, want 0 (clamped)", got.LatencyMs)
	}
}
