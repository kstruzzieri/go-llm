package provider

import (
	"regexp"
	"strings"
	"sync"
)

// regexpCache caches compiled regexps keyed by the tag pair to avoid
// recompilation on every call.
var (
	regexpCacheMu sync.Mutex
	regexpCache   = make(map[ThinkTags]*regexp.Regexp)
)

// getThinkRegexp returns a compiled regexp for the given tag pair,
// caching it for reuse.
func getThinkRegexp(tags ThinkTags) *regexp.Regexp {
	regexpCacheMu.Lock()
	defer regexpCacheMu.Unlock()

	if re, ok := regexpCache[tags]; ok {
		return re
	}

	pattern := regexp.QuoteMeta(tags.Open) + `([\s\S]*?)` + regexp.QuoteMeta(tags.Close)
	re := regexp.MustCompile(pattern)
	regexpCache[tags] = re
	return re
}

// ExtractThinking extracts thinking content from a complete (non-streaming)
// response. It uses regex matching since the full response is already
// buffered. Multiple think blocks are joined with newlines.
//
// Returns the cleaned content (think blocks removed) and the extracted
// thinking text. If no think blocks are found, cleaned equals the original
// content and thinking is empty.
func ExtractThinking(content string, tags ThinkTags) (cleaned string, thinking string) {
	if content == "" {
		return "", ""
	}

	re := getThinkRegexp(tags)
	matches := re.FindAllStringSubmatch(content, -1)

	if len(matches) == 0 {
		return content, ""
	}

	var thinkParts []string
	for _, match := range matches {
		if len(match) > 1 {
			thinkParts = append(thinkParts, match[1])
		}
	}

	cleaned = re.ReplaceAllString(content, "")
	thinking = strings.Join(thinkParts, "\n")

	return cleaned, thinking
}
