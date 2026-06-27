package rag

import (
	"strings"
	"unicode"
)

// corpusKeywordTerms reduces a query to quoted-literal phrases plus code-shaped
// tokens from the remaining text, then returns the deduped term list plus a
// disjunctive FTS5 MATCH expression ("t1" OR "t2" ...).
//
// Disjunction is deliberate: corpus-absence must not false-negative. OR over
// useful terms over-detects presence (the safe direction for an absence proof)
// rather than risking a false "absent" when terms are split across chunks.
// Returns nil, "" when no useful terms exist.
func corpusKeywordTerms(query string) ([]string, string) {
	remainder, spans := extractQuotedSpans(query)
	var terms []string
	seen := make(map[string]bool)
	add := func(tok string) {
		tok = strings.TrimSpace(tok)
		if tok == "" || seen[tok] {
			return
		}
		seen[tok] = true
		terms = append(terms, tok)
	}
	for _, span := range spans {
		add(span)
	}

	extracted := extractCodeQuery(remainder)
	var cur strings.Builder
	flush := func() {
		add(cur.String())
		cur.Reset()
	}
	for _, r := range extracted {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	if len(terms) == 0 {
		return nil, ""
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return terms, strings.Join(quoted, " OR ")
}
