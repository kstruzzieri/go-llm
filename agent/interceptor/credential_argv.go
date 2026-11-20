package interceptor

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
)

// CredentialArgv blocks an exec argv that names a credential path as a word,
// or, for an inline shell script, anywhere in the script's fields (#777). A
// value that is not a string array is not a violation.
type CredentialArgv struct{}

func (CredentialArgv) check(raw json.RawMessage) (string, bool) {
	var argv []string
	if err := json.Unmarshal(raw, &argv); err != nil {
		return "", false
	}
	for _, w := range argv {
		if wordNamesCredentialPath(w) {
			return credentialArgvDetail(w), true
		}
	}
	rest, _, status := peelWrappers(argv)
	if status != peelOK {
		return "", false
	}
	if _, _, script, form := inlineShellScript(rest); form == shellFormInline {
		for _, f := range scriptFields(script) {
			if namesCredentialPath(f) {
				return credentialArgvDetail(f), true
			}
		}
	}
	return "", false
}

func credentialArgvDetail(w string) string {
	return "argv word " + strconv.Quote(w) + " names a credential path"
}

// isExclusionPathspec reports a git pathspec that excludes: ":!x", ":^x" or
// ":(...exclude...)x".
func isExclusionPathspec(w string) bool {
	if strings.HasPrefix(w, ":!") || strings.HasPrefix(w, ":^") {
		return true
	}
	rest, ok := strings.CutPrefix(w, ":(")
	if !ok {
		return false
	}
	magic, _, _ := strings.Cut(rest, ")")
	for _, m := range strings.Split(magic, ",") {
		if m == "exclude" {
			return true
		}
	}
	return false
}

// namesCredentialPath reports whether w, or the text after any "=", ":" or
// "@" in it, or the pattern after the magic of a long-form git pathspec
// (":(top).env"), or the text after a short-flag prefix, is a credential path.
func namesCredentialPath(w string) bool {
	if IsCredentialPath(w) {
		return true
	}
	for i := 0; i < len(w); i++ {
		if (w[i] == '=' || w[i] == ':' || w[i] == '@') && IsCredentialPath(w[i+1:]) {
			return true
		}
	}
	// A git pathspec with long-form magic, ":(top).env": the pattern follows
	// the closing parenthesis. Exclusions never reach here as whole words.
	if rest, ok := strings.CutPrefix(w, ":("); ok {
		if _, pat, ok := strings.Cut(rest, ")"); ok && IsCredentialPath(pat) {
			return true
		}
	}
	if len(w) > 2 && w[0] == '-' && (w[1]|0x20 >= 'a' && w[1]|0x20 <= 'z') {
		return IsCredentialPath(w[2:])
	}
	return false
}

// wordNamesCredentialPath is namesCredentialPath after the exclusion-pathspec
// exemption, which applies to whole argv words only.
func wordNamesCredentialPath(w string) bool {
	return !isExclusionPathspec(w) && namesCredentialPath(w)
}

// scriptFields cuts a shell script on whitespace and shell metacharacters.
// It deliberately does not parse words: every fragment is a candidate.
func scriptFields(script string) []string {
	return strings.FieldsFunc(script, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(";|&()<>\"'`$\\{}", r)
	})
}
