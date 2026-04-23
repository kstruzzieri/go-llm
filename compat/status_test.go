package compat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
