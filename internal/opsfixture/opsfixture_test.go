package opsfixture

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLlamaSwapRecordsEveryRequestAndCountsDispatch(t *testing.T) {
	f := NewLlamaSwap(t)
	for _, p := range []string{"/api/version", "/running", "/props?model=x", "/upstream/m/props", "/nope"} {
		resp, err := http.Get(f.URL() + p)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	got := f.Requests()
	want := []Request{{"GET", "/api/version"}, {"GET", "/running"}, {"GET", "/props?model=x"}, {"GET", "/upstream/m/props"}, {"GET", "/nope"}}
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("request %d = %v, want %v", i, got[i], want[i])
		}
	}
	if n := f.Loads.Load(); n != 2 {
		t.Fatalf("would-be loads = %d, want 2 (/props and /upstream)", n)
	}
}

func TestLlamaSwapStatusOverrideCarriesSentinel(t *testing.T) {
	f := NewLlamaSwap(t)
	f.SetStatus("/api/metrics", http.StatusInternalServerError)
	resp, err := http.Get(f.URL() + "/api/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 500 || !strings.Contains(string(body), SentinelError) {
		t.Fatalf("status/body = %d %q", resp.StatusCode, body)
	}
}

func TestHeldDispatchCountsAsLoadWhenClientGivesUp(t *testing.T) {
	f := NewLlamaSwap(t)
	f.HoldFor(t, "/props")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.URL()+"/props?model=x", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for f.InflightOn("/props") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	for f.InflightOn("/props") != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := f.Loads.Load(); n != 1 {
		t.Fatalf("would-be loads = %d, want 1: a dispatch request counts when received, even if never answered", n)
	}
}

func TestOllamaStatusOverrideWithoutBody(t *testing.T) {
	f := NewOllama(t, `{"models":[]}`)
	f.SetStatus("/api/version", http.StatusServiceUnavailable)
	for _, tc := range []struct {
		path   string
		status int
		want   string
	}{
		{"/api/ps", 200, `{"models":[]}`},
		{"/api/version", 503, SentinelError},
	} {
		resp, err := http.Get(f.URL() + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || !strings.Contains(string(body), tc.want) {
			t.Fatalf("%s: status/body = %d %q, want %d containing %q", tc.path, resp.StatusCode, body, tc.status, tc.want)
		}
	}
}
