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

// harnessSpans holds, per doc, the markers around its harness-list span. The
// check is scoped to the span because the docs name .claude, .agent,
// .golem.json, models.json and agents.md elsewhere, which would hide a
// dropped entry.
var harnessSpans = []struct{ file, start, end string }{
	{"doc.go", "the set is", "the answer a"},
	{"../../docs/golem.md", "the target sits under:", "what happens on a match:"},
}

// TestHarnessPathsDocumented fails when a harness entry is missing from the
// harness-list span of the Golem package doc or the user guide.
func TestHarnessPathsDocumented(t *testing.T) {
	for _, sp := range harnessSpans {
		b, err := os.ReadFile(sp.file)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(b))
		i := strings.Index(text, sp.start)
		if i < 0 {
			t.Fatalf("%s: start marker %q not found", sp.file, sp.start)
		}
		j := strings.Index(text[i:], sp.end)
		if j < 0 {
			t.Fatalf("%s: end marker %q not found after %q", sp.file, sp.end, sp.start)
		}
		span := text[i : i+j]
		for _, e := range interceptor.HarnessPaths() {
			if !docMentions(span, e) {
				t.Errorf("%s: harness list does not mention %q", sp.file, e)
			}
		}
	}
}
