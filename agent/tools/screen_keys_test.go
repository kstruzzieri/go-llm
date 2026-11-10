package tools

import (
	"bytes"
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
		{"no-eq", "machine example.invalid password value-0010-abcdef", []string{"machine example.invalid password value-0010-abcdef"}},
		{"pem", "-----BEGIN PRIVATE KEY-----\npem-body-0013-abcdef\nshort\n-----END PRIVATE KEY-----\n", []string{"-----BEGIN PRIVATE KEY-----", "pem-body-0013-abcdef", "-----END PRIVATE KEY-----"}},
		{"csi-inside", "NAME=va\x1b[1mlue-0011-abcdef", []string{"NAME=value-0011-abcdef", "value-0011-abcdef"}},
		{"cr-splits-records", "NAME=abcd\rwxyz", []string{"NAME=abcd"}},
		{"colon-no-eq", "password: value-0014-abcdef", []string{"password: value-0014-abcdef"}},
		{"empty", "", nil},
		{"csi-in-eq-run", "NAME\x1b[=1m=value-0042-abcdef", []string{"NAME=value-0042-abcdef", "1m=value-0042-abcdef"}},
		{"csi-before-semicolon", "NAME=value-0043-abcdef \x1b[1m; note", []string{"NAME=value-0043-abcdef ; note", "value-0043-abcdef ; note"}},
	}
	for _, tc := range exact {
		t.Run(tc.label, func(t *testing.T) {
			keys, over := credentialFileKeys([]byte(tc.content), false, 1<<14)
			if over {
				t.Fatalf("overLong set")
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
		{"git-trailing-backslash", "extraheader = ordinary-0038-abcdef\\\n", []string{"ordinary-0038-abcdef"}, nil},
	}
	for _, tc := range git {
		t.Run(tc.label, func(t *testing.T) {
			keys, _ := credentialFileKeys([]byte(tc.content), true, 1<<14)
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
		if _, over := credentialFileKeys([]byte(strings.Repeat("a=", 32)+"x"), true, 1<<14); over {
			t.Fatal("32 named = should not set overLong")
		}
		if _, over := credentialFileKeys([]byte(strings.Repeat("a=", 33)+"x"), true, 1<<14); !over {
			t.Fatal("33 named = should set overLong")
		}
	})
	t.Run("overlong-no-keys", func(t *testing.T) {
		keys, over := credentialFileKeys([]byte("abcdefghijkl\nNAME=value-0001-abcdef"), false, 22)
		if over || len(keys) != 3 {
			t.Fatalf("control: over=%v keys=%d", over, len(keys))
		}
		keys, over = credentialFileKeys([]byte("abcdefghijkl\nshort-ok-1"), false, 11)
		if !over || len(keys) != 1 {
			t.Fatalf("over=%v keys=%d, want over with only the other record's key", over, len(keys))
		}
	})
	t.Run("overlong", func(t *testing.T) {
		if _, over := credentialFileKeys([]byte("abcdefghijk"), false, 10); !over {
			t.Fatal("keyCap+1 record should set overLong")
		}
		if _, over := credentialFileKeys([]byte("abcdefghij"), false, 10); over {
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
