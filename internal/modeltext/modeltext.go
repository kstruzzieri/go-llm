// Package modeltext normalizes raw local-model output before a caller parses
// it.
//
// Local models routinely wrap a response in a Markdown code fence even when the
// prompt forbids markdown, so every caller that parses model output structurally
// — generated code, a JSON contract — has to strip that wrapper first or reject
// otherwise-valid answers. Keeping one implementation here means the two call
// sites cannot drift on which shapes count as a wrapper.
package modeltext

import "strings"

// StripCodeFence removes a single outer Markdown code fence when the ENTIRE
// content is one fenced block (```lang\n...\n```), the common way local models
// wrap output despite instructions. It strips only when the content starts with
// a fence line, ends with a closing fence, the inner body is non-empty, and the
// body contains no further fence — so a genuine multi-block Markdown document
// (which does not reduce to a single fence pair) is left untouched.
//
// It does not trim the input: callers that care about surrounding whitespace
// must TrimSpace before calling, because a leading blank line means the content
// does not start with a fence and nothing is stripped.
func StripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s // single line starting with ``` — not a real block
	}
	inner := strings.TrimRight(s[nl+1:], " \t\n")
	if !strings.HasSuffix(inner, "```") {
		return s
	}
	inner = strings.TrimRight(strings.TrimSuffix(inner, "```"), " \t\n")
	if strings.TrimSpace(inner) == "" {
		return ""
	}
	if strings.Contains(inner, "```") {
		return s // multi-block document — not a single wrapping fence
	}
	return inner
}
