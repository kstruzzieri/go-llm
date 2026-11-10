package tools

import (
	"bytes"
	"strings"
)

// screenKeyFloor is the shortest key the screen will withhold.
const screenKeyFloor = 8

// gitConfigValue returns the value git 2.56.0 prints for the text after a
// setting's "=", for a value that ends where its record ends. It returns nil
// when git refuses the line. The body follows git's parse_value (config.c)
// step for step; do not simplify it.
func gitConfigValue(r []byte) []byte {
	out := []byte{}
	trim := 0 // 0 means unset, as in git
	quote, comment := false, false
loop:
	for i := 0; i < len(r); i++ {
		c := r[i]
		if comment {
			continue
		}
		if (c == ' ' || c == '\t' || c == '\r' || c == '\n') && !quote {
			if trim == 0 {
				trim = len(out)
			}
			if len(out) > 0 {
				out = append(out, c)
			}
			continue
		}
		if !quote && (c == ';' || c == '#') {
			comment = true
			continue
		}
		trim = 0
		if c == '\\' {
			if i+1 >= len(r) {
				break loop
			}
			i++
			switch r[i] {
			case 't':
				c = '\t'
			case 'b':
				c = '\b'
			case 'n':
				c = '\n'
			case '\\', '"':
				c = r[i]
			default:
				return nil
			}
			out = append(out, c)
			continue
		}
		if c == '"' {
			quote = !quote
			continue
		}
		out = append(out, c)
	}
	if quote {
		return nil
	}
	if trim != 0 {
		out = out[:trim]
	}
	if n := bytes.IndexByte(out, 0); n >= 0 {
		out = out[:n]
	}
	return out
}

// credentialFileKeys returns the byte strings to withhold from command output
// for one credential file: each record, and the values inside it. Keys are
// CSI-stripped, at least screenKeyFloor bytes, and unique. overLong reports a
// record longer than keyCap.
func credentialFileKeys(content []byte, gitConfig bool, keyCap int) (keys [][]byte, overLong bool) {
	seen := map[string]bool{}
	add := func(k []byte) {
		k = stripCSI(k)
		if len(k) < screenKeyFloor || seen[string(k)] {
			return
		}
		seen[string(k)] = true
		keys = append(keys, k)
	}
	content = bytes.TrimPrefix(content, []byte("\xef\xbb\xbf"))
	for _, rec := range bytes.FieldsFunc(content, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if len(rec) > keyCap {
			overLong = true
		}
		rec = stripCSI(rec)
		line := bytes.TrimSpace(rec)
		add(line)
		if gitConfig {
			gitConfigKeys(rec, add)
		} else if eq := bytes.IndexByte(line, '='); eq >= 0 {
			envValueKeys(bytes.TrimSpace(line[eq+1:]), add)
		}
	}
	return keys, overLong
}

// envValueKeys adds the K2 forms of r, the trimmed text after a .env line's
// first "=".
func envValueKeys(r []byte, add func([]byte)) {
	add(r)
	if len(r) > 0 && (r[0] == '"' || r[0] == '\'') {
		if end := bytes.IndexByte(r[1:], r[0]); end >= 0 {
			add(r[1 : 1+end])
		}
		return
	}
	if i := bytes.IndexByte(r, '#'); i >= 0 {
		add(bytes.TrimSpace(r[:i]))
	}
	for i := 1; i < len(r); i++ {
		if r[i] == ';' && (r[i-1] == ' ' || r[i-1] == '\t') {
			add(bytes.TrimSpace(r[:i]))
			break
		}
	}
}

func isSettingNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-'
}

// gitConfigKeys adds the keys for one git config record: what git prints for
// each setting on the line, URL userinfo in the record and in those values,
// and the extraheader forms.
func gitConfigKeys(rec []byte, add func([]byte)) {
	texts := [][]byte{rec}
	for eq := range rec {
		if rec[eq] != '=' {
			continue
		}
		end := eq
		for end > 0 && (rec[end-1] == ' ' || rec[end-1] == '\t') {
			end--
		}
		start := end
		for start > 0 && isSettingNameByte(rec[start-1]) {
			start--
		}
		if start == end {
			continue
		}
		v := gitConfigValue(rec[eq+1:])
		if v != nil {
			texts = append(texts, v)
		}
		if strings.EqualFold(string(rec[start:end]), "extraheader") {
			add(bytes.TrimSpace(rec[eq+1:]))
			if v != nil {
				add(v)
				add(bytes.TrimSpace(v))
			}
		}
	}
	for _, t := range texts {
		urlUserinfoKeys(t, add)
	}
}

// urlUserinfoKeys adds the userinfo of each scheme://userinfo@ in t, read to
// the first "@" and to the last "@" of the authority, with its user and
// password parts, each also percent-decoded.
func urlUserinfoKeys(t []byte, add func([]byte)) {
	for rest := t; ; {
		i := bytes.Index(rest, []byte("://"))
		if i < 0 {
			return
		}
		auth := rest[i+3:]
		rest = auth
		if n := bytes.IndexAny(auth, "/ \t"); n >= 0 {
			auth = auth[:n]
		}
		first, last := bytes.IndexByte(auth, '@'), bytes.LastIndexByte(auth, '@')
		if first <= 0 {
			continue
		}
		for _, u := range [][]byte{auth[:first], auth[:last]} {
			add(u)
			add(pctDecode(u))
			if c := bytes.IndexByte(u, ':'); c >= 0 {
				for _, p := range [][]byte{u[:c], u[c+1:]} {
					add(p)
					add(pctDecode(p))
				}
			}
		}
	}
}

// pctDecode undoes %XX only; "+" stays as it is.
func pctDecode(s []byte) []byte {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && unhex(s[i+1]) >= 0 && unhex(s[i+2]) >= 0 {
			out = append(out, byte(unhex(s[i+1])<<4|unhex(s[i+2])))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return out
}

func unhex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
