package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestStatusHandler_ReadyWithProviders(t *testing.T) {
	mp := &mockProvider{name: "ollama", caps: provider.CapChat | provider.CapGenerate}
	srv, teardown := newTestServer(t, mp)
	defer teardown()

	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var resp StatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ready" {
		t.Errorf("status = %q, want ready", resp.Status)
	}
	if len(resp.Providers) != 1 || resp.Providers[0].Name != "ollama" {
		t.Errorf("providers = %+v", resp.Providers)
	}
	// Regression guard: capabilities must JSON-encode as [] not null, so
	// JavaScript clients can safely call .length on it.
	if resp.Providers[0].Capabilities == nil {
		t.Error("Capabilities is nil; want non-nil slice (JSON [] not null)")
	}
	// Regression guard: uptime must be meaningful even when handler runs
	// via buildHandler without ListenAndServe. Parse and sanity-check.
	if resp.Uptime == "" {
		t.Error("Uptime is empty")
	}
	if strings.Contains(resp.Uptime, "h") && strings.HasPrefix(resp.Uptime, "17") {
		t.Errorf("Uptime looks like time.Since(zero): %q", resp.Uptime)
	}
	if d, err := time.ParseDuration(resp.Uptime); err != nil {
		t.Errorf("Uptime not parseable: %q (%v)", resp.Uptime, err)
	} else if d > time.Hour {
		t.Errorf("Uptime unreasonably large: %v", d)
	}
}

func TestStatusHandler_UnavailableWithNoProviders(t *testing.T) {
	// Regression guard for the switch ordering in handleStatus: empty
	// registry must produce "unavailable", not "ready" (healthy==0==len).
	provReg := provider.NewRegistry()
	modelReg, err := provider.NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	router := provider.NewRouter(modelReg, provReg)
	defer func() { _ = router.Close() }()
	srv := New(router, modelReg, provReg)

	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp StatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "unavailable" {
		t.Errorf("Status = %q, want unavailable", resp.Status)
	}
	if len(resp.Providers) != 0 {
		t.Errorf("Providers = %+v, want empty", resp.Providers)
	}
}

func TestStatusHandler_DegradedWhenHealthFails(t *testing.T) {
	mp := &mockProvider{
		name:   "ollama",
		caps:   provider.CapChat,
		health: errExpected,
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()

	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))

	var resp StatusResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if resp.Status != "degraded" {
		t.Errorf("status = %q, want degraded", resp.Status)
	}
	if resp.Providers[0].Healthy {
		t.Error("provider healthy=true despite health() error")
	}
}

var errExpected = fakeErr("expected")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

// TestStatusHandler_BoundedErrorAndStableWarmth pins the /v1/status
// projection: an unhealthy provider reports its bounded routing error class,
// never the raw error text (a *url.Error carries the endpoint URL), providers
// are sorted by name, and warm models are sorted by provider then model with
// expires_at in UTC. The registry and the warmth source both hand back
// unsorted input, so every read must sort.
func TestStatusHandler_BoundedErrorAndStableWarmth(t *testing.T) {
	provReg := provider.NewRegistry()
	for _, mp := range []*mockProvider{
		{name: "beta", caps: provider.CapChat},
		{name: "alpha", caps: provider.CapChat},
		{name: "ollama", caps: provider.CapChat, health: &url.Error{
			Op:  "Get",
			URL: "http://user:hunter2@10.9.8.7:11434/api/tags?key=sk-secret",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")},
		}},
	} {
		if err := provReg.Register(mp); err != nil {
			t.Fatalf("register provider: %v", err)
		}
	}
	modelReg, err := provider.NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	expires := time.Date(2026, 10, 3, 8, 5, 30, 500_000_000, time.FixedZone("EDT", -4*60*60))
	router := provider.NewRouter(modelReg, provReg, provider.WithWarmthSource(&fakeWarmthSource{
		// Deliberately unsorted, the way a map-backed source returns it.
		models: []provider.WarmModel{
			{Key: provider.ModelKey{Provider: "ollama", Model: "qwen3:8b"}, Info: provider.WarmthInfo{Loaded: true, ExpiresAt: expires, VRAM: 5.5}},
			{Key: provider.ModelKey{Provider: "ollama", Model: "qwen3-embedding:8b"}, Info: provider.WarmthInfo{Loaded: true, ExpiresAt: expires}},
			{Key: provider.ModelKey{Provider: "cloud", Model: "zeta"}, Info: provider.WarmthInfo{Loaded: true}},
		},
	}))
	defer func() { _ = router.Close() }()
	srv := New(router, modelReg, provReg)

	wantProviders := `[{"name":"alpha","healthy":true,"capabilities":["chat"]},` +
		`{"name":"beta","healthy":true,"capabilities":["chat"]},` +
		`{"name":"ollama","healthy":false,"capabilities":["chat"],"error_class":"network"}]`
	wantWarm := `[{"provider":"cloud","model":"zeta","loaded":true},` +
		`{"provider":"ollama","model":"qwen3-embedding:8b","loaded":true,"expires_at":"2026-10-03T12:05:30.5Z"},` +
		`{"provider":"ollama","model":"qwen3:8b","loaded":true,"vram_gb":5.5,"expires_at":"2026-10-03T12:05:30.5Z"}]`
	// Several reads, so an unsorted registry cannot match by map-order luck.
	for read := 0; read < 5; read++ {
		rec := httptest.NewRecorder()
		srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
		body := rec.Body.String()
		for _, leak := range []string{"10.9.8.7", "hunter2", "sk-secret", "refused"} {
			if strings.Contains(body, leak) {
				t.Fatalf("/v1/status leaks raw error text %q", leak)
			}
		}
		var resp struct {
			Providers json.RawMessage `json:"providers"`
			Warm      json.RawMessage `json:"warm"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, body)
		}
		if got := string(resp.Providers); got != wantProviders {
			t.Fatalf("read %d: providers = %s\nwant %s", read, got, wantProviders)
		}
		if got := string(resp.Warm); got != wantWarm {
			t.Fatalf("read %d: warm = %s\nwant %s", read, got, wantWarm)
		}
	}
}

// TestStatusHandler_LogsTruncatedHealthErrors pins the operator half of the
// /v1/status error projection: the health error that the response reduces to
// a class reaches the server log, truncated so a polled endpoint cannot copy
// a 64 KiB upstream error body into the log on every read.
func TestStatusHandler_LogsTruncatedHealthErrors(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	provReg := provider.NewRegistry()
	for _, mp := range []*mockProvider{
		{name: "local", caps: provider.CapChat, health: &url.Error{
			Op:  "Get",
			URL: "http://10.9.8.7:8080/v1/models",
			Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")},
		}},
		{name: "upstream", caps: provider.CapChat, health: errors.New("503 Service Unavailable: " + strings.Repeat("Z", 64*1024))},
	} {
		if err := provReg.Register(mp); err != nil {
			t.Fatalf("register provider: %v", err)
		}
	}
	modelReg, err := provider.NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	router := provider.NewRouter(modelReg, provReg)
	defer func() { _ = router.Close() }()
	srv := New(router, modelReg, provReg)

	srv.buildHandler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	log.SetOutput(orig)
	logged := buf.String()

	for _, want := range []string{"provider=local", "10.9.8.7", "connection refused", "provider=upstream"} {
		if !strings.Contains(logged, want) {
			t.Errorf("server log lacks %q:\n%.2000s", want, logged)
		}
	}
	if n := strings.Count(logged, "Z"); n == 0 || n > 512 {
		t.Errorf("logged %d runes of the upstream error body, want 1..512", n)
	}
}
