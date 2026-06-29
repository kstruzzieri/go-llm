package rag

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ATX heading: up to 3 leading spaces, 1-6 '#', then either end-of-line or a
// space/tab followed by the title. Group 2 (the remainder) must start with a
// space/tab or be absent, so "###Title" is not a heading. 4+ leading spaces
// fall outside the {0,3} cap and are treated as indented code, not a heading.
var headingRe = regexp.MustCompile(`^ {0,3}(#{1,6})([ \t].*)?$`)

// closingHashRe strips a CommonMark closing hash sequence: a run of '#' at end
// of line preceded by whitespace. It will NOT strip a '#' that is part of the
// title (e.g. "C#"), because that '#' is not preceded by whitespace.
var closingHashRe = regexp.MustCompile(`[ \t]+#+[ \t]*$`)

// fenceRe matches a fenced-code delimiter line: after up to 3 leading spaces, a
// run of 3 or more backticks OR 3 or more tildes.
var fenceRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

// isMarkdown reports whether path has a Markdown extension.
func isMarkdown(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

// rebaseChunkLines shifts each chunk's line numbers by lineOffset and recomputes
// its ID. The ID recompute is mandatory: chunkID embeds StartLine, so a chunk
// produced against a within-section span (1-based) would otherwise carry an ID
// reflecting the wrong file line.
func rebaseChunkLines(chunks []Chunk, lineOffset int) {
	for i := range chunks {
		chunks[i].StartLine += lineOffset
		chunks[i].EndLine += lineOffset
		chunks[i].ID = chunkID(chunks[i].Source, chunks[i].Content, chunks[i].StartLine)
	}
}

// populateMarkdownChunkMetadata assigns chunk_ordinal per identical full
// section_path in document order (mirrors populateCodeChunkMetadata). Chunks
// with no section_path (preamble/fallback) are left untouched — they keep the
// anchor_hash + ordinal the sliding-window chunker already gave them.
func populateMarkdownChunkMetadata(chunks []Chunk) {
	ordinals := make(map[string]int)
	for i := range chunks {
		path := chunks[i].Metadata["section_path"]
		if path == "" {
			continue
		}
		ord := ordinals[path]
		ordinals[path] = ord + 1
		chunks[i].Metadata["chunk_ordinal"] = strconv.Itoa(ord)
	}
}

// parseHeading reports whether line is an ATX heading. On match it returns the
// level (1-6) and the derived title. An empty title (bare "#") is normalized to
// level-many '#' characters so the section-path segment is deterministic and
// non-empty — otherwise a chunk built from it would carry no stable-key field.
func parseHeading(line string) (level int, title string, ok bool) {
	m := headingRe.FindStringSubmatch(line)
	if m == nil {
		return 0, "", false
	}
	level = len(m[1])
	rest := closingHashRe.ReplaceAllString(m[2], "")
	title = strings.TrimSpace(rest)
	if title == "" {
		title = strings.Repeat("#", level)
	}
	return level, title, true
}

// splitByHeadings splits Markdown into section-aware chunks on ATX heading
// boundaries. It returns (nil, nil) when no heading is found, so the caller can
// fall back to whole-file sliding-window chunking (unchanged behavior). Content
// before the first heading is preserved as preamble chunks (anchor_hash, no
// section_path). Every section is run through the sliding-window chunker so an
// oversized section is bounded by the same max/overlap behavior as the rest of
// the corpus; a section that fits yields a single sub-chunk.
func splitByHeadings(source, content string, maxSize, overlap int) ([]Chunk, error) {
	lines := strings.Split(content, "\n")

	type section struct {
		startLine int      // 1-based file line of the heading
		path      string   // e.g. "Parent > Child"
		body      []string // lines including the heading line
	}
	type frame struct {
		level int
		title string
	}

	var (
		preamble []string
		sections []section
		stack    []frame
		inFence  bool
		curIdx   = -1 // index into sections of the open section; -1 = preamble
	)

	appendLine := func(s string) {
		// Index-based, NOT a pointer into sections: appending a new section may
		// reallocate the backing array and dangle a held pointer.
		if curIdx < 0 {
			preamble = append(preamble, s)
		} else {
			sections[curIdx].body = append(sections[curIdx].body, s)
		}
	}

	for i, line := range lines {
		if fenceRe.MatchString(line) {
			inFence = !inFence
			appendLine(line)
			continue
		}
		if !inFence {
			if level, title, ok := parseHeading(line); ok {
				for len(stack) > 0 && stack[len(stack)-1].level >= level {
					stack = stack[:len(stack)-1]
				}
				stack = append(stack, frame{level: level, title: title})
				titles := make([]string, len(stack))
				for k := range stack {
					titles[k] = stack[k].title
				}
				sections = append(sections, section{
					startLine: i + 1,
					path:      strings.Join(titles, " > "),
					body:      []string{line},
				})
				curIdx = len(sections) - 1
				continue
			}
		}
		appendLine(line)
	}

	if len(sections) == 0 {
		return nil, nil
	}

	sw, err := NewSlidingWindowChunker(maxSize, overlap)
	if err != nil {
		return nil, fmt.Errorf("rag: markdown chunker for %q: %w", source, err)
	}

	var chunks []Chunk
	if text := strings.Join(preamble, "\n"); strings.TrimSpace(text) != "" {
		pre, err := sw.Chunk(source, text)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, pre...) // keep anchor_hash, no section_path
	}
	for _, sec := range sections {
		sub, err := sw.Chunk(source, strings.Join(sec.body, "\n"))
		if err != nil {
			return nil, err
		}
		rebaseChunkLines(sub, sec.startLine-1)
		for j := range sub {
			sub[j].Language = "markdown"
			sub[j].Metadata["section_path"] = sec.path
			delete(sub[j].Metadata, "anchor_hash")
		}
		chunks = append(chunks, sub...)
	}
	populateMarkdownChunkMetadata(chunks)
	return chunks, nil
}
