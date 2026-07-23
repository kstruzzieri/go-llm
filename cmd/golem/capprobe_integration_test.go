//go:build integration

// Integration coverage for the tool-capability probe against a real
// OpenAI-compatible server (llama.cpp llama-server, vLLM, LM Studio).
//
// Requires a running server. Configure via environment:
//
//	GO_LLM_TEST_OPENAI_BASE    server root, no /v1 (e.g. http://127.0.0.1:8091)
//	GO_LLM_TEST_TOOL_MODEL     a model the server lists that supports tool calls
//	GO_LLM_TEST_NOTOOL_MODEL   (optional) a listed model that does NOT
//
// When GO_LLM_TEST_OPENAI_BASE / GO_LLM_TEST_TOOL_MODEL are unset the whole
// file is skipped, so a plain `go test ./...` without a server is unaffected.
// Run with: go test -tags integration ./cmd/golem/
package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/fingerprint/probers"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
	_ "modernc.org/sqlite"
)

// newInMemoryCapProbeStore opens a fresh in-memory SQLite fingerprint store,
// which satisfies fingerprint.CapProbeStore. Clamped to one connection because
// each :memory: connection is a separate database.
func newInMemoryCapProbeStore(t *testing.T, ctx context.Context) *fingerprint.SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	store, err := fingerprint.NewStore(ctx, db)
	if err != nil {
		t.Fatalf("fingerprint.NewStore: %v", err)
	}
	return store
}

// capIntegrationEnv reads and validates the server env, skipping the test when
// the required vars are absent.
type capIntegrationEnv struct {
	base        string
	toolModel   string
	notoolModel string
}

func capIntegrationEnvOrSkip(t *testing.T) capIntegrationEnv {
	t.Helper()
	base := os.Getenv("GO_LLM_TEST_OPENAI_BASE")
	toolModel := os.Getenv("GO_LLM_TEST_TOOL_MODEL")
	if base == "" || toolModel == "" {
		t.Skip("set GO_LLM_TEST_OPENAI_BASE and GO_LLM_TEST_TOOL_MODEL to run the capability-probe integration test")
	}
	return capIntegrationEnv{
		base:        strings.TrimRight(base, "/"),
		toolModel:   toolModel,
		notoolModel: os.Getenv("GO_LLM_TEST_NOTOOL_MODEL"),
	}
}

// TestProbeToolCall_LiveOpenAICompat drives the raw prober against the live
// server: the tool-capable model must probe "yes"; an optional non-tool model
// must probe "no" or "inconclusive" (never a false "yes").
func TestProbeToolCall_LiveOpenAICompat(t *testing.T) {
	env := capIntegrationEnvOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	prober := probers.NewOpenAICompatProber(openaicompat.NewProvider(openaicompat.NewClient(env.base)))

	outcome, err := prober.ProbeToolCall(ctx, env.toolModel)
	if err != nil {
		t.Fatalf("ProbeToolCall(%q) error: %v", env.toolModel, err)
	}
	if outcome.State != fingerprint.CapProbeYes {
		t.Fatalf("tool model %q probe state = %q, want yes (detail: %q)", env.toolModel, outcome.State, outcome.Detail)
	}

	if env.notoolModel == "" {
		t.Logf("GO_LLM_TEST_NOTOOL_MODEL unset; skipping negative assertion")
		return
	}
	outcome, err = prober.ProbeToolCall(ctx, env.notoolModel)
	if err != nil {
		t.Fatalf("ProbeToolCall(%q) error: %v", env.notoolModel, err)
	}
	switch outcome.State {
	case fingerprint.CapProbeNo, fingerprint.CapProbeInconclusive:
		// Expected: the model must not falsely claim tool support.
	default:
		t.Fatalf("non-tool model %q probe state = %q, want no or inconclusive (detail: %q)", env.notoolModel, outcome.State, outcome.Detail)
	}
}

// countingProxy fronts the real server with a reverse proxy that counts the
// upstream chat-completions requests, so a cache hit (zero additional chat
// requests) is observable.
type countingProxy struct {
	srv      *httptest.Server
	chatReqs atomic.Int64
}

func newCountingProxy(t *testing.T, base string) *countingProxy {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base url %q: %v", base, err)
	}
	cp := &countingProxy{}
	rp := httputil.NewSingleHostReverseProxy(target)
	cp.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			cp.chatReqs.Add(1)
		}
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(cp.srv.Close)
	return cp
}

func (cp *countingProxy) chatCount() int64 { return cp.chatReqs.Load() }

// TestResolveToolCall_LiveCacheHit exercises the full registry resolve path
// through a recording proxy: the first ResolveToolCall probes (>=1 upstream
// chat request), the verdict persists to a temp fingerprint store, and the
// second ResolveToolCall is a cache hit (0 additional upstream chat requests).
func TestResolveToolCall_LiveCacheHit(t *testing.T) {
	env := capIntegrationEnvOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	proxy := newCountingProxy(t, env.base)

	const providerName = "openai-local"
	prov := openaicompat.NewProvider(
		openaicompat.NewClient(proxy.srv.URL),
		openaicompat.WithProviderName(providerName),
	)

	pReg := provider.NewRegistry()
	if err := pReg.Register(prov); err != nil {
		t.Fatalf("Register(%q): %v", providerName, err)
	}
	if err := pReg.RefreshModels(ctx, providerName); err != nil {
		t.Fatalf("RefreshModels(%q): %v", providerName, err)
	}

	// A temp in-memory fingerprint store doubles as the cap-probe store, as in
	// providerbootstrap (the SQLite store satisfies both interfaces).
	store := newInMemoryCapProbeStore(t, ctx)

	// The prober factory returns a real OpenAICompatProber wired to the same
	// proxied provider. ModelDigest is left empty so the registry falls back to
	// key.String() on both the write and read side (mirrors the openai-compat
	// path, whose runtime info has no digest).
	factory := func(_ context.Context, key provider.ModelKey, _ *provider.ModelInfo, p provider.Provider) (*provider.FingerprintProberSpec, error) {
		oc, ok := p.(*openaicompat.Provider)
		if !ok {
			t.Fatalf("prober factory: provider is %T, want *openaicompat.Provider", p)
		}
		return &provider.FingerprintProberSpec{Prober: probers.NewOpenAICompatProber(oc)}, nil
	}

	mr, err := provider.NewModelRegistry(pReg, store,
		provider.WithCapabilityProbeStore(store),
		provider.WithCapabilityProber(factory),
	)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	key := provider.ModelKey{Provider: providerName, Model: env.toolModel}

	before := proxy.chatCount()
	state, err := mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("first ResolveToolCall(%v): %v", key, err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("first resolve state = %q, want yes", state)
	}
	afterFirst := proxy.chatCount()
	if afterFirst-before < 1 {
		t.Fatalf("first resolve made %d upstream chat requests, want >=1 (probe must hit the server)", afterFirst-before)
	}

	state, err = mr.ResolveToolCall(ctx, key)
	if err != nil {
		t.Fatalf("second ResolveToolCall(%v): %v", key, err)
	}
	if state != fingerprint.CapProbeYes {
		t.Fatalf("second resolve state = %q, want yes", state)
	}
	if got := proxy.chatCount() - afterFirst; got != 0 {
		t.Fatalf("second resolve made %d additional upstream chat requests, want 0 (cache hit)", got)
	}
}
