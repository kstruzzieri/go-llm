package golem_test

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

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// policyHarness serves a local openai-compat backend and writes a config
// with the #477 shape: local agent role, remote analysis role, no summarize
// key — the summarize side-task falls through to the remote.
func policyHarness(t *testing.T) (configPath string, requests *atomic.Int64) {
	cp, reqs, _ := policyHarnessWithBystander(t)
	return cp, reqs
}

// policyHarnessWithBystander adds a configured provider on NO route: a gated
// bootstrap must send it zero requests; an ungated one would refresh it.
func policyHarnessWithBystander(t *testing.T) (configPath string, requests, bystander *atomic.Int64) {
	t.Helper()
	requests = &atomic.Int64{}
	bystander = &atomic.Int64{}
	bystanderSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bystander.Add(1)
		http.NotFound(w, nil)
	}))
	t.Cleanup(bystanderSrv.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	configPath = filepath.Join(t.TempDir(), "models.json")
	cfg := fmt.Sprintf(`{
  "providers": {
    "local":     {"base_url": %q, "api_format": "openai-compat"},
    "remote":    {"base_url": "https://hosted.invalid/api", "api_format": "openai-compat"},
    "bystander": {"base_url": %q, "api_format": "openai-compat"}
  },
  "models": {
    "agent":     {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"]},
    "cloud-pro": {"name": "remote-model", "provider": "remote", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream"]}
  },
  "defaults": {"agent": "agent", "analysis": "cloud-pro"}
}`, srv.URL, bystanderSrv.URL)
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, requests, bystander
}

func localOnlyConfig(t *testing.T, baseURL string) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "models.json")
	cfg := fmt.Sprintf(`{
  "providers": {"local": {"base_url": %q, "api_format": "openai-compat"}},
  "models": {"agent": {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`, baseURL)
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// I17/I18, the D9-a contract: the ZERO policy fails closed for config-driven
// use. Firn's commit-message generator — golem.New with a ConfigPath and no
// Orchestrator — is exactly this call shape: after upgrading it must either
// keep a local-only config or pass an explicit policy.
func TestNewZeroPolicyDeniesRemoteBeforeIO(t *testing.T) {
	configPath, requests := policyHarness(t)

	_, err := golem.New(context.Background(), golem.Options{
		Root:       t.TempDir(),
		ConfigPath: configPath,
	})
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("zero policy with reachable remote = %v, want ErrDestinationDenied", err)
	}
	// Typed error BEFORE any I/O: the local provider saw nothing either.
	if got := requests.Load(); got != 0 {
		t.Errorf("denied construction sent %d requests, want 0", got)
	}
}

// The zero value permits local-only configurations untouched (I20).
func TestNewZeroPolicyLocalOnlyWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	rt, err := golem.New(context.Background(), golem.Options{
		Root:       t.TempDir(),
		ConfigPath: localOnlyConfig(t, srv.URL),
	})
	if err != nil {
		t.Fatalf("zero policy on a local-only config: %v", err)
	}
	_ = rt.Close()
}

// An exact policy admits the named remote; AllowAllDestinations is the
// greppable opt-out. Both construct successfully against a config whose
// remote is reachable-but-dead — admission is consent, not traffic.
func TestNewExplicitPoliciesAdmitRemote(t *testing.T) {
	remote, err := provider.NewDestination("remote", "https://hosted.invalid/api")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		policy provider.DestinationPolicy
	}{
		{"exact set", provider.NewDestinationPolicy(remote)},
		{"allow all", provider.AllowAllDestinations()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath, requests, bystander := policyHarnessWithBystander(t)
			rt, err := golem.New(context.Background(), golem.Options{
				Root:              t.TempDir(),
				ConfigPath:        configPath,
				DestinationPolicy: tc.policy,
			})
			if err != nil {
				t.Fatalf("explicit policy: %v", err)
			}
			defer func() { _ = rt.Close() }()
			if requests.Load() == 0 {
				t.Error("local provider never refreshed; bootstrap cannot have run")
			}
			// The bundle is GATED: the configured bystander is on no route
			// and must see zero requests even under allow-all — the policy
			// admits destinations, the PLAN decides what is dialed.
			if got := bystander.Load(); got != 0 {
				t.Errorf("bystander received %d requests, want 0 (ungated bootstrap?)", got)
			}
		})
	}
}

// A nonzero policy alongside a caller-supplied Orchestrator protects no
// repository-owned transport: construction fails rather than implying
// protection that does not exist (D9).
func TestNewPolicyWithCustomOrchestratorRefused(t *testing.T) {
	remote, err := provider.NewDestination("remote", "https://hosted.invalid/api")
	if err != nil {
		t.Fatal(err)
	}
	orch := agent.New(nil, agent.ContextManager{})

	for _, tc := range []struct {
		name   string
		policy provider.DestinationPolicy
	}{
		// Both nonzero shapes must be refused: an inverted zero-check that
		// only caught the exact-set form would wave allow-all through.
		{"exact set", provider.NewDestinationPolicy(remote)},
		{"allow all", provider.AllowAllDestinations()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := golem.New(context.Background(), golem.Options{
				Root:              t.TempDir(),
				Orchestrator:      orch,
				DestinationPolicy: tc.policy,
			})
			if !errors.Is(err, golem.ErrDestinationPolicyIneffective) {
				t.Fatalf("nonzero policy with custom orchestrator = %v, want ErrDestinationPolicyIneffective", err)
			}
		})
	}
}

// A custom Orchestrator with the ZERO policy keeps working untouched —
// Firn's main runtime path (host-owned transports, host-owned consent).
func TestNewCustomOrchestratorZeroPolicyUnchanged(t *testing.T) {
	orch := agent.New(nil, agent.ContextManager{})
	rt, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		Orchestrator: orch,
	})
	if err != nil {
		t.Fatalf("custom orchestrator with zero policy: %v", err)
	}
	_ = rt.Close()
}
