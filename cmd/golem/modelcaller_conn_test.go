package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
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
	epCreds := preflightEndpoint{BaseURL: "http://u:p@127.0.0.1:8080", ModelsPath: "/v1/models"}
	epMalformedCreds := preflightEndpoint{BaseURL: "user:pass@127.0.0.1:8080", ModelsPath: "/v1/models"}

	tests := []struct {
		name     string
		sel      string
		provider string
		ep       preflightEndpoint
		epOK     bool
		err      error
		want     string
	}{
		{
			name: "status hit + endpoint", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: ep, epOK: true, err: wrapped,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at http://127.0.0.1:8080 (GET /v1/models -> 404 Not Found); check server/base_url`,
		},
		{
			name: "no status + endpoint", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: ep, epOK: true, err: netFailure,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at http://127.0.0.1:8080 (GET /v1/models): lookup llamacpp.local: no such host`,
		},
		{
			name: "empty discovery response", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: ep, epOK: true, err: emptyResponse,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at http://127.0.0.1:8080 (GET /v1/models): openaicompat: decode response: EOF`,
		},
		{
			name: "no endpoint (resolver miss)", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: preflightEndpoint{}, epOK: false, err: wrapped,
			want: `agent fallback "llamacpp/gemma4:31b": provider lookup failed: provider: lookup llamacpp/gemma4:31b: list models: 404`,
		},
		{
			name: "endpoint ok but empty base_url", sel: "x/y", provider: "x",
			ep: preflightEndpoint{BaseURL: "", ModelsPath: "/v1/models"}, epOK: true, err: netFailure,
			want: `agent fallback "x/y": provider lookup failed: lookup llamacpp.local: no such host`,
		},
		{
			name: "model not found is not reachability", sel: "ollama/missing", provider: "ollama",
			ep: ollamaEP, epOK: true, err: notFound,
			want: `agent fallback "ollama/missing": provider lookup failed: model "missing" not found on "ollama"`,
		},
		{
			name: "credentials redacted", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: epCreds, epOK: true, err: wrapped,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at http://127.0.0.1:8080 (GET /v1/models -> 404 Not Found); check server/base_url`,
		},
		{
			name: "malformed credentials not leaked", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep: epMalformedCreds, epOK: true, err: netFailure,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at <invalid base_url> (GET /v1/models): lookup llamacpp.local: no such host`,
		},
		{
			name: "status hit + endpoint + explicit source", sel: "llamacpp/gemma4:31b", provider: "llamacpp",
			ep:   preflightEndpoint{BaseURL: "http://127.0.0.1:8083", ModelsPath: "/v1/models", Source: "-base-url"},
			epOK: true, err: wrapped,
			want: `agent fallback "llamacpp/gemma4:31b": cannot reach provider "llamacpp" at http://127.0.0.1:8083 (from -base-url) (GET /v1/models -> 404 Not Found); check server/base_url`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preflightConnectivityWarn(tt.sel, tt.provider, tt.ep, tt.epOK, tt.err)
			if got != tt.want {
				t.Errorf("\n got = %q\nwant = %q", got, tt.want)
			}
		})
	}
}
