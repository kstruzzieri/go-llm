package tools

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"unicode"
)

// bom is the byte order mark; an "@" right after it starts a token.
const bom = '\ufeff'

// importTokenMax is the longest import token kept, in bytes. A longer token
// cannot name a file, so it is dropped without being buffered.
const importTokenMax = 4096

// scanMarkdownImports streams r once and calls emit for every Markdown "@"
// import token (spec D3). An "@" starts a token at the start of the input, after
// U+FEFF, or after any unicode.IsSpace rune; the token runs to the next
// unescaped space rune, and a backslash-space continues it. For each token emit
// gets the token as written and, when different and non-empty, the token with
// trailing ".,;:)" removed. Tokens over importTokenMax bytes are dropped. The
// scan holds at most importTokenMax+1 token bytes, whatever the line length.
func scanMarkdownImports(r io.Reader, emit func(token string)) error {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReader(r)
	}
	var tok []byte
	var inTok, over bool
	atStart := true // an "@" here would start a token
	flush := func() {
		if inTok && !over && len(tok) > 0 {
			s := string(tok)
			emit(s)
			if trimmed := strings.TrimRight(s, ".,;:)"); trimmed != s && trimmed != "" {
				emit(trimmed)
			}
		}
		tok, inTok, over = tok[:0], false, false
	}
	add := func(b ...byte) {
		if over {
			return
		}
		if len(tok)+len(b) > importTokenMax {
			over, tok = true, tok[:0]
			return
		}
		tok = append(tok, b...)
	}
	for {
		c, _, err := br.ReadRune()
		if err != nil {
			flush()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch {
		case inTok && c == '\\':
			n, _, err := br.ReadRune()
			switch {
			case err == nil && n == ' ':
				add(' ')
			case err == nil:
				add('\\')
				_ = br.UnreadRune()
			default:
				add('\\')
			}
		case inTok && unicode.IsSpace(c):
			flush()
			atStart = true
		case inTok:
			add([]byte(string(c))...)
		case c == '@' && atStart:
			inTok = true
			atStart = false
		default:
			atStart = c == bom || unicode.IsSpace(c)
		}
	}
}
