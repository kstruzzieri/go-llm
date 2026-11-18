package configio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/responsebound"
	"github.com/kstruzzieri/go-llm/provider"
)

// ccStatus is an error carrying only an HTTP status.
type ccStatus int

func (s ccStatus) Error() string       { return fmt.Sprintf("status %d", int(s)) }
func (s ccStatus) HTTPStatusCode() int { return int(s) }

// ccTimeout is a net.Error that timed out.
type ccTimeout struct{}

func (ccTimeout) Error() string   { return "i/o timeout" }
func (ccTimeout) Timeout() bool   { return true }
func (ccTimeout) Temporary() bool { return true }

func ccURL(err error) error {
	return &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1/chat/completions", Err: err}
}

// TestCheckCodes_Table: every branch of the classification, by status code or
// error type. Statuses go through the real clients against a stub, each with a
// body whose text names another code; types are built directly.
func TestCheckCodes_Table(t *testing.T) {
	const marker = "SECRET-MARKER"
	statuses := []struct {
		status int
		body   string
		want   ErrorCode
	}{
		// One status, two misleading bodies: one code.
		{401, `{"error":{"message":"rate limit exceeded; server error; timeout"}}`, CodeAuthFailed},
		{401, `429 Too Many Requests`, CodeAuthFailed},
		{403, `{"error":{"message":"unauthorized"}}`, CodeRefused},
		{404, `{"error":"rate limited"}`, CodeRefused},
		// One body, two statuses: two codes.
		{429, `{"error":{"message":"invalid api key"}}`, CodeRateLimited},
		{500, `{"error":{"message":"invalid api key"}}`, CodeServerError},
		{409, `{"error":{"message":"overloaded"}}`, CodeRejected},
		{503, marker, CodeServerError},
		{300, `{"error":{"message":"unauthorized"}}`, CodeMalformedResponse},
	}
	for _, format := range []string{"openai-compat", "ollama"} {
		for _, tt := range statuses {
			t.Run(fmt.Sprintf("%s %d %.12s", format, tt.status, tt.body), func(t *testing.T) {
				stub := newRCStub(t, rcStatus(tt.status, tt.body))
				cfg := rcConfig("p", stub.srv.URL, format, cpKey, "m")
				plan := cpPlan(t, cfg, nil, "p", []string{"m"}, cpTools, false)
				res, err := rcRun(t, plan, RunOptions{Config: cfg})
				if err != nil || len(res) != 1 {
					t.Fatalf("RunCheck = %+v, %v; want one result", res, err)
				}
				if res[0].Code != tt.want || len(res[0].Answers) != 0 || res[0].Persisted {
					t.Fatalf("result %+v; want %q, nothing answered or saved", res[0], tt.want)
				}
				shown, _ := json.Marshal(res[0])
				for _, text := range []string{fmt.Sprintf("%+v", res[0]), string(shown)} {
					if strings.Contains(text, marker) || strings.Contains(text, "api key") {
						t.Fatalf("result carries provider text: %s", text)
					}
				}
			})
		}
	}

	types := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{"unreadable rejection keeps its status", &responsebound.RejectionReadError{StatusCode: 403, Err: io.ErrUnexpectedEOF}, CodeRefused},
		{"unreadable 401", &responsebound.RejectionReadError{StatusCode: 401, Err: io.ErrUnexpectedEOF}, CodeAuthFailed},
		{"unreadable 5xx", &responsebound.RejectionReadError{StatusCode: 502, Err: io.ErrUnexpectedEOF}, CodeServerError},
		{"a status beats its cause", &responsebound.RejectionReadError{StatusCode: 429, Err: ccURL(context.DeadlineExceeded)}, CodeRateLimited},
		{"a 3xx status", fmt.Errorf("x: %w", ccStatus(304)), CodeMalformedResponse},
		{"over the byte limit", fmt.Errorf("decode: %w", responsebound.ErrExceeded), CodeResponseTooLarge},
		{"a destination denial", ccURL(&provider.DestinationDeniedError{Provider: "p", Purpose: "capability-probe"}), CodeDestinationNotGranted},
		{"a refused redirect", ccURL(fmt.Errorf("%w: redirect refused by destination guard", provider.ErrDestinationDenied)), CodeMalformedResponse},
		{"a transport timeout", ccURL(context.DeadlineExceeded), CodeTimeout},
		{"a refused connection", ccURL(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}), CodeUnreachable},
		{"a hang-up", ccURL(io.EOF), CodeUnreachable},
		{"a deadline mid-read", fmt.Errorf("read: %w", context.DeadlineExceeded), CodeTimeout},
		{"a net timeout", fmt.Errorf("read: %w", ccTimeout{}), CodeTimeout},
		{"a reset mid-read", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, CodeUnreachable},
		{"bad JSON", fmt.Errorf("decode: %w", &json.SyntaxError{}), CodeMalformedResponse},
		{"mistyped JSON", fmt.Errorf("decode: %w", &json.UnmarshalTypeError{}), CodeMalformedResponse},
		{"an empty reply", io.EOF, CodeMalformedResponse},
		{"a cut reply", io.ErrUnexpectedEOF, CodeMalformedResponse},
		{"an incomplete stream", errors.New("stream ended without a final response"), CodeMalformedResponse},
		{"text never decides", errors.New("401 Unauthorized: 429 rate limited, timeout, unreachable"), CodeMalformedResponse},
	}
	for _, tt := range types {
		t.Run(tt.name, func(t *testing.T) {
			if got := checkCode(tt.err); got != tt.want {
				t.Fatalf("checkCode(%v) = %q; want %q", tt.err, got, tt.want)
			}
		})
	}
}
