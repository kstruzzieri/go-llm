package rag

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode"
)

// KeywordScorer computes keyword relevance using FTS5 BM25 ranking.
// It requires a *sql.DB handle to query the chunks_fts virtual table,
// which must have been created via the v2 schema migration.
type KeywordScorer struct {
	db *sql.DB
}

// NewKeywordScorer creates a keyword scorer backed by the given database.
// The database must have the chunks_fts FTS5 virtual table (see the v2 schema migration).
func NewKeywordScorer(db *sql.DB) *KeywordScorer {
	return &KeywordScorer{db: db}
}

// Name returns "keyword".
func (s *KeywordScorer) Name() string { return "keyword" }

// ScoreBatch computes BM25-based keyword relevance for each chunk.
// Scores are normalized to the [0, 1] range using max-normalization.
//
// The raw query is sanitized into FTS5-safe tokens before issuing MATCH,
// so code-style queries like "pkg/main.go", "foo-bar", and "qwen2.5:72b"
// are properly tokenized rather than causing FTS5 parse errors.
func (s *KeywordScorer) ScoreBatch(ctx context.Context, chunks []Chunk, query string,
	queryEmbedding []float64, qCtx QueryContext) ([]float64, error) {
	if len(chunks) == 0 || query == "" {
		return make([]float64, len(chunks)), nil
	}

	// Sanitize the query into FTS5-safe quoted tokens.
	sanitized := sanitizeFTS5Query(query)
	if sanitized == "" {
		// Query contained no searchable tokens (e.g., all punctuation).
		return make([]float64, len(chunks)), nil
	}

	// Query FTS5 for BM25 scores. bm25() returns negative values where
	// more negative = more relevant. We negate to get positive scores.
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, -bm25(chunks_fts) as score
		 FROM chunks_fts
		 JOIN chunks c ON c.rowid = chunks_fts.rowid
		 WHERE chunks_fts MATCH ?`, sanitized)
	if err != nil {
		// After sanitization, FTS5 syntax errors should not occur.
		// Propagate database errors (disk full, locked, etc.) rather
		// than silently returning zero scores.
		return nil, fmt.Errorf("rag: keyword FTS5 query: %w", err)
	}
	defer rows.Close()

	// Build map of chunk ID -> raw BM25 score.
	bm25Scores := make(map[string]float64)
	var maxScore float64
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, fmt.Errorf("rag: scan BM25 score: %w", err)
		}
		bm25Scores[id] = score
		if score > maxScore {
			maxScore = score
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rag: iterate BM25 scores: %w", err)
	}

	// Normalize scores to [0, 1] range using max-normalization.
	scores := make([]float64, len(chunks))
	if maxScore > 0 {
		for i, chunk := range chunks {
			if raw, ok := bm25Scores[chunk.ID]; ok {
				scores[i] = raw / maxScore
			}
		}
	}

	return scores, nil
}

// sanitizeFTS5Query converts a raw user query into a safe FTS5 MATCH expression.
// It extracts tokens consisting of letters, digits, and underscores, then wraps
// each in double quotes to prevent FTS5 syntax interpretation.
//
// Underscores are preserved within tokens so that FTS5 applies phrase semantics:
// the unicode61 tokenizer will split "snake_case" into sub-tokens [snake, case],
// but because they appear inside a single quoted string, FTS5 requires them to be
// adjacent and in order. Splitting into separate quoted tokens ("snake" "case")
// would allow matches anywhere in the document.
//
// Examples:
//
//	"pkg/main.go"   → `"pkg" "main" "go"`
//	"foo-bar"       → `"foo" "bar"`
//	"qwen2.5:72b"   → `"qwen2" "5" "72b"`
//	"snake_case"    → `"snake_case"` (phrase: adjacent tokens)
//	"hello world"   → `"hello" "world"`
//	"..."           → "" (no tokens)
func sanitizeFTS5Query(query string) string {
	var tokens []string
	var current strings.Builder
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			current.WriteRune(r)
		} else if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	if len(tokens) == 0 {
		return ""
	}

	// Quote each token. Double quotes inside tokens are escaped by doubling
	// (standard FTS5 quoting), though alphanumeric+underscore tokens won't
	// contain them.
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " ")
}
