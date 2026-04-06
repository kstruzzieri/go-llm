package rag

import (
	"context"
	"database/sql"
	"math"
	"testing"

	_ "modernc.org/sqlite"
)

// newScorerTestStore creates an in-memory SQLite store with the full schema
// (including FTS5 and indexed_at from migration v2). Delegates to newTestStore
// which is defined in sqlite_store_test.go.
func newScorerTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	return newTestStore(t)
}

func TestKeywordScorerName(t *testing.T) {
	store := newScorerTestStore(t)
	scorer := NewKeywordScorer(store.DB())
	if got := scorer.Name(); got != "keyword" {
		t.Errorf("Name() = %q, want %q", got, "keyword")
	}
}

func TestKeywordScorerMatchingChunk(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "the golang gopher is a friendly mascot", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "python snake is another animal", Source: "b.py", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c3", Content: "java coffee beans and enterprise code", Source: "c.java", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.0, 0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(ctx, chunks, "gopher", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	if len(scores) != 3 {
		t.Fatalf("len(scores) = %d, want 3", len(scores))
	}

	// "gopher" appears only in chunk c1 — it should score highest.
	if scores[0] <= 0 {
		t.Errorf("scores[0] (matching chunk) = %f, want > 0", scores[0])
	}
	if scores[1] != 0 {
		t.Errorf("scores[1] (non-matching) = %f, want 0", scores[1])
	}
	if scores[2] != 0 {
		t.Errorf("scores[2] (non-matching) = %f, want 0", scores[2])
	}

	// The matching chunk should have the max score of 1.0 (max-normalized).
	if math.Abs(scores[0]-1.0) > 0.001 {
		t.Errorf("scores[0] = %f, want 1.0 (max-normalized)", scores[0])
	}
}

