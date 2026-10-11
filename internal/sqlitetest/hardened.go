package sqlitetest

import (
	"context"
	"database/sql"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
)

// HardenedPathsOptions configures RunHardenedPaths.
type HardenedPathsOptions struct {
	// URI renders a file: URI for a filename as written, without cleaning, so
	// "file:" literals stay in the caller's _test.go (the sqlitedsn guard
	// rejects them in ordinary .go files). Typically
	// func(name string) string { return "file:" + name }.
	URI func(name string) string
	// MemoryOK says whether the opener accepts an input that names no file
	// (a mode=memory URI). When false the open must fail and create nothing.
	MemoryOK bool
	// Open opens the store under test.
	Open func(ctx context.Context, input string) (io.Closer, error)
}

// RunHardenedPaths checks that an opener which promises private (0600)
// database files secures the file SQLite actually opens, for each input form
// where the raw input and SQLite's file differ: a file: URI, ".." after a
// symlink (plain and URI), a symlinked final component, a relative URI, and
// an input naming no file. Every case runs in its own tree, from a working
// directory inside it. The expected file is built from that tree, never
// derived from the opener's own resolution. A loose (0644) WAL database with
// non-empty sidecars is seeded at that file and held open first, so the check
// sees the opener secure files it did not create. After the open, the main file and both sidecars must be
// 0600 (Unix only: mode bits are not portable on Windows), and the working
// directory must hold no decoy named after the raw URI. A vfs= URI must be rejected without
// creating anything. Symlink cases skip where symlinks are unavailable.
func RunHardenedPaths(t *testing.T, opts HardenedPathsOptions) {
	t.Helper()
	type hardenedCase struct {
		name  string
		setup func(t *testing.T, base string) (input, want string)
	}
	link := func(t *testing.T, target, name string) {
		t.Helper()
		if err := os.Symlink(target, name); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	slash := func(p string) string { // a URI filename for an absolute path
		p = filepath.ToSlash(p)
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return p
	}
	cases := []hardenedCase{
		{"absolute file URI", func(t *testing.T, base string) (string, string) {
			want := filepath.Join(base, "real", "t.db")
			return opts.URI(slash(want)), want
		}},
		{"relative file URI", func(t *testing.T, base string) (string, string) {
			return opts.URI("t.db"), filepath.Join(base, "cwd", "t.db")
		}},
		{"plain dotdot after symlink", func(t *testing.T, base string) (string, string) {
			link(t, filepath.Join(base, "real", "sub"), filepath.Join(base, "cwd", "link"))
			// FileURL cleans a plain path lexically before SQLite sees it; the
			// kernel resolves the raw string through the link. Concatenated:
			// filepath.Join would clean the ".." away before the opener sees it.
			sep := string(filepath.Separator)
			return "link" + sep + ".." + sep + "t.db", filepath.Join(base, "cwd", "t.db")
		}},
		{"URI dotdot after symlink", func(t *testing.T, base string) (string, string) {
			link(t, filepath.Join(base, "real", "sub"), filepath.Join(base, "cwd", "link"))
			if runtime.GOOS == "windows" { // GetFullPathNameW is lexical
				return opts.URI(slash(filepath.Join(base, "cwd")) + "/link/../t.db"), filepath.Join(base, "cwd", "t.db")
			}
			// SQLite follows link to real/sub, then ".." to real.
			return opts.URI(slash(filepath.Join(base, "cwd")) + "/link/../t.db"), filepath.Join(base, "real", "t.db")
		}},
		{"symlinked final component", func(t *testing.T, base string) (string, string) {
			target := filepath.Join(base, "real", "real.db")
			link(t, target, filepath.Join(base, "cwd", "t.db"))
			if runtime.GOOS == "windows" {
				t.Skip("Windows SQLite names the database and its sidecars after the link, not the target")
			}
			return "t.db", target
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := hardenedTree(t)
			input, want := tc.setup(t, base)
			seedLooseWAL(t, want)
			t.Chdir(filepath.Join(base, "cwd"))
			store, err := opts.Open(t.Context(), input)
			if err != nil {
				t.Fatalf("open %q: %v", input, err)
			}
			defer func() { _ = store.Close() }()
			for _, p := range []string{want, want + "-wal", want + "-shm"} {
				info, err := os.Stat(p)
				if err != nil {
					t.Fatalf("open %q: %s: %v", input, p, err)
				}
				if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
					t.Errorf("open %q left %s at %o, want 0600", input, p, info.Mode().Perm())
				}
			}
			assertNoDecoy(t, filepath.Join(base, "cwd"), input)
		})
	}
	t.Run("input naming no file", func(t *testing.T) {
		base := hardenedTree(t)
		t.Chdir(filepath.Join(base, "cwd"))
		input := opts.URI("m.db") + "?mode=memory"
		store, err := opts.Open(t.Context(), input)
		switch {
		case opts.MemoryOK && err != nil:
			t.Fatalf("open %q: %v", input, err)
		case !opts.MemoryOK && err == nil:
			_ = store.Close()
			t.Fatalf("open %q succeeded, want a rejection: there is no file to harden", input)
		case err == nil:
			defer func() { _ = store.Close() }()
		}
		assertNoFiles(t, base, input)
	})
	t.Run("rejected input", func(t *testing.T) {
		base := hardenedTree(t)
		t.Chdir(filepath.Join(base, "cwd"))
		input := opts.URI("x.db") + "?vfs=unix-none"
		if store, err := opts.Open(t.Context(), input); err == nil {
			_ = store.Close()
			t.Fatalf("open %q succeeded, want the vfs parameter rejected", input)
		}
		assertNoFiles(t, base, input)
	})
}

// seedLooseWAL leaves a loose (0644) WAL database at path with non-empty -wal
// and -shm, held open by another connection until the test ends, as a store
// left by an older run would be. The sidecars must be non-empty: SQLite itself
// fchmods an existing zero-size sidecar to the database file's mode when it
// opens it, which would hide an opener that never secures them.
func seedLooseWAL(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlitedsn.EnableWAL(t.Context(), db); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	if _, err := db.Exec("CREATE TABLE zz_seed (x); DROP TABLE zz_seed"); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if err != nil || info.Size() == 0 {
			t.Fatalf("seed %s: %v (size %d), want a non-empty file", p, err, sizeOf(info))
		}
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func sizeOf(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}

// hardenedTree returns a fresh base directory, symlinks resolved so expected
// paths compare equal to SQLite's (macOS temp dirs sit under the /var link),
// holding cwd/ and real/sub/.
func hardenedTree(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"cwd", filepath.Join("real", "sub")} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func assertNoDecoy(t *testing.T, dir, input string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A decoy is the raw URI taken as a file name, so it holds the scheme's
	// colon; no file these cases create legitimately does.
	for _, e := range entries {
		if strings.Contains(e.Name(), ":") {
			t.Errorf("open %q created %q in the working directory", input, e.Name())
		}
	}
}

func assertNoFiles(t *testing.T, base, input string) {
	t.Helper()
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			t.Errorf("open %q created %s", input, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
