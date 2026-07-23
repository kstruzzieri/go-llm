package fingerprint

import "testing"

func TestNormalizeBackendID(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		want    string
		wantErr bool
	}{
		{"standard", "http://localhost:11434", "http://localhost:11434", false},
		{"trailing slash", "http://localhost:11434/", "http://localhost:11434", false},
		{"uppercase host", "HTTP://LocalHost:11434", "http://localhost:11434", false},
		{"loopback ipv4", "http://127.0.0.1:11434", "http://localhost:11434", false},
		{"loopback ipv6", "http://[::1]:11434", "http://localhost:11434", false},
		{"zero addr", "http://0.0.0.0:11434", "http://localhost:11434", false},
		{"non-default port", "http://localhost:8080", "http://localhost:8080", false},
		{"remote host", "http://gpu-server:11434", "http://gpu-server:11434", false},
		{"reverse proxy path", "http://host:8080/ollama/", "http://host:8080/ollama", false},
		{"strip query and fragment", "http://localhost:11434?foo=bar#baz", "http://localhost:11434", false},
		{"strip userinfo", "http://user:pass@localhost:11434", "http://localhost:11434", false},
		{"empty string", "", "", true},
		{"no scheme", "localhost:11434", "", true},
		{"invalid url", "://bad", "", true},
		{"https scheme", "HTTPS://gpu-server:11434", "https://gpu-server:11434", false},
		{"ipv6 remote", "http://[fe80::1]:11434", "http://[fe80::1]:11434", false},
		{"multiple trailing slashes", "http://localhost:11434///", "http://localhost:11434", false},
		{"deep path with trailing slash", "http://host:8080/v1/ollama/", "http://host:8080/v1/ollama", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeBackendID(tt.rawURL)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizeBackendID(%q) error = %v, wantErr %v", tt.rawURL, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("NormalizeBackendID(%q) = %q, want %q", tt.rawURL, got, tt.want)
			}
		})
	}
}

func TestIsLocalBackend(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want bool
	}{
		{"localhost", "http://localhost:11434", true},
		{"127.0.0.1", "http://127.0.0.1:11434", true},
		{"ipv6 loopback", "http://[::1]:11434", true},
		{"0.0.0.0", "http://0.0.0.0:11434", true},
		{"remote host", "http://gpu-server:11434", false},
		{"remote ip", "http://192.168.1.100:11434", false},
		{"empty string", "", false},
		{"no scheme", "localhost:11434", false},
		{"invalid url", "://bad", false},
		{"ipv6 remote", "http://[fe80::1]:11434", false},
		{"schemeless with host", "//localhost:11434", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsLocalBackend(tt.url)
			if got != tt.want {
				t.Errorf("IsLocalBackend(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}
