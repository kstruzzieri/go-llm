package sqlitedsn

import (
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
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if path == self || strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || isFile(filepath.Join(path, "go.mod")) {
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
		for _, h := range sqliteSetupHits(src) {
			hits = append(hits, filepath.ToSlash(rel)+":"+h)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Errorf("use sqlitedsn.EnableWAL and sqlitedsn.FileURL instead of:\n%s", strings.Join(hits, "\n"))
	}
}

var (
	// A WAL switch statement anywhere in a literal, ended by ';' or the end of
	// the literal. That ending lets error prefixes such as
	// "PRAGMA journal_mode=WAL: %w" pass.
	walStatement = regexp.MustCompile(`(?i)\bpragma\s+journal_mode\s*=\s*'?wal'?\s*(;|$)`)
	// The DSN form, _pragma=journal_mode(WAL).
	walDSNPragma = regexp.MustCompile(`(?i)journal_mode\s*\(\s*'?wal'?\s*\)`)
)

// sqliteSetupHits reports "line: what" for each WAL switch literal and each
// Scheme: "file" composite field in src.
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
