package agentflow

import "testing"

func TestMatchesPath(t *testing.T) {
	cases := []struct {
		path     string
		patterns []string
		want     bool
	}{
		{"src/a.go", []string{"src/a.go"}, true},     // exact
		{"src/a.go", []string{"src/*.go"}, true},     // fnmatch * crosses nothing here
		{"src/deep/a.go", []string{"src/*"}, true},   // fnmatch: * crosses '/'
		{"src/deep/a.go", []string{"docs/*"}, false}, // no match
		{"docs/x.md", []string{"docs/"}, true},       // trailing-slash prefix
		{"docsx/x.md", []string{"docs/"}, false},     // prefix must include the slash
		{"a.go", []string{"", "  ", "a.go"}, true},   // blank patterns skipped
		// fnmatch.translate wraps in (?s:...) (DOTALL): '*' crosses newlines too.
		{"src/a\nb.go", []string{"src/*"}, true},
		// Bracket class: '!' negates ONLY as the first char after '[' (Python fnmatch).
		{"a", []string{"[a!b]"}, true},  // '!' is a literal member here
		{"!", []string{"[a!b]"}, true},  // literal '!' matches
		{"^", []string{"[a!b]"}, false}, // regression: must NOT become [a^b]
		{"a", []string{"[!ab]"}, false}, // leading '!' negates
		{"c", []string{"[!ab]"}, true},  // negated class allows others
		// Leading '^' is a LITERAL member in Python fnmatch (only '!' negates),
		// so it must not become an RE2 negation. Cross-checked against CPython:
		// fnmatch('a','[^ab]')=True, fnmatch('z','[^ab]')=False.
		{"a", []string{"[^ab]"}, true},  // '^','a','b' are members -> 'a' matches
		{"z", []string{"[^ab]"}, false}, // 'z' is not a member -> no match
		{"^", []string{"[^ab]"}, true},  // literal caret matches itself
		// '[!ab]' still negates.
		{"z", []string{"[!ab]"}, true},
		{"a", []string{"[!ab]"}, false},
		// A '^' in a non-leading position is already a literal member.
		{"^", []string{"[a^b]"}, true},
	}
	for _, c := range cases {
		if got := MatchesPath(c.path, c.patterns); got != c.want {
			t.Errorf("MatchesPath(%q,%v) = %v, want %v", c.path, c.patterns, got, c.want)
		}
	}
}

func TestEffectiveScope(t *testing.T) {
	plan := &Plan{
		AllowedFiles: []string{"src/*"},
		BlockedFiles: []string{"src/secret.go"},
		Steps:        []Step{{ID: "P1", Files: []string{"src/a.go", "src/secret.go", "docs/x.md"}}},
	}
	allowed, blocked := EffectiveScope(plan, "P1")
	// docs/x.md is not under allowed_files, secret is allowed-by-glob but blocked.
	if len(allowed) != 2 || allowed[0] != "src/a.go" || allowed[1] != "src/secret.go" {
		t.Fatalf("allowed = %v", allowed)
	}
	if !MatchesPath("src/a.go", allowed) || MatchesPath("src/a.go", blocked) {
		t.Fatalf("src/a.go should be in scope")
	}
	if !MatchesPath("src/secret.go", blocked) {
		t.Fatalf("secret must be blocked")
	}
	_ = blocked
}
