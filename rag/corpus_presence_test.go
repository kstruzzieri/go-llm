package rag

import (
	"context"
	"strings"
	"testing"
)

func TestCorpusKeywordTerms(t *testing.T) {
	t.Run("code query -> tokens and OR expr", func(t *testing.T) {
		terms, expr := corpusKeywordTerms("where is BuildContext defined")
		if len(terms) == 0 || !contains(terms, "BuildContext") {
			t.Fatalf("terms = %v, want BuildContext", terms)
		}
		if !strings.Contains(expr, `"BuildContext"`) || (len(terms) > 1 && !strings.Contains(expr, " OR ")) {
			t.Errorf("expr = %q", expr)
		}
	})
	t.Run("quoted literal phrase preserved", func(t *testing.T) {
		terms, expr := corpusKeywordTerms(`find "hello world"`)
		if len(terms) != 1 || terms[0] != "hello world" {
			t.Fatalf("terms = %v, want [hello world]", terms)
		}
		if !strings.Contains(expr, `"hello world"`) {
			t.Errorf("expr = %q, want quoted phrase", expr)
		}
	})
	t.Run("prose only -> no terms", func(t *testing.T) {
		terms, expr := corpusKeywordTerms("what is the meaning of all this")
		if len(terms) != 0 || expr != "" {
			t.Errorf("terms = %v expr = %q, want empty", terms, expr)
		}
	})
	t.Run("dedupe", func(t *testing.T) {
		// getUserID is unambiguously code-shaped (internal case boundary), so it
		// survives extractCodeQuery; repeated, it must collapse to one term.
		terms, _ := corpusKeywordTerms("getUserID getUserID")
		if len(terms) != 1 || terms[0] != "getUserID" {
			t.Errorf("terms = %v, want exactly [getUserID]", terms)
		}
	})
	t.Run("punctuation-only quoted literal dropped", func(t *testing.T) {
		// A quoted literal with no letter/digit has no searchable FTS5 content.
		terms, expr := corpusKeywordTerms(`find "==="`)
		if len(terms) != 0 || expr != "" {
			t.Errorf("terms = %v expr = %q, want empty", terms, expr)
		}
	})
}

// contains is shared by the corpus_presence tests in this file.
func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func TestKeywordPresence(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(t.TempDir() + "/p.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	chunks := []Chunk{
		{ID: "c1", Source: "a.go", StartLine: 1, EndLine: 1, Content: "func BuildContext() {}"},
		{ID: "c2", Source: "b.go", StartLine: 1, EndLine: 1, Content: "func numberLines() {}"},
		{ID: "c3", Source: "c.go", StartLine: 1, EndLine: 1, Content: "package main // unrelated prose"},
		{ID: "c4", Source: "d.go", StartLine: 1, EndLine: 1, Content: `const greeting = "hello world"`},
	}
	embs := [][]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {1, 1, 0}}
	if err := store.Store(ctx, chunks, embs); err != nil {
		t.Fatal(err)
	}

	t.Run("present across corpus", func(t *testing.T) {
		cp, err := store.KeywordPresence(ctx, "where is BuildContext", nil)
		if err != nil {
			t.Fatal(err)
		}
		if cp.TotalMatches != 1 || cp.OutsideMatches != 1 {
			t.Fatalf("cp = %+v, want total=1 outside=1", cp)
		}
		if !contains(cp.Terms, "BuildContext") {
			t.Errorf("terms = %v", cp.Terms)
		}
	})

	t.Run("outside excludes retrieved id", func(t *testing.T) {
		cp, err := store.KeywordPresence(ctx, "BuildContext", []string{"c1"})
		if err != nil {
			t.Fatal(err)
		}
		if cp.TotalMatches != 1 || cp.OutsideMatches != 0 {
			t.Fatalf("cp = %+v, want total=1 outside=0", cp)
		}
	})

	t.Run("absent term", func(t *testing.T) {
		cp, err := store.KeywordPresence(ctx, "NonexistentSymbolXYZ", nil)
		if err != nil {
			t.Fatal(err)
		}
		if cp.TotalMatches != 0 {
			t.Fatalf("cp = %+v, want total=0", cp)
		}
	})

	t.Run("no useful terms", func(t *testing.T) {
		cp, err := store.KeywordPresence(ctx, "what is the meaning of it all", nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(cp.Terms) != 0 || cp.TotalMatches != 0 {
			t.Fatalf("cp = %+v, want no terms", cp)
		}
	})

	t.Run("quoted literal phrase", func(t *testing.T) {
		cp, err := store.KeywordPresence(ctx, `find "hello world"`, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cp.TotalMatches != 1 || cp.OutsideMatches != 1 {
			t.Fatalf("cp = %+v, want total=1 outside=1", cp)
		}
		if !contains(cp.Terms, "hello world") {
			t.Errorf("terms = %v, want hello world", cp.Terms)
		}
	})

	t.Run("disjunction: terms split across chunks both count", func(t *testing.T) {
		cp, err := store.KeywordPresence(ctx, "BuildContext and numberLines", nil)
		if err != nil {
			t.Fatal(err)
		}
		if cp.TotalMatches != 2 {
			t.Fatalf("cp = %+v, want total=2 (OR semantics)", cp)
		}
	})
}
