package configio

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCodeOf(t *testing.T) {
	base := errors.New("dial tcp 127.0.0.1:11434: connection refused")
	wrapped := wrapCode(CodeProbeFailed, fmt.Errorf("configio: probe tool_call ollama/m: %w", base))

	code, ok := CodeOf(wrapped)
	if !ok || code != CodeProbeFailed {
		t.Fatalf("CodeOf(wrapped) = %q, %v; want %q, true", code, ok, CodeProbeFailed)
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("errors.Is(wrapped, base) = false; wrapper broke the chain")
	}
	want := "configio: probe tool_call ollama/m: dial tcp 127.0.0.1:11434: connection refused"
	if wrapped.Error() != want {
		t.Fatalf("wrapped.Error() = %q; want %q", wrapped.Error(), want)
	}
	if code, ok := CodeOf(errors.New("plain")); ok || code != "" {
		t.Fatalf("CodeOf(plain) = %q, %v; want \"\", false", code, ok)
	}
	if code, ok := CodeOf(nil); ok || code != "" {
		t.Fatalf("CodeOf(nil) = %q, %v; want \"\", false", code, ok)
	}
}

func TestCancellationIsUnclassified(t *testing.T) {
	if code, ok := CodeOf(context.Canceled); ok || code != "" {
		t.Fatalf("CodeOf(context.Canceled) = %q, %v; cancellation must stay unclassified", code, ok)
	}
	if code, ok := CodeOf(fmt.Errorf("op: %w", context.DeadlineExceeded)); ok || code != "" {
		t.Fatalf("CodeOf(wrapped deadline) = %q, %v; cancellation must stay unclassified", code, ok)
	}
}

func TestCheckCodes_AllTyped(t *testing.T) {
	// Each value goes into an interface as declared: a constant written
	// CodeX = "x", without the type, is an untyped string constant, arrives as
	// a plain string and fails.
	cases := []struct {
		code any
		want string
	}{
		{CodeAuthFailed, "auth_failed"},
		{CodeModelRefused, "model_refused"},
		{CodeRefused, "refused"},
		{CodeRateLimited, "rate_limited"},
		{CodeTimeout, "timeout"},
		{CodeUnreachable, "unreachable"},
		{CodeServerError, "server_error"},
		{CodeRejected, "rejected"},
		{CodeMalformedResponse, "malformed_response"},
		{CodeDestinationNotGranted, "destination_not_granted"},
		{CodePlanStale, "plan_stale"},
		{CodeListingRequired, "listing_required"},
		{CodeBatchTooLarge, "batch_too_large"},
		{CodeResponseTooLarge, "response_too_large"},
		{CodeInvalidArgument, "invalid_argument"},
	}
	for _, tc := range cases {
		code, ok := tc.code.(ErrorCode)
		if !ok {
			t.Errorf("%v is a %T; want ErrorCode", tc.code, tc.code)
			continue
		}
		if string(code) != tc.want {
			t.Errorf("code = %q; want %q", code, tc.want)
		}
	}
}
