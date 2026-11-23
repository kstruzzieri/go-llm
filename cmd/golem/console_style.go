package main

import (
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// SGR parameters used by the console renderers.
const (
	sgrDim        = "2"
	sgrBold       = "1"
	sgrCyan       = "36"
	sgrYellow     = "33"
	sgrYellowBold = "1;33"
	sgrGreen      = "32"
)

// consoleStyle is how startup console output is drawn: color on or off and
// the line width to fill.
type consoleStyle struct {
	color bool
	width int
}

// styleFor returns the style for w: color and the terminal width when w is a
// terminal file, plain at 80 columns otherwise.
func styleFor(w io.Writer, noColor bool) consoleStyle {
	f, ok := w.(*os.File)
	if !ok {
		return consoleStyle{false, 80}
	}
	return styleForOps(realTermOps{}, f, noColor)
}

// styleForOps is styleFor with the terminal seam injected. A size error or a
// non-positive width gives 80; otherwise the width is clamped to at least 40.
func styleForOps(ops termOps, f *os.File, noColor bool) consoleStyle {
	fd := int(f.Fd())
	if !ops.IsTerminal(fd) {
		return consoleStyle{false, 80}
	}
	width, _, err := ops.GetSize(fd)
	if err != nil || width < 1 {
		width = 80
	}
	return consoleStyle{colorPermitted(noColor), max(40, width)}
}

// paint wraps text in the SGR sequence when color is on.
func (s consoleStyle) paint(sgr, text string) string {
	if !s.color {
		return text
	}
	return "\x1b[" + sgr + "m" + text + "\x1b[0m"
}

// rule draws a section rule: a bold title, an optional dim subtitle, and dashes
// to the line width (at least three). A subtitle that would leave fewer than
// three dashes within the width is dropped, so the rule does not wrap.
// ponytail: each box-drawing dash counts as one column; ambiguous-width
// terminals (CJK) can wrap it.
func (s consoleStyle) rule(title, subtitle string) string {
	const dashes = "\u2500\u2500 "
	cols := func(t string) int { return displayCols(strings.ReplaceAll(t, "\u2500", "-")) }
	head, tail := dashes+title+" ", ""
	if subtitle != "" && cols(head+dashes+subtitle+" ")+3 <= s.width {
		tail = dashes + subtitle + " "
	}
	fill := strings.Repeat("\u2500", max(3, s.width-cols(head+tail)))
	return s.paint(sgrDim, dashes) + s.paint(sgrBold, title) + " " + s.paint(sgrDim, tail+fill)
}

// wrapList joins items with ", " under firstPrefix, breaking only between
// items and continuing under contPrefix. An item too long for a line stands
// alone, whole.
func wrapList(items []string, firstPrefix, contPrefix string, width int) []string {
	var lines []string
	cur := firstPrefix
	empty := true
	for i, it := range items {
		piece := it
		if i < len(items)-1 {
			piece += ","
		}
		if !empty && displayCols(cur)+1+displayCols(piece) > width {
			lines = append(lines, cur)
			cur, empty = contPrefix, true
		}
		if !empty {
			cur += " "
		}
		cur += piece
		empty = false
	}
	return append(lines, cur)
}

// escapeNonGraphic turns invalid UTF-8 into U+FFFD and visibly escapes every
// non-graphic rune (C0, DEL, C1, bidi and format controls, tab, LF) with
// strconv.QuoteToGraphic's convention.
func escapeNonGraphic(s string) string {
	s = strings.ToValidUTF8(s, "\ufffd")
	var b strings.Builder
	for _, r := range s {
		if unicode.IsGraphic(r) {
			b.WriteRune(r)
			continue
		}
		q := strconv.QuoteToGraphic(string(r))
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}

// displayField is the one cleaner for untrusted text shown on the console:
// combining-mark floods are quoted, then non-graphic runes are escaped.
func displayField(s string) string {
	return escapeNonGraphic(quoteMarkFloods(s))
}
