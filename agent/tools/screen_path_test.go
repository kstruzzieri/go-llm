package tools

import (
	"bytes"
	"testing"
)

// Rows hold the bytes as they appear in output; labels name the row because
// an input can itself contain text this feature withholds.

func TestStripCSI(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	cases := []struct{ label, in, want string }{
		{"bold", "\x1b[1mdiff\x1b[m", "diff"},
		{"color then erase", "a\x1b[01;31m\x1b[Kb", "ab"},
		{"truncated sequence", "ab\x1b[01;3", "ab\x1b[01;3"},
		{"binary bytes", "\x00\xff", "\x00\xff"},
		{"private param", "\x1b[?25lA", "A"},
		{"space intermediate", "\x1b[ qA", "A"},
		{"slash intermediate", "\x1b[/xA", "A"},
		{"hash intermediate", "\x1b[#xA", "A"},
		{"at final", "\x1b[@A", "A"},
		{"tilde final", "\x1b[3~A", "A"},
		{"escape not csi", "\x1bX/.envA", "\x1bX/.envA"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			if got := stripCSI([]byte(c.in)); string(got) != c.want {
				t.Errorf("row %q: stripCSI mismatch", c.label)
			}
		})
	}
}

func TestGitUnquote(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	cases := []struct {
		label, in, want string
		n               int
		ok              bool
	}{
		{"escaped quotes", `"a/my \"dir\"/.env" rest`, `a/my "dir"/.env`, 19, true},
		{"quote then space", `"x\" /.env"`, `x" /.env`, 11, true},
		{"octal zero width joiner", `"b/my dir/.s\342\200\215sh/key"`, "b/my dir/.s\u200dsh/key", 31, true},
		{"tab escape", `"a\tb"`, "a\tb", 6, true},
		{"bell escape", `"\a"`, "\a", 4, true},
		{"backspace escape", `"\b"`, "\b", 4, true},
		{"form feed escape", `"\f"`, "\f", 4, true},
		{"newline escape", `"\n"`, "\n", 4, true},
		{"carriage return escape", `"\r"`, "\r", 4, true},
		{"vertical tab escape", `"\v"`, "\v", 4, true},
		{"backslash escape", `"\\"`, "\\", 4, true},
		{"low octal", `"\001"`, "\x01", 6, true},
		{"octal second 7", `"\370"`, "\xf8", 6, true},
		{"octal third 7", `"\307"`, "\xc7", 6, true},
		{"literal u escape", `"caf\u00e9/.env"`, "", 0, false},
		{"no close", `"no close`, "", 0, false},
		{"bad escape", `"bad \8"`, "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			got, n, ok := gitUnquote([]byte(c.in))
			if ok != c.ok {
				t.Fatalf("row %q: ok = %v, want %v", c.label, ok, c.ok)
			}
			if !ok {
				return
			}
			if string(got) != c.want || n != c.n {
				t.Errorf("row %q: unquote mismatch (n=%d, want %d)", c.label, n, c.n)
			}
		})
	}
}

