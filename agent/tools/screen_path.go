package tools

import (
	"bytes"

	"github.com/kstruzzieri/go-llm/agent/interceptor"
)

// stripCSI removes ANSI CSI sequences (ESC [, parameter bytes, intermediate
// bytes, one final byte) from rec. A sequence the record ends inside stays as
// written.
func stripCSI(rec []byte) []byte {
	out := make([]byte, 0, len(rec))
	for i := 0; i < len(rec); {
		if rec[i] != 0x1b || i+1 >= len(rec) || rec[i+1] != '[' {
			out = append(out, rec[i])
			i++
			continue
		}
		j := i + 2
		for j < len(rec) && rec[j] >= 0x30 && rec[j] <= 0x3f {
			j++
		}
		for j < len(rec) && rec[j] >= 0x20 && rec[j] <= 0x2f {
			j++
		}
		if j < len(rec) && rec[j] >= 0x40 && rec[j] <= 0x7e {
			i = j + 1
			continue
		}
		out = append(out, rec[i])
		i++
	}
	return out
}

// gitUnquote decodes a C-style quoted path as git prints it. s starts at a
// double quote; n counts the bytes consumed through the closing quote. Only
// \a \b \f \n \r \t \v \" \\ and three-digit octal escapes up to \377 are
// accepted.
func gitUnquote(s []byte) (path []byte, n int, ok bool) {
	if len(s) == 0 || s[0] != '"' {
		return nil, 0, false
	}
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			return path, i + 1, true
		case c != '\\':
			path = append(path, c)
		default:
			i++
			if i >= len(s) {
				return nil, 0, false
			}
			e := s[i]
			if m, found := gitSimpleEscapes[e]; found {
				path = append(path, m)
				continue
			}
			if e < '0' || e > '3' || i+2 >= len(s) ||
				s[i+1] < '0' || s[i+1] > '7' || s[i+2] < '0' || s[i+2] > '7' {
				return nil, 0, false
			}
			path = append(path, (e-'0')<<6|(s[i+1]-'0')<<3|(s[i+2]-'0'))
			i += 2
		}
	}
	return nil, 0, false
}

var gitSimpleEscapes = map[byte]byte{
	'a': '\a', 'b': '\b', 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v',
	'"': '"', '\\': '\\',
}

// credentialPatchHeader reports whether a CSI-stripped record, without its
// terminator, looks like a patch header naming a credential path. After every
// "diff -" or "+++ " the rest of the record is read as pieces: a git-quoted
// run is one piece, anything else runs to the next space or tab and loses one
// quote at either end. The rule is deliberately loose; "--- " is not a marker.
func credentialPatchHeader(rec []byte) bool {
	for _, marker := range [][]byte{[]byte("diff -"), []byte("+++ ")} {
		for off := 0; ; {
			k := bytes.Index(rec[off:], marker)
			if k < 0 {
				break
			}
			off += k + len(marker)
			if anyCredentialPiece(rec[off:]) {
				return true
			}
		}
	}
	return false
}

func anyCredentialPiece(s []byte) bool {
	for len(s) > 0 {
		s = bytes.TrimLeft(s, " \t")
		if len(s) == 0 {
			break
		}
		if s[0] == '"' {
			if p, n, ok := gitUnquote(s); ok {
				if interceptor.IsCredentialPath(string(p)) {
					return true
				}
				s = s[n:]
				continue
			}
		}
		end := bytes.IndexAny(s, " \t")
		if end < 0 {
			end = len(s)
		}
		p := s[:end]
		if p[0] == '"' {
			p = p[1:]
		}
		if len(p) > 0 && p[len(p)-1] == '"' {
			p = p[:len(p)-1]
		}
		if interceptor.IsCredentialPath(string(p)) {
			return true
		}
		s = s[end:]
	}
	return false
}

// credentialPathPrefix reports whether a CSI-stripped record, without its
// terminator, has a credential path in any colon-separated field but the
// last, taken as written (grep -n, git grep, and error messages).
func credentialPathPrefix(rec []byte) bool {
	fields := bytes.Split(rec, []byte{':'})
	for _, f := range fields[:len(fields)-1] {
		if interceptor.IsCredentialPath(string(f)) {
			return true
		}
	}
	return false
}