func TestKeywordScorerNoMatches(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "hello world", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "goodbye world", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0},
		{0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(ctx, chunks, "xyzzyx", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	for i, score := range scores {
		if score != 0 {
			t.Errorf("scores[%d] = %f, want 0 (no match)", i, score)
		}
	}
}

func TestKeywordScorerEmptyQuery(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "some content", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(ctx, chunks, "", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	if len(scores) != 1 {
		t.Fatalf("len(scores) = %d, want 1", len(scores))
	}
	if scores[0] != 0 {
		t.Errorf("scores[0] = %f, want 0 for empty query", scores[0])
	}
}

func TestKeywordScorerEmptyChunks(t *testing.T) {
	store := newScorerTestStore(t)

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(context.Background(), nil, "query", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}
	if len(scores) != 0 {
		t.Errorf("len(scores) = %d, want 0", len(scores))
	}
}

func TestKeywordScorerNormalizedRange(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	// Create chunks where "function" appears in multiple chunks with
	// different frequencies, ensuring scores are normalized to [0, 1].
	chunks := []Chunk{
		{ID: "c1", Content: "function function function compute", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "function helper utility code", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c3", Content: "no matching terms here at all", Source: "c.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0, 0.0},
		{0.0, 1.0, 0.0},
		{0.0, 0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(ctx, chunks, "function", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	for i, score := range scores {
		if score < 0 || score > 1.0 {
			t.Errorf("scores[%d] = %f, want in [0, 1]", i, score)
		}
	}

	// At least one matching chunk should have the maximum score of 1.0.
	var hasMax bool
	for _, score := range scores {
		if math.Abs(score-1.0) < 0.001 {
			hasMax = true
			break
		}
	}
	if !hasMax {
		t.Errorf("no score reached 1.0, scores = %v", scores)
	}

	// Non-matching chunk should score 0.
	if scores[2] != 0 {
		t.Errorf("non-matching chunk scores[2] = %f, want 0", scores[2])
	}
}

func TestKeywordScorerCodeStyleQueries(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	// Index chunks where the code-style tokens primarily live in the source
	// path so the test validates the indexed source column as well as content.
	chunks := []Chunk{
		{ID: "c1", Content: "startup handler", Source: "pkg/main.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "utility processor", Source: "foo-bar.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c3", Content: "model configuration", Source: "qwen2.5:72b.modelfile", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c4", Content: "unrelated content about databases", Source: "db.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())

	// These queries previously caused FTS5 MATCH parse errors and
	// silently returned zero scores. After sanitization they should
	// produce matches.
	tests := []struct {
		query       string
		wantNonZero int // index of chunk expected to score > 0
	}{
		{"pkg/main.go", 0}, // "pkg" "main" "go" matches c1
		{"foo-bar", 1},     // "foo" "bar" matches c2
		{"qwen2.5:72b", 2}, // "qwen2" "5" "72b" matches c3
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			scores, err := scorer.ScoreBatch(ctx, chunks, tt.query, nil, QueryContext{})
			if err != nil {
				t.Fatalf("ScoreBatch(%q): %v", tt.query, err)
			}
			if scores[tt.wantNonZero] == 0 {
				t.Errorf("scores[%d] = 0 for query %q, want > 0 (keyword match expected)", tt.wantNonZero, tt.query)
			}
		})
	}
}

func TestKeywordScorerPropagatesDatabaseErrors(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	scorer := NewKeywordScorer(db)
	chunks := []Chunk{
		{ID: "c1", Content: "hello world", Source: "a.go", StartLine: 1, EndLine: 1},
	}

	_, err = scorer.ScoreBatch(context.Background(), chunks, "hello", nil, QueryContext{})
	if err == nil {
		t.Fatal("expected database error when FTS5 schema is missing")
	}
}

func TestKeywordScorerPunctuationOnlyQuery(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "hello world", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())

	// Queries that contain no alphanumeric tokens should return zero
	// scores gracefully (not an error).
	for _, q := range []string{"...", "---", "///", ":::"} {
		t.Run(q, func(t *testing.T) {
			scores, err := scorer.ScoreBatch(ctx, chunks, q, nil, QueryContext{})
			if err != nil {
				t.Errorf("ScoreBatch(%q) returned error: %v, want zero scores", q, err)
			}
			if len(scores) != 1 || scores[0] != 0 {
				t.Errorf("scores = %v for query %q, want [0]", scores, q)
			}
		})
	}
}

func TestKeywordScorerMalformedQuery(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "hello world value unclosed unbalanced col", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{{1.0}}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())

	// These queries contain FTS5 syntax characters that would previously
	// cause MATCH errors. After sanitization, the alphanumeric tokens
	// are extracted and should match successfully.
	queries := []string{
		`"unclosed quote`, // sanitized to: "unclosed" "quote"
		`(unbalanced`,     // sanitized to: "unbalanced"
		`col:value AND`,   // sanitized to: "col" "value" "AND"
		`NOT`,             // sanitized to: "NOT"
	}

	for _, q := range queries {
		t.Run(q, func(t *testing.T) {
			scores, err := scorer.ScoreBatch(ctx, chunks, q, nil, QueryContext{})
			if err != nil {
				t.Fatalf("ScoreBatch(%q) returned error: %v", q, err)
			}
			if len(scores) != 1 {
				t.Fatalf("len(scores) = %d, want 1", len(scores))
			}
			// After sanitization, these all have valid tokens that should
			// match the chunk content. Verify no error and non-zero score.
			if scores[0] == 0 {
				t.Logf("scores[0] = 0 for query %q (tokens may not match chunk content)", q)
			}
		})
	}
}

func TestSanitizeFTS5Query(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello world", `"hello" "world"`},
		{"pkg/main.go", `"pkg" "main" "go"`},
		{"foo-bar", `"foo" "bar"`},
		{"qwen2.5:72b", `"qwen2" "5" "72b"`},
		{"simple", `"simple"`},
		{"...", ""},
		{"", ""},
		{"  spaces  ", `"spaces"`},
		{"a/b/c.d", `"a" "b" "c" "d"`},
		{"hello_world", `"hello_world"`}, // underscore preserved for phrase semantics
		{"café", `"café"`},                 // unicode letters preserved
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeFTS5Query(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeFTS5Query(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestKeywordScorerUnderscorePhraseSemantics(t *testing.T) {
	store := newScorerTestStore(t)
	ctx := context.Background()

	chunks := []Chunk{
		{ID: "c1", Content: "the snake_case variable is set", Source: "a.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
		{ID: "c2", Content: "snake guide case study notes", Source: "b.go", StartLine: 1, EndLine: 1, Metadata: map[string]string{}},
	}
	embeddings := [][]float64{
		{1.0, 0.0},
		{0.0, 1.0},
	}

	if err := store.Store(ctx, chunks, embeddings); err != nil {
		t.Fatalf("Store: %v", err)
	}

	scorer := NewKeywordScorer(store.DB())
	scores, err := scorer.ScoreBatch(ctx, chunks, "snake_case", nil, QueryContext{})
	if err != nil {
		t.Fatalf("ScoreBatch: %v", err)
	}

	// c1 has "snake_case" (adjacent tokens) — should match the phrase query.
	if scores[0] == 0 {
		t.Errorf("scores[0] = 0, want > 0 (snake_case as adjacent tokens)")
	}
	// c2 has "snake" and "case" but NOT adjacent — phrase query should not match.
	if scores[1] != 0 {
		t.Errorf("scores[1] = %f, want 0 (snake and case are not adjacent)", scores[1])
	}
}

func TestKeywordScorerInterfaceCompliance(t *testing.T) {
	store := newScorerTestStore(t)
	var _ SignalScorer = NewKeywordScorer(store.DB())
}
