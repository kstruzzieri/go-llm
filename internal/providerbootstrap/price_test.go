package providerbootstrap

import (
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

func priceOf(in, out float64, cached, write *float64) *config.ModelPrice {
	return &config.ModelPrice{Input: in, Output: out, CachedInput: cached, CacheWrite: write}
}

func TestBuildPriceOverrides(t *testing.T) {
	lc := provider.ModelKey{Provider: "lc", Model: "qwen"}
	oll := provider.ModelKey{Provider: "ollama", Model: "qwen"}
	tests := []struct {
		name    string
		models  map[string]config.ModelConfig
		want    map[provider.ModelKey]config.ModelPrice
		errText []string // all must appear; nil = no error
	}{
		{
			name: "exact values, optional fields kept",
			models: map[string]config.ModelConfig{
				"agent": {Provider: "lc", Name: "qwen", Price: priceOf(3, 15, provider.Ptr(0.3), provider.Ptr(3.75))},
			},
			want: map[provider.ModelKey]config.ModelPrice{lc: {Input: 3, Output: 15, CachedInput: provider.Ptr(0.3), CacheWrite: provider.Ptr(3.75)}},
		},
		{
			name: "roles with equal prices behind distinct pointers merge",
			models: map[string]config.ModelConfig{
				"agent": {Provider: "lc", Name: "qwen", Price: priceOf(3, 15, provider.Ptr(0.3), nil)},
				"chat":  {Provider: "lc", Name: "qwen", Price: priceOf(3, 15, provider.Ptr(0.3), nil)},
			},
			want: map[provider.ModelKey]config.ModelPrice{lc: {Input: 3, Output: 15, CachedInput: provider.Ptr(0.3)}},
		},
		{
			name: "a priced role fills in for an unpriced role on the same key",
			models: map[string]config.ModelConfig{
				"agent": {Provider: "lc", Name: "qwen"},
				"chat":  {Provider: "lc", Name: "qwen", Price: priceOf(3, 15, nil, nil)},
			},
			want: map[provider.ModelKey]config.ModelPrice{lc: {Input: 3, Output: 15}},
		},
		{
			name: "roles without a name or a price are skipped",
			models: map[string]config.ModelConfig{
				"nameless": {Provider: "lc", Price: priceOf(1, 2, nil, nil)},
				"unpriced": {Provider: "lc", Name: "plain"},
			},
			want: map[provider.ModelKey]config.ModelPrice{},
		},
		{
			name: "empty provider means ollama",
			models: map[string]config.ModelConfig{
				"agent": {Name: "qwen", Price: priceOf(3, 15, nil, nil)},
			},
			want: map[provider.ModelKey]config.ModelPrice{oll: {Input: 3, Output: 15}},
		},
		{
			name: "implicit and explicit ollama roles share one key",
			models: map[string]config.ModelConfig{
				"agent": {Name: "qwen", Price: priceOf(3, 15, nil, nil)},
				"chat":  {Provider: "ollama", Name: "qwen", Price: priceOf(3, 15, nil, nil)},
			},
			want: map[provider.ModelKey]config.ModelPrice{oll: {Input: 3, Output: 15}},
		},
		{
			name: "same name under different providers is two keys",
			models: map[string]config.ModelConfig{
				"agent": {Provider: "lc", Name: "qwen", Price: priceOf(1, 2, nil, nil)},
				"chat":  {Provider: "other", Name: "qwen", Price: priceOf(5, 6, nil, nil)},
			},
			want: map[provider.ModelKey]config.ModelPrice{
				lc:                                 {Input: 1, Output: 2},
				{Provider: "other", Model: "qwen"}: {Input: 5, Output: 6},
			},
		},
		{
			name: "conflicting prices name both roles",
			models: map[string]config.ModelConfig{
				"agent": {Provider: "lc", Name: "qwen", Price: priceOf(3, 15, nil, nil)},
				"chat":  {Provider: "lc", Name: "qwen", Price: priceOf(4, 15, nil, nil)},
			},
			errText: []string{"conflicting price", `"agent"`, `"chat"`},
		},
		{
			name: "cached_input present vs absent conflicts",
			models: map[string]config.ModelConfig{
				"agent": {Provider: "lc", Name: "qwen", Price: priceOf(3, 15, provider.Ptr(0.3), nil)},
				"chat":  {Provider: "lc", Name: "qwen", Price: priceOf(3, 15, nil, nil)},
			},
			errText: []string{"conflicting price"},
		},
		{
			name: "implicit ollama conflicts with explicit ollama",
			models: map[string]config.ModelConfig{
				"agent": {Name: "qwen", Price: priceOf(3, 15, nil, nil)},
				"chat":  {Provider: "ollama", Name: "qwen", Price: priceOf(9, 15, nil, nil)},
			},
			errText: []string{"conflicting price"},
		},
		{
			name:    "negative input",
			models:  map[string]config.ModelConfig{"agent": {Provider: "lc", Name: "qwen", Price: priceOf(-1, 15, nil, nil)}},
			errText: []string{"price", `"agent"`, "input"},
		},
		{
			name:    "negative output",
			models:  map[string]config.ModelConfig{"agent": {Provider: "lc", Name: "qwen", Price: priceOf(3, -15, nil, nil)}},
			errText: []string{"price", `"agent"`, "output"},
		},
		{
			name:    "NaN input",
			models:  map[string]config.ModelConfig{"agent": {Provider: "lc", Name: "qwen", Price: priceOf(math.NaN(), 15, nil, nil)}},
			errText: []string{"price", "input"},
		},
		{
			name:    "+Inf output",
			models:  map[string]config.ModelConfig{"agent": {Provider: "lc", Name: "qwen", Price: priceOf(3, math.Inf(1), nil, nil)}},
			errText: []string{"price", "output"},
		},
		{
			name:    "-Inf cached_input",
			models:  map[string]config.ModelConfig{"agent": {Provider: "lc", Name: "qwen", Price: priceOf(3, 15, provider.Ptr(math.Inf(-1)), nil)}},
			errText: []string{"price", "cached_input"},
		},
		{
			name:    "negative cache_write",
			models:  map[string]config.ModelConfig{"agent": {Provider: "lc", Name: "qwen", Price: priceOf(3, 15, nil, provider.Ptr(-0.5))}},
			errText: []string{"price", "cache_write"},
		},
		{
			// A nameless role is skipped like every other override, so an
			// invalid price on it is not this builder's concern.
			name:   "invalid price on a nameless role is skipped",
			models: map[string]config.ModelConfig{"nameless": {Provider: "lc", Price: priceOf(-1, 2, nil, nil)}},
			want:   map[provider.ModelKey]config.ModelPrice{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildPriceOverrides(&config.Config{Models: tt.models})
			if tt.errText != nil {
				if err == nil {
					t.Fatalf("buildPriceOverrides = %v, want error containing %q", got, tt.errText)
				}
				for _, s := range tt.errText {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error %q does not contain %q", err, s)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("buildPriceOverrides: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("prices = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestBuildPriceOverridesNilConfig(t *testing.T) {
	got, err := buildPriceOverrides(nil)
	if err != nil {
		t.Fatalf("buildPriceOverrides(nil): %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("prices = %#v, want an empty non-nil map", got)
	}
}

// openAICompatServer serves one model and counts every request it receives.
func openAICompatServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"byo-model"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestNew_PopulatesBundlePrices(t *testing.T) {
	srv, _ := openAICompatServer(t)
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc":     {APIFormat: "openai-compat", BaseURL: srv.URL},
			"ollama": {APIFormat: "openai-compat", BaseURL: srv.URL},
		},
		Models: map[string]config.ModelConfig{
			"agent":   {Provider: "lc", Name: "byo-model", Type: "dense", Price: priceOf(3, 15, provider.Ptr(0.3), provider.Ptr(3.75))},
			"implied": {Name: "byo-model", Type: "dense", Price: priceOf(0.5, 1.5, nil, nil)}, // empty provider = ollama
			"free":    {Provider: "lc", Name: "unpriced", Type: "dense"},
		},
	}
	bundle, err := New(t.Context(), Options{Config: cfg})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = bundle.Close() }()

	want := map[provider.ModelKey]config.ModelPrice{
		{Provider: "lc", Model: "byo-model"}:     {Input: 3, Output: 15, CachedInput: provider.Ptr(0.3), CacheWrite: provider.Ptr(3.75)},
		{Provider: "ollama", Model: "byo-model"}: {Input: 0.5, Output: 1.5},
	}
	if !reflect.DeepEqual(bundle.Prices, want) {
		t.Fatalf("Bundle.Prices = %+v, want %+v", bundle.Prices, want)
	}
}

func TestNew_NilConfigHasNoPrices(t *testing.T) {
	bundle, err := New(t.Context(), Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = bundle.Close() }()
	if len(bundle.Prices) != 0 {
		t.Fatalf("Bundle.Prices = %+v, want none", bundle.Prices)
	}
}

// Conflicting or invalid prices are user-config errors: New fails, and fails
// before it touches any server (the slot_discovery precedent).
func TestNew_RejectsBadPricesBeforeAnyRequest(t *testing.T) {
	for _, tt := range []struct {
		name   string
		models map[string]config.ModelConfig
		want   string
	}{
		{"conflict", map[string]config.ModelConfig{
			"agent": {Provider: "lc", Name: "byo-model", Type: "dense", Price: priceOf(3, 15, nil, nil)},
			"chat":  {Provider: "lc", Name: "byo-model", Type: "dense", Price: priceOf(4, 15, nil, nil)},
		}, "conflicting price"},
		{"negative", map[string]config.ModelConfig{
			"agent": {Provider: "lc", Name: "byo-model", Type: "dense", Price: priceOf(-3, 15, nil, nil)},
		}, "price"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, hits := openAICompatServer(t)
			cfg := &config.Config{
				Providers: map[string]config.ProviderConfig{"lc": {APIFormat: "openai-compat", BaseURL: srv.URL}},
				Models:    tt.models,
			}
			bundle, err := New(t.Context(), Options{Config: cfg})
			if err == nil {
				_ = bundle.Close()
				t.Fatal("New succeeded, want a price error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
			if n := hits.Load(); n != 0 {
				t.Errorf("server saw %d request(s) before the price error, want 0", n)
			}
		})
	}
}
