package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

// statusErr is a fake registry error carrying an HTTP status, mirroring the
// real *openaicompat.statusError / *ollama.APIError shape.
type statusErr struct{ code int }

func (e statusErr) Error() string       { return fmt.Sprintf("list models: %d", e.code) }
func (e statusErr) HTTPStatusCode() int { return e.code }

func TestPreflightConnectivityWarn(t *testing.T) {
	wrapped := fmt.Errorf("provider: lookup llamacpp/gemma4:31b: %w", statusErr{404})
	netFailure := &net.DNSError{Err: "no such host", Name: "llamacpp.local"}
	emptyResponse := fmt.Errorf("openaicompat: decode response: %w", io.EOF)
	notFound := errors.New(`model "missing" not found on "ollama"`)

	ep := preflightEndpoint{BaseURL: "http://127.0.0.1:8080", ModelsPath: "/v1/models"}
	ollamaEP := preflightEndpoint{BaseURL: "http://localhost:11434", ModelsPath: "/api/tags"}
	detailKey := provider.ModelKey{Provider: "ollama", Model: "m1"}
	epCreds := preflightEndpoint{BaseURL: "http://u:p@127.0.0.1:8080", ModelsPath: "/v1/models"}
	epMalformedCreds := preflightEndpoint{BaseURL: "user:pass@127.0.0.1:8080", ModelsPath: "/v1/models"}

	tests := []struct {
		name     string
		useCase  string
		sel      string
		provider string
		ep       preflightEndpoint
		epOK     bool
		err      error
		want     string
	}{
		{
			name: "detail status with unknown endpoint", sel: "m1",
			err:  &provider.ModelDetailError{Key: detailKey, Err: statusErr{429}},
			want: `agent fallback "m1": model detail lookup failed on "ollama" (429 Too Many Requests)`,
		},
		{
			name: "detail transport failure", sel: "ollama/m1", ep: ollamaEP, epOK: true,
			err:  &provider.ModelDetailError{Key: detailKey, Err: io.ErrUnexpectedEOF},
			want: `agent fallback "ollama/m1": POST /api/show failed on "ollama": "unexpected EOF"`,
		},
		{
			name: "status hit + endpoint", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: ep, epOK: true, err: wrapped,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at "http://127.0.0.1:8080" (GET /v1/models -> 404 Not Found); check server/base_url`,
		},
		{
			name: "planning route", useCase: "planning", sel: "llamacpp/planner", provider: "llamacpp",
			ep: ep, epOK: true, err: wrapped,
			want: `planning fallback "llamacpp/planner": cannot reach provider "llamacpp" at "http://127.0.0.1:8080" (GET /v1/models -> 404 Not Found); check server/base_url`,
		},
		{
			name: "no status + endpoint", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: ep, epOK: true, err: netFailure,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at "http://127.0.0.1:8080" (GET /v1/models): "lookup llamacpp.local: no such host"`,
		},
		{
			name: "empty discovery response", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: ep, epOK: true, err: emptyResponse,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at "http://127.0.0.1:8080" (GET /v1/models): "openaicompat: decode response: EOF"`,
		},
		{
			name: "no endpoint (resolver miss)", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: preflightEndpoint{}, epOK: false, err: wrapped,
			want: `agent fallback "llamacpp/gemma4:31b": provider lookup failed: "provider: lookup llamacpp/gemma4:31b: list models: 404"`,
		},
		{
			name: "endpoint ok but empty base_url", sel: "x/y", provider: "x",
			ep: preflightEndpoint{BaseURL: "", ModelsPath: "/v1/models"}, epOK: true, err: netFailure,
			want: `agent fallback "x/y": provider lookup failed: "lookup llamacpp.local: no such host"`,
		},
		{
			name: "model not found is not reachability", sel: "ollama/missing", provider: "ollama",
			ep: ollamaEP, epOK: true, err: notFound,
			want: `agent fallback "ollama/missing": provider lookup failed: "model \"missing\" not found on \"ollama\""`,
		},
		{
			name: "credentials redacted", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: epCreds, epOK: true, err: wrapped,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at "http://127.0.0.1:8080" (GET /v1/models -> 404 Not Found); check server/base_url`,
		},
		{
			name: "malformed credentials not leaked", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: epMalformedCreds, epOK: true, err: netFailure,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at "<invalid base_url>" (GET /v1/models): "lookup llamacpp.local: no such host"`,
		},
		{
			name: "status hit + endpoint + explicit source", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep:   preflightEndpoint{BaseURL: "http://127.0.0.1:8083", ModelsPath: "/v1/models", Source: "-base-url"},
			epOK: true, err: wrapped,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at "http://127.0.0.1:8083" (from -base-url) (GET /v1/models -> 404 Not Found); check server/base_url`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolve := func(string) (preflightEndpoint, bool) { return tt.ep, tt.epOK }
			got := preflightConnectivityWarn(tt.useCase, tt.sel, tt.provider, resolve, tt.err)
			if got != tt.want {
				t.Errorf("\n got = %q\nwant = %q", got, tt.want)
			}
		})
	}
}

// #856 through the real Ollama client: the model list answers, /api/show
// fails with 500. The listing worked, so preflight must not blame the list
// endpoint or the base URL.
func TestPreflightConnectivityWarn_ShowFailureIsNotAListingFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_, _ = fmt.Fprint(w, `{"models":[{"name":"other:7b"}]}`)
		case "/api/show":
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	providers := provider.NewRegistry()
	if err := providers.Register(provider.NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(srv.URL)))); err != nil {
		t.Fatal(err)
	}
	models, err := provider.NewModelRegistry(providers, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, lookupErr := models.Lookup(t.Context(), provider.ModelKey{Provider: "ollama", Model: "qwen3"})
	if lookupErr == nil {
		t.Fatal("Lookup succeeded, want an error")
	}
	resolve := func(name string) (preflightEndpoint, bool) {
		return preflightEndpoint{BaseURL: srv.URL, ModelsPath: "/api/tags"}, name == "ollama"
	}
	got := preflightConnectivityWarn("", "ollama/qwen3", "ollama", resolve, lookupErr)
	if want := `agent fallback "ollama/qwen3": POST /api/show failed on "ollama" (500 Internal Server Error)`; got != want {
		t.Errorf("warning = %q, want %q", got, want)
	}
}
