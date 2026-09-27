package provider

import (
	"errors"
	"strings"
	"testing"
)

// ParseDestinationFlag is the shared -allow-destination parser for golem and
// go-llm-mcp: only the canonical "<provider>/<base URL>" grant is accepted.
func TestParseDestinationFlag(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		provider string
		baseURL  string
		wantErr  string
	}{
		{name: "canonical https", in: "openai/https://api.openai.com", provider: "openai", baseURL: "https://api.openai.com"},
		{name: "canonical http normalizes", in: "backup/http://EXAMPLE.com:80/root/", provider: "backup", baseURL: "http://example.com/root"},
		{name: "legacy https", in: "openai=https://api.openai.com", wantErr: `expected "<provider>/<base URL>"`},
		{name: "legacy http", in: "backup=http://EXAMPLE.com:80/root/", wantErr: `expected "<provider>/<base URL>"`},
		{name: "legacy provider with embedded equals", in: "team=prod=https://host/base", wantErr: `expected "<provider>/<base URL>"`},
		// The guard keys on the FIRST scheme marker; a later marker after a
		// slash must not let the legacy value fall through to ParseDestination.
		{name: "legacy with second scheme marker in path", in: "a=http://h/p=https://q", wantErr: `expected "<provider>/<base URL>"`},
		// A bare URL (no provider) takes the same guard: without it, the
		// scheme's colon becomes the provider name and the diagnostic
		// misreports the grammar as a scheme error.
		{name: "bare URL", in: "https://api.openai.com", wantErr: `expected "<provider>/<base URL>"`},
		{name: "bare URL with userinfo", in: "https://user:secret@api.openai.com/v1", wantErr: `expected "<provider>/<base URL>"`},
		{name: "canonical provider keeps embedded equals", in: "team=prod/https://host/base", provider: "team=prod", baseURL: "https://host/base"},
		{name: "canonical wins over legacy marker in path", in: "p/https://host/next=https://evil.example", provider: "p", baseURL: "https://host/next=https://evil.example"},
		{name: "no separator", in: "not-a-destination", wantErr: `expected "<provider>/<base URL>"`},
		{name: "legacy userinfo rejected", in: "p=https://user:secret@host", wantErr: `expected "<provider>/<base URL>"`},
		// A rejected canonical query with a secret and scheme marker must
		// not expose the submitted value either.
		{name: "canonical query secret with embedded marker", in: "p/https://h/a?key=secret=https://x", wantErr: "base URL must not carry a query"},
		{name: "legacy query rejected", in: "p=https://host?key=secret", wantErr: `expected "<provider>/<base URL>"`},
		{name: "empty", in: "", wantErr: `expected "<provider>/<base URL>"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDestinationFlag(tt.in)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseDestinationFlag(%q) = %v, want error", tt.in, got)
				}
				if !errors.Is(err, ErrDestinationInvalid) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("ParseDestinationFlag(%q) error = %v, want ErrDestinationInvalid and %q", tt.in, err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "secret") || (tt.in != "" && strings.Contains(err.Error(), tt.in)) {
					t.Errorf("ParseDestinationFlag(%q) error echoes the rejected value: %v", tt.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDestinationFlag(%q) error = %v", tt.in, err)
			}
			want, err := NewDestination(tt.provider, tt.baseURL)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("ParseDestinationFlag(%q) = %v, want %v", tt.in, got, want)
			}
		})
	}
}
