package provider

import (
	"strings"
	"testing"
)

// ParseDestinationFlag is the shared -allow-destination parser for golem and
// go-llm-mcp: the canonical "<provider>/<base URL>" grant plus the deprecated
// "<provider>=<base URL>" spelling go-llm-mcp historically accepted. Both
// forms must normalize to the same canonical Destination identity.
func TestParseDestinationFlagBothForms(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		provider string
		baseURL  string
		wantErr  bool
	}{
		{name: "canonical https", in: "openai/https://api.openai.com", provider: "openai", baseURL: "https://api.openai.com"},
		{name: "canonical http normalizes", in: "backup/http://EXAMPLE.com:80/root/", provider: "backup", baseURL: "http://example.com/root"},
		{name: "legacy https", in: "openai=https://api.openai.com", provider: "openai", baseURL: "https://api.openai.com"},
		{name: "legacy http normalizes", in: "backup=http://EXAMPLE.com:80/root/", provider: "backup", baseURL: "http://example.com/root"},
		{name: "legacy provider keeps embedded equals", in: "team=prod=https://host/base", provider: "team=prod", baseURL: "https://host/base"},
		{name: "legacy earliest scheme marker wins", in: "a=http://h/p=https://q", provider: "a", baseURL: "http://h/p=https://q"},
		{name: "canonical wins over legacy marker in path", in: "p/https://host/next=https://evil.example", provider: "p", baseURL: "https://host/next=https://evil.example"},
		{name: "no separator", in: "not-a-destination", wantErr: true},
		{name: "legacy userinfo rejected", in: "p=https://user:secret@host", wantErr: true},
		// The legacy rewrite must not open an echo path ParseDestination
		// lacks: a rejected canonical value whose query carries both a
		// secret and an embedded scheme marker re-parses through the
		// fallback, and the diagnostic still must not render the secret.
		{name: "canonical query secret with embedded marker", in: "p/https://h/a?key=secret=https://x", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDestinationFlag(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseDestinationFlag(%q) = %v, want error", tt.in, got)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Errorf("ParseDestinationFlag(%q) error echoes the secret: %v", tt.in, err)
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
