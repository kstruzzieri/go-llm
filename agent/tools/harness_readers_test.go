package tools

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHarnessReadMarkdownImports(t *testing.T) {
	rows := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", "@docs/a.md", []string{"docs/a.md"}},
		{"trailing dot", "see @docs/b.md.", []string{"docs/b.md.", "docs/b.md"}},
		{"paren before at", "(@x/c.md)", nil},
		{"escaped space", "@my\\ file.md", []string{"my file.md"}},
		{"email", "mail a@b.com", nil},
		{"home", "@~/notes.md", []string{"~/notes.md"}},
		{"absolute", "@/abs/p.md", []string{"/abs/p.md"}},
		{"crlf", "line1\r\n@docs/crlf.md\r\n", []string{"docs/crlf.md"}},
		{"backtick", "`@code/span.md`", nil},
		{"tab", "\t@tab.md", []string{"tab.md"}},
		{"bom", "\ufeff@docs/bom.md", []string{"docs/bom.md"}},
		{"nbsp", "see\u00a0@docs/nbsp.md", []string{"docs/nbsp.md"}},
		{"long token", "@" + strings.Repeat("a", 5000), nil},
		{"long text no newline", strings.Repeat("x", 1<<20) + " @docs/late.md", []string{"docs/late.md"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			var got []string
			if err := scanMarkdownImports(strings.NewReader(r.in), func(s string) { got = append(got, s) }); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, r.want) {
				t.Fatalf("got %q, want %q", got, r.want)
			}
		})
	}
}

func TestHarnessReadOpencodeInstructions(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"instructions":["CONTRIBUTING.md","docs/*.md","https://x/y.md",3]}`), &doc); err != nil {
		t.Fatal(err)
	}
	paths, patterns := opencodeInstructions(doc)
	if !reflect.DeepEqual(paths, []string{"CONTRIBUTING.md"}) || !reflect.DeepEqual(patterns, []string{"docs/*.md"}) {
		t.Fatalf("got %q %q", paths, patterns)
	}
	if err := json.Unmarshal([]byte(`{"instructions":"x"}`), &doc); err != nil {
		t.Fatal(err)
	}
	if p, g := opencodeInstructions(doc); len(p) != 0 || len(g) != 0 {
		t.Fatalf("got %q %q", p, g)
	}
}

func TestHarnessReadGlobAgrees(t *testing.T) {
	rows := []struct{ pat, name string }{
		{"**/*.md", "a/b/c.md"},
		{"**/*.md", "c.md"},
		{"**/*.md", "a/b/c.txt"},
		{"a/**", "a"},
		{"a/**", "a/b/c"},
		{"a/*/c", "a/b/c"},
		{"a/*/c", "a/b/d/c"},
		{"[ab].md", "a.md"},
		{"[ab].md", "c.md"},
		{"a/**/c", "a/c"},
		{"**", ""},
		{"a/?", "a/bc"},
	}
	for _, r := range rows {
		if got, want := matchGlobBounded(r.pat, r.name), matchGlob(r.pat, r.name); got != want {
			t.Errorf("%q vs %q: bounded=%v matchGlob=%v", r.pat, r.name, got, want)
		}
	}
}

func TestHarnessReadGlobAdversarial(t *testing.T) {
	done := make(chan bool, 1)
	go func() {
		done <- matchGlobBounded(strings.Repeat("**/a/", 20)+"b", strings.Repeat("a/", 60)+"c")
	}()
	select {
	case got := <-done:
		if got {
			t.Fatal("unexpected match")
		}
	case <-time.After(time.Second):
		t.Fatal("matchGlobBounded did not finish within 1s")
	}
}
