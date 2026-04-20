package compat

import (
	"testing"
	"time"
)

func TestNormalizeBase(t *testing.T) {
	cases := map[string]string{
		"/v1":      "/v1",
		"/v1/":     "/v1",
		"v1":       "/v1",
		"/":        "",
		"":         "",
		"/api/v1/": "/api/v1",
		"//":       "",
		"///":      "",
		"/v1//":    "/v1",
		"api/v1":   "/api/v1",
	}
	for in, want := range cases {
		if got := normalizeBase(in); got != want {
			t.Errorf("normalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWithAddr(t *testing.T) {
	s := &Server{}
	WithAddr("127.0.0.1:12345")(s)
	if s.addr != "127.0.0.1:12345" {
		t.Fatalf("addr = %q, want %q", s.addr, "127.0.0.1:12345")
	}
}

func TestWithBasePath_Normalizes(t *testing.T) {
	s := &Server{}
	WithBasePath("/openai/v1/")(s)
	if s.basePath != "/openai/v1" {
		t.Fatalf("basePath = %q, want %q", s.basePath, "/openai/v1")
	}
}

func TestWithCORS(t *testing.T) {
	s := &Server{}
	WithCORS("https://example.com")(s)
	if s.corsOrigin != "https://example.com" {
		t.Fatalf("corsOrigin = %q, want %q", s.corsOrigin, "https://example.com")
	}
	WithCORS("")(s)
	if s.corsOrigin != "" {
		t.Fatalf("corsOrigin = %q, want empty (disabled)", s.corsOrigin)
	}
}

func TestWithTLS(t *testing.T) {
	s := &Server{}
	WithTLS("/etc/tls/cert.pem", "/etc/tls/key.pem")(s)
	if s.tlsCert != "/etc/tls/cert.pem" || s.tlsKey != "/etc/tls/key.pem" {
		t.Fatalf("tlsCert=%q tlsKey=%q, want (cert.pem, key.pem) paths",
			s.tlsCert, s.tlsKey)
	}
}

func TestWithAliases_CopiesInput(t *testing.T) {
	src := map[string]string{"gpt-4": "qwen3-coder-next:latest"}
	s := &Server{}
	WithAliases(src)(s)
	src["gpt-4"] = "evil"
	if s.aliases["gpt-4"] != "qwen3-coder-next:latest" {
		t.Fatalf("aliases map aliased caller input: got %q", s.aliases["gpt-4"])
	}
}

func TestWithAliases_Nil(t *testing.T) {
	s := &Server{aliases: map[string]string{"x": "y"}}
	WithAliases(nil)(s)
	if len(s.aliases) != 0 {
		t.Fatalf("WithAliases(nil) did not clear aliases: %v", s.aliases)
	}
}

func TestWithMaxConcurrency_ClampBelowOne(t *testing.T) {
	s := &Server{}
	WithMaxConcurrency(0)(s)
	if s.maxConcurrency != 1 {
		t.Fatalf("got %d, want 1", s.maxConcurrency)
	}
	WithMaxConcurrency(-5)(s)
	if s.maxConcurrency != 1 {
		t.Fatalf("got %d, want 1", s.maxConcurrency)
	}
}

func TestWithMaxConcurrency_Positive(t *testing.T) {
	s := &Server{}
	WithMaxConcurrency(16)(s)
	if s.maxConcurrency != 16 {
		t.Fatalf("got %d, want 16", s.maxConcurrency)
	}
}

func TestWithEmbeddings(t *testing.T) {
	s := &Server{}
	if s.embeddingsEnabled {
		t.Fatal("embeddingsEnabled should default to false before options apply")
	}
	WithEmbeddings(true)(s)
	if !s.embeddingsEnabled {
		t.Fatal("WithEmbeddings(true) did not set embeddingsEnabled")
	}
	WithEmbeddings(false)(s)
	if s.embeddingsEnabled {
		t.Fatal("WithEmbeddings(false) did not clear embeddingsEnabled")
	}
}

func TestWithShutdownTimeout_Positive(t *testing.T) {
	s := &Server{shutdownTimeout: 30 * time.Second}
	WithShutdownTimeout(5 * time.Second)(s)
	if s.shutdownTimeout != 5*time.Second {
		t.Fatalf("got %s, want 5s", s.shutdownTimeout)
	}
}

func TestWithShutdownTimeout_ZeroOrNegativeIgnored(t *testing.T) {
	// Documented contract: values <= 0 leave the existing value untouched so
	// the default set by New() survives. Guard against regressions that
	// accidentally zero the field.
	baseline := 30 * time.Second
	for _, bad := range []time.Duration{0, -1, -500 * time.Millisecond} {
		s := &Server{shutdownTimeout: baseline}
		WithShutdownTimeout(bad)(s)
		if s.shutdownTimeout != baseline {
			t.Fatalf("WithShutdownTimeout(%s) mutated field to %s, want %s",
				bad, s.shutdownTimeout, baseline)
		}
	}
}
