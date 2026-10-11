package sqlitedsn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxSymlinks is SQLite's SQLITE_MAX_SYMLINK: unixFullPathname fails an open
// that follows more links than this.
const maxSymlinks = 200

// Resolve returns, from one parse of input, the DSN that opens it with every
// connection's busy_timeout set to busy (as WithBusyTimeout does) and the
// on-disk path SQLite will open for that DSN. Store openers that promise
// private database files prepare, chmod and derive paths from diskPath, never
// from the raw input, because the two differ for file: URIs, for ".." after a
// symlink, and for a symlinked final component.
//
// diskPath is "" when input names no file: "", ":memory:", file::memory:, a
// mode=memory URI, or an empty URI filename; the DSN then keeps its spelling.
// Otherwise diskPath is absolute, and its "-wal" and "-shm" siblings are the
// sidecars SQLite uses. On Unix it is computed by the component walk SQLite's
// unixFullPathname performs (".." drops the component emitted so far, each
// prefix is lstat'ed, symlinks are spliced in, a missing prefix is kept), so it
// is free of symlinks and "..". On Windows SQLite keeps the lexical full
// pathname (GetFullPathNameW), and so does Resolve.
//
// A relative file: URI is rewritten to an absolute one in the DSN, so a
// connection database/sql opens after a working-directory change reaches the
// same file. Resolve rejects, before anything is created, inputs whose target
// cannot be predicted or that SQLite reads differently from Go: a NUL anywhere
// in the decoded filename or query, more than one mode parameter, a vfs
// parameter, a query Go's parser would drop pairs from (such as one with ";"),
// a user or a host other than localhost, a fragment, and everything FileURL
// rejects. The file a
// symlink names can still change between Resolve and the open; CheckMainFile
// detects SQLite and Resolve disagreeing, not a concurrent swap.
func Resolve(input string, busy time.Duration) (dsn, diskPath string, err error) {
	if input == "" || input == ":memory:" {
		return input, "", nil
	}
	u, err := FileURL(input)
	if err != nil {
		return "", "", err
	}
	name := u.Path
	if strings.HasPrefix(input, "file:") {
		var memory bool
		if name, memory, err = checkURI(u); err != nil {
			return "", "", err
		}
		if memory {
			return withBusyTimeout(u, busy), "", nil
		}
		if err := absolutizeURI(u, &name); err != nil {
			return "", "", err
		}
	}
	if runtime.GOOS == "windows" {
		diskPath, err = windowsDiskPath(name)
	} else {
		diskPath, err = sqliteUnixFullPath(name)
	}
	if err != nil {
		return "", "", fmt.Errorf("sqlitedsn: resolve %q: %w", input, err)
	}
	return withBusyTimeout(u, busy), diskPath, nil
}

// checkURI validates a parsed file: URI and returns its decoded filename and
// whether it names no file (memory or temporary database).
func checkURI(u *url.URL) (name string, memory bool, err error) {
	if u.User != nil || u.Host != "" && u.Host != "localhost" {
		return "", false, fmt.Errorf("sqlitedsn: URI %q names host %q; only localhost is allowed", u.String(), u.Host)
	}
	// modernc reads everything after the first "?" as the query, so a
	// fragment would end up inside the last query value (#680).
	if u.Fragment != "" {
		return "", false, fmt.Errorf("sqlitedsn: URI %q has a fragment, which modernc would read into the query", u.String())
	}
	name = u.Path
	if u.Opaque != "" {
		if name, err = url.PathUnescape(u.Opaque); err != nil {
			return "", false, fmt.Errorf("sqlitedsn: decode URI %q: %w", u.String(), err)
		}
	}
	if strings.ContainsRune(name, 0) {
		return "", false, fmt.Errorf("sqlitedsn: URI %q has a NUL in its filename", u.String())
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", false, fmt.Errorf("sqlitedsn: URI %q has a query SQLite and Go would read differently: %w", u.String(), err)
	}
	for k, vs := range q {
		if strings.ContainsRune(k, 0) || strings.ContainsRune(strings.Join(vs, ""), 0) {
			return "", false, fmt.Errorf("sqlitedsn: URI %q has a NUL in its query", u.String())
		}
	}
	if _, ok := q["vfs"]; ok {
		return "", false, fmt.Errorf("sqlitedsn: URI %q selects a VFS; the opened file could not be predicted", u.String())
	}
	modes := q["mode"]
	if len(modes) > 1 {
		return "", false, fmt.Errorf("sqlitedsn: URI %q repeats mode; give it once", u.String())
	}
	memory = name == "" || name == ":memory:" || len(modes) == 1 && modes[0] == "memory"
	return name, memory, nil
}

