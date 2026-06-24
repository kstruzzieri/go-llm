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
