package main

import (
	"strings"
	"testing"
)

func TestValidateBaseURLOverride(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr string // substring; "" => no error
	}{
		{"valid root", "http://127.0.0.1:8081", "http://127.0.0.1:8081", ""},
		{"valid with path prefix (reverse proxy)", "http://127.0.0.1:8081/llm", "http://127.0.0.1:8081/llm", ""},
		{"trims whitespace", "  http://127.0.0.1:8081  ", "http://127.0.0.1:8081", ""},
		{"empty", "", "", "value is empty"},
		{"whitespace only", "   ", "", "value is empty"},
		{"no scheme", "127.0.0.1:8081", "", "scheme and host"},
		{"garbage", "http://", "", "scheme and host"},
		{"v1 suffix", "http://127.0.0.1:8081/v1", "", "without the /v1 suffix"},
		{"v1 suffix trailing slash", "http://127.0.0.1:8081/v1/", "", "without the /v1 suffix"},
		{"proxy path ending in v1", "http://127.0.0.1:8081/llm/v1", "", "without the /v1 suffix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateBaseURLOverride(tt.raw, "-base-url")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateBaseURLOverride_RedactsUserinfoInError(t *testing.T) {
	_, err := validateBaseURLOverride("http://u:sekret@127.0.0.1:8081/v1", "-base-url")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if strings.Contains(err.Error(), "sekret") {
		t.Fatalf("error leaks userinfo: %v", err)
	}
}

func TestParseFlags_BaseURLAndNoProbe(t *testing.T) {
	f, err := parseFlags([]string{"-base-url", "http://127.0.0.1:8083", "-no-probe"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.baseURL != "http://127.0.0.1:8083" {
		t.Fatalf("baseURL = %q", f.baseURL)
	}
	if !f.baseURLSet {
		t.Fatal("baseURLSet = false, want true")
	}
	if !f.noProbe {
		t.Fatal("noProbe = false, want true")
	}
	// -base-url plus -no-probe is allowed (no-probe is then redundant).
	if err := validateFlags(f); err != nil {
		t.Fatalf("validateFlags: %v", err)
	}
	// Unset flag => baseURLSet false (distinguishes explicit empty).
	f2, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags(nil): %v", err)
	}
	if f2.baseURLSet {
		t.Fatal("baseURLSet = true for absent flag, want false")
	}
}
