package rag

import (
	"context"
	"path/filepath"
)

// StructuralScorer scores chunks by filesystem proximity to the user's
// current editing context. Chunks from the same file score highest,
// followed by same directory, parent directory, and open files.
type StructuralScorer struct{}

// Compile-time interface check.
var _ SignalScorer = (*StructuralScorer)(nil)

// NewStructuralScorer creates a StructuralScorer.
func NewStructuralScorer() *StructuralScorer {
	return &StructuralScorer{}
}

// Name returns the scorer identifier.
func (s *StructuralScorer) Name() string { return "structural" }

// ScoreBatch scores each chunk based on its source path's proximity to
// the current file in qCtx. Scoring rules:
//   - Same file as CurrentFile:            1.0
//   - Same directory as CurrentFile:       0.8
//   - Same parent directory (one level up): 0.5
//   - Source is in qCtx.OpenFiles:         max(existing, 0.3)
//   - Otherwise:                           0.0
//
// If qCtx.CurrentFile is empty, all scores are 0 (no structural signal).
func (s *StructuralScorer) ScoreBatch(ctx context.Context, chunks []Chunk, query string,
	queryEmbedding []float64, qCtx QueryContext) ([]float64, error) {

	scores := make([]float64, len(chunks))

	if qCtx.CurrentFile == "" {
		return scores, nil
	}

	// Normalize all paths through WorkspaceRoot so that relative chunk
	// sources (e.g., "rag/store.go") and absolute editor paths (e.g.,
	// "/home/user/project/rag/store.go") resolve to the same canonical form.
	currentClean := normalizePath(qCtx.CurrentFile, qCtx.WorkspaceRoot)
	currentDir := filepath.Dir(currentClean)
	parentDir := filepath.Dir(currentDir)

	// Build a set of open files for O(1) lookup.
	openSet := make(map[string]struct{}, len(qCtx.OpenFiles))
	for _, f := range qCtx.OpenFiles {
		openSet[normalizePath(f, qCtx.WorkspaceRoot)] = struct{}{}
	}

	for i, chunk := range chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		chunkClean := normalizePath(chunk.Source, qCtx.WorkspaceRoot)
		chunkDir := filepath.Dir(chunkClean)

		switch {
		case chunkClean == currentClean:
			scores[i] = 1.0
		case chunkDir == currentDir:
			scores[i] = 0.8
		case chunkDir == parentDir:
			scores[i] = 0.5
		}

		// Open-file bonus: apply if the chunk source is an open file and
		// the existing score is below the open-file threshold.
		if _, open := openSet[chunkClean]; open && scores[i] < 0.3 {
			scores[i] = 0.3
		}
	}

	return scores, nil
}

// normalizePath resolves a file path to a canonical absolute form using the
// workspace root. If the path is relative and a non-empty workspace root is
// provided, the path is joined with the root. This ensures that relative
// chunk sources from indexing and absolute editor paths compare correctly.
func normalizePath(path, workspaceRoot string) string {
	cleaned := filepath.Clean(path)
	if workspaceRoot == "" || filepath.IsAbs(cleaned) {
		return cleaned
	}
	return filepath.Clean(filepath.Join(workspaceRoot, cleaned))
}
