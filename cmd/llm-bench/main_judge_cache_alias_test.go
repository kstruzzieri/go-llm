package main

import (
	"bytes"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
)

func TestMainRejectsReportJudgeCacheURIAlias(t *testing.T) {
	for _, fixture := range []string{"existing", "future", "future-ro", "future-rw", "relative", "fragment", "hardlink", "symlink-dotdot"} {
		t.Run(fixture, func(t *testing.T) {
			if fixture == "symlink-dotdot" && runtime.GOOS == "windows" {
				t.Skip("SQLite resolves Windows paths lexically")
			}
			dir := t.TempDir()
			report := filepath.Join(dir, "judge %#.db")
			cache := report
			if fixture == "symlink-dotdot" {
				_, report, cache = makeSymlinkDotDotPath(t, "judge.db")
			}
			var before []byte
			if !strings.HasPrefix(fixture, "future") {
				c, err := openJudgeCache(report)
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				before, err = os.ReadFile(report)
				if err != nil {
					t.Fatal(err)
				}
			}
			if fixture == "hardlink" {
				cache = filepath.Join(dir, "cache-link.db")
				if err := os.Link(report, cache); err != nil {
					t.Skipf("hardlinks unsupported: %v", err)
				}
			}
			u, err := sqlitedsn.FileURLPreservingPath(cache)
			if err != nil {
				t.Fatal(err)
			}
			if fixture == "relative" {
				u.Path, u.Opaque = "", url.PathEscape(filepath.Base(cache))
			}
			if fixture == "fragment" {
				u.Fragment = "ignored"
			}
			if strings.HasPrefix(fixture, "future-") {
				u.RawQuery = "mode=" + strings.TrimPrefix(fixture, "future-")
			}
			trace := filepath.Join(dir, "invalid-trace.json")
			if err := os.WriteFile(trace, []byte("invalid trace; guard must run first"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-traces", trace, "-models", "m", "-judge-cache", u.String(), "-report", report)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "-judge-cache") || !strings.Contains(string(out), "same file") && !strings.Contains(string(out), "must differ") {
				t.Fatalf("report/cache alias was not refused before loading traces: %v\n%s", err, out)
			}
			if strings.HasPrefix(fixture, "future") {
				if _, err := os.Stat(report); !os.IsNotExist(err) {
					t.Fatalf("alias rejection created a database: %v", err)
				}
			} else {
				after, err := os.ReadFile(report)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("alias rejection modified the database: %v", err)
				}
			}
		})
	}
}

func TestMainAllowsDistinctReportWithMissingJudgeCacheReadModes(t *testing.T) {
	for _, mode := range []string{"ro", "rw"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			cache := filepath.Join(dir, "missing", "cache.db")
			u, err := sqlitedsn.FileURLPreservingPath(cache)
			if err != nil {
				t.Fatal(err)
			}
			u.RawQuery = "mode=" + mode
			trace := filepath.Join(dir, "invalid-trace.json")
			if err := os.WriteFile(trace, []byte("invalid trace"), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-traces", trace, "-models", "m", "-judge-cache", u.String(), "-report", filepath.Join(dir, "report.json"))
			cmd.Env = append(os.Environ(), "LLM_BENCH_TEST_MAIN=1")
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "load traces:") {
				t.Fatalf("distinct report should pass alias checks and reach trace loading: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Dir(cache)); !os.IsNotExist(err) {
				t.Fatalf("alias check created the missing cache directory: %v", err)
			}
		})
	}
}
