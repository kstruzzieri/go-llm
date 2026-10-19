package agent

import (
	"strconv"
	"strings"
	"unicode"
)

// SanitizePreview returns preview with every rune a terminal or log viewer
// could act on rendered as visible, inert text. LF and graphic runes pass
// through unchanged; every other rune (ESC, C0 and C1 controls, CR, BS, DEL,
// tab, bidi and other format characters, line and paragraph separators) is
// replaced by its strconv.QuoteToGraphic escape without the surrounding quotes.
// An invalid UTF-8 byte decodes to U+FFFD, which is graphic, so a raw C1 byte
// never survives. The result is idempotent: its escapes are plain graphic
// characters.
//
// It sanitizes a display copy only. The runtime applies it where a preview
// reaches an Approver (approve in dispatch.go) so a plain, keyed, risk-aware or
// embedded approver all receive the same inert text. The approval key, the
// planned and invoked arguments, and any bytes a tool writes are never
// sanitized: they are not display data.
func SanitizePreview(preview string) string {
	var safe strings.Builder
	for _, r := range preview {
		if r == '\n' || unicode.IsGraphic(r) {
			safe.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteToGraphic(string(r))
		safe.WriteString(quoted[1 : len(quoted)-1])
	}
	return safe.String()
}
