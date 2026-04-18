package completion

import (
	"strings"

	"github.com/kstruzzieri/go-llm/provider"
)

const maxStopTokens = 10

// assembleFIMPrompt constructs the FIM prompt from control tokens and source text.
func assembleFIMPrompt(fim *provider.FIMConfig, prefix, suffix string) string {
	totalLen := len(fim.Prefix) + len(prefix) + len(fim.Suffix) + len(suffix) + len(fim.Middle)
	var b strings.Builder
	b.Grow(totalLen)
	b.WriteString(fim.Prefix)
	b.WriteString(prefix)
	b.WriteString(fim.Suffix)
	b.WriteString(suffix)
	b.WriteString(fim.Middle)
	return b.String()
}

// stripStopTokens removes any effective stop token that appears at the tail
// of the completion text. Only the tail is checked — interior matches are
// left alone so that the model may legitimately emit these bytes.
func stripStopTokens(completion string, stopTokens []string) string {
	if completion == "" || len(stopTokens) == 0 {
		return completion
	}
	for _, tok := range stopTokens {
		if tok == "" {
			continue
		}
		if strings.HasSuffix(completion, tok) {
			return completion[:len(completion)-len(tok)]
		}
	}
	return completion
}

// mergeStopTokens combines model-native and language-specific stop tokens
// into a single deduplicated list, capped at maxStopTokens. Model stops
// have highest priority and are never dropped.
func mergeStopTokens(modelStops, langStops []string) []string {
	if len(modelStops) == 0 && len(langStops) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(modelStops)+len(langStops))
	result := make([]string, 0, len(modelStops)+len(langStops))

	for _, tok := range modelStops {
		if !seen[tok] {
			seen[tok] = true
			result = append(result, tok)
		}
	}

	for _, tok := range langStops {
		if !seen[tok] && len(result) < maxStopTokens {
			seen[tok] = true
			result = append(result, tok)
		}
	}

	return result
}
