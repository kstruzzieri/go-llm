package opsbackend

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestClassify(t *testing.T) {
	live := context.Background()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: "http://127.0.0.1:1/x?secret=1", Err: err} }
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want Code
		ok   bool
	}{
		{"refused", live, wrap(ErrRefused), CodeRefused, true},
		{"denied", live, wrap(fmt.Errorf("%w: redirect", provider.ErrDestinationDenied)), CodeDenied, true},
		// A refusal is a console bug and outranks a gate denial in the same chain.
		{"refused beats denied", live, wrap(fmt.Errorf("%w: %w", ErrRefused, provider.ErrDestinationDenied)), CodeRefused, true},
		{"deadline", live, wrap(context.DeadlineExceeded), CodeTimeout, true},
		{"net timeout", live, wrap(timeoutErr{}), CodeTimeout, true},
		{"dial", live, wrap(errors.New("connect: connection refused")), CodeUnreachable, true},
		{"coded passthrough", live, newCoded(CodeMalformed, "running"), CodeMalformed, true},
		// The inner error has no Timeout method, so only the DeadlineExceeded arm codes it.
		{"wrapped deadline", live, &url.Error{Op: "Get", URL: "http://127.0.0.1:1/x", Err: fmt.Errorf("read body: %w", context.DeadlineExceeded)}, CodeTimeout, true},
		{"caller canceled", canceled, wrap(context.Canceled), "", false},
		// A tick deadline is an outage, not caller cancellation.
		{"tick deadline", expired, wrap(context.DeadlineExceeded), CodeTimeout, true},
		// Cancellation is judged by the caller's context, not by the error.
		{"canceled ctx, dial err", canceled, wrap(errors.New("connect: connection refused")), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classify(tc.ctx, "running", tc.err)
			code, ok := CodeOf(err)
			if code != tc.want || ok != tc.ok {
				t.Fatalf("CodeOf = %q, %v; want %q, %v", code, ok, tc.want, tc.ok)
			}
			if tc.ok && strings.Contains(err.Error(), "secret") {
				t.Fatalf("classified error leaked the request URL: %q", err)
			}
		})
	}
}
