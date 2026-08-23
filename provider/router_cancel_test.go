// provider/router_cancel_test.go
//
// Route-level cancellation propagation (#401): when the caller's context
// ends while capability probes are resolving, Route returns the raw
// ctx.Err() instead of classifying the route as ErrNoViableCandidate.
// Diagnostic joins stay stringified (%s); ordinary probe failures still
// classify as ErrNoViableCandidate.
package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// ctxErrToolCallProber's probes fail with the context's error as soon as
// the context is done -- the shape a real transport probe takes when the
// caller cancels or the deadline fires mid-probe.
type ctxErrToolCallProber struct {
	*fakeToolCallProber
}

func newCtxErrToolCallProber() *ctxErrToolCallProber {
	return &ctxErrToolCallProber{fakeToolCallProber: newFakeToolCallProber()}
}

func (p *ctxErrToolCallProber) ProbeToolCall(ctx context.Context, model string) (fingerprint.CapProbeOutcome, error) {
	p.mu.Lock()
	p.calls[model]++
	p.mu.Unlock()
	<-ctx.Done()
	return fingerprint.CapProbeOutcome{}, ctx.Err()
}

func TestRoute_ContextEndPropagatesFromProbeResolution(t *testing.T) {
	canceled := func(t *testing.T) (context.Context, error) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx, context.Canceled
	}
	deadline := func(t *testing.T) (context.Context, error) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
		t.Cleanup(cancel)
		return ctx, context.DeadlineExceeded
	}

	direct := RoutingRequest{
		UseCase:      "chat",
		RequiredCaps: CapChat | CapToolCall,
		Model:        "p/a",
		Messages:     []ChatMessage{{Role: "user", Content: "hi"}},
	}
	chain := RoutingRequest{
		UseCase:        "chat",
		RequiredCaps:   CapChat | CapToolCall,
		PreferredChain: []string{"p/a"},
		StrictChain:    true,
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
	}

	cases := []struct {
		name string
		ctx  func(t *testing.T) (context.Context, error)
		req  RoutingRequest
	}{
		{"canceled direct", canceled, direct},
		{"deadline direct", deadline, direct},
		{"canceled strict chain", canceled, chain},
		{"deadline strict chain", deadline, chain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prober := newCtxErrToolCallProber()
			r := newChainProbeRouter(t, prober, "a")

			ctx, wantErr := tc.ctx(t)
			_, err := r.Route(ctx, tc.req)
			if !errors.Is(err, wantErr) {
				t.Fatalf("Route() error = %v, want errors.Is %v", err, wantErr)
			}
			if errors.Is(err, ErrNoViableCandidate) {
				t.Fatalf("Route() error = %v, must not classify a dead context as no-viable-candidate", err)
			}
		})
	}
}

func TestRoute_ContextEndWinsCandidateResolutionError(t *testing.T) {
	cases := []struct {
		name    string
		context func(t *testing.T) (context.Context, error)
	}{
		{"canceled", func(t *testing.T) (context.Context, error) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, context.Canceled
		}},
		{"deadline", func(t *testing.T) (context.Context, error) {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
			t.Cleanup(cancel)
			return ctx, context.DeadlineExceeded
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, wantErr := tc.context(t)
			providers := NewRegistry()
			if err := providers.Register(&mrMockProvider{name: "p", modelsErr: wantErr}); err != nil {
				t.Fatalf("Register() error: %v", err)
			}
			registry, err := NewModelRegistry(providers, nil)
			if err != nil {
				t.Fatalf("NewModelRegistry() error: %v", err)
			}
			router := NewRouter(registry, providers)
			cleanupRouter(t, router)

			_, err = router.Route(ctx, RoutingRequest{
				UseCase:      "chat",
				RequiredCaps: CapChat,
				Messages:     []ChatMessage{{Role: "user", Content: "hi"}},
			})
			if !errors.Is(err, wantErr) {
				t.Fatalf("Route() error = %v, want errors.Is %v", err, wantErr)
			}
		})
	}
}

func TestRoute_OrdinaryProbeFailureStillClassifiesNoViable(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  RoutingRequest
	}{
		{"direct", RoutingRequest{
			UseCase:      "chat",
			RequiredCaps: CapChat | CapToolCall,
			Model:        "p/a",
			Messages:     []ChatMessage{{Role: "user", Content: "hi"}},
		}},
		{"strict chain", RoutingRequest{
			UseCase:        "chat",
			RequiredCaps:   CapChat | CapToolCall,
			PreferredChain: []string{"p/a"},
			StrictChain:    true,
			Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prober := newFakeToolCallProber()
			prober.errs["a"] = errors.New("401 unauthorized")
			r := newChainProbeRouter(t, prober, "a")

			_, err := r.Route(context.Background(), tc.req)
			if !errors.Is(err, ErrNoViableCandidate) {
				t.Fatalf("Route() error = %v, want ErrNoViableCandidate", err)
			}
			if !strings.Contains(err.Error(), "401 unauthorized") {
				t.Fatalf("Route() error %q does not carry the probe diagnostic", err)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("Route() error = %v, must not read as cancellation", err)
			}
		})
	}
}
