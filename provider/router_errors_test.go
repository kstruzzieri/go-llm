package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestIsInfrastructureError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			name: "net.OpError dial",
			err: &net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: errors.New("connection refused"),
			},
			want: true,
		},
		{
			name: "context.DeadlineExceeded",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "context.Canceled",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "HTTP 500",
			err:  &HTTPStatusError{StatusCode: 500, Status: "500 Internal Server Error"},
			want: true,
		},
		{
			name: "HTTP 502",
			err:  &HTTPStatusError{StatusCode: 502, Status: "502 Bad Gateway"},
			want: true,
		},
		{
			name: "HTTP 503",
			err:  &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"},
			want: true,
		},
		{
			name: "HTTP 400",
			err:  &HTTPStatusError{StatusCode: 400, Status: "400 Bad Request"},
			want: false,
		},
		{
			name: "HTTP 404",
			err:  &HTTPStatusError{StatusCode: 404, Status: "404 Not Found"},
			want: false,
		},
		{
			name: "HTTP 429",
			err:  &HTTPStatusError{StatusCode: 429, Status: "429 Too Many Requests"},
			want: true,
		},
		{
			name: "wrapped HTTP 503",
			err:  fmt.Errorf("provider error: %w", &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}),
			want: true,
		},
		{
			name: "wrapped context.Canceled",
			err:  fmt.Errorf("request failed: %w", context.Canceled),
			want: false,
		},
		{
			name: "generic error",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "connection reset net.OpError read",
			err: &net.OpError{
				Op:  "read",
				Net: "tcp",
				Err: errors.New("connection reset by peer"),
			},
			want: true,
		},
		{
			name: "net.DNSError",
			err:  &net.DNSError{Err: "no such host", Name: "example.com"},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsInfrastructureError(tt.err)
			if got != tt.want {
				t.Errorf("IsInfrastructureError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestSentinelErrors(t *testing.T) {
	sentinels := []struct {
		name string
		err  error
	}{
		{"ErrNoViableCandidate", ErrNoViableCandidate},
		{"ErrAllBreakersOpen", ErrAllBreakersOpen},
		{"ErrBudgetAdaptationRequired", ErrBudgetAdaptationRequired},
		{"ErrBudgetExceeded", ErrBudgetExceeded},
		{"ErrRouterClosed", ErrRouterClosed},
	}

	// Each sentinel should match itself.
	for _, s := range sentinels {
		t.Run(s.name+" matches itself", func(t *testing.T) {
			if !errors.Is(s.err, s.err) {
				t.Errorf("errors.Is(%v, %v) = false, want true", s.err, s.err)
			}
		})
	}

	// Each sentinel should be distinct from all others.
	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			t.Run(a.name+" distinct from "+b.name, func(t *testing.T) {
				if errors.Is(a.err, b.err) {
					t.Errorf("errors.Is(%v, %v) = true, want false", a.err, b.err)
				}
			})
		}
	}

	// Wrapping preserves matching.
	for _, s := range sentinels {
		t.Run("wrapped "+s.name, func(t *testing.T) {
			wrapped := fmt.Errorf("router: %w", s.err)
			if !errors.Is(wrapped, s.err) {
				t.Errorf("errors.Is(wrapped, %v) = false, want true", s.err)
			}
		})
	}
}

func TestBreakerStateString(t *testing.T) {
	tests := []struct {
		name  string
		state BreakerState
		want  string
	}{
		{name: "closed", state: BreakerClosed, want: "closed"},
		{name: "open", state: BreakerOpen, want: "open"},
		{name: "half-open", state: BreakerHalfOpen, want: "half-open"},
		{name: "unknown", state: BreakerState(99), want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.state.String()
			if got != tt.want {
				t.Errorf("BreakerState(%d).String() = %q, want %q", tt.state, got, tt.want)
			}
		})
	}
}

func TestHTTPStatusError_Error(t *testing.T) {
	err := &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}
	got := err.Error()
	want := "HTTP 503: 503 Service Unavailable"
	if got != want {
		t.Errorf("HTTPStatusError.Error() = %q, want %q", got, want)
	}
}
