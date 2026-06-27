package rag

import (
	"context"
	"fmt"
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

// CorpusPresence reports deterministic keyword/literal presence across the whole
// indexed corpus. It is a keyword absence signal, not a semantic absence proof.
type CorpusPresence struct {
	Terms          []string // code-shaped or quoted-literal terms extracted from the query
	TotalMatches   int      // distinct chunks matching any useful term across the corpus
	OutsideMatches int      // matching chunks whose ID is not in retrievedIDs
}

// KeywordPresence runs a deterministic disjunctive FTS5 search for the query's
// useful terms across all chunks. retrievedIDs are excluded when computing
// OutsideMatches; pass nil to treat every match as outside. No LLM call.
func (s *SQLiteStore) KeywordPresence(ctx context.Context, query string, retrievedIDs []string) (CorpusPresence, error) {
	terms, expr := corpusKeywordTerms(query)
	if expr == "" {
		return CorpusPresence{}, nil
	}
	cp := CorpusPresence{Terms: terms}

	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT c.id)
		 FROM chunks_fts JOIN chunks c ON c.rowid = chunks_fts.rowid
		 WHERE chunks_fts MATCH ?`, expr).Scan(&cp.TotalMatches); err != nil {
		return CorpusPresence{}, fmt.Errorf("rag: corpus keyword count: %w", err)
	}

	if len(retrievedIDs) == 0 {
		cp.OutsideMatches = cp.TotalMatches
		return cp, nil
	}

	// COUNT matches whose chunk id is not in the retrieved set.
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(retrievedIDs)), ",")
	q := fmt.Sprintf(
		`SELECT COUNT(DISTINCT c.id) FROM chunks_fts JOIN chunks c ON c.rowid = chunks_fts.rowid
		 WHERE chunks_fts MATCH ? AND c.id NOT IN (%s)`, placeholders)
	sqlArgs := make([]any, 0, len(retrievedIDs)+1)
	sqlArgs = append(sqlArgs, expr)
	for _, id := range retrievedIDs {
		sqlArgs = append(sqlArgs, id)
	}
	if err := s.db.QueryRowContext(ctx, q, sqlArgs...).Scan(&cp.OutsideMatches); err != nil {
		return CorpusPresence{}, fmt.Errorf("rag: corpus keyword outside count: %w", err)
	}
	return cp, nil
}
