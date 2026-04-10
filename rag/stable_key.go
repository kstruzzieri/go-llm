package rag

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// stableKeyVersion identifies the current stable-key derivation scheme.
// Bump this when ComputeStableKey semantics change so persisted source
// signatures force a full re-index instead of reusing stale embeddings.
const stableKeyVersion = "v1"

// ComputeStableKey derives a logical identity key for a chunk that survives
// re-indexes and benign line shifts. The key format depends on the metadata
// available on the chunk:
//
//	code:     {rel_path}::{symbol_path}#{ordinal}
//	text:     {rel_path}::{section_path}#{ordinal}
//	fallback: {rel_path}::{anchor_hash}#{ordinal}
//
// workspaceRoot must be an absolute, cleaned path. The chunk's Source must
// also be an absolute path so that filepath.Rel can produce a deterministic
// relative path.
func ComputeStableKey(chunk Chunk, workspaceRoot string) (string, error) {
	if workspaceRoot == "" {
		return "", fmt.Errorf("rag: ComputeStableKey: workspaceRoot must not be empty")
	}

	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("rag: ComputeStableKey: resolve workspaceRoot: %w", err)
	}
	absRoot = filepath.Clean(absRoot)

	absSource, err := filepath.Abs(chunk.Source)
	if err != nil {
		return "", fmt.Errorf("rag: ComputeStableKey: resolve source %q: %w", chunk.Source, err)
	}
	absSource = filepath.Clean(absSource)

	relPath, err := filepath.Rel(absRoot, absSource)
	if err != nil {
		return "", fmt.Errorf("rag: ComputeStableKey: compute relative path: %w", err)
	}
	// Normalize to forward slashes and strip leading "./"
	relPath = filepath.ToSlash(relPath)
	relPath = strings.TrimPrefix(relPath, "./")

	ordinal := chunk.Metadata["chunk_ordinal"]
	if ordinal == "" {
		ordinal = "0"
	}

	// Determine key type based on available metadata.
	if symbolPath := chunk.Metadata["symbol_path"]; symbolPath != "" {
		return fmt.Sprintf("%s::%s#%s", relPath, symbolPath, ordinal), nil
	}
	if sectionPath := chunk.Metadata["section_path"]; sectionPath != "" {
		return fmt.Sprintf("%s::%s#%s", relPath, sectionPath, ordinal), nil
	}
	if anchorHash := chunk.Metadata["anchor_hash"]; anchorHash != "" {
		return fmt.Sprintf("%s::%s#%s", relPath, anchorHash, ordinal), nil
	}

	return "", fmt.Errorf("rag: ComputeStableKey: chunk has no symbol_path, section_path, or anchor_hash metadata")
}

// populateCodeChunkMetadata sets the metadata fields required for stable key
// computation on code chunks produced by the boundary-based code chunker.
// It extracts the symbol name from the first significant line of the chunk.
func populateCodeChunkMetadata(chunks []Chunk, lang string) {
	// Track ordinals per symbol_path for disambiguation.
	ordinals := make(map[string]int)
	for i := range chunks {
		symbol := extractSymbolName(chunks[i].Content, lang)
		if symbol == "" {
			symbol = "unknown"
		}
		chunks[i].Metadata["symbol"] = symbol
		chunks[i].Metadata["symbol_path"] = symbol

		ord := ordinals[symbol]
		ordinals[symbol] = ord + 1
		chunks[i].Metadata["chunk_ordinal"] = fmt.Sprintf("%d", ord)
	}
}

// populateSlidingWindowMetadata sets the metadata fields required for stable
// key computation on sliding-window (fallback) chunks. These chunks have no
// structural symbols, so we use a content-based anchor hash.
func populateSlidingWindowMetadata(chunks []Chunk) {
	for i := range chunks {
		chunks[i].Metadata["anchor_hash"] = anchorHash(chunks[i].Content)
		chunks[i].Metadata["chunk_ordinal"] = fmt.Sprintf("%d", i)
	}
}

// anchorHash produces a short hex digest of the normalized content. The
// normalization strips leading/trailing whitespace from each line and
// collapses runs of blank lines, making the hash resilient to minor
// formatting changes while still distinguishing meaningfully different content.
func anchorHash(content string) string {
	lines := strings.Split(content, "\n")
	var normalized []string
	prevBlank := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if !prevBlank {
				normalized = append(normalized, "")
			}
			prevBlank = true
		} else {
			normalized = append(normalized, trimmed)
			prevBlank = false
		}
	}
	joined := strings.Join(normalized, "\n")
	h := sha256.Sum256([]byte(joined))
	return fmt.Sprintf("%x", h[:8])
}

// Symbol extraction patterns by language. These intentionally match the
// same boundary patterns used in chunker_code.go, extracting the identifier
// that follows the keyword.
var symbolExtractors = map[string]*regexp.Regexp{
	"go":         regexp.MustCompile(`^func\s+(?:\(\s*\w+\s+(\*?\w+(?:\[[\w,\s]+\])?)\)\s+)?(\w+)`),
	"python":     regexp.MustCompile(`^(?:def|class)\s+(\w+)`),
	"typescript": regexp.MustCompile(`^(?:export\s+)?(?:function|class|const|interface|type)\s+(\w+)`),
	"javascript": regexp.MustCompile(`^(?:export\s+)?(?:function|class|const)\s+(\w+)`),
	"rust":       regexp.MustCompile(`^(?:pub\s+)?(?:fn|struct|impl|enum|trait)\s+(\w+)`),
	"java":       regexp.MustCompile(`(?:public|private|protected|static)?\s*(?:class|interface|void|int|String|boolean|static)\s+(\w+)`),
	"ruby":       regexp.MustCompile(`^(?:def|class|module)\s+(\w+)`),
}

// extractSymbolName scans the chunk content for the first recognizable symbol
// declaration and returns its name. For Go methods, the result includes the
// receiver type (e.g. "Server.Start") to disambiguate same-named methods on
// different types. Generic type params are stripped (Server[T] -> Server).
// Returns "" if no symbol is found.
func extractSymbolName(content string, lang string) string {
	pattern, ok := symbolExtractors[lang]
	if !ok {
		return ""
	}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if m := pattern.FindStringSubmatch(trimmed); m != nil {
			// Go regex has 2 capture groups: receiver type (may be empty) and func name.
			if lang == "go" && len(m) >= 3 {
				receiver := m[1]
				funcName := m[2]
				if receiver != "" {
					receiver = strings.TrimPrefix(receiver, "*")
					if idx := strings.IndexByte(receiver, '['); idx >= 0 {
						receiver = receiver[:idx]
					}
					return receiver + "." + funcName
				}
				return funcName
			}
			return m[1]
		}
	}
	return ""
}
