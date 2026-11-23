package openaicompat

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// #856 through the real adapter: a /v1/models answer with no listing key is
// not a listing, so a model missing from it is unknown, not "not found". A
// present but empty listing is the provider saying it serves nothing.
func TestModelRegistry_NotFoundNeedsAListing(t *testing.T) {
	for _, tt := range []struct {
		name         string
		body         string
		wantNotFound bool
	}{
		{"empty listing", `{"data":[]}`, true},
		{"listing without the model", `{"data":[{"id":"other"}]}`, true},
		{"no listing key", `{}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/models" {
					_, _ = fmt.Fprint(w, tt.body)
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()
			providers := provider.NewRegistry()
			p := NewProvider(NewClient(srv.URL))
			if err := providers.Register(p); err != nil {
				t.Fatal(err)
			}
			models, err := provider.NewModelRegistry(providers, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = models.Lookup(t.Context(), provider.ModelKey{Provider: p.Name(), Model: "gone"})
			if err == nil {
				t.Fatal("Lookup succeeded, want an error")
			}
			if got := errors.Is(err, provider.ErrModelNotFound); got != tt.wantNotFound {
				t.Errorf("errors.Is(err, ErrModelNotFound) = %v, want %v (err: %v)", got, tt.wantNotFound, err)
			}
		})
	}
}
