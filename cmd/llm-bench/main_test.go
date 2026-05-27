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

func TestCaptureSampleAndLimitMutuallyExclusive(t *testing.T) {
	if err := validateCaptureSampleAndLimit(10, "n=20"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("want mutex error; got %v", err)
	}
}

func TestCaptureSampleNotYetImplementedReturnsError(t *testing.T) {
	if err := resolveCaptureSample("n=20"); err == nil || !strings.Contains(err.Error(), "sampling not yet implemented") {
		t.Fatalf("want not-yet-implemented error; got %v", err)
	}
}

func TestCaptureSampleEmptyOK(t *testing.T) {
	if err := resolveCaptureSample(""); err != nil {
		t.Fatalf("empty spec: %v", err)
	}
}
