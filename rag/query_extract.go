package rag

import (
	"strings"
	"unicode"
)

// extractCodeQuery derives a conservative keyword search string from a raw
// query by keeping only code-shaped tokens — paths, snake_case, camelCase /
// PascalCase identifiers, dotted or namespaced names, alphanumeric
// identifiers — plus the contents of double-quoted or backtick-quoted spans.
// Plain natural-language words are dropped so FTS5 keyword matching focuses on
// exact code terms instead of burying identifiers in prose. Because FTS5
// combines terms with an implicit AND, prose tokens that miss a code chunk
// would otherwise filter it out entirely; stripping them lets the identifier
// drive the match.
//
// It is deterministic and stdlib-only. It returns "" when the query contains
// no code-shaped tokens (for example, pure natural language), which lets
// callers fall back to embedding/searching the original query unchanged.
//
// Kept tokens retain any attached punctuation (e.g. "SearchMulti(ctx"); the
// result is a keyword hint, not an FTS5 expression, and must be passed through
// sanitizeFTS5Query before use in a MATCH clause.
//
// The classifier is intentionally conservative and errs toward dropping: a
// leading-acronym identifier whose lowercase tail is a single character
// (plural acronyms like "APIs", "IDs", "URLs") is treated as prose, and a
// bare lowercase word ("main", "rag") is indistinguishable from prose and is
// also dropped. When in doubt the token is dropped, so the caller falls back
// to the full original query rather than over-narrowing the keyword search.
func extractCodeQuery(query string) string {
	remainder, kept := extractQuotedSpans(query)

	for _, tok := range strings.Fields(remainder) {
		if isCodeShaped(tok) {
			kept = append(kept, tok)
		}
	}

	return strings.Join(kept, " ")
}

// extractQuotedSpans removes double-quoted and backtick-quoted spans from the
// query, returning the remaining text plus the trimmed inner content of each
// closed span (the user quoted it deliberately, so it is kept verbatim).
// Single quotes are not treated as delimiters to avoid colliding with
// apostrophes in prose ("don't", "it's"). An unclosed delimiter is treated as
// a literal character so the trailing text is still tokenized normally.
func extractQuotedSpans(query string) (remainder string, spans []string) {
	runes := []rune(query)
	var b strings.Builder
	for i := 0; i < len(runes); {
		r := runes[i]
		if r == '"' || r == '`' {
			if j := indexRune(runes, r, i+1); j >= 0 {
				if inner := strings.TrimSpace(string(runes[i+1 : j])); inner != "" {
					spans = append(spans, inner)
				}
				b.WriteByte(' ') // replace the span with a token separator
				i = j + 1
				continue
			}
		}
		b.WriteRune(r)
		i++
	}
	return b.String(), spans
}

// indexRune returns the index of the first occurrence of target in runes at or
// after start, or -1 if absent.
func indexRune(runes []rune, target rune, start int) int {
	for i := start; i < len(runes); i++ {
		if runes[i] == target {
			return i
		}
	}
	return -1
}

// isCodeShaped reports whether a whitespace-delimited token looks like a code
// identifier or path rather than a prose word. A token must contain at least
// one letter, then qualify on any one signal: a camelCase/PascalCase boundary,
// an adjacent letter+digit (sha256, qwen2), or an internal structural
// separator (_ / . :) between word characters.
func isCodeShaped(tok string) bool {
	var hasLetter, hasDigit, letterDigitAdj bool
	var prevLetter, prevDigit bool
	for _, r := range tok {
		isL := unicode.IsLetter(r)
		isD := unicode.IsDigit(r)
		if isL {
			hasLetter = true
		}
		if isD {
			hasDigit = true
		}
		if (isL && prevDigit) || (isD && prevLetter) {
			letterDigitAdj = true
		}
		prevLetter, prevDigit = isL, isD
	}
	if !hasLetter {
		return false
	}
	if hasCaseBoundary(tok) {
		return true
	}
	if hasDigit && letterDigitAdj {
		return true
	}
	if isDottedSingleLetterAbbreviation(tok) {
		return false
	}
	for _, sep := range []rune{'_', '/', '.', ':'} {
		if hasInternalRune(tok, sep) {
			return true
		}
	}
	return false
}

// hasCaseBoundary reports whether a token has a camelCase or PascalCase word
// boundary. It accepts a lower-to-upper hump (searchMulti, vectorStore, iOS)
// and an acronym-to-word boundary whose lowercase tail is at least two runes
// (HTTPServer, OAuthToken). The two-rune tail requirement excludes pluralized
// acronyms (APIs, IDs, URLs), which are prose, not identifiers.
func hasCaseBoundary(tok string) bool {
	runes := []rune(tok)
	for i := 1; i < len(runes); i++ {
		if isLowerLetter(runes[i-1]) && isUpperLetter(runes[i]) {
			return true // lower -> upper: camelCase hump
		}
	}
	for i := 2; i < len(runes); i++ {
		if isUpperLetter(runes[i-2]) && isUpperLetter(runes[i-1]) && isLowerLetter(runes[i]) {
			run := 0
			for j := i; j < len(runes) && isLowerLetter(runes[j]); j++ {
				run++
			}
			if run >= 2 {
				return true // ACRONYM -> word boundary (HTTPServer)
			}
		}
	}
	return false
}

// hasInternalRune reports whether sep appears in tok with a word character
// (letter or digit) on both sides, marking a structural separator inside an
// identifier or path rather than trailing/leading punctuation.
func hasInternalRune(tok string, sep rune) bool {
	runes := []rune(tok)
	for i := 1; i < len(runes)-1; i++ {
		if runes[i] == sep && isWordRune(runes[i-1]) && isWordRune(runes[i+1]) {
			return true
		}
	}
	return false
}

// isDottedSingleLetterAbbreviation rejects prose abbreviations like "e.g." or
// "(i.e.)," before dots are treated as code/path separators.
func isDottedSingleLetterAbbreviation(tok string) bool {
	core := strings.TrimFunc(tok, func(r rune) bool {
		return !isWordRune(r) && r != '.'
	})
	if !strings.HasSuffix(core, ".") {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(core, "."), ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		runes := []rune(part)
		if len(runes) != 1 || !unicode.IsLetter(runes[0]) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool    { return unicode.IsLetter(r) || unicode.IsDigit(r) }
func isLowerLetter(r rune) bool { return unicode.IsLetter(r) && unicode.IsLower(r) }
func isUpperLetter(r rune) bool { return unicode.IsLetter(r) && unicode.IsUpper(r) }
