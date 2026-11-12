package interceptor

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// oracleIsCredentialPath and oracleIsGitConfigPath are a verbatim copy of the
// pre-#789 bodies of IsCredentialPath and IsGitConfigPath. They are the
// reference the in-place, cheaper matchers must agree with on every input.
// Do not share helpers with production or simplify them.
func oracleIsCredentialPath(p string) bool {
	parts := strings.Split(normalizePath(p), "/")
	base := parts[len(parts)-1]
	for _, c := range parts {
		if slices.Contains(credentialDirs, c) {
			return true
		}
	}
	switch {
	case slices.Contains(credentialFiles, base):
		return true
	case strings.HasPrefix(base, ".env.") && !slices.Contains(envTemplates, base):
		return true
	}
	return oracleIsGitConfigPath(p)
}

func oracleIsGitConfigPath(p string) bool {
	parts := strings.Split(normalizePath(p), "/")
	return parts[len(parts)-1] == "config" && slices.Contains(parts[:len(parts)-1], ".git")
}

// oracleCorpusComponents are the building blocks of the generated corpus.
var oracleCorpusComponents = []string{".git", ".GIT", "config", "CONFIG", ".env", ".env.example", ".env.local", ".ENV.Example",
	".ssh", ".\u017Fsh", ".aws", "a", "..", ".", "", "x\u200dy", "\u200c..", ".git\u200c", ".npmrc", "_netrc",
	".kube", ".git-credentials", "conf\uFB01g"}

// oracleCorpus calls check with every path of the generated corpus.
func oracleCorpus(check func(p string)) {
	comps := oracleCorpusComponents
	var rec func(prefix []string, depth int)
	rec = func(prefix []string, depth int) {
		if len(prefix) > 0 {
			for _, sep := range []string{"/", "//"} {
				j := strings.Join(prefix, sep)
				for _, lead := range []string{"", "/", "./"} {
					for _, trail := range []string{"", "/"} {
						check(lead + j + trail)
					}
				}
			}
		}
		if depth == 0 {
			return
		}
		for _, c := range comps {
			rec(append(prefix[:len(prefix):len(prefix)], c), depth-1)
		}
	}
	rec(nil, 4)
}

// oracleCorpusShort calls check with every one- and two-component path of the
// generated corpus (552 prefixes times 12 spellings).
func oracleCorpusShort(check func(p string)) {
	comps := oracleCorpusComponents
	emit := func(prefix ...string) {
		for _, sep := range []string{"/", "//"} {
			j := strings.Join(prefix, sep)
			for _, lead := range []string{"", "/", "./"} {
				for _, trail := range []string{"", "/"} {
					check(lead + j + trail)
				}
			}
		}
	}
	for _, a := range comps {
		emit(a)
		for _, b := range comps {
			emit(a, b)
		}
	}
}

// oracleExistingTables is group (a): every path the package's own tests pass
// to IsCredentialPath or IsGitConfigPath, read from the variables those tests
// range over so the two cannot drift.
func oracleExistingTables() []string {
	var out []string
	for _, r := range gitConfigPathRows {
		out = append(out, r.in)
	}
	out = append(out, credentialMatrixBlocked...)
	out = append(out, credentialMatrixAllowed...)
	for _, s := range ancestorScopes {
		if IsProtectedPath(s) {
			continue
		}
		for _, c := range ancestorChildren {
			out = append(out, s+"/"+c, c)
		}
	}
	return out
}

// oracleLiteralRows is group (c).
func oracleLiteralRows() []string {
	return []string{"", "/", ".", "..", ".env/", ".ssh/../plain", ".git/\u200c../config", "a/.git/x/../config",
		"A/.Git/Config", ".env.EXAMPLE", "sub/.Env.Local", "a\\.env", ".ssh\\id", "\x80/.env", ".env\xff",
		"\xc3\x28/.git/config", ".ssh./id", ".netrc:x", ".env::$DATA", "c:/work/.env", ".kube. /x"}
}

