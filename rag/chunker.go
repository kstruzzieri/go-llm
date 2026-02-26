// Package rag provides retrieval-augmented generation with text chunking,
// vector storage, indexing, and retrieval capabilities.
package rag

import (
	"crypto/sha256"
	"fmt"
	"strings"
)


// Chunk represents a segment of text suitable for embedding.
type Chunk struct {
	ID        string            // deterministic hash of content + source
	Content   string            // the text
	Source    string            // file path
	StartLine int              // line number in source (1-indexed)
	EndLine   int
	Language  string            // "go", "python", "typescript", etc.
	Metadata  map[string]string // arbitrary k/v (function name, class, etc.)
}

// Chunker splits text into chunks suitable for embedding.
type Chunker interface {
	Chunk(source string, content string) ([]Chunk, error)
}

// SlidingWindowChunker splits text using a sliding window with overlap.
type SlidingWindowChunker struct {
	maxSize int
	overlap int
}

// ChunkerOption configures a chunker.
type ChunkerOption func(*codeChunker)

// WithMaxChunkSize sets the maximum chunk size in characters (default: 1500).
func WithMaxChunkSize(n int) ChunkerOption {
	return func(c *codeChunker) {
		c.maxSize = n
	}
}

// WithOverlap sets the overlap size in characters (default: 200).
func WithOverlap(n int) ChunkerOption {
	return func(c *codeChunker) {
		c.overlap = n
	}
}

// WithLanguage sets the language for code-aware chunking (auto-detect if empty).
func WithLanguage(lang string) ChunkerOption {
	return func(c *codeChunker) {
		c.language = lang
	}
}

// NewSlidingWindowChunker creates a chunker that splits on character boundaries
// with overlapping windows. Returns an error if maxSize is <= 0 or overlap >= maxSize.
func NewSlidingWindowChunker(maxSize, overlap int) (*SlidingWindowChunker, error) {
	if maxSize <= 0 {
		return nil, fmt.Errorf("rag: maxSize must be > 0, got %d", maxSize)
	}
	if overlap < 0 {
		return nil, fmt.Errorf("rag: overlap must be >= 0, got %d", overlap)
	}
	if overlap >= maxSize {
		return nil, fmt.Errorf("rag: overlap (%d) must be less than maxSize (%d)", overlap, maxSize)
	}
	return &SlidingWindowChunker{
		maxSize: maxSize,
		overlap: overlap,
	}, nil
}

// Chunk splits content into overlapping sliding window chunks.
func (sw *SlidingWindowChunker) Chunk(source string, content string) ([]Chunk, error) {
	if content == "" {
		return nil, nil
	}

	lines := strings.Split(content, "\n")
	var chunks []Chunk
	var buf strings.Builder
	startLine := 1

	for i, line := range lines {
		lineNum := i + 1
		if buf.Len() > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(line)

		if buf.Len() >= sw.maxSize {
			text := buf.String()
			chunks = append(chunks, makeChunk(source, text, startLine, lineNum, ""))
			buf.Reset()
			// Backtrack for overlap
			overlapStart := findOverlapStart(lines, lineNum, sw.overlap)
			startLine = overlapStart + 1
			for j := overlapStart; j <= i; j++ {
				if buf.Len() > 0 {
					buf.WriteString("\n")
				}
				buf.WriteString(lines[j])
			}
		}
	}

	// Remaining content
	if buf.Len() > 0 {
		chunks = append(chunks, makeChunk(source, buf.String(), startLine, len(lines), ""))
	}

	return chunks, nil
}

// findOverlapStart finds the line index to start the overlap from.
func findOverlapStart(lines []string, currentLine int, overlapChars int) int {
	charCount := 0
	for i := currentLine - 1; i >= 0; i-- {
		charCount += len(lines[i]) + 1 // +1 for newline
		if charCount >= overlapChars {
			return i
		}
	}
	return 0
}

// makeChunk creates a Chunk with a deterministic ID.
func makeChunk(source, content string, startLine, endLine int, lang string) Chunk {
	return Chunk{
		ID:        chunkID(source, content),
		Content:   content,
		Source:    source,
		StartLine: startLine,
		EndLine:   endLine,
		Language:  lang,
		Metadata:  make(map[string]string),
	}
}

// chunkID generates a deterministic ID from source and content.
func chunkID(source, content string) string {
	h := sha256.Sum256([]byte(source + "\x00" + content))
	return fmt.Sprintf("%x", h[:12])
}
