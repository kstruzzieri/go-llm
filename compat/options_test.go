package compat

import "testing"

func TestNormalizeBase(t *testing.T) {
	cases := map[string]string{
		"/v1":      "/v1",
		"/v1/":     "/v1",
		"v1":       "/v1",
		"/":        "",
		"":         "",
		"/api/v1/": "/api/v1",
	}
	for in, want := range cases {
		if got := normalizeBase(in); got != want {
			t.Errorf("normalizeBase(%q) = %q, want %q", in, got, want)
		}
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
