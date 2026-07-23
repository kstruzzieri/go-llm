package provider

import (
	"testing"
)

func TestExtractThinking(t *testing.T) {
	defaultTags := DefaultThinkTags()

	tests := []struct {
		name         string
		content      string
		tags         ThinkTags
		wantCleaned  string
		wantThinking string
	}{
		{
			name:         "basic extraction",
			content:      "<think>reasoning here</think>The answer is 42",
			tags:         defaultTags,
			wantCleaned:  "The answer is 42",
			wantThinking: "reasoning here",
		},
		{
			name:         "no think tags",
			content:      "Just a plain response",
			tags:         defaultTags,
			wantCleaned:  "Just a plain response",
			wantThinking: "",
		},
		{
			name:         "empty think block",
			content:      "<think></think>The answer",
			tags:         defaultTags,
			wantCleaned:  "The answer",
			wantThinking: "",
		},
		{
			name:         "multiple think blocks",
			content:      "<think>first</think>middle<think>second</think>end",
			tags:         defaultTags,
			wantCleaned:  "middleend",
			wantThinking: "first\nsecond",
		},
		{
			name:         "think at end (unclosed)",
			content:      "content<think>dangling",
			tags:         defaultTags,
			wantCleaned:  "content<think>dangling",
			wantThinking: "",
		},
		{
			name:         "custom tags",
			content:      "<reasoning>deep thoughts</reasoning>answer",
			tags:         ThinkTags{Open: "<reasoning>", Close: "</reasoning>"},
			wantCleaned:  "answer",
			wantThinking: "deep thoughts",
		},
		{
			name:         "multiline thinking",
			content:      "<think>\nLine 1\nLine 2\n</think>Response",
			tags:         defaultTags,
			wantCleaned:  "Response",
			wantThinking: "\nLine 1\nLine 2\n",
		},
		{
			name:         "thinking only (no content after)",
			content:      "<think>just thinking</think>",
			tags:         defaultTags,
			wantCleaned:  "",
			wantThinking: "just thinking",
		},
		{
			name:         "content before think",
			content:      "prefix <think>thought</think> suffix",
			tags:         defaultTags,
			wantCleaned:  "prefix  suffix",
			wantThinking: "thought",
		},
		{
			name:         "empty input",
			content:      "",
			tags:         defaultTags,
			wantCleaned:  "",
			wantThinking: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned, thinking := ExtractThinking(tt.content, tt.tags)
			if cleaned != tt.wantCleaned {
				t.Errorf("cleaned = %q, want %q", cleaned, tt.wantCleaned)
			}
			if thinking != tt.wantThinking {
				t.Errorf("thinking = %q, want %q", thinking, tt.wantThinking)
			}
		})
	}
}
