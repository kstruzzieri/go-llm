package rag

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// codeChunker splits source code on function/method/class boundaries.
// Falls back to sliding window for non-code or unrecognized files.
type codeChunker struct {
	maxSize  int
	overlap  int
	language string
}

func (c *codeChunker) sourceSignature() string {
	return fmt.Sprintf("%T:max=%d:overlap=%d:language=%s", c, c.maxSize, c.overlap, c.language)
}

// NewCodeChunker returns a chunker that respects code boundaries.
// It splits on function/method/class boundaries rather than arbitrary line counts.
// Falls back to sliding window for non-code files.
func NewCodeChunker(opts ...ChunkerOption) Chunker {
	c := &codeChunker{
		maxSize: 1500,
		overlap: 200,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Chunk splits content respecting code structure when possible.
func (c *codeChunker) Chunk(source string, content string) ([]Chunk, error) {
	if content == "" {
		return nil, nil
	}

	lang := c.language
	if lang == "" {
		lang = detectLanguage(source)
	}

	if lang == "" {
		// Fall back to sliding window for unknown file types
		sw, err := NewSlidingWindowChunker(c.maxSize, c.overlap)
		if err != nil {
			return nil, fmt.Errorf("rag: create fallback chunker for %q: %w", source, err)
		}
		swChunks, err := sw.Chunk(source, content)
		if err != nil {
			return nil, err
		}
		populateSlidingWindowMetadata(swChunks)
		return swChunks, nil
	}

	chunks := c.splitByBoundaries(source, content, lang)
	if len(chunks) == 0 {
		// No boundaries found, fall back to sliding window
		sw, err := NewSlidingWindowChunker(c.maxSize, c.overlap)
		if err != nil {
			return nil, fmt.Errorf("rag: create fallback chunker for %q: %w", source, err)
		}
		swChunks, err := sw.Chunk(source, content)
		if err != nil {
			return nil, err
		}
		populateSlidingWindowMetadata(swChunks)
		return swChunks, nil
	}

	populateCodeChunkMetadata(chunks, lang)
	return chunks, nil
}

// detectLanguage infers the programming language from a file path.
func detectLanguage(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx":
		return "javascript"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".rb":
		return "ruby"
	default:
		return ""
	}
}

// boundaryPattern returns a regex that matches function/class boundaries
// for the given language.
var boundaryPatterns = map[string]*regexp.Regexp{
	"go":         regexp.MustCompile(`^func\s`),
	"python":     regexp.MustCompile(`^(def|class)\s`),
	"typescript": regexp.MustCompile(`^(export\s+)?(function|class|const|interface|type)\s`),
	"javascript": regexp.MustCompile(`^(export\s+)?(function|class|const)\s`),
	"rust":       regexp.MustCompile(`^(pub\s+)?(fn|struct|impl|enum|trait)\s`),
	"java":       regexp.MustCompile(`^\s*(public|private|protected|static)?\s*(class|interface|void|int|String|boolean|static)\s`),
	"ruby":       regexp.MustCompile(`^(def|class|module)\s`),
}

// splitByBoundaries splits code at function/class boundaries.
func (c *codeChunker) splitByBoundaries(source, content, lang string) []Chunk {
	pattern, ok := boundaryPatterns[lang]
	if !ok {
		return nil
	}

	lines := strings.Split(content, "\n")
	var chunks []Chunk
	var buf strings.Builder
	startLine := 1
	chunkCount := 0

	for i, line := range lines {
		lineNum := i + 1
		trimmed := strings.TrimSpace(line)

		// Check if this line starts a new boundary
		isBoundary := pattern.MatchString(trimmed)
		bufTooLarge := buf.Len() > 0 && buf.Len()+len(line)+1 > c.maxSize

		if (isBoundary || bufTooLarge) && buf.Len() > 0 {
			text := strings.TrimRight(buf.String(), "\n")
			if text != "" {
				chunks = append(chunks, makeChunk(source, text, startLine, lineNum-1, lang))
				chunkCount++
			}
			buf.Reset()
			startLine = lineNum
		}

		if buf.Len() > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(line)
	}

	// Remaining content
	if buf.Len() > 0 {
		text := strings.TrimRight(buf.String(), "\n")
		if text != "" {
			chunks = append(chunks, makeChunk(source, text, startLine, len(lines), lang))
		}
	}

	return chunks
}
