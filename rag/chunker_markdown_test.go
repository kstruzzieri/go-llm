package rag

import (
	"testing"
)

func TestIsMarkdown(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"README.md", true},
		{"docs/guide.MD", true},
		{"notes.markdown", true},
		{"main.go", false},
		{"data.json", false},
		{"noext", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isMarkdown(tt.path); got != tt.want {
				t.Errorf("isMarkdown(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestParseHeading(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantLevel int
		wantTitle string
		wantOK    bool
	}{
		{"h1", "# Title", 1, "Title", true},
		{"h3", "### Deep", 3, "Deep", true},
		{"h6", "###### Six", 6, "Six", true},
		{"three leading spaces", "   ## Indented", 2, "Indented", true},
		{"no space after hashes", "###Title", 0, "", false},
		{"seven hashes not a heading", "####### Seven", 0, "", false},
		{"bare hash normalizes", "#", 1, "#", true},
		{"bare double hash normalizes", "##", 2, "##", true},
		{"trailing closing hashes stripped", "## Heading ##", 2, "Heading", true},
		{"hash in title kept (no preceding space)", "# C#", 1, "C#", true},
		{"spaced trailing hash stripped", "# C #", 1, "C", true},
		{"four leading spaces not a heading", "    # Code", 0, "", false},
		{"empty line", "", 0, "", false},
		{"plain text", "just text", 0, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, title, ok := parseHeading(tt.line)
			if ok != tt.wantOK || level != tt.wantLevel || title != tt.wantTitle {
				t.Errorf("parseHeading(%q) = (%d, %q, %v), want (%d, %q, %v)",
					tt.line, level, title, ok, tt.wantLevel, tt.wantTitle, tt.wantOK)
			}
		})
	}
}

func TestRebaseChunkLines(t *testing.T) {
	content := "alpha\nbeta"
	chunks := []Chunk{{
		ID:        chunkID("doc.md", content, 1),
		Content:   content,
		Source:    "doc.md",
		StartLine: 1,
		EndLine:   2,
		Metadata:  map[string]string{},
	}}
	rebaseChunkLines(chunks, 9) // section started at file line 10

	if chunks[0].StartLine != 10 || chunks[0].EndLine != 11 {
		t.Fatalf("lines = (%d, %d), want (10, 11)", chunks[0].StartLine, chunks[0].EndLine)
	}
	// ID must be recomputed against the rebased StartLine, not the within-span one.
	want := chunkID("doc.md", content, 10)
	if chunks[0].ID != want {
		t.Errorf("ID = %q, want %q (recomputed at rebased line)", chunks[0].ID, want)
	}
}

func TestPopulateMarkdownChunkMetadata(t *testing.T) {
	chunks := []Chunk{
		{Metadata: map[string]string{"anchor_hash": "p0", "chunk_ordinal": "0"}}, // preamble, no section_path
		{Metadata: map[string]string{"section_path": "A"}},
		{Metadata: map[string]string{"section_path": "A > B"}},
		{Metadata: map[string]string{"section_path": "A > B"}}, // duplicate path
		{Metadata: map[string]string{"section_path": "A"}},     // duplicate path
	}
	populateMarkdownChunkMetadata(chunks)

	// Preamble chunk untouched.
	if chunks[0].Metadata["chunk_ordinal"] != "0" || chunks[0].Metadata["section_path"] != "" {
		t.Errorf("preamble chunk altered: %v", chunks[0].Metadata)
	}
	wantOrd := []string{"", "0", "0", "1", "1"}
	for i := 1; i < len(chunks); i++ {
		if got := chunks[i].Metadata["chunk_ordinal"]; got != wantOrd[i] {
			t.Errorf("chunk %d ordinal = %q, want %q", i, got, wantOrd[i])
		}
	}
}
