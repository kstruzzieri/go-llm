package main

import (
	"strings"
	"testing"
)

func TestRedactStringsStripsLocalPaths(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"failed reading /tmp/foo.json", "failed reading <redacted-path>"},
		{"home is /Users/keith.struzzieri/work", "home is <redacted-path>"},
		{"$HOME/.cache trashed", "<redacted-path>/.cache trashed"},
		{"no paths here", "no paths here"},
	}
	for _, tc := range cases {
		if got := redactString(tc.in); got != tc.want {
			t.Errorf("redactString(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactStringsStripsJudgeJustification(t *testing.T) {
	// LLMJudgeScorer writes "justification: ..." sentences into Notes
	// when in debug mode. The redactor drops them so the artifact never
	// contains free-form judge reasoning.
	got := redactString(`score: 0.8; justification: "the model called the wrong tool then recovered"`)
	if strings.Contains(got, "justification") {
		t.Fatalf("redactString left judge justification in place: %q", got)
	}
	if !strings.Contains(got, "score: 0.8") {
		t.Fatalf("redactString stripped the wrong content; got %q", got)
	}
}

func TestRedactErrorMessage(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"context deadline exceeded", "<error: timeout>"},
		{"dial tcp 127.0.0.1:11434: connection refused", "<error: network>"},
		{"json: cannot unmarshal number into Go struct field", "<error: parse>"},
		{"something totally unexpected happened", "<error: other>"},
	}
	for _, tc := range cases {
		if got := redactErrorMessage(tc.in); got != tc.want {
			t.Errorf("redactErrorMessage(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactStringPreservesAllowedSubstrings(t *testing.T) {
	const allowed = "qwen3-coder-next:latest trace-001 sha256:abc123"
	if got := redactString(allowed); got != allowed {
		t.Fatalf("redactString stripped allowed content: %q", got)
	}
}
