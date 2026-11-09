package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// docMentions reports whether the lower-cased doc names entry as a whole
// token, so "opencode.jsonc" does not satisfy "opencode.json".
func docMentions(doc, entry string) bool {
	return regexp.MustCompile("(?:^|[^a-z0-9_.-])" + regexp.QuoteMeta(entry) +
		`(?:$|[^a-z0-9_.-]|\.(?:$|[^a-z0-9]))`).MatchString(doc)
}

func TestDocMentions(t *testing.T) {
	cases := []struct {
		doc, entry string
		want       bool
	}{
		{"see opencode.jsonc", "opencode.json", false},
		{".cursorrules only", ".cursor", false},
		{".github/copilot-instructions.md", ".github/copilot", false},
		{"`.cursor/`", ".cursor", true},
		{"opencode.json.", "opencode.json", true},
		{"sub/agents.md,", "agents.md", true},
	}
	for _, c := range cases {
		if got := docMentions(c.doc, c.entry); got != c.want {
			t.Errorf("docMentions(%q, %q) = %v, want %v", c.doc, c.entry, got, c.want)
		}
	}
}

// TestHarnessPathsDocumented fails when a harness entry is missing from the
// Golem package doc or the user guide.
func TestHarnessPathsDocumented(t *testing.T) {
	for _, file := range []string{"doc.go", "../../docs/golem.md"} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(b))
		for _, e := range interceptor.HarnessPaths() {
			if !docMentions(text, e) {
				t.Errorf("%s does not mention harness path %q", file, e)
			}
		}
	}
}
