package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestModelsHandler_ListsRegisteredModels(t *testing.T) {
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapChat | provider.CapGenerate,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768},
			{Name: "qwen3-embedding:8b", ContextWindow: 8192},
		},
	}
	srv, teardown := newTestServer(t, mp)
	defer teardown()

	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp ModelsListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected at least one model")
	}
	for _, m := range resp.Data {
		if m.Object != "model" {
			t.Errorf("model object = %q", m.Object)
		}
		if m.ID == "" {
			t.Error("empty ID")
		}
		if m.OwnedBy == "" {
			t.Error("empty OwnedBy")
		}
	}
}

// TestModelsHandler_ListsAliasEntriesAndXResources exercises Amendment #7:
// canonical entries must carry go-llm-specific extension fields (x_capabilities,
// x_context_window, x_resources) and configured aliases must be listed with
// x_alias_for pointing at the canonical provider/model selector.
//
// Note: warm-first ordering is exercised in TestSortModelsWarmFirst_WarmBeforeCold
// directly against the sort helper. Driving the router's warmth source through
// this HTTP-level test would require running real requests through the router,
// which is more ceremony than the assertion warrants.
func TestModelsHandler_ListsAliasEntriesAndXResources(t *testing.T) {
	mp := &mockProvider{
		name: "ollama",
		caps: provider.CapChat | provider.CapGenerate,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768},
		},
	}
	aliases := map[string]string{
		"gpt-4":         "ollama/qwen3:8b",
		"gpt-3.5-turbo": "qwen3:8b",
	}
	srv, teardown := newTestServer(t, mp, WithAliases(aliases))
	defer teardown()

	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp ModelsListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byID := make(map[string]ModelObject, len(resp.Data))
	for _, m := range resp.Data {
		byID[m.ID] = m
	}

	canonical, ok := byID["ollama/qwen3:8b"]
	if !ok {
		t.Fatalf("missing canonical entry ollama/qwen3:8b; got IDs=%v", idsOf(resp.Data))
	}
	if canonical.OwnedBy != "ollama" {
		t.Errorf("canonical OwnedBy = %q, want ollama", canonical.OwnedBy)
	}
	if len(canonical.Capabilities) == 0 {
		t.Error("canonical x_capabilities is empty; want at least chat/generate")
	}
	// Quality tier should serialize as a known string ("basic"/.../"unknown").
	if canonical.Quality == "" {
		t.Error("canonical x_quality is empty; want a tier string")
	}
	if canonical.AliasFor != "" {
		t.Errorf("canonical entry must not have x_alias_for, got %q", canonical.AliasFor)
	}

	// Alias with qualified target: "gpt-4" -> "ollama/qwen3:8b".
	gpt4, ok := byID["gpt-4"]
	if !ok {
		t.Fatalf("missing alias entry gpt-4; got IDs=%v", idsOf(resp.Data))
	}
	if gpt4.OwnedBy != "alias" {
		t.Errorf("alias OwnedBy = %q, want alias", gpt4.OwnedBy)
	}
	if gpt4.AliasFor != "ollama/qwen3:8b" {
		t.Errorf("alias x_alias_for = %q, want ollama/qwen3:8b", gpt4.AliasFor)
	}
	// Alias entries should be pointers only — no duplicated capability/context fields.
	if len(gpt4.Capabilities) != 0 {
		t.Errorf("alias x_capabilities = %v, want empty", gpt4.Capabilities)
	}
	if gpt4.ContextWindow != 0 {
		t.Errorf("alias x_context_window = %d, want 0", gpt4.ContextWindow)
	}
	if gpt4.Quality != "" {
		t.Errorf("alias x_quality = %q, want empty", gpt4.Quality)
	}
	if gpt4.Resources != nil {
		t.Errorf("alias x_resources = %+v, want nil", gpt4.Resources)
	}
	if gpt4.Warm {
		t.Error("alias x_warm = true, want false")
	}

	// Alias with unqualified target: "gpt-3.5-turbo" -> "qwen3:8b" (bare model).
	// Per implementation guidance step 5, when an alias target carries no provider
	// segment we do NOT synthesize one, so the raw model name is carried through.
	turbo, ok := byID["gpt-3.5-turbo"]
	if !ok {
		t.Fatalf("missing alias entry gpt-3.5-turbo; got IDs=%v", idsOf(resp.Data))
	}
	if turbo.AliasFor != "qwen3:8b" {
		t.Errorf("unqualified alias x_alias_for = %q, want qwen3:8b", turbo.AliasFor)
	}
}

func TestModelsHandler_NoRegistry(t *testing.T) {
	srv := New(nil, nil, nil)

	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestModelsHandler_EmptyRegistryReturnsEmptyList(t *testing.T) {
	// Regression guard: All() returns (nil, nil) when no providers are
	// registered. The handler must emit an empty list with Data=[] (never
	// null) so JavaScript clients can safely access .length.
	provReg := provider.NewRegistry()
	modelReg, err := provider.NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	router := provider.NewRouter(modelReg, provReg)
	defer func() { _ = router.Close() }()
	srv := New(router, modelReg, provReg)

	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// The raw body must contain "data":[] not "data":null.
	body := rec.Body.String()
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("body = %q, want Data to serialize as []", body)
	}
}

