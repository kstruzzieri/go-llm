package ollama

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewClientDefaults(t *testing.T) {
	c := NewClient()
	if c.baseURL != defaultBaseURL {
		t.Errorf("expected baseURL %q, got %q", defaultBaseURL, c.baseURL)
	}
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("expected timeout %v, got %v", defaultTimeout, c.httpClient.Timeout)
	}
}

func TestNewClientWithOptions(t *testing.T) {
	customURL := "http://example.com:1234"
	customTimeout := 10 * time.Second

	c := NewClient(
		WithBaseURL(customURL),
		WithTimeout(customTimeout),
	)
	if c.baseURL != customURL {
		t.Errorf("expected baseURL %q, got %q", customURL, c.baseURL)
	}
	if c.httpClient.Timeout != customTimeout {
		t.Errorf("expected timeout %v, got %v", customTimeout, c.httpClient.Timeout)
	}
}

func TestWithHTTPClient(t *testing.T) {
	custom := &http.Client{Timeout: 42 * time.Second}
	c := NewClient(WithHTTPClient(custom))
	if c.httpClient != custom {
		t.Error("expected custom HTTP client to be used")
	}
}

func TestIsAvailable(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		want       bool
	}{
		{"ok", http.StatusOK, true},
		{"server error", http.StatusInternalServerError, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
			}))
			defer srv.Close()

			c := NewClient(WithBaseURL(srv.URL))
			got := c.IsAvailable(context.Background())
			if got != tt.want {
				t.Errorf("IsAvailable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsAvailableUnreachable(t *testing.T) {
	c := NewClient(WithBaseURL("http://127.0.0.1:1"))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if c.IsAvailable(ctx) {
		t.Error("expected IsAvailable to return false for unreachable server")
	}
}
