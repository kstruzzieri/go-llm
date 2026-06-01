package main

import (
	"strings"
	"testing"
)

func TestResolveToolSchemaSourceStdio(t *testing.T) {
	src, err := resolveToolSchemaSource("echo mock-server", "")
	if err != nil {
		t.Fatalf("resolveToolSchemaSource: %v", err)
	}
	if src == nil {
		t.Fatalf("nil source for non-empty stdio command")
	}
}

func TestResolveToolSchemaSourceHTTP(t *testing.T) {
	src, err := resolveToolSchemaSource("", "http://localhost:9999")
	if err != nil {
		t.Fatalf("resolveToolSchemaSource: %v", err)
	}
	if src == nil {
		t.Fatalf("nil source for non-empty http endpoint")
	}
}

func TestResolveToolSchemaSourceMutuallyExclusive(t *testing.T) {
	_, err := resolveToolSchemaSource("echo a", "http://x")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutex error; got %v", err)
	}
}

func TestResolveToolSchemaSourceBothEmptyIsNil(t *testing.T) {
	src, err := resolveToolSchemaSource("", "")
	if err != nil {
		t.Fatalf("resolveToolSchemaSource(empty): %v", err)
	}
	if src != nil {
		t.Fatalf("want nil source when no transport configured; got %T", src)
	}
}

func TestResolveJudgeTransportConfig_APIKeyFlagWins(t *testing.T) {
	cfg := resolveJudgeTransportConfig("openai-compat", " https://api.example.com ", "flag-key", func(string) string { return "env-key" })
	if cfg.apiKey != "flag-key" {
		t.Fatalf("apiKey = %q; want flag-key (flag overrides env)", cfg.apiKey)
	}
	if cfg.baseURL != "https://api.example.com" {
		t.Fatalf("baseURL = %q; want trimmed value", cfg.baseURL)
	}
	if cfg.transport != "openai-compat" {
		t.Fatalf("transport = %q; want openai-compat", cfg.transport)
	}
}

func TestResolveJudgeTransportConfig_APIKeyEnvFallback(t *testing.T) {
	calls := 0
	cfg := resolveJudgeTransportConfig("openai-compat", "https://x", "", func(name string) string {
		calls++
		if name != judgeAPIKeyEnvVar {
			t.Errorf("looked up %q; want %q", name, judgeAPIKeyEnvVar)
		}
		return "env-key"
	})
	if cfg.apiKey != "env-key" {
		t.Fatalf("apiKey = %q; want env-key fallback", cfg.apiKey)
	}
	if calls != 1 {
		t.Fatalf("env lookups = %d; want exactly 1", calls)
	}
}

func TestCaptureSampleAndLimitMutuallyExclusive(t *testing.T) {
	if err := validateCaptureSampleAndLimit(10, "n=20"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutex error; got %v", err)
	}
}

func TestResolveCaptureSampleParsesValidSpec(t *testing.T) {
	spec, err := resolveCaptureSample("n=10,stratify=token-length")
	if err != nil {
		t.Fatalf("resolveCaptureSample: %v", err)
	}
	if spec == nil || spec.N != 10 {
		t.Fatalf("spec=%+v; want N=10", spec)
	}
}

func TestResolveCaptureSampleEmptyReturnsNil(t *testing.T) {
	spec, err := resolveCaptureSample("")
	if err != nil {
		t.Fatalf("resolveCaptureSample(empty): %v", err)
	}
	if spec != nil {
		t.Fatalf("want nil spec for empty input; got %+v", spec)
	}
}
