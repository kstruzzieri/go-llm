// Package completion provides IDE inline code completion using Fill-in-the-Middle (FIM)
// prompting with Ollama models.
package completion

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
func TruncateToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	maxChars := maxTokens * 4
	if len(text) <= maxChars {
		return text
	}
	return text[len(text)-maxChars:]
}
