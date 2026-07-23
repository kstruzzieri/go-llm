// Package completion provides IDE inline code completion using Fill-in-the-Middle (FIM)
// prompting with Ollama models.
package completion

import "unicode/utf8"

// EstimateTokens returns a rough token count for the given text.
// It uses a simple heuristic of ~4 characters per token, which is
// a reasonable approximation for code across most tokenizers.
func EstimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	return (len(text) + 3) / 4
}

// TruncateToTokens truncates text to fit within the given token budget,
// keeping the last (most recent) content. This is useful for prefix context
// where the code nearest to the cursor is most relevant.
// The cut point is adjusted to avoid splitting multi-byte UTF-8 runes.
func TruncateToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	maxChars := maxTokens * 4
	if len(text) <= maxChars {
		return text
	}
	start := len(text) - maxChars
	// Advance past any continuation bytes to the next rune boundary
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

// TruncateSuffixToTokens truncates text to fit within the given token budget,
// keeping the first (nearest to cursor) content. This is useful for suffix
// context where the code immediately after the cursor is most relevant.
// The cut point is adjusted to avoid splitting multi-byte UTF-8 runes.
func TruncateSuffixToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	maxChars := maxTokens * 4
	if len(text) <= maxChars {
		return text
	}
	// Walk back from the cut point to avoid splitting a multi-byte rune
	end := maxChars
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}