func TestSortModelsWarmFirst_WarmBeforeCold(t *testing.T) {
	// Synthesize a mixed slice and verify sortModelsWarmFirst:
	//   1. moves warm entries ahead of cold
	//   2. sorts within each group by ID ascending
	in := []ModelObject{
		{ID: "ollama/zebra:8b", Warm: false},
		{ID: "ollama/apple:8b", Warm: true},
		{ID: "gpt-4", Warm: false}, // alias, cold
		{ID: "ollama/banana:8b", Warm: true},
		{ID: "ollama/cherry:8b", Warm: false},
	}
	sortModelsWarmFirst(in)

	wantIDs := []string{
		"ollama/apple:8b",  // warm
		"ollama/banana:8b", // warm
		"gpt-4",            // cold
		"ollama/cherry:8b", // cold
		"ollama/zebra:8b",  // cold
	}
	for i, want := range wantIDs {
		if in[i].ID != want {
			t.Errorf("sorted[%d].ID = %q, want %q (full=%v)", i, in[i].ID, want, idsOf(in))
		}
	}
}

// fakeWarmthSource is a minimal WarmthSource that always reports the same
// snapshot. It is used to drive x_warm through a real Router for
// TestModelsHandler_WarmModelsGetXWarmTrue. Only Snapshot/Close are exercised
// by the models handler; the other interface methods are stubbed to satisfy
// the compile-time assertion.
type fakeWarmthSource struct {
	models []provider.WarmModel
}

func (f *fakeWarmthSource) IsWarm(key provider.ModelKey) bool {
	for _, wm := range f.models {
		if wm.Key == key && wm.Info.Loaded {
			return true
		}
	}
	return false
}

func (f *fakeWarmthSource) WarmthState(key provider.ModelKey) *provider.WarmthInfo {
	for _, wm := range f.models {
		if wm.Key == key {
			info := wm.Info
			return &info
		}
	}
	return nil
}

func (f *fakeWarmthSource) RecordUse(provider.ModelKey)    {}
func (f *fakeWarmthSource) Snapshot() []provider.WarmModel { return f.models }
func (f *fakeWarmthSource) Close() error                   { return nil }

// TestModelsHandler_WarmModelsGetXWarmTrue plugs a fake WarmthSource into a
// real Router and verifies x_warm propagates end-to-end through the handler.
// This guards against a future refactor where ModelKey formatting drift (e.g.
// provider-name capitalization) could silently mark every canonical entry cold
// with no HTTP-level test to catch it. TestSortModelsWarmFirst_WarmBeforeCold
// only exercises the sort helper in isolation, so the warm map lookup would be
// untested otherwise.
func TestModelsHandler_WarmModelsGetXWarmTrue(t *testing.T) {
	warmModel := &mockProvider{
		name: "ollama",
		caps: provider.CapChat | provider.CapGenerate,
		models: []provider.ModelInfo{
			{Name: "qwen3:8b", ContextWindow: 32768},
			{Name: "qwen3-embedding:8b", ContextWindow: 8192},
		},
	}

	provReg := provider.NewRegistry()
	if err := provReg.Register(warmModel); err != nil {
		t.Fatalf("provider registry register: %v", err)
	}
	if err := provReg.RefreshModels(context.Background(), warmModel.Name()); err != nil {
		t.Fatalf("provider registry refresh models: %v", err)
	}
	modelReg, err := provider.NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}

	// Mark qwen3:8b warm, leave qwen3-embedding:8b cold. The handler's warmth
	// lookup keys on provider.ModelKey, so the fake's entry must match the
	// canonical key exactly.
	warmKey := provider.ModelKey{Provider: "ollama", Model: "qwen3:8b"}
	ws := &fakeWarmthSource{
		models: []provider.WarmModel{
			{
				Key: warmKey,
				Info: provider.WarmthInfo{
					Loaded:    true,
					Since:     time.Now().Add(-1 * time.Minute),
					ExpiresAt: time.Now().Add(5 * time.Minute),
					VRAM:      6.0,
				},
			},
		},
	}
	router := provider.NewRouter(modelReg, provReg,
		provider.WithStickyTTL(time.Second),
		provider.WithAvailableRAM(256),
		provider.WithWarmthSource(ws),
	)
	defer func() { _ = router.Close() }()
	srv := New(router, modelReg, provReg)

	rec := httptest.NewRecorder()
	srv.buildHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var resp ModelsListResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byID := make(map[string]ModelObject, len(resp.Data))
	for _, m := range resp.Data {
		byID[m.ID] = m
	}

	hot, ok := byID["ollama/qwen3:8b"]
	if !ok {
		t.Fatalf("missing canonical entry ollama/qwen3:8b; got IDs=%v", idsOf(resp.Data))
	}
	if !hot.Warm {
		t.Errorf("ollama/qwen3:8b x_warm = false, want true (warmth snapshot did not propagate)")
	}

	cold, ok := byID["ollama/qwen3-embedding:8b"]
	if !ok {
		t.Fatalf("missing canonical entry ollama/qwen3-embedding:8b; got IDs=%v", idsOf(resp.Data))
	}
	if cold.Warm {
		t.Errorf("ollama/qwen3-embedding:8b x_warm = true, want false (cold model reported warm)")
	}
}

func idsOf(models []ModelObject) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}