// oracleAliasRows is group (d): every key of pathAliases, written out, placed
// three ways.
func oracleAliasRows() []string {
	keys := []string{"\u017F", "\u212A", "\u00DF", "\u1E9E", "\uFB00", "\uFB01", "\uFB02", "\uFB03", "\uFB04",
		"\uFB05", "\uFB06", "\u200C", "\u200D", "\u200E", "\u200F", "\u202A", "\u202B", "\u202C", "\u202D",
		"\u202E", "\u206A", "\u206B", "\u206C", "\u206D", "\u206E", "\u206F", "\uFEFF"}
	var out []string
	for _, k := range keys {
		out = append(out, ".e"+k+"nv", k+".ssh/id", ".git/"+k+"config")
	}
	return out
}

// oracleAgrees reports the four booleans and whether the two pairs agree.
func oracleAgrees(p string) (cred, credOld, git, gitOld, ok bool) {
	cred, credOld = IsCredentialPath(p), oracleIsCredentialPath(p)
	git, gitOld = IsGitConfigPath(p), oracleIsGitConfigPath(p)
	return cred, credOld, git, gitOld, cred == credOld && git == gitOld
}

func TestCredentialPathMatchesOracle(t *testing.T) {
	// containment: CMP-EXEC-CREDENTIAL-OUTPUT
	groups := []struct {
		name  string
		paths []string
	}{
		{"a", oracleExistingTables()},
		{"c", oracleLiteralRows()},
		{"d", oracleAliasRows()},
	}
	// Report the first oracleMaxReports mismatches, then stop: a broken
	// matcher disagrees on a large share of 3.5 million paths.
	const oracleMaxReports = 20
	mismatches := 0
	report := func(group string, i int, cred, credOld, git, gitOld bool) {
		mismatches++
		t.Errorf("group %s index %d: IsCredentialPath=%v oracle=%v IsGitConfigPath=%v oracle=%v", group, i, cred, credOld, git, gitOld)
		if mismatches >= oracleMaxReports {
			t.Fatalf("stopped after %d mismatches (cap reached; more may follow)", mismatches)
		}
	}
	// (a) existing tables first, then (b), (c), (d) in the brief's order.
	run := func(group string, i int, p string) {
		if c, co, g, goOld, ok := oracleAgrees(p); !ok {
			report(group, i, c, co, g, goOld)
		}
	}
	for i, p := range groups[0].paths {
		run("a", i, p)
	}
	calls := 0
	oracleCorpus(func(p string) {
		if c, co, g, goOld, ok := oracleAgrees(p); !ok {
			report("b", calls, c, co, g, goOld)
		}
		calls++
	})
	if calls != 3510720 {
		t.Fatalf("group b made %d calls, want 3510720", calls)
	}
	for _, g := range groups[1:] {
		for i, p := range g.paths {
			run(g.name, i, p)
		}
	}
	if n := len(oracleAliasRows()); n != 81 {
		t.Fatalf("group d has %d rows, want 81", n)
	}
}

func FuzzCredentialPathMatchesOracle(f *testing.F) {
	seeds := 0
	add := func(p string) { f.Add(p); seeds++ }
	for _, p := range oracleExistingTables() {
		add(p)
	}
	for _, p := range oracleLiteralRows() {
		add(p)
	}
	for _, p := range oracleAliasRows() {
		add(p)
	}
	n := 0
	oracleCorpusShort(func(p string) { add(p); n++ })
	if n != 6624 {
		f.Fatalf("short corpus has %d seeds, want 6624", n)
	}
	if seeds != 57363 {
		f.Fatalf("fuzz seeded %d paths, want 57363", seeds)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if c, co, g, goOld, ok := oracleAgrees(p); !ok {
			t.Fatalf("IsCredentialPath=%v oracle=%v IsGitConfigPath=%v oracle=%v", c, co, g, goOld)
		}
	})
}

// BenchmarkIsCredentialPath measures the matcher on plain ASCII workspace
// paths of three lengths.
func BenchmarkIsCredentialPath(b *testing.B) {
	for _, c := range []struct{ name, path string }{
		{"len20", "a003/b12/c07/f041.go"},
		{"len51", ".claude/worktrees/wt-03/src/m01/pkg/p11/r07/f011.go"},
		{"len60", ".claude/worktrees/wt-00/src/m00/pkg/p00/internal/q00/f000.go"},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if IsCredentialPath(c.path) {
					b.Fatal(fmt.Sprint("unexpected match for ", c.name))
				}
			}
		})
	}
}
