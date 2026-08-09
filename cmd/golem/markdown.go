package main

import (
	"io"
	"unicode/utf8"
)

const markdownPrefixLimit = 16
const markdownRunLimit = 16
const markdownEmphasisLimit = 128

const (
	markdownReset    = "\x1b[0m"
	markdownHeading  = "\x1b[1m"
	markdownEmphasis = "\x1b[3m"
	markdownAccent   = "\x1b[36m"
	markdownFence    = "\x1b[2;36m"
)

type markdownLine uint8

const (
	markdownPrefix markdownLine = iota
	markdownPlain
	markdownHeadingLine
	markdownCodeLine
	markdownFenceLine
)

type markdownPrefixKind uint8

const (
	markdownWait markdownPrefixKind = iota
	markdownUsePlain
	markdownUseHeading
	markdownUseList
	markdownOpenFence
	markdownCloseFence
	markdownUseCode
)

type markdownPrefixDecision struct {
	kind      markdownPrefixKind
	indent    int
	fenceSize int
}

// markdownWriter preserves the byte stream and adds only ANSI SGR styling.
// It holds at most markdownPrefixLimit ambiguous bytes plus one deciding byte;
// overflow degrades that line to prose/code verbatim.
type markdownWriter struct {
	out io.Writer
	on  bool

	line        markdownLine
	prefix      []byte
	inFence     bool
	fenceSize   int
	openingSize int
	closing     bool
	closeSpace  bool
	emphasis    []byte
	styled      []byte
	styledStyle string
	runStyle    string
	run         []byte
	lastNL      bool
}

type markdownRawWriter struct{ markdown *markdownWriter }

func (w markdownRawWriter) Write(p []byte) (int, error) { return w.markdown.WriteRaw(p) }

func newMarkdownWriter(out io.Writer, enabled bool) *markdownWriter {
	return &markdownWriter{out: out, on: enabled, line: markdownPrefix, lastNL: true}
}

