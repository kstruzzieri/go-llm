package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// mcpAdmissionHarness: one LOCAL ollama-format provider (counted) plus one
// REMOTE openai-compat provider in the config.
func mcpAdmissionHarness(t *testing.T) (configPath string, local *atomic.Int64) {
	t.Helper()
	local = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local.Add(1)
		switch r.URL.Path {
		case "/", "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"name":"m1"}]}`)
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"details":{}}`)
		case "/api/ps":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	configPath = filepath.Join(t.TempDir(), "models.json")
	cfg := fmt.Sprintf(`{
  "providers": {
    "ollama": {"base_url": %q, "api_format": "ollama"},
    "hosted": {"base_url": "https://hosted.invalid/api", "api_format": "openai-compat"}
  },
  "models": {
    "chat": {"name": "m1", "provider": "ollama", "type": "dense", "context_window": 32768}
  },
  "defaults": {"chat": "chat"}
}`, srv.URL)
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, local
}

// I18/I21/M22: standalone MCP with a configured remote and the ZERO policy
// fails NewServer with the typed denial — never a prompt, never degraded
// health — and sends zero requests anywhere, local included.
func TestNewServerZeroPolicyDeniesRemote(t *testing.T) {
	configPath, local := mcpAdmissionHarness(t)

	_, err := NewServer(context.Background(), WithConfig(configPath), WithRAGDisabled())
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("NewServer with remote + zero policy = %v, want ErrDestinationDenied", err)
	}
	if got := local.Load(); got != 0 {
		t.Errorf("denied startup sent %d requests to the local provider, want 0", got)
	}
}

// An explicit policy admits the remote; the server starts, health and
// refresh flow through guarded clients (arrival proves the capability
// chain), and the recommend-everything plan refreshes the local provider.
func TestNewServerExplicitPolicyStarts(t *testing.T) {
	configPath, local := mcpAdmissionHarness(t)
	remote, err := provider.NewDestination("hosted", "https://hosted.invalid/api")
	if err != nil {
		t.Fatal(err)
	}

	s, err := NewServer(context.Background(),
		WithConfig(configPath), WithRAGDisabled(),
		WithDestinationPolicy(provider.NewDestinationPolicy(remote)))
	if err != nil {
		t.Fatalf("NewServer with exact policy: %v", err)
	}
	defer func() { _ = s.Close() }()

	if !s.ollamaAvailable {
		t.Error("guarded health probe reported unavailable against a live local backend")
	}
	if local.Load() == 0 {
		t.Error("local provider never contacted; refresh cannot have run")
	}
}

// A local-only config needs no policy at all (I20): the server starts, the
// guarded warmth poller reaches /api/ps, and the models listing works
// through the bound sweep.
func TestNewServerLocalOnlyNoPolicy(t *testing.T) {
	local := &atomic.Int64{}
	psHits := &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local.Add(1)
		switch r.URL.Path {
		case "/", "/api/tags":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[{"name":"m1"}]}`)
		case "/api/show":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"details":{}}`)
		case "/api/ps":
			psHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[]}`)
		case "/api/chat":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"model":"m1","message":{"role":"assistant","content":"ok"},"done":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	configPath := filepath.Join(t.TempDir(), "models.json")
	cfg := fmt.Sprintf(`{
  "providers": {"ollama": {"base_url": %q, "api_format": "ollama"}},
  "models": {"chat": {"name": "m1", "provider": "ollama", "type": "dense", "context_window": 32768}},
  "defaults": {"chat": "chat"}
}`, srv.URL)
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := NewServer(context.Background(), WithConfig(configPath), WithRAGDisabled())
	if err != nil {
		t.Fatalf("local-only NewServer: %v", err)
	}
	defer func() { _ = s.Close() }()

	// The warmth poller fires its first guarded poll on construction.
	deadline := time.Now().Add(5 * time.Second)
	for psHits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if psHits.Load() == 0 {
		t.Error("guarded warmth poller never reached /api/ps")
	}

	// The models listing runs the bound sweep.
	models, err := s.listModels(context.Background())
	if err != nil {
		t.Fatalf("listModels through the guarded sweep: %v", err)
	}
	if len(models) == 0 {
		t.Error("listing returned no models")
	}

	// A SERVED purpose routes end to end — arrival at /api/chat proves the
	// chat edge is in the frozen vocabulary and the capability chain holds.
	router := s.routerSnapshot()
	if router == nil {
		t.Fatal("router unavailable")
	}
	rp, err := router.Route(context.Background(), provider.RoutingRequest{
		UseCase:        "chat",
		Messages:       []provider.ChatMessage{{Role: "user", Content: "hi"}},
		PreferredChain: []string{"ollama/m1"},
		StrictChain:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rp.ExecuteChat(context.Background()); err != nil {
		t.Fatalf("served chat purpose failed: %v", err)
	}
}

// M26-adjacent: a selector routing OUTSIDE the frozen purpose set is denied
// by the gate at the routing layer — the plan is the whole vocabulary.
func TestNewServerUnservedPurposeDenied(t *testing.T) {
	configPath, _ := mcpAdmissionHarness(t)
	remote, err := provider.NewDestination("hosted", "https://hosted.invalid/api")
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(context.Background(),
		WithConfig(configPath), WithRAGDisabled(),
		WithDestinationPolicy(provider.NewDestinationPolicy(remote)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	router := s.routerSnapshot()
	if router == nil {
		t.Fatal("router unavailable")
	}
	rp, err := router.Route(context.Background(), provider.RoutingRequest{
		UseCase:        "summarize", // deliberately outside mcpServedPurposes
		Messages:       []provider.ChatMessage{{Role: "user", Content: "hi"}},
		PreferredChain: []string{"ollama/m1"},
		StrictChain:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rp.ExecuteChat(context.Background())
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("unserved purpose = %v, want ErrDestinationDenied", err)
	}
}
