package rag

import (
	"path/filepath"
	"strconv"
	"strings"
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

// findChunk returns the first chunk whose section_path equals path.
func findChunk(t *testing.T, chunks []Chunk, path string) Chunk {
	t.Helper()
	for _, c := range chunks {
		if c.Metadata["section_path"] == path {
			return c
		}
	}
	t.Fatalf("no chunk with section_path %q (have %d chunks)", path, len(chunks))
	return Chunk{}
}

func TestSplitByHeadingsNested(t *testing.T) {
	content := "# A\nintro\n## B\nbody text\n"
	chunks, err := splitByHeadings("doc.md", content, 1500, 200)
	if err != nil {
		t.Fatalf("splitByHeadings() error: %v", err)
	}
	a := findChunk(t, chunks, "A")
	if a.Language != "markdown" {
		t.Errorf("A language = %q, want markdown", a.Language)
	}
	if !strings.Contains(a.Content, "# A") || !strings.Contains(a.Content, "intro") {
		t.Errorf("A content missing heading/intro: %q", a.Content)
	}
	b := findChunk(t, chunks, "A > B")
	if !strings.Contains(b.Content, "## B") || !strings.Contains(b.Content, "body text") {
		t.Errorf("A>B content wrong: %q", b.Content)
	}
	if a.Metadata["anchor_hash"] != "" {
		t.Errorf("section chunk should not carry anchor_hash: %v", a.Metadata)
	}
}

func TestSplitByHeadingsDuplicatePaths(t *testing.T) {
	content := "# Top\n## Setup\nfirst\n## Other\nx\n## Setup\nsecond\n"
	chunks, err := splitByHeadings("doc.md", content, 1500, 200)
	if err != nil {
		t.Fatalf("splitByHeadings() error: %v", err)
	}
	var setupOrdinals []string
	for _, c := range chunks {
		if c.Metadata["section_path"] == "Top > Setup" {
			setupOrdinals = append(setupOrdinals, c.Metadata["chunk_ordinal"])
		}
	}
	if len(setupOrdinals) != 2 || setupOrdinals[0] != "0" || setupOrdinals[1] != "1" {
		t.Errorf("Top > Setup ordinals = %v, want [0 1]", setupOrdinals)
	}
}

func TestSplitByHeadingsPreamble(t *testing.T) {
	content := "intro paragraph before any heading\n\n# Real\nbody\n"
	chunks, err := splitByHeadings("doc.md", content, 1500, 200)
	if err != nil {
		t.Fatalf("splitByHeadings() error: %v", err)
	}
	if chunks[0].Metadata["section_path"] != "" {
		t.Errorf("preamble chunk has section_path: %v", chunks[0].Metadata)
	}
	if chunks[0].Metadata["anchor_hash"] == "" {
		t.Errorf("preamble chunk missing anchor_hash: %v", chunks[0].Metadata)
	}
	if !strings.Contains(chunks[0].Content, "intro paragraph") {
		t.Errorf("preamble content lost: %q", chunks[0].Content)
	}
}

func TestSplitByHeadingsFenceGuard(t *testing.T) {
	content := "# Real\nbefore\n```sh\n# not a heading\necho hi\n```\nafter\n"
	chunks, err := splitByHeadings("doc.md", content, 1500, 200)
	if err != nil {
		t.Fatalf("splitByHeadings() error: %v", err)
	}
	count := 0
	for _, c := range chunks {
		if c.Metadata["section_path"] != "" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d section chunks, want 1 (fence '#' must not split)", count)
	}
	real := findChunk(t, chunks, "Real")
	if !strings.Contains(real.Content, "# not a heading") {
		t.Errorf("fenced '#' should stay inside the Real section: %q", real.Content)
	}
}

func TestSplitByHeadingsOversized(t *testing.T) {
	body := strings.Repeat("padding line of text\n", 20)
	content := "preamble\n# Big\n" + body
	chunks, err := splitByHeadings("doc.md", content, 50, 10)
	if err != nil {
		t.Fatalf("splitByHeadings() error: %v", err)
	}
	var big []Chunk
	for _, c := range chunks {
		if c.Metadata["section_path"] == "Big" {
			big = append(big, c)
		}
	}
	if len(big) < 2 {
		t.Fatalf("oversized section produced %d chunks, want >= 2", len(big))
	}
	for i, c := range big {
		if c.Metadata["chunk_ordinal"] != strconv.Itoa(i) {
			t.Errorf("sub-chunk %d ordinal = %q, want %q", i, c.Metadata["chunk_ordinal"], strconv.Itoa(i))
		}
		if c.StartLine < 2 {
			t.Errorf("sub-chunk %d StartLine = %d, want >= 2 (rebased)", i, c.StartLine)
		}
		if c.ID != chunkID(c.Source, c.Content, c.StartLine) {
			t.Errorf("sub-chunk %d ID not consistent with rebased StartLine", i)
		}
	}
}

func TestSplitByHeadingsEmptyHeadingHasStableKey(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "doc.md")
	content := "#\nbody under a bare heading\n"
	chunks, err := splitByHeadings(source, content, 1500, 200)
	if err != nil {
		t.Fatalf("splitByHeadings() error: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}
	if got := chunks[0].Metadata["section_path"]; got != "#" {
		t.Fatalf("empty heading section_path = %q, want %q", got, "#")
	}
	key, err := ComputeStableKey(chunks[0], root)
	if err != nil {
		t.Fatalf("ComputeStableKey() error: %v", err)
	}
	if key != "doc.md::##0" {
		t.Errorf("stable key = %q, want %q", key, "doc.md::##0")
	}
}

func TestSplitByHeadingsHeadingless(t *testing.T) {
	content := "just text\nno headings here\n"
	chunks, err := splitByHeadings("doc.md", content, 1500, 200)
	if err != nil {
		t.Fatalf("splitByHeadings() error: %v", err)
	}
	if chunks != nil {
		t.Errorf("headingless content should return nil (caller falls back), got %d chunks", len(chunks))
	}
}

func TestSplitByHeadingsConfigError(t *testing.T) {
	content := "# H\nbody\n"
	_, err := splitByHeadings("doc.md", content, 0, 0)
	if err == nil {
		t.Fatal("expected error for invalid maxSize/overlap, got nil")
	}
}
