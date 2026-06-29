package rag

import "testing"

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
