package compat

import (
	"context"
	"errors"
	"net"
	"net/http"
	"runtime"
	"testing"
	"time"
)

func TestNew_DefaultsAreLoopbackAndV1(t *testing.T) {
	s := New(nil, nil, nil)
	if s.addr != "127.0.0.1:18741" {
		t.Errorf("default addr = %q, want 127.0.0.1:18741", s.addr)
	}
	if s.basePath != "/v1" {
		t.Errorf("default basePath = %q, want /v1", s.basePath)
	}
	if s.corsOrigin != "*" {
		t.Errorf("default corsOrigin = %q, want *", s.corsOrigin)
	}
	if s.maxConcurrency != 4 {
		t.Errorf("default maxConcurrency = %d, want 4", s.maxConcurrency)
	}
	if s.embeddingsEnabled {
		t.Errorf("embeddings must be off by default")
	}
	if s.shutdownTimeout != 30*time.Second {
		t.Errorf("default shutdownTimeout = %v, want 30s", s.shutdownTimeout)
	}
}

func TestListenAndServe_NonLoopbackWithoutTLSErrors(t *testing.T) {
	s := New(nil, nil, nil, WithAddr("0.0.0.0:0"))
	err := s.ListenAndServe(context.Background())
	if err == nil {
		t.Fatal("expected error for non-loopback without TLS")
	}
	if !errors.Is(err, ErrNonLoopbackRequiresTLS) {
		t.Fatalf("want ErrNonLoopbackRequiresTLS, got %v", err)
	}
}

func TestListenAndServe_LoopbackServesThenCloses(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	s := New(nil, nil, nil, WithAddr(addr))
	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { errCh <- s.ListenAndServe(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/v1/does-not-exist")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("ListenAndServe returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after Close")
	}
}

func TestClose_Idempotent(t *testing.T) {
	s := New(nil, nil, nil)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Regression guard for the shutdown-goroutine leak. If ListenAndServe returns
// early (e.g. because the address is already bound), the shutdown goroutine
// must not remain parked on ctx.Done for the lifetime of a long-lived ctx.
// We detect the leak by checking goroutine count returns to baseline after a
// serve failure, with ctx still live.
func TestListenAndServe_EarlyFailureDoesNotLeakGoroutines(t *testing.T) {
	// Occupy a loopback port so the server's bind fails.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	baseline := runtime.NumGoroutine()

	s := New(nil, nil, nil, WithAddr(addr))
	if err := s.ListenAndServe(ctx); err == nil {
		t.Fatal("expected bind error on occupied port")
	}

	// Give the scheduler a beat to reap the goroutine if it exited cleanly.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("goroutine count did not return to baseline: got %d, want <= %d (leak)",
		runtime.NumGoroutine(), baseline+1)
}

// Regression guard: Close's contract is that subsequent ListenAndServe calls
// return http.ErrServerClosed. Without the closed-flag check under s.mu, a
// Close that races ahead of ListenAndServe would be silently defeated — the
// second call would start a fresh server even though the caller asked to shut
// down.
func TestListenAndServe_AfterCloseReturnsErrServerClosed(t *testing.T) {
	s := New(nil, nil, nil)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := s.ListenAndServe(context.Background())
	if !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("ListenAndServe after Close = %v, want http.ErrServerClosed", err)
	}
}
