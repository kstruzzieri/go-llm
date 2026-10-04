package compat

import (
	"bytes"
	"context"
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
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
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
	srv := New(router, modelReg, provReg, allowHTTPTestHost)

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
	srv := New(router, modelReg, provReg, allowHTTPTestHost)

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

// Health diagnostics retain the provider, request ID and error class without
// copying credentials or control characters from real provider errors.
func TestStatusHandler_LogsSafeHealthDiagnostics(t *testing.T) {
	const apiKey = "synthetic-review-bearer"
	for _, tc := range []struct {
		name      string
		status    int
		jsonError bool
		wantClass provider.ErrorClass
	}{
		{name: "healthy", status: http.StatusOK},
		{name: "json error", status: http.StatusUnauthorized, jsonError: true, wantClass: provider.ErrorClass4xx},
		{name: "plain error", status: http.StatusServiceUnavailable, wantClass: provider.ErrorClass5xx},
		{name: "transport error", wantClass: provider.ErrorClassNetwork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				if tc.status == http.StatusOK {
					_, _ = w.Write([]byte(`{"data":[]}`))
					return
				}
				message := "UPSTREAM_ERROR " + r.Header.Get("Authorization") + "\nFORGED_LOG_RECORD\r\x1b[31m" + strings.Repeat("Z", 64*1024)
				if tc.jsonError {
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message[:256]}})
				} else {
					_, _ = w.Write([]byte(message))
				}
			}))
			defer upstream.Close()
			baseURL := upstream.URL
			if tc.status == 0 {
				upstream.Close()
				baseURL = strings.Replace(baseURL, "http://", "http://user:synthetic-password@", 1) + "?api_key=synthetic-query-key"
			}
			reg := provider.NewRegistry()
			p := openaicompat.NewProvider(openaicompat.NewClient(baseURL, openaicompat.WithAPIKey(apiKey)), openaicompat.WithProviderName("upstream"))
			if err := reg.Register(p); err != nil {
				t.Fatal(err)
			}
			srv := New(nil, nil, reg, allowHTTPTestHost)
			var buf bytes.Buffer
			orig := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(orig)

			req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
			req.Header.Set("X-Request-Id", "review-639")
			rec := httptest.NewRecorder()
			srv.buildHandler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var resp StatusResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if len(resp.Providers) != 1 || resp.Providers[0].ErrorClass != tc.wantClass || resp.Providers[0].Healthy != (tc.wantClass == "") {
				t.Fatalf("providers = %+v, want healthy=%t, class=%q", resp.Providers, tc.wantClass == "", tc.wantClass)
			}
			logged := buf.String()
			for _, leak := range []string{apiKey, "synthetic-password", "synthetic-query-key", "127.0.0.1", "UPSTREAM_ERROR", "FORGED_LOG_RECORD", "\x1b", "ZZZZ"} {
				if strings.Contains(logged, leak) || strings.Contains(rec.Body.String(), leak) {
					t.Errorf("provider error content %q reached diagnostics", leak)
				}
			}
			if tc.wantClass == "" {
				if strings.Contains(logged, "compat: status health") {
					t.Error("healthy provider produced a failure log")
				}
				return
			}
			for _, want := range []string{"compat: status health", `provider="upstream"`, `rid="review-639"`, "error_class=" + string(tc.wantClass)} {
				if !strings.Contains(logged, want) {
					t.Errorf("health log lacks %q", want)
				}
			}
		})
	}
}

func TestStatusHandler_BoundsAndQuotesHealthLogIdentifiers(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	reg := provider.NewRegistry()
	if err := reg.Register(&mockProvider{name: "provider\n\x1b" + strings.Repeat("P", 1024), health: errExpected}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxRequestID, "request\r\n\x1b"+strings.Repeat("R", 1024)))
	// Exercise this log sink directly; access logging is a separate boundary.
	New(nil, nil, reg).handleStatus(httptest.NewRecorder(), req)
	logged := buf.String()
	if strings.Count(logged, "\n") != 1 || strings.ContainsAny(logged, "\r\x1b") {
		t.Fatal("health log contains unescaped control characters")
	}
	for _, want := range []string{`provider="provider\n\x1b`, `rid="request\r\n\x1b`, "error_class=unknown"} {
		if !strings.Contains(logged, want) {
			t.Errorf("health log lacks %q", want)
		}
	}
	for _, padding := range []string{"P", "R"} {
		if n := strings.Count(logged, padding); n == 0 || n > 512 {
			t.Errorf("logged %d identifier padding runes, want 1..512", n)
		}
	}
}