func (w *markdownWriter) Write(p []byte) (int, error) {
	if !w.on {
		return w.directWrite(p)
	}
	for i, b := range p {
		if err := w.writeByte(b); err != nil {
			return i, err
		}
	}
	if w.runStyle == "" {
		if err := w.flushRun(); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// WriteStyled writes raw terminal content with one SGR style while retaining
// only an incomplete trailing UTF-8 rune across calls.
func (w *markdownWriter) WriteStyled(style string, p []byte) (int, error) {
	data := append(w.styled, p...)
	w.styled = nil
	w.styledStyle = style
	complete := 0
	for complete < len(data) && utf8.FullRune(data[complete:]) {
		_, size := utf8.DecodeRune(data[complete:])
		complete += size
	}
	if complete > 0 {
		if err := w.emit(style, data[:complete]); err != nil {
			return len(p), err
		}
	}
	if complete < len(data) {
		w.styled = append(w.styled, data[complete:]...)
	}
	if err := w.flushRun(); err != nil {
		return len(p), err
	}
	return len(p), nil
}

func (w *markdownWriter) writeByte(b byte) error {
	if w.line != markdownPrefix {
		return w.writeKnown(b)
	}
	w.prefix = append(w.prefix, b)
	decision := w.decidePrefix()
	if decision.kind == markdownWait && len(w.prefix) > markdownPrefixLimit {
		if w.inFence {
			w.closing = true
			last := w.prefix[len(w.prefix)-1]
			w.closeSpace = last == ' ' || last == '\t' || last == '\r'
			decision.kind = markdownUseCode
		} else {
			decision.kind = markdownUsePlain
		}
	}
	if decision.kind == markdownWait {
		return nil
	}
	return w.resolvePrefix(decision)
}

func (w *markdownWriter) decidePrefix() markdownPrefixDecision {
	if w.inFence {
		return w.decideFenceClose(false)
	}
	return decideMarkdownOpen(w.prefix)
}

func decideMarkdownOpen(p []byte) markdownPrefixDecision {
	indent := leadingMarkdownIndent(p)
	if indent < 0 {
		return markdownPrefixDecision{kind: markdownUsePlain}
	}
	if indent == len(p) {
		return markdownPrefixDecision{kind: markdownWait}
	}
	rest := p[indent:]
	switch rest[0] {
	case '\n':
		return markdownPrefixDecision{kind: markdownUsePlain}
	case '#':
		n := countPrefix(rest, '#')
		if n > 6 {
			return markdownPrefixDecision{kind: markdownUsePlain}
		}
		if len(rest) == n {
			return markdownPrefixDecision{kind: markdownWait}
		}
		if rest[n] == ' ' || rest[n] == '\t' {
			return markdownPrefixDecision{kind: markdownUseHeading}
		}
		return markdownPrefixDecision{kind: markdownUsePlain}
	case '-', '+', '*':
		if len(rest) == 1 {
			return markdownPrefixDecision{kind: markdownWait}
		}
		if rest[1] == ' ' || rest[1] == '\t' {
			return markdownPrefixDecision{kind: markdownUseList, indent: indent}
		}
		return markdownPrefixDecision{kind: markdownUsePlain}
	case '`':
		n := countPrefix(rest, '`')
		if n < 3 && len(rest) == n {
			return markdownPrefixDecision{kind: markdownWait}
		}
		if n < 3 {
			return markdownPrefixDecision{kind: markdownUsePlain}
		}
		if len(rest) == n {
			return markdownPrefixDecision{kind: markdownWait}
		}
		return markdownPrefixDecision{kind: markdownOpenFence, fenceSize: n}
	default:
		if rest[0] < '0' || rest[0] > '9' {
			return markdownPrefixDecision{kind: markdownUsePlain}
		}
		n := 0
		for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
			n++
		}
		if n == len(rest) {
			return markdownPrefixDecision{kind: markdownWait}
		}
		if rest[n] != '.' && rest[n] != ')' {
			return markdownPrefixDecision{kind: markdownUsePlain}
		}
		if n+1 == len(rest) {
			return markdownPrefixDecision{kind: markdownWait}
		}
		if rest[n+1] == ' ' || rest[n+1] == '\t' {
			return markdownPrefixDecision{kind: markdownUseList, indent: indent}
		}
		return markdownPrefixDecision{kind: markdownUsePlain}
	}
}

func decideFenceOpenAtLineEnd(p []byte) markdownPrefixDecision {
	indent := leadingMarkdownIndent(p)
	if indent < 0 || indent == len(p) {
		return markdownPrefixDecision{kind: markdownUsePlain}
	}
	rest := p[indent:]
	if rest[0] != '`' {
		return markdownPrefixDecision{kind: markdownUsePlain}
	}
	n := countPrefix(rest, '`')
	if n >= 3 && n == len(rest) {
		return markdownPrefixDecision{kind: markdownOpenFence, fenceSize: n}
	}
	return markdownPrefixDecision{kind: markdownUsePlain}
}

func (w *markdownWriter) decideFenceClose(atLineEnd bool) markdownPrefixDecision {
	indent := leadingMarkdownIndent(w.prefix)
	if indent < 0 {
		return markdownPrefixDecision{kind: markdownUseCode}
	}
	if indent == len(w.prefix) {
		if atLineEnd {
			return markdownPrefixDecision{kind: markdownUseCode}
		}
		return markdownPrefixDecision{kind: markdownWait}
	}
	rest := w.prefix[indent:]
	if rest[0] == '\n' {
		return markdownPrefixDecision{kind: markdownUseCode}
	}
	if rest[0] != '`' {
		return markdownPrefixDecision{kind: markdownUseCode}
	}
	n := countPrefix(rest, '`')
	if len(rest) == n {
		if atLineEnd {
			if n >= w.fenceSize {
				return markdownPrefixDecision{kind: markdownCloseFence, fenceSize: n}
			}
			return markdownPrefixDecision{kind: markdownUseCode}
		}
		return markdownPrefixDecision{kind: markdownWait}
	}
	if n < w.fenceSize {
		return markdownPrefixDecision{kind: markdownUseCode}
	}
	for _, b := range rest[n:] {
		switch b {
		case ' ', '\t', '\r':
		case '\n':
			return markdownPrefixDecision{kind: markdownCloseFence, fenceSize: n}
		default:
			return markdownPrefixDecision{kind: markdownUseCode}
		}
	}
	if atLineEnd {
		return markdownPrefixDecision{kind: markdownCloseFence, fenceSize: n}
	}
	return markdownPrefixDecision{kind: markdownWait}
}

func leadingMarkdownIndent(p []byte) int {
	i := 0
	for i < len(p) && p[i] == ' ' {
		i++
		if i > 3 {
			return -1
		}
	}
	return i
}

func countPrefix(p []byte, want byte) int {
	i := 0
	for i < len(p) && p[i] == want {
		i++
	}
	return i
}

func (w *markdownWriter) resolvePrefix(decision markdownPrefixDecision) error {
	pending := w.prefix
	w.prefix = nil
	switch decision.kind {
	case markdownUseHeading:
		w.line = markdownHeadingLine
	case markdownUseList:
		if err := w.emit("", pending[:decision.indent]); err != nil {
			return err
		}
		if err := w.emit(markdownAccent, pending[decision.indent:]); err != nil {
			return err
		}
		w.line = markdownPlain
		return nil
	case markdownOpenFence:
		w.line = markdownFenceLine
		w.openingSize = decision.fenceSize
		w.closing = false
	case markdownCloseFence:
		w.line = markdownFenceLine
		w.closing = true
	case markdownUseCode:
		w.line = markdownCodeLine
	case markdownUsePlain:
		w.line = markdownPlain
		for _, b := range pending {
			if err := w.writePlain(b); err != nil {
				return err
			}
		}
		return nil
	default:
		w.line = markdownPlain
	}
	if err := w.emit(w.styleForLine(), pending); err != nil {
		return err
	}
	if len(pending) > 0 && pending[len(pending)-1] == '\n' {
		w.endLine()
	}
	return nil
}

func (w *markdownWriter) writeKnown(b byte) error {
	if w.line == markdownPlain {
		return w.writePlain(b)
	}
	if w.line == markdownCodeLine && w.closing {
		switch b {
		case '`':
			if w.closeSpace {
				w.closing = false
			}
		case ' ', '\t', '\r':
			w.closeSpace = true
		case '\n':
		default:
			w.closing = false
		}
	}
	if err := w.emit(w.styleForLine(), []byte{b}); err != nil {
		return err
	}
	if b != '\n' {
		return nil
	}
	w.endLine()
	return nil
}

func (w *markdownWriter) writePlain(b byte) error {
	if b == '\n' {
		if err := w.flushEmphasis(); err != nil {
			return err
		}
		if err := w.emit("", []byte{b}); err != nil {
			return err
		}
		w.endLine()
		return nil
	}
	if len(w.emphasis) == 0 {
		if b == '*' {
			w.emphasis = append(w.emphasis, b)
			return nil
		}
		return w.emit("", []byte{b})
	}
	w.emphasis = append(w.emphasis, b)
	if b == '*' {
		if len(w.emphasis) == 2 {
			return w.flushEmphasis()
		}
		pending := w.emphasis
		w.emphasis = nil
		return w.emit(markdownEmphasis, pending)
	}
	if len(w.emphasis) > markdownEmphasisLimit {
		return w.flushEmphasis()
	}
	return nil
}

func (w *markdownWriter) flushEmphasis() error {
	if len(w.emphasis) == 0 {
		return nil
	}
	pending := w.emphasis
	w.emphasis = nil
	return w.emit("", pending)
}

func (w *markdownWriter) flushStyled() error {
	if len(w.styled) == 0 {
		return nil
	}
	pending := w.styled
	w.styled = nil
	return w.emit(w.styledStyle, pending)
}

func (w *markdownWriter) endLine() {
	if w.closing {
		w.inFence = false
		w.fenceSize = 0
	} else if w.line == markdownFenceLine {
		w.inFence = true
		w.fenceSize = w.openingSize
	}
	w.startLine()
}

func (w *markdownWriter) styleForLine() string {
	switch w.line {
	case markdownHeadingLine:
		return markdownHeading
	case markdownCodeLine:
		return markdownAccent
	case markdownFenceLine:
		return markdownFence
	default:
		return ""
	}
}

func (w *markdownWriter) emit(style string, p []byte) error {
	for _, b := range p {
		if b == '\n' {
			if err := w.flushRun(); err != nil {
				return err
			}
			if err := w.writeOut([]byte{'\n'}); err != nil {
				return err
			}
			continue
		}
		if len(w.run) > 0 && w.runStyle != style {
			if err := w.flushRun(); err != nil {
				return err
			}
		}
		if len(w.run) >= markdownRunLimit && (utf8.RuneStart(b) || len(w.run) >= markdownRunLimit+utf8.UTFMax-1) {
			if err := w.flushRun(); err != nil {
				return err
			}
		}
		w.runStyle = style
		w.run = append(w.run, b)
	}
	return nil
}

func (w *markdownWriter) flushRun() error {
	if len(w.run) == 0 {
		return nil
	}
	if w.runStyle != "" {
		if err := w.writeOut([]byte(w.runStyle)); err != nil {
			return err
		}
	}
	if err := w.writeOut(w.run); err != nil {
		return err
	}
	if w.runStyle != "" {
		if err := w.writeOut([]byte(markdownReset)); err != nil {
			return err
		}
	}
	w.run = nil
	w.runStyle = ""
	return nil
}

func (w *markdownWriter) writeOut(p []byte) error {
	_, err := w.directWrite(p)
	return err
}

func (w *markdownWriter) directWrite(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.observe(p[:n])
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (w *markdownWriter) observe(p []byte) {
	if len(p) > 0 {
		w.lastNL = p[len(p)-1] == '\n'
	}
}

// WriteRaw flushes pending Markdown and writes terminal chrome through the
// same cursor tracker without interpreting it as model content.
func (w *markdownWriter) WriteRaw(p []byte) (int, error) {
	if err := w.flush(len(p) > 0 && p[0] == '\n'); err != nil {
		return 0, err
	}
	n, err := w.directWrite(p)
	if n > 0 {
		if p[n-1] == '\n' {
			w.startLine()
		} else if w.inFence {
			w.line = markdownCodeLine
		} else {
			w.line = markdownPlain
		}
	}
	return n, err
}

func (w *markdownWriter) BreakLine() error {
	if err := w.flush(true); err != nil {
		return err
	}
	if w.lastNL {
		w.startLine()
		return nil
	}
	_, err := w.WriteRaw([]byte{'\n'})
	return err
}

func (w *markdownWriter) startLine() {
	w.line = markdownPrefix
	w.prefix = nil
	w.emphasis = nil
	w.styled = nil
	w.styledStyle = ""
	w.openingSize = 0
	w.closing = false
	w.closeSpace = false
}

// flush emits ambiguous input verbatim and closes any active SGR. A forced
// line end also commits a complete pending closing fence.
func (w *markdownWriter) flush(atLineEnd bool) error {
	if !w.on {
		return nil
	}
	if len(w.prefix) > 0 {
		if atLineEnd {
			decision := decideFenceOpenAtLineEnd(w.prefix)
			if w.inFence {
				decision = w.decideFenceClose(true)
			}
			if decision.kind == markdownOpenFence || decision.kind == markdownCloseFence {
				if err := w.resolvePrefix(decision); err != nil {
					return err
				}
			}
		}
		if len(w.prefix) > 0 {
			pending := w.prefix
			w.prefix = nil
			if w.inFence {
				w.line = markdownCodeLine
			} else {
				w.line = markdownPlain
			}
			if err := w.emit(w.styleForLine(), pending); err != nil {
				return err
			}
		}
	}
	if err := w.flushEmphasis(); err != nil {
		return err
	}
	if err := w.flushStyled(); err != nil {
		return err
	}
	if err := w.flushRun(); err != nil {
		return err
	}
	if atLineEnd && (w.line == markdownFenceLine || w.closing) {
		w.endLine()
	}
	return nil
}

// Finish flushes the stream and discards unterminated block state. It never
// adds a newline or changes the Markdown bytes.
func (w *markdownWriter) Finish() error {
	if err := w.flush(true); err != nil {
		return err
	}
	w.inFence = false
	w.fenceSize = 0
	w.startLine()
	return nil
}