func TestCredentialPatchHeader(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	colored := string(stripCSI([]byte("\x1b[1mdiff --git a/.env b/.env\x1b[m")))
	cases := []struct {
		label, in string
		want      bool
	}{
		{"git header", "diff --git a/.env b/.env", true},
		{"graph prefix", "| | diff --git a/.env b/.env", true},
		{"no prefix header", "diff --git .env .env", true},
		{"custom prefixes", "diff --git x/.env y/.env", true},
		{"combined", "diff --cc .env", true},
		{"deep graph", ">> diff --git a/.env b/.env", true},
		{"plus header with date", "+++ .env\t2026-11-09 19:19:41", true},
		{"recursive diff", "diff -r dA/.env dB/.env", true},
		{"quoted octal", `diff --git "a/.s\342\200\215sh/key" "b/.s\342\200\215sh/key"`, true},
		{"quoted escaped quotes", `diff --git "a/my \"dir\"/.env" "b/my \"dir\"/.env"`, true},
		{"quoted space and octal", `diff --git "a/my dir/.s\342\200\215sh/key" "b/my dir/.s\342\200\215sh/key"`, true},
		{"quoted quote space", `diff --git "x\" /.env" "y\" /.env"`, true},
		{"literal u escape", `+++ "caf\u00e9/.env"`, true},
		{"unclosed quote", `+++ my "dir/.env`, true},
		{"quote before marker", `diff - "diff --git a/.env b/.env`, true},
		{"marker then marker", "+++ .env diff --git a/x b/x", true},
		{"trace line cost", "+ git diff --stat .env", true},
		{"name with spaces", "diff --git a/notes .env draft b/notes .env draft", true},
		{"test failure quoting header", `y_test.go:8: screen("diff --git a/.env b/.env") cut = false`, true},
		{"colored header", colored, true},
		{"marker in quoted run", `diff -q "diff --git a/.env b/x"`, true},
		{"lone open quote", `+++ ".env`, true},
		{"after quoted run", `+++ "a/b" .env`, true},

		{"plain files", "diff --git a/x b/x", false},
		{"key basename", "diff --git a/key b/key", false},
		{"upper case prefix", "diff --git X.env Y.env", false},
		{"new file", "+++ new", false},
		{"minus header with date", "--- .env\t2026-11-09 19:19:41", false},
		{"minus header", "--- a/.env", false},
		{"context header", "*** /dev/null\tTue Nov 10 05:10:48 2026", false},
		{"go fail line", "    --- FAIL: TestNames/.env (0.00s)", false},
		{"go pass line", `--- PASS: TestSplitShellWords/curl_"x (0.00s)`, false},
		{"go fail header name", "--- FAIL: TestHeader/diff_--git_a/.env_b/.env (0.00s)", false},
		{"trace unclosed quote", `+ diff -u "a b`, false},
		{"template", "diff --git a/.env.example b/.env.example", false},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			if got := credentialPatchHeader([]byte(c.in)); got != c.want {
				t.Errorf("row %q: got %v, want %v", c.label, got, c.want)
			}
		})
	}
}

func TestCredentialPathPrefix(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	colored := stripCSI([]byte("\x1b[35mHEAD~1:.env\x1b[m\x1b[36m:\x1b[m\x1b[32m1\x1b[m\x1b[36m:\x1b[m\x1b[1;31mNAME\x1b[m=x"))
	cases := []struct {
		label string
		in    []byte
		want  bool
	}{
		{"grep line", []byte(".env:3:NAME=x"), true},
		{"dir with space", []byte("./my dir/.env:1:x"), true},
		{"git grep rev", []byte("HEAD~1:.env:1:NAME=x"), true},
		{"double colon", []byte("HEAD~1::.env:1:NAME=x"), true},
		{"colon in dir", []byte("HEAD~1:my dir:.env:1:NAME=x"), true},
		{"many fields", []byte("a:b:c:d:.env:1:x"), true},
		{"colored", colored, true},
		{"error message", []byte("Error: cannot load /workspace/.env: ENOENT"), true},
		{"test message", []byte(`x_test.go:12: IsCredentialPath("/home/u/.ssh/id_rsa"): got false`), true},
		{"test header with rev", []byte("    --- FAIL: TestHeader/HEAD~1:.env:1:NAME (0.00s)"), true},

		{"bare name", []byte(".env"), false},
		{"rev and name", []byte("HEAD~1:.env"), false},
		{"open error", []byte("open /app/.env"), false},
		{"go fail line", []byte("    --- FAIL: TestNames/.env (0.00s)"), false},
		{"compiler error", []byte("main.go:12:5: undefined: x"), false},
		{"template", []byte(".env.example:1:NAME="), false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			if got := credentialPathPrefix(c.in); got != c.want {
				t.Errorf("row %q: got %v, want %v", c.label, got, c.want)
			}
		})
	}
	_ = bytes.MinRead
}
