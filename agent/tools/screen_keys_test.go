package tools

import (
	"bytes"
	"slices"
	"sort"
	"strings"
	"testing"
)

func keyStrings(keys [][]byte) []string {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = string(k)
	}
	sort.Strings(out)
	return out
}

func TestCredentialFileKeys(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	exact := []struct {
		label   string
		content string
		want    []string
	}{
		{"env-plain", "NAME=value-0001-abcdef", []string{"NAME=value-0001-abcdef", "value-0001-abcdef"}},
		{"env-quoted-note", `NAME="value-0002-abcdef" # note`, []string{`NAME="value-0002-abcdef" # note`, `"value-0002-abcdef" # note`, "value-0002-abcdef"}},
		{"env-hash-spaced", "NAME=value-0003-abcdef # note", []string{"NAME=value-0003-abcdef # note", "value-0003-abcdef # note", "value-0003-abcdef"}},
		{"env-hash-glued", "NAME=value-0004-abcdef#note", []string{"NAME=value-0004-abcdef#note", "value-0004-abcdef#note", "value-0004-abcdef"}},
		{"env-export", "export NAME=value-0005-abcdef", []string{"export NAME=value-0005-abcdef", "value-0005-abcdef"}},
		{"env-spaced-eq", "NAME = value-0006-abcdef", []string{"NAME = value-0006-abcdef", "value-0006-abcdef"}},
		{"env-colon-eq", "NAME := value-0007-abcdef", []string{"NAME := value-0007-abcdef", "value-0007-abcdef"}},
		{"env-inner-eq", "NAME=a=value-0008-abcdef", []string{"NAME=a=value-0008-abcdef", "a=value-0008-abcdef"}},
		{"env-semicolon", "NAME=value-0009-abcdef ; note", []string{"NAME=value-0009-abcdef ; note", "value-0009-abcdef ; note", "value-0009-abcdef"}},
		{"bom-crlf", "\xef\xbb\xbfNAME=value-0001-abcdef\r\n", []string{"NAME=value-0001-abcdef", "value-0001-abcdef"}},
		{"floor-7-value", "NAME=1234567", []string{"NAME=1234567"}},
		{"floor-8-value", "NAME=12345678", []string{"NAME=12345678", "12345678"}},
		{"floor-7-line", "ABCD=12", nil},
		// No "=": only the line key (K1) can key these, at exactly 8 and 7 bytes.
		{"floor-8-line-only", "line-047", []string{"line-047"}},
		{"floor-7-line-only", "line-48", nil},
		{"no-eq", "machine example.invalid password value-0010-abcdef", []string{"machine example.invalid password value-0010-abcdef"}},
		{"pem", "-----BEGIN PRIVATE KEY-----\npem-body-0013-abcdef\nshort\n-----END PRIVATE KEY-----\n", []string{"-----BEGIN PRIVATE KEY-----", "pem-body-0013-abcdef", "-----END PRIVATE KEY-----"}},
		{"csi-inside", "NAME=va\x1b[1mlue-0011-abcdef", []string{"NAME=value-0011-abcdef", "value-0011-abcdef"}},
		{"cr-splits-records", "NAME=abcd\rwxyz", []string{"NAME=abcd"}},
		{"colon-no-eq", "password: value-0014-abcdef", []string{"password: value-0014-abcdef"}},
		{"empty", "", nil},
		{"csi-in-eq-run", "NAME\x1b[=1m=value-0042-abcdef", []string{"NAME=value-0042-abcdef", "1m=value-0042-abcdef"}},
		{"csi-before-semicolon", "NAME=value-0043-abcdef \x1b[1m; note", []string{"NAME=value-0043-abcdef ; note", "value-0043-abcdef ; note"}},
		{"env-padded", "  NAME=value-0046-abcdef  ", []string{"NAME=value-0046-abcdef", "value-0046-abcdef"}},
		{"env-single-quoted", "NAME='value-0111-abcdef' x", []string{"NAME='value-0111-abcdef' x", "'value-0111-abcdef' x", "value-0111-abcdef"}},
		{"env-unclosed-quote", "NAME=\"unterminated-0112", []string{"NAME=\"unterminated-0112", "\"unterminated-0112"}},
		{"env-tab-semicolon", "NAME=value-0113-abcdef\t; note", []string{"NAME=value-0113-abcdef\t; note", "value-0113-abcdef\t; note", "value-0113-abcdef"}},
		// A line that starts with "=": the text after it is still a value.
		{"env-leading-eq", "=value-0110-abcdef", []string{"=value-0110-abcdef", "value-0110-abcdef"}},
		// Nothing after "=": the line is the only key.
		{"env-empty-value", "LONGNAME=", []string{"LONGNAME="}},
		// A credential-store line (.git-credentials) is itself a URL: its
		// userinfo is keyed as a .git/config URL's is, under the same floor.
		{"store-user-pass", "https://user-0139:pass-0139-abcdef@example.invalid",
			[]string{"https://user-0139:pass-0139-abcdef@example.invalid", "user-0139:pass-0139-abcdef", "user-0139", "pass-0139-abcdef"}},
		{"store-pct-pass", "https://user-0140:p%40ss-0140-abcdef@example.invalid",
			[]string{"https://user-0140:p%40ss-0140-abcdef@example.invalid", "user-0140:p%40ss-0140-abcdef", "user-0140:p@ss-0140-abcdef", "user-0140", "p%40ss-0140-abcdef", "p@ss-0140-abcdef"}},
		{"store-user-only", "https://tokn-0141-abcdef@example.invalid", []string{"https://tokn-0141-abcdef@example.invalid", "tokn-0141-abcdef"}},
		{"store-short-parts", "https://u-142:p-142@example.invalid", []string{"https://u-142:p-142@example.invalid", "u-142:p-142"}},
		// A URL after a name or other text is not a store line: only the
		// line and NAME=value forms are keys.
		{"store-after-name", "DATABASE_URL=postgres://user-0143:pass-0143-abcdef@db.example.invalid",
			[]string{"DATABASE_URL=postgres://user-0143:pass-0143-abcdef@db.example.invalid", "postgres://user-0143:pass-0143-abcdef@db.example.invalid"}},
		{"store-after-text", "see https://user-0144:pass-0144-abcdef@example.invalid", []string{"see https://user-0144:pass-0144-abcdef@example.invalid"}},
	}
	for _, tc := range exact {
		t.Run(tc.label, func(t *testing.T) {
			keys, over, crowded := credentialFileKeys([]byte(tc.content), false, 1<<14)
			if over || crowded {
				t.Fatalf("overLong %t, crowded %t", over, crowded)
			}
			got := keyStrings(keys)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
				t.Fatalf("key set differs: got %d keys, want %d", len(got), len(want))
			}
		})
	}

	git := []struct {
		label   string
		content string
		present []string
		absent  []string
	}{
		{"git-mixed",
			"url = https://user-0012:pass-0012-abcdef@example.invalid/r.git\nextraheader = AUTHORIZATION: basic dmFsdWUtMDAxMw\nmerge = refs/heads/feature-long-name\nemail = person@example.invalid\n",
			[]string{"url = https://user-0012:pass-0012-abcdef@example.invalid/r.git", "user-0012:pass-0012-abcdef", "user-0012", "pass-0012-abcdef",
				"extraheader = AUTHORIZATION: basic dmFsdWUtMDAxMw", "AUTHORIZATION: basic dmFsdWUtMDAxMw",
				"merge = refs/heads/feature-long-name", "email = person@example.invalid"},
			[]string{"refs/heads/feature-long-name", "person@example.invalid"}},
		{"git-empty-quotes", `extraheader = ""   ordinary-0028-abcdef`, []string{"ordinary-0028-abcdef"}, nil},
		{"git-nul", "extraheader = ordinary-0035-abcdef\x00suffix", []string{"ordinary-0035-abcdef"}, nil},
		{"git-same-line-a", `[http "https://a.example.invalid/"] extraheader = "AUTH: basic placeholder-0036-abcdef"`, []string{"AUTH: basic placeholder-0036-abcdef"}, nil},
		{"git-same-line-b", `[http "https://b.example.invalid/?x=1"] ExtraHeader="AUTH: basic placeholder-0037-abcdef"`, []string{"AUTH: basic placeholder-0037-abcdef"}, nil},
		{"git-url-quoted-pass", `url = https://user-0022:"pass-0022-abcdef"@example.invalid/r.git`, []string{"user-0022:pass-0022-abcdef", "pass-0022-abcdef"}, nil},
		{"git-url-token-user", "url = https://tokn-0024-abcdef:x-oauth-basic@example.invalid/r.git", []string{"tokn-0024-abcdef", "x-oauth-basic", "tokn-0024-abcdef:x-oauth-basic"}, nil},
		{"git-url-percent", "url = https://user-0023:p%40ss-0023-abcdef@example.invalid/r.git", []string{"p%40ss-0023-abcdef", "p@ss-0023-abcdef", "user-0023"}, nil},
		{"git-url-two-at", "url = https://user-0027:p@ss-0027-abcdef@example.invalid/r.git", []string{"p@ss-0027-abcdef"}, nil},
		{"git-csi-in-name", "extraheader\x1b[=5h = \"AUTH: basic placeholder-0044-abcdef\"",
			[]string{"extraheader = \"AUTH: basic placeholder-0044-abcdef\""}, []string{"AUTH: basic placeholder-0044-abcdef"}},
		{"git-csi-in-pass", "url = https://user-0045:pa\x1b[1mss-0045-abcdef@example.invalid/r.git",
			[]string{"user-0045:pass-0045-abcdef", "pass-0045-abcdef"}, nil},
		// The password alone is the only key exactly 8 bytes long; the
		// 7-byte one on the next line must not be keyed.
		{"git-floor-pass", "url = https://u:pass-049@example.invalid/r.git\nurl = https://u:pass-50@example.invalid/r.git",
			[]string{"pass-049"}, []string{"pass-50"}},
		// The password is under the floor encoded (6 bytes) and decoded (4),
		// so only the decoded whole userinfo covers it once decoded.
		{"git-pct-userinfo", "url = https://user-0049:p%40ss@example.invalid/r.git",
			[]string{"user-0049:p@ss"}, nil},
		// A URL in a section header has no setting value: only the record
		// itself is read for userinfo.
		{"git-url-in-section", `[http "https://user-0051:pass-0051-abcdef@example.invalid/"]`,
			[]string{"pass-0051-abcdef"}, nil},
		// An escape git decodes: the value as written differs from the
		// value git prints, so both are keys.
		{"git-xh-escape", "extraheader = AUTH: basic\\tplaceholder-0052-abcdef",
			[]string{"AUTH: basic\\tplaceholder-0052-abcdef", "AUTH: basic\tplaceholder-0052-abcdef"}, nil},
		// Quoted spaces: the value git prints is 11 bytes, its trimmed form 7,
		// so only the untrimmed value is a key.
		{"git-xh-padded-short", `extraheader = "  ab-0054  "`, []string{"  ab-0054  "}, []string{"ab-0054"}},
		// Quoted spaces around a long value: the trimmed value is a key too.
		{"git-xh-padded", `extraheader = "  AUTH: basic placeholder-0053-abcdef  "`,
			[]string{"AUTH: basic placeholder-0053-abcdef"}, nil},
		// Two "@": the userinfo read to the first one is a key of its own.
		{"git-url-first-at", "url = https://user-0055:pw@ss-0055-abcdef@example.invalid/r.git",
			[]string{"user-0055:pw"}, nil},
		// Percent-encoded and short: the parts are under the floor, so only
		// the whole userinfo as written covers it.
		{"git-pct-userinfo-as-written", "url = https://u-56:p%40ss@example.invalid/r.git",
			[]string{"u-56:p%40ss"}, nil},
		// A URL after one with no userinfo in the same text: reading goes on
		// to the next "://".
		{"git-url-second-url", "url = https://example.invalid/x https://user-0057:pass-0057-abcdef@example.invalid/r.git",
			[]string{"pass-0057-abcdef"}, nil},
		// A percent-encoded user part: only its decoded form is this key.
		{"git-pct-user", "url = https://us%65r-0058-abcdef:x@example.invalid/r.git",
			[]string{"user-0058-abcdef"}, nil},
		// Two "@": the password part of the userinfo read to the first "@"
		// is a key of its own (the one read to the last "@" ends later).
		{"git-first-at-pass", "url = https://user-0059:pass-0059-abcdef@x@example.invalid/r.git",
			[]string{"pass-0059-abcdef"}, nil},
		// Two "@" and percent-encoding: the whole userinfo read to the last
		// "@", decoded.
		{"git-last-at-decoded", "url = https://u-60:p%40ss-60@x@example.invalid/r.git",
			[]string{"u-60:p@ss-60@x"}, nil},
		// Two "@" and percent-encoding: the whole userinfo read to the first
		// "@", decoded.
		{"git-first-at-decoded", "url = https://u-61:p%40s@x@example.invalid/r.git",
			[]string{"u-61:p@s"}, nil},
		// Two "@" and percent-encoding: the whole userinfo read to the last
		// "@", as written.
		{"git-last-at-as-written", "url = https://u-62:p%40s@x@example.invalid/r.git",
			[]string{"u-62:p%40s@x"}, nil},
		// A comment after the value: git prints the value without it.
		{"git-xh-comment", "extraheader = AUTH: basic placeholder-0095-abcdef ; note",
			[]string{"AUTH: basic placeholder-0095-abcdef"}, nil},
		{"git-xh-hash-comment", "extraheader = AUTH: basic placeholder-0099-abcdef # note",
			[]string{"AUTH: basic placeholder-0099-abcdef"}, nil},
		// Inside quotes ";" is part of the value, not a comment.
		{"git-xh-quoted-semicolon", `extraheader = "AUTH: basic;placeholder-0100-abcdef"`,
			[]string{"AUTH: basic;placeholder-0100-abcdef"}, nil},
		// The value git prints is 11 bytes and its trimmed form 7, so only
		// the printed value is a key: a tab before the quote, and spaces
		// after it, are dropped from what git prints.
		{"git-xh-tab-before-quote", "extraheader = \t\"  ab-0097  \"", []string{"  ab-0097  "}, nil},
		{"git-xh-padded-short-trailing", `extraheader = "  ab-0098  "  `, []string{"  ab-0098  "}, nil},
		// An escape as the last byte of the value: git prints the escaped
		// character, and the printed value is exactly 8 bytes.
		{"git-xh-escape-at-end", `extraheader = ab-0104\\`, []string{`ab-0104\`}, nil},
		{"git-xh-backspace", `extraheader = AUTH\bplaceholder-0105-abcdef`, []string{"AUTH\bplaceholder-0105-abcdef"}, nil},
		{"git-xh-escaped-quote", `extraheader = AUTH\"placeholder-0109-abcdef`, []string{`AUTH"placeholder-0109-abcdef`}, nil},
		// A tab between the setting name and "=" is skipped like a space.
		{"git-xh-tab-before-eq", "extraheader\t= AUTH: basic placeholder-0116-abcdef",
			[]string{"AUTH: basic placeholder-0116-abcdef"}, nil},
		// No space after "=": the value as written starts right after it.
		{"git-xh-escape-no-space", "extraheader=AUTH: basic\\tplaceholder-0117-abcdef",
			[]string{"AUTH: basic\\tplaceholder-0117-abcdef"}, nil},
		// A record that starts with "=": no setting name, only the line.
		{"git-leading-eq", "=value-0115-abcdef", []string{"=value-0115-abcdef"}, nil},
		// A "://" at the start of a text is read too.
		{"git-url-no-scheme", "://user-0118:pass-0118-abcdef@example.invalid/r.git", []string{"pass-0118-abcdef"}, nil},
		// An authority that ends the text, with no path.
		{"git-url-no-path", "url = https://user-0119:pass-0119-abcdef@example.invalid", []string{"pass-0119-abcdef"}, nil},
		// A userinfo with no ":" is a user part only.
		{"git-url-user-only", "url = https://tokn-0120-abcdef@example.invalid/r.git", []string{"tokn-0120-abcdef"}, nil},
		// An empty user part: the password is still a key on its own.
		{"git-url-empty-user", "url = https://:pass-0121-abcdef@example.invalid/r.git", []string{"pass-0121-abcdef"}, nil},
		// A "%" too close to the end to start an escape stays as written.
		{"git-pct-at-end", "url = https://user-0122:pass-0122-abcd%2@example.invalid/r.git", []string{"pass-0122-abcd%2"}, nil},
		// An escape as the last three bytes of a part is decoded.
		{"git-pct-last", "url = https://user-0123:pass-0123-abcde%66@example.invalid/r.git", []string{"pass-0123-abcdef"}, nil},
		// An escape whose first digit is 0 is decoded.
		{"git-pct-control", "url = https://user-0124:pass-0124%09abcdef@example.invalid/r.git", []string{"pass-0124\tabcdef"}, nil},
		// Hex digits at each end of both letter ranges decode.
		{"git-pct-hex-lower-a", "url = https://user-0128:pass-0128%4aabcdef@example.invalid/r.git", []string{"pass-0128Jabcdef"}, nil},
		{"git-pct-hex-lower-f", "url = https://user-0129:pass-0129%4fabcdef@example.invalid/r.git", []string{"pass-0129Oabcdef"}, nil},
		{"git-pct-hex-upper-a", "url = https://user-0130:pass-0130%4Aabcdef@example.invalid/r.git", []string{"pass-0130Jabcdef"}, nil},
		{"git-pct-hex-upper-f", "url = https://user-0131:pass-0131%4Fabcdef@example.invalid/r.git", []string{"pass-0131Oabcdef"}, nil},
		{"git-trailing-backslash", "extraheader = ordinary-0038-abcdef\\\n", []string{"ordinary-0038-abcdef"}, nil},
		// An escaped newline as the last or first byte of the value: git
		// prints the newline, and the trimmed printed value is the key a
		// record of git's output matches.
		{"git-xh-nl-end", `extraheader = AUTH: basic placeholder-0132-abcdef\n`, []string{"AUTH: basic placeholder-0132-abcdef"}, nil},
		{"git-xh-nl-start", `extraheader = \nAUTH: basic placeholder-0133-abcdef`, []string{"AUTH: basic placeholder-0133-abcdef"}, nil},
		// Two "@" in the authority and a third after its end (a path, a tab,
		// a space): the password git reads ends at the authority's last "@".
		{"git-url-auth-cut", "url = https://user-0134:pass-0134-abcdef@tail-0134-abcdef@example.invalid/a@b",
			[]string{"pass-0134-abcdef@tail-0134-abcdef"}, nil},
		{"git-url-auth-cut-tab", "url = https://user-0135:pass-0135-abcdef@tail-0135-abcdef@example.invalid\tx@b",
			[]string{"pass-0135-abcdef@tail-0135-abcdef"}, nil},
		{"git-url-auth-cut-space", "url = https://user-0136:pass-0136-abcdef@tail-0136-abcdef@example.invalid x@b",
			[]string{"pass-0136-abcdef@tail-0136-abcdef"}, nil},
		// A byte outside the setting-name class right before the name: the
		// name is still extraheader, so its value is a key.
		{"git-xh-name-after-tilde", "~extraheader = AUTH: basic placeholder-0137-abcdef", []string{"AUTH: basic placeholder-0137-abcdef"}, nil},
	}
	for _, tc := range git {
		t.Run(tc.label, func(t *testing.T) {
			keys, _, _ := credentialFileKeys([]byte(tc.content), true, 1<<14)
			set := map[string]bool{}
			for _, k := range keys {
				if len(k) < screenKeyFloor {
					t.Fatalf("key under floor")
				}
				set[string(k)] = true
			}
			if len(set) != len(keys) {
				t.Fatalf("duplicate keys: %d unique of %d", len(set), len(keys))
			}
			for i, p := range tc.present {
				if !set[p] {
					t.Errorf("expected key %d missing", i)
				}
			}
			for i, a := range tc.absent {
				if set[a] {
					t.Errorf("non-key %d present", i)
				}
			}
		})
	}

	t.Run("git-eq-cap", func(t *testing.T) {
		if _, over, crowded := credentialFileKeys([]byte(strings.Repeat("a=", 32)+"x"), true, 1<<14); over || crowded {
			t.Fatal("32 named = should set neither overLong nor crowded")
		}
		if _, over, crowded := credentialFileKeys([]byte(strings.Repeat("a=", 33)+"x"), true, 1<<14); over || !crowded {
			t.Fatal("33 named = should set crowded, not overLong")
		}
	})
	// Every byte at the edge of the setting-name class names an "=", so 33
	// of them reach the cap.
	t.Run("git-eq-cap-name-bytes", func(t *testing.T) {
		for _, b := range []string{"a", "z", "A", "Z", "0", "9", "-"} {
			if _, _, crowded := credentialFileKeys([]byte(strings.Repeat(b+"=", 33)+"x"), true, 1<<14); !crowded {
				t.Errorf("name byte %q: 33 named = should set crowded", b)
			}
		}
	})
	// A "%" followed by a pair that is not two hex digits stays as written,
	// beside a valid escape that is decoded. Each bad byte sits just outside
	// one edge of the digit classes.
	t.Run("git-pct-bad-digit", func(t *testing.T) {
		bad := []struct{ class, b string }{{"below 0", "-"}, {"between 9 and A", "<"}, {"between F and a", "G"}, {"above f", "z"}}
		for _, c := range bad {
			for _, pos := range []string{"first", "second"} {
				pair := c.b + "1"
				if pos == "second" {
					pair = "4" + c.b
				}
				content := "url = https://user-0138:p%" + pair + "ass%41-0138-abcdef@example.invalid/r.git"
				keys, _, _ := credentialFileKeys([]byte(content), true, 1<<14)
				if !slices.Contains(keyStrings(keys), "p%"+pair+"assA-0138-abcdef") {
					t.Errorf("bad digit %s in the %s position: decoded password missing", c.class, pos)
				}
			}
		}
	})
	t.Run("overlong-no-keys", func(t *testing.T) {
		keys, over, _ := credentialFileKeys([]byte("abcdefghijkl\nNAME=value-0001-abcdef"), false, 22)
		if over || len(keys) != 3 {
			t.Fatalf("control: over=%v keys=%d", over, len(keys))
		}
		keys, over, _ = credentialFileKeys([]byte("abcdefghijkl\nshort-ok-1"), false, 11)
		if !over || len(keys) != 1 {
			t.Fatalf("over=%v keys=%d, want over with only the other record's key", over, len(keys))
		}
	})
	t.Run("overlong", func(t *testing.T) {
		if _, over, _ := credentialFileKeys([]byte("abcdefghijk"), false, 10); !over {
			t.Fatal("keyCap+1 record should set overLong")
		}
		if _, over, _ := credentialFileKeys([]byte("abcdefghij"), false, 10); over {
			t.Fatal("keyCap record should not set overLong")
		}
	})
}

func TestGitConfigValue(t *testing.T) {
	rows := []struct {
		label string
		in    string
		want  string
		isNil bool
	}{
		{"quoted", `"AUTHORIZATION: basic placeholder-0015-abcdef"`, "AUTHORIZATION: basic placeholder-0015-abcdef", false},
		{"trailing-comment", `AUTHORIZATION: basic placeholder-0016-abcdef # trailing note`, "AUTHORIZATION: basic placeholder-0016-abcdef", false},
		{"escaped-quotes", `"AUTH: say \"hi\" placeholder-0017"`, `AUTH: say "hi" placeholder-0017`, false},
		{"middle-quoted", `label-"middle"-tail-0018`, "label-middle-tail-0018", false},
		{"quote-hides-hash", `pre-0019 "q # not a comment" post ; trailing`, "pre-0019 q # not a comment post", false},
		{"adjacent-quote", `"ab"cd-0020-efgh`, "abcd-0020-efgh", false},
		{"tab-escape", `tab\there-0021   spaced   `, "tab\there-0021   spaced", false},
		{"unterminated", `"unterminated-0025-abcdef`, "", true},
		{"bad-escape", `bad\qescape-0026-abcdef`, "", true},
		{"leading-empty-quotes", `""   ordinary-0028-abcdef`, "ordinary-0028-abcdef", false},
		{"trailing-empty-quotes", `ordinary-0029-abcdef   ""`, "ordinary-0029-abcdef   ", false},
		{"trailing-empty-quotes-comment", `ordinary-0030-abcdef   "" # note`, "ordinary-0030-abcdef   ", false},
		{"spaced-quote", `a-0031   "b"   c-0031   `, "a-0031   b   c-0031", false},
		{"nul", "ordinary\x00suffix-0032", "ordinary", false},
		{"vt", "vt\x0bmid-0033\x0b", "vt\x0bmid-0033\x0b", false},
		{"ff", "ff\x0cmid-0034\x0c", "ff\x0cmid-0034\x0c", false},
		{"trailing-backslash", `ordinary-0038-abcdef\`, "ordinary-0038-abcdef", false},
	}
	for _, tc := range rows {
		t.Run(tc.label, func(t *testing.T) {
			got := gitConfigValue([]byte(tc.in))
			if tc.isNil {
				if got != nil {
					t.Fatalf("want nil, got %d bytes", len(got))
				}
				return
			}
			if got == nil || !bytes.Equal(got, []byte(tc.want)) {
				t.Fatalf("value differs: got %d bytes (nil=%v), want %d bytes", len(got), got == nil, len(tc.want))
			}
		})
	}
}
