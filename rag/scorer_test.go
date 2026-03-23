package rag

import (
	"context"
	"math"
	"testing"
)

// --- SemanticScorer tests ---

func TestSemanticScorerName(t *testing.T) {
	s := NewSemanticScorer()
	if got := s.Name(); got != "semantic" {
		t.Errorf("Name() = %q, want %q", got, "semantic")
	}
}

func TestSemanticScorerScoreBatch(t *testing.T) {
	tests := []struct {
		name           string
		chunks         []Chunk
		embeddings     map[string][]float64
		queryEmbedding []float64
		wantScores     []float64
		wantErr        bool
	}{
		{
			name: "basic cosine similarity",
			chunks: []Chunk{
				{ID: "c1", Content: "identical direction", Source: "a.go"},
				{ID: "c2", Content: "orthogonal", Source: "b.go"},
				{ID: "c3", Content: "similar direction", Source: "c.go"},
			},
			embeddings: map[string][]float64{
				"c1": {1.0, 0.0, 0.0},
				"c2": {0.0, 1.0, 0.0},
				"c3": {0.9, 0.1, 0.0},
			},
			queryEmbedding: []float64{1.0, 0.0, 0.0},
			wantScores:     []float64{1.0, 0.0, cosineSimilarity([]float64{1.0, 0.0, 0.0}, []float64{0.9, 0.1, 0.0})},
		},
		{
			name: "missing embedding returns zero",
			chunks: []Chunk{
				{ID: "c1", Content: "has embedding", Source: "a.go"},
				{ID: "c2", Content: "no embedding", Source: "b.go"},
			},
			embeddings: map[string][]float64{
				"c1": {1.0, 0.0},
			},
			queryEmbedding: []float64{1.0, 0.0},
			wantScores:     []float64{1.0, 0.0},
		},
		{
			name:           "empty chunks returns empty scores",
			chunks:         []Chunk{},
			embeddings:     map[string][]float64{},
			queryEmbedding: []float64{1.0, 0.0},
			wantScores:     []float64{},
		},
		{
			name: "opposite vectors",
			chunks: []Chunk{
				{ID: "c1", Content: "opposite", Source: "a.go"},
			},
			embeddings: map[string][]float64{
				"c1": {-1.0, 0.0},
			},
			queryEmbedding: []float64{1.0, 0.0},
			wantScores:     []float64{-1.0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scorer := NewSemanticScorer()
			scorer.SetEmbeddings(tt.embeddings)

			scores, err := scorer.ScoreBatch(context.Background(), tt.chunks, "test query",
				tt.queryEmbedding, QueryContext{})
			if (err != nil) != tt.wantErr {
				t.Fatalf("ScoreBatch() error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(scores) != len(tt.wantScores) {
				t.Fatalf("ScoreBatch() returned %d scores, want %d", len(scores), len(tt.wantScores))
			}
			for i, want := range tt.wantScores {
				if math.Abs(scores[i]-want) > 0.001 {
					t.Errorf("scores[%d] = %f, want %f", i, scores[i], want)
				}
			}
		})
	}
}

func TestSemanticScorerContextCancellation(t *testing.T) {
	scorer := NewSemanticScorer()
	scorer.SetEmbeddings(map[string][]float64{
		"c1": {1.0, 0.0},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	chunks := []Chunk{{ID: "c1", Content: "test", Source: "a.go"}}
	_, err := scorer.ScoreBatch(ctx, chunks, "query", []float64{1.0, 0.0}, QueryContext{})
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

// --- StructuralScorer tests ---

func TestStructuralScorerName(t *testing.T) {
	s := NewStructuralScorer()
	if got := s.Name(); got != "structural" {
		t.Errorf("Name() = %q, want %q", got, "structural")
	}
}

func TestStructuralScorerScoreBatch(t *testing.T) {
	tests := []struct {
		name       string
		chunks     []Chunk
		qCtx       QueryContext
		wantScores []float64
	}{
		{
			name: "same file scores 1.0",
			chunks: []Chunk{
				{ID: "c1", Source: "/project/src/main.go"},
			},
			qCtx: QueryContext{
				CurrentFile: "/project/src/main.go",
			},
			wantScores: []float64{1.0},
		},
		{
			name: "same directory scores 0.8",
			chunks: []Chunk{
				{ID: "c1", Source: "/project/src/util.go"},
			},
			qCtx: QueryContext{
				CurrentFile: "/project/src/main.go",
			},
			wantScores: []float64{0.8},
		},
		{
			name: "parent directory scores 0.5",
			chunks: []Chunk{
				{ID: "c1", Source: "/project/config.go"},
			},
			qCtx: QueryContext{
				CurrentFile: "/project/src/main.go",
			},
			wantScores: []float64{0.5},
		},
		{
			name: "open file bonus scores 0.3",
			chunks: []Chunk{
				{ID: "c1", Source: "/other/place/lib.go"},
			},
			qCtx: QueryContext{
				CurrentFile: "/project/src/main.go",
				OpenFiles:   []string{"/other/place/lib.go"},
			},
			wantScores: []float64{0.3},
		},
		{
			name: "open file does not override higher score",
			chunks: []Chunk{
				{ID: "c1", Source: "/project/src/main.go"},
			},
			qCtx: QueryContext{
				CurrentFile: "/project/src/main.go",
				OpenFiles:   []string{"/project/src/main.go"},
			},
			wantScores: []float64{1.0},
		},
		{
			name: "unrelated path scores 0.0",
			chunks: []Chunk{
				{ID: "c1", Source: "/completely/different/path.go"},
			},
			qCtx: QueryContext{
				CurrentFile: "/project/src/main.go",
			},
			wantScores: []float64{0.0},
		},
		{
			name: "no current file returns all zeros",
			chunks: []Chunk{
				{ID: "c1", Source: "/project/src/main.go"},
				{ID: "c2", Source: "/project/src/util.go"},
			},
			qCtx:       QueryContext{},
			wantScores: []float64{0.0, 0.0},
		},
		{
			name:       "empty chunks returns empty scores",
			chunks:     []Chunk{},
			qCtx:       QueryContext{CurrentFile: "/project/src/main.go"},
			wantScores: []float64{},
		},
		{
			name: "mixed proximity levels",
			chunks: []Chunk{
				{ID: "c1", Source: "/project/src/main.go"},    // same file
				{ID: "c2", Source: "/project/src/handler.go"}, // same dir
				{ID: "c3", Source: "/project/config.go"},      // parent dir
				{ID: "c4", Source: "/other/lib.go"},           // open file
				{ID: "c5", Source: "/unrelated/something.go"}, // unrelated
			},
			qCtx: QueryContext{
				CurrentFile: "/project/src/main.go",
				OpenFiles:   []string{"/other/lib.go"},
			},
			wantScores: []float64{1.0, 0.8, 0.5, 0.3, 0.0},
		},
		{
			name: "path cleaning normalizes slashes",
			chunks: []Chunk{
				{ID: "c1", Source: "/project/src/../src/main.go"},
			},
			qCtx: QueryContext{
				CurrentFile: "/project/src/main.go",
			},
			wantScores: []float64{1.0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scorer := NewStructuralScorer()
			scores, err := scorer.ScoreBatch(context.Background(), tt.chunks, "query",
				nil, tt.qCtx)
			if err != nil {
				t.Fatalf("ScoreBatch() error: %v", err)
			}
			if len(scores) != len(tt.wantScores) {
				t.Fatalf("ScoreBatch() returned %d scores, want %d", len(scores), len(tt.wantScores))
			}
			for i, want := range tt.wantScores {
				if math.Abs(scores[i]-want) > 0.001 {
					t.Errorf("scores[%d] = %f, want %f", i, scores[i], want)
				}
			}
		})
	}
}

func TestStructuralScorerContextCancellation(t *testing.T) {
	scorer := NewStructuralScorer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	chunks := []Chunk{{ID: "c1", Source: "/project/src/main.go"}}
	_, err := scorer.ScoreBatch(ctx, chunks, "query", nil, QueryContext{
		CurrentFile: "/project/src/main.go",
	})
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}
