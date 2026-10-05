package opsbackend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/opsfixture"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestRootDestinationRefusesPrefixesBeforeIO(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:8090/upstream/gemma4:31b",
		"http://127.0.0.1:8090/v1",
		"http://user@127.0.0.1:8090",
		"http://127.0.0.1:8090/?x=1",
		"ftp://127.0.0.1:8090",
		"",
	} {
		if _, err := rootDestination("p", base); !isCode(err, CodeInvalidConfiguration) {
			t.Fatalf("rootDestination(%q) = %v, want invalid_configuration", base, err)
		}
	}
	for _, base := range []string{"http://127.0.0.1:8090", "http://127.0.0.1:8090/", "http://localhost:11434"} {
		if _, err := rootDestination("p", base); err != nil {
			t.Fatalf("rootDestination(%q) = %v", base, err)
		}
	}
}

func TestClientGetsAllowedPathWithBearer(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	d, err := rootDestination("ls", f.URL())
	if err != nil {
		t.Fatal(err)
	}
	var refused atomic.Int64
	c, err := newClient(d, opsfixture.SentinelAPIKey, provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.get(context.Background(), "version", "/api/version", versionLimit)
	if err != nil || string(body) != opsfixture.DefaultVersion {
		t.Fatalf("get = %q, %v", body, err)
	}
	if got := f.Auth(); len(got) != 1 || got[0] != "Bearer "+opsfixture.SentinelAPIKey {
		t.Fatalf("Authorization = %v", got)
	}
	if _, err := c.get(context.Background(), "props", "/props", versionLimit); !isCode(err, CodeRefused) {
		t.Fatalf("get /props = %v, want refused", err)
	}
	if n := len(f.Requests()); n != 1 {
		t.Fatalf("server saw %d requests, want 1 (refusal must not reach it)", n)
	}
}

func TestClientStatusAndCap(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	d, _ := rootDestination("ls", f.URL())
	var refused atomic.Int64
	c, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	f.SetStatus("/running", http.StatusUnauthorized)
	if _, err := c.get(context.Background(), "running", "/running", runningLimit); !isCode(err, CodeUnauthorized) {
		t.Fatalf("401 = %v", err)
	}
	f.SetStatus("/v1/models", http.StatusForbidden)
	if _, err := c.get(context.Background(), "models", "/v1/models", modelsLimit); !isCode(err, CodeUnauthorized) {
		t.Fatalf("403 = %v", err)
	}
	f.SetStatus("/running", http.StatusMultipleChoices) // Location-less 3xx: a plain response, not a redirect
	if _, err := c.get(context.Background(), "running", "/running", runningLimit); !isCode(err, CodeHTTPStatus) {
		t.Fatalf("300 = %v", err)
	}
	f.SetStatus("/api/metrics", http.StatusInternalServerError)
	_, err = c.get(context.Background(), "metrics", "/api/metrics", metricsLimit)
	if !isCode(err, CodeHTTPStatus) {
		t.Fatalf("500 = %v", err)
	}
	if containsAny(err.Error(), opsfixture.AllSentinels) {
		t.Fatalf("error carried body text: %q", err)
	}
	if _, err := c.get(context.Background(), "version", "/api/version", 8); !isCode(err, CodeTooLarge) {
		t.Fatalf("over cap = %v", err)
	}
	exact := int64(len(opsfixture.DefaultVersion))
	if body, err := c.get(context.Background(), "version", "/api/version", exact); err != nil || string(body) != opsfixture.DefaultVersion {
		t.Fatalf("body of exactly the cap = %q, %v; want it returned whole", body, err)
	}
}

func TestClientRefusesRealRedirect(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/running", http.StatusFound) // allowed -> allowed, same origin
	}))
	t.Cleanup(srv.Close)
	d, _ := rootDestination("ls", srv.URL)
	var refused atomic.Int64
	c, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.get(context.Background(), "version", "/api/version", versionLimit)
	if !isCode(err, CodeDenied) || hits.Load() != 1 {
		t.Fatalf("redirect: err=%v hits=%d; want denied and exactly one request", err, hits.Load())
	}
}

func TestRemoteDestinationCannotBuildClient(t *testing.T) {
	// A hosted base with a path (opencode's https://host/zen/go shape) is
	// remote, not invalid_configuration (spec §4.1): never contacted either way.
	for _, base := range []string{"https://api.example.com", "https://api.example.com/zen/go"} {
		d, err := rootDestination("remote", base)
		if err != nil || d.IsLocal() {
			t.Fatalf("rootDestination(%q) = local=%v, %v; want a remote destination", base, d.IsLocal(), err)
		}
		var refused atomic.Int64
		if _, err := newClient(d, "", provider.DestinationPurposeHealth, llamaSwapRoutes, &refused); !isCode(err, CodeDenied) {
			t.Fatalf("remote client for %q = %v, want denied", base, err)
		}
	}
}

func isCode(err error, want Code) bool {
	code, ok := CodeOf(err)
	return ok && code == want && !errors.Is(err, context.Canceled)
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
