package rag

import (
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
