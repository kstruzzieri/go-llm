package main

import "testing"

func TestStatusLabel(t *testing.T) {
	tests := []struct {
		name string
		code int
		want string
	}{
		{"known 404", 404, "404 Not Found"},
		{"known 500", 500, "500 Internal Server Error"},
		{"unknown 599", 599, "HTTP 599"},
		{"zero", 0, "HTTP 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statusLabel(tt.code); got != tt.want {
				t.Errorf("statusLabel(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestRedactBaseURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain unchanged", "http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"trailing path kept", "http://localhost:11434/api", "http://localhost:11434/api"},
		{"userinfo stripped", "http://user:pass@host:8080", "http://host:8080"},
		{"user only stripped", "http://user@host:8080", "http://host:8080"},
		{"malformed with at", "://bad@thing", "<invalid base_url>"},
		{"scheme-less userinfo shaped", "user:pass@host:8080", "<invalid base_url>"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactBaseURL(tt.in); got != tt.want {
				t.Errorf("redactBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
