package sqlitedsn

import (
	"bytes"
	"fmt"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Store openers must switch to WAL through EnableWAL and build file: URLs
// through FileURL. EnableWAL retries the switch's SQLITE_BUSY, which
// busy_timeout cannot; FileURL renders Windows paths SQLite accepts and
// rejects UNC paths. A raw switch, a hand-built file: URL, or a "file:" URI
// literal elsewhere reintroduces the bugs of #619, so this test fails on any
// of them in a non-test Go file outside this package. It reads string
// literals and tokens, not comments, and cannot see a switch or URI assembled
// at run time or spelled another way. AssertWAL in the per-opener
// cross-process tests still catches an opener that never switches; one that
// switches without retrying is caught there only statistically.
func TestStoreOpenersUseSharedHelpers(t *testing.T) {
	root := moduleRoot(t)
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	var hits []string
	scanned := map[string]bool{} // files calling sqlitedsn.EnableWAL, by root-relative path
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if path == self || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "docs" || name == "vendor" || isFile(filepath.Join(path, "go.mod")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if bytes.Contains(src, []byte("sqlitedsn.EnableWAL(")) {
			scanned[rel] = true
		}
		for _, h := range sqliteSetupHits(src) {
			hits = append(hits, rel+":"+h)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Errorf("raw SQLite setup outside internal/sqlitedsn (#619): switch to WAL with sqlitedsn.EnableWAL (a raw switch fails at once with SQLITE_BUSY when openers race), and build DSNs with sqlitedsn.WithBusyTimeout or sqlitedsn.FileURL (hand-built file: URLs break on Windows):\n%s", strings.Join(hits, "\n"))
	}
	// The walk scans nothing if a skip rule swallows the root or an opener's
	// directory, and then the guard passes with a raw switch planted anywhere.
	// Pinning the known openers makes losing one as loud as adding one. A new
	// opener that calls EnableWAL needs no entry.
	for _, f := range []string{
		"cmd/golem/feedback.go",
		"cmd/golem/session.go",
		"mcp/server.go",
		"memory/open.go",
		"provider/routing_feedback_sqlite.go",
		"rag/sqlite_store.go",
		"transcript/store.go",
	} {
		if !scanned[f] {
			t.Errorf("store opener %s was not scanned or no longer calls sqlitedsn.EnableWAL; if it moved, update this census deliberately", f)
		}
	}
}

var (
	// A WAL switch anywhere in a literal (statement, schema-qualified, or a DSN
	// _pragma value), ended by ';', '&' or the end of the literal. That ending
	// lets error prefixes such as "PRAGMA journal_mode=WAL: %w" pass.
	walStatement = regexp.MustCompile(`(?i)\bjournal_mode\s*=\s*'?wal'?\s*(;|&|$)`)
	// The DSN form, _pragma=journal_mode(WAL).
	walDSNPragma = regexp.MustCompile(`(?i)journal_mode\s*\(\s*'?wal'?\s*\)`)
)

// sqliteSetupHits reports "line: what" for each WAL switch literal, each
// Scheme: "file" composite field, and each literal starting with file: in src.
func sqliteSetupHits(src []byte) []string {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0) // mode 0: comments are skipped
	var hits []string
	var prev, prev2 token.Token
	var prevLit string
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return hits
		}
		if tok == token.STRING {
			if v, err := strconv.Unquote(lit); err == nil {
				if walStatement.MatchString(strings.TrimSpace(v)) || walDSNPragma.MatchString(v) {
					hits = append(hits, fmt.Sprintf("%d: WAL switch %s", file.Line(pos), lit))
				}
				if v == "file" && prev == token.COLON && prev2 == token.IDENT && prevLit == "Scheme" {
					hits = append(hits, fmt.Sprintf("%d: hand-built file URL", file.Line(pos)))
				}
				if strings.HasPrefix(v, "file:") {
					hits = append(hits, fmt.Sprintf("%d: file: URI literal %s", file.Line(pos), lit))
				}
			}
		}
		if tok == token.IDENT {
			prevLit = lit
		} else if tok != token.COLON {
			prevLit = ""
		}
		prev2, prev = prev, tok
	}
}

func TestSQLiteSetupHits(t *testing.T) {
	src := []byte(`package p

// PRAGMA journal_mode=WAL in a comment is fine.
var a = db.Exec("PRAGMA journal_mode=WAL")
var b = fmt.Errorf("transcript: PRAGMA journal_mode=WAL: %w", err)
var c = fmt.Errorf("PRAGMA journal_mode=WAL: %w", err)
var d = "pragma journal_mode = 'wal'; PRAGMA foreign_keys=ON"
var e = "file:x.db?_pragma=journal_mode(WAL)"
var f = url.URL{Scheme: "file", Path: p}
var g = url.URL{Scheme: "https"}
var h = map[string]string{"Scheme": "file"}
var i = "PRAGMA journal_mode"
var j = "PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL"
var k = strings.HasPrefix(p, "file:")
var l = "a file: b"
var m = q.Add("_pragma", "journal_mode=WAL")
var n = "PRAGMA main.journal_mode=WAL"
var o = "x.db?_pragma=busy_timeout(5000)&_pragma=journal_mode=WAL&mode=rwc"
`)
	got := strings.Join(sqliteSetupHits(src), "\n")
	want := strings.Join([]string{
		`4: WAL switch "PRAGMA journal_mode=WAL"`,
		`7: WAL switch "pragma journal_mode = 'wal'; PRAGMA foreign_keys=ON"`,
		`8: WAL switch "file:x.db?_pragma=journal_mode(WAL)"`,
		`8: file: URI literal "file:x.db?_pragma=journal_mode(WAL)"`,
		`9: hand-built file URL`,
		`13: WAL switch "PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL"`,
		`14: file: URI literal "file:"`,
		`16: WAL switch "journal_mode=WAL"`,
		`17: WAL switch "PRAGMA main.journal_mode=WAL"`,
		`18: WAL switch "x.db?_pragma=busy_timeout(5000)&_pragma=journal_mode=WAL&mode=rwc"`,
	}, "\n")
	if got != want {
		t.Errorf("hits:\n%s\nwant:\n%s", got, want)
	}
}

// moduleRoot walks up from the package directory to the directory holding
// go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for !isFile(filepath.Join(dir, "go.mod")) {
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the package directory")
		}
		dir = parent
	}
	return dir
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