// absolutizeURI rewrites a relative file: URI in place to an absolute one,
// without lexical cleaning, and updates name to match. Absolute URIs are left
// as given.
func absolutizeURI(u *url.URL, name *string) error {
	if runtime.GOOS == "windows" {
		// SQLite's Windows full pathname is lexical, so rendering it is exact.
		full, err := windowsDiskPath(*name)
		if err != nil {
			return fmt.Errorf("sqlitedsn: resolve URI %q: %w", u.String(), err)
		}
		p, err := windowsURIPath(full)
		if err != nil {
			return err
		}
		*name = full
		u.Opaque, u.Path = "", p
		return nil
	}
	if strings.HasPrefix(*name, "/") {
		return nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("sqlitedsn: resolve URI %q: %w", u.String(), err)
	}
	// Plain concatenation: filepath.Join would erase ".." before SQLite sees it.
	*name = wd + "/" + *name
	u.Opaque, u.Path = "", *name
	return nil
}

// windowsDiskPath is the full pathname SQLite's winFullPathname gives a URI
// filename: the "/" before a drive letter dropped, then GetFullPathNameW,
// which filepath.Abs calls on Windows.
func windowsDiskPath(name string) (string, error) {
	if len(name) >= 3 && name[0] == '/' && isASCIILetter(name[1]) && name[2] == ':' {
		name = name[1:]
	}
	return filepath.Abs(name)
}

// sqliteUnixFullPath mirrors SQLite's unixFullPathname (os_unix.c) for an
// absolute name: components are appended in order from "/"; "" and "." are
// skipped, ".." drops the last component emitted so far, and each appended
// prefix is lstat'ed. A symlink's target replaces the component (an absolute
// target restarts from "/"); a prefix that does not exist is kept; any other
// lstat error fails, as SQLite fails the open.
func sqliteUnixFullPath(name string) (string, error) {
	if !strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("path %q is not absolute", name)
	}
	var out []string
	links := 0
	var walk func(p string) error
	walk = func(p string) error {
		if strings.HasPrefix(p, "/") {
			out = out[:0]
		}
		for _, c := range strings.Split(p, "/") {
			switch c {
			case "", ".":
				continue
			case "..":
				if len(out) > 0 {
					out = out[:len(out)-1]
				}
				continue
			}
			out = append(out, c)
			cur := "/" + strings.Join(out, "/")
			info, err := os.Lstat(cur)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode()&fs.ModeSymlink == 0 {
				continue
			}
			// SQLite tests the count before incrementing it.
			if links > maxSymlinks {
				return fmt.Errorf("more than %d symlinks", maxSymlinks)
			}
			links++
			target, err := os.Readlink(cur)
			if err != nil {
				return err
			}
			out = out[:len(out)-1]
			if err := walk(target); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(name); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", errors.New("path names the root directory")
	}
	return "/" + strings.Join(out, "/"), nil
}

// CheckMainFile returns an error unless db's main database is the file
// diskPath names ("" for none), as SQLite reports it. Store openers call it on
// their first connection after the open, so a Resolve that disagrees with
// SQLite fails the open instead of leaving an unhardened file in use. It
// compares names, not file identity, and does not detect a file swapped
// between the open and the check.
func CheckMainFile(ctx context.Context, db *sql.DB, diskPath string) error {
	var file string
	if err := db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file); err != nil {
		return fmt.Errorf("sqlitedsn: read main database file: %w", err)
	}
	if file != diskPath {
		return fmt.Errorf("sqlitedsn: SQLite opened %q, not the hardened file %q", file, diskPath)
	}
	return nil
}

// withBusyTimeout renders u with a busy_timeout pragma unless its query
// already carries one; WithBusyTimeout documents the rule.
func withBusyTimeout(u *url.URL, timeout time.Duration) string {
	q := u.Query()
	for _, p := range q["_pragma"] {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), "busy_timeout") {
			return u.String()
		}
	}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", timeout.Milliseconds()))
	u.RawQuery = q.Encode()
	return u.String()
}
