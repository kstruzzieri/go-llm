// Package sqlitedsn builds modernc.org/sqlite DSNs for go-llm's store openers
// and switches their databases to WAL journal mode.
package sqlitedsn

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// FileURL returns the file: URL that opens path. A plain path is made
// absolute; a "file:" URI is parsed and kept, query included. Callers add
// their own query parameters (mode=ro, immutable=1, _pragma=..., _txlock=...).
// On Windows a drive-letter path renders as file:///C:/dir/name.db, and UNC
// or device paths are rejected in either form, because SQLite WAL does not
// work on network filesystems; the only URI authority accepted is localhost.
// A relative or rooted URI filename is checked as SQLite will resolve it,
// against the working directory at the time of the call. A drive letter mapped
// to a network share is not detected and has the same WAL limits.
// FileURL rejects "". Callers handle ":memory:" before calling it.
func FileURL(path string) (*url.URL, error) {
	if path == "" {
		return nil, errors.New("sqlitedsn: empty database path")
	}
	if strings.HasPrefix(path, "file:") {
		u, err := url.Parse(path)
		if err != nil {
			return nil, fmt.Errorf("sqlitedsn: parse URI %q: %w", path, err)
		}
		if runtime.GOOS == "windows" {
			if err := windowsURIFilename(u, filepath.Abs); err != nil {
				return nil, err
			}
		}
		return u, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("sqlitedsn: resolve path %q: %w", path, err)
	}
	if runtime.GOOS == "windows" {
		if abs, err = windowsURIPath(abs); err != nil {
			return nil, err
		}
	}
	return &url.URL{Scheme: "file", Path: abs}, nil
}

// FileURLPreservingPath is FileURL without lexical path cleaning on Unix.
// Plain relative paths are made absolute by prepending the working directory,
// keeping symlinks and ".." for SQLite to resolve in filesystem order. This
// lets read-only callers open the same file as os.Stat of the original path.
// File URIs are kept as given; Windows uses FileURL's native path resolution
// and UNC/device-path checks.
func FileURLPreservingPath(path string) (*url.URL, error) {
	if path == "" || strings.HasPrefix(path, "file:") || runtime.GOOS == "windows" {
		return FileURL(path)
	}
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("sqlitedsn: resolve path %q: %w", path, err)
		}
		// filepath.Join would erase symlink/.. before SQLite sees it.
		path = wd + string(filepath.Separator) + path
	}
	return &url.URL{Scheme: "file", Path: path}, nil
}

// windowsURIPath turns an absolute Windows path into the path of a file: URL
// ("/C:/dir/x.db"). Only drive-letter paths qualify: UNC (\\server\share) and
// device (\\?\, \\.\) paths start with two separators instead. It is plain
// string handling so every OS can test it.
func windowsURIPath(abs string) (string, error) {
	p := strings.ReplaceAll(abs, `\`, "/")
	if len(p) < 3 || !isASCIILetter(p[0]) || p[1] != ':' || p[2] != '/' {
		return "", fmt.Errorf("sqlitedsn: %q is not a local drive-letter path; UNC and device paths are not supported (SQLite WAL does not work on network filesystems)", abs)
	}
	return "/" + p, nil
}

// windowsURIFilename rejects a file: URI that SQLite would resolve to a UNC
// or device path on Windows: any authority but localhost, a decoded filename
// (Opaque when set, else Path) that starts with two separators, or one that
// abs, resolving it against the working directory as SQLite does with
// GetFullPathNameW, turns into anything but a drive-letter path. That covers
// file:%5C%5Cserver%5Cshare, file:/%5C%5C%3F%5CUNC%5C..., file:////server/share,
// file://server/share, and file:x.db or file:/x.db under a UNC working
// directory. A temporary ("") or :memory: database names no file and is not
// resolved; the skip reads only the filename, because SQLite applies the last
// mode option and callers may change the query after FileURL returns.
func windowsURIFilename(u *url.URL, abs func(string) (string, error)) error {
	if u.Host != "" && u.Host != "localhost" {
		return fmt.Errorf("sqlitedsn: URI %q names host %q; only localhost is allowed", u.String(), u.Host)
	}
	name := u.Path
	if u.Opaque != "" {
		var err error
		if name, err = url.PathUnescape(u.Opaque); err != nil {
			return fmt.Errorf("sqlitedsn: decode URI %q: %w", u.String(), err)
		}
	}
	if strings.HasPrefix(strings.ReplaceAll(name, `\`, "/"), "//") {
		return fmt.Errorf("sqlitedsn: URI %q names a UNC or device path; use a local drive path (SQLite WAL does not work on network filesystems)", u.String())
	}
	if name == "" || name == ":memory:" {
		return nil
	}
	// SQLite drops the "/" before a drive letter ("/C:/x.db") and resolves
	// the rest with GetFullPathNameW, as filepath.Abs does on Windows.
	if len(name) >= 3 && name[0] == '/' && isASCIILetter(name[1]) && name[2] == ':' {
		name = name[1:]
	}
	full, err := abs(name)
	if err != nil {
		return fmt.Errorf("sqlitedsn: resolve URI %q: %w", u.String(), err)
	}
	if _, err := windowsURIPath(full); err != nil {
		return fmt.Errorf("sqlitedsn: URI %q resolves against the working directory to %q; UNC and device paths are not supported (SQLite WAL does not work on network filesystems)", u.String(), full)
	}
	return nil
}

func isASCIILetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// WithBusyTimeout returns a DSN for path whose every connection starts with the
// given busy_timeout, including connections database/sql opens to replace a
// discarded one. A one-off PRAGMA configures only the connection that ran it,
// and database/sql discards a modernc connection after a context-cancelled
// statement run outside a transaction. "" and ":memory:" are returned
// unchanged. Any other path goes through FileURL: a "file:" URI keeps its query
// parameters, including its own busy_timeout pragma, which then replaces
// timeout (modernc leaves the order of two busy_timeout pragmas undefined); a
// plain path is made absolute and escaped, so '?', '#', and '%' in a file name
// stay part of the path.
func WithBusyTimeout(path string, timeout time.Duration) (string, error) {
	if path == "" || path == ":memory:" {
		return path, nil
	}
	u, err := FileURL(path)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for _, p := range q["_pragma"] {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(p)), "busy_timeout") {
			return u.String(), nil
		}
	}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", timeout.Milliseconds()))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// EnableWAL switches db's database to WAL journal mode. The switch upgrades a
// read lock to a write lock, and SQLite skips the busy handler on that upgrade,
// so two connections racing a database's first switch fail at once with
// SQLITE_BUSY whatever busy_timeout says. EnableWAL retries on SQLITE_BUSY
// within one budget: the connection's busy_timeout, or the time left to ctx's
// deadline if that is sooner. The budget also caps SQLite's own lock waits
// during each attempt's prepare and execute; a zero busy_timeout allows one
// attempt. The total wait is bounded by the budget plus scheduling and I/O
// overhead. It returns an error if SQLite reports a journal mode other than
// "wal"; an in-memory database reports "memory" and is accepted unchanged.
// Cancellation is checked between attempts; a lock wait already in progress
// ends by the budget's deadline. Shared-cache (cache=shared) connections are
// outside this contract: modernc waits on an unlock notification outside the
// busy handler, and a concurrent writer on the same shared cache makes SQLite
// decline the switch, which EnableWAL reports as an error. rag.NewSQLiteStore
// forwards a caller's cache=shared URI; go-llm's own openers never build one.
func EnableWAL(ctx context.Context, db *sql.DB) error { return enableWAL(ctx, db, nil) }

// enableWAL is EnableWAL with onBusy called after each attempt that fails with
// SQLITE_BUSY and will be retried; tests release their lock holder there.
func enableWAL(ctx context.Context, db *sql.DB, onBusy func()) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	// Background, not ctx: these PRAGMAs never touch the database file, and an
	// interrupt raised here by an expiring ctx would stay set on the
	// connection, so database/sql would discard it (as in provider's
	// runInTxBefore).
	var saved int64
	if err := conn.QueryRowContext(context.Background(), "PRAGMA busy_timeout").Scan(&saved); err != nil {
		_ = conn.Close()
		return fmt.Errorf("sqlitedsn: read busy_timeout: %w", err)
	}
	defer func() {
		if _, rsErr := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout = %d", saved)); rsErr != nil {
			err = errors.Join(err, fmt.Errorf("sqlitedsn: restore busy_timeout: %w", rsErr))
			// Never return a connection with a lowered timeout to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}()
	deadline := time.Now().Add(time.Duration(saved) * time.Millisecond)
	// An earlier ctx deadline ends the budget too, as in provider's
	// runInTxBefore; the cap below then stops a lock wait in progress by it,
	// which ctx alone cannot (SQLite's busy handler does not watch ctx).
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	capBusy := func() error { return setBusyTimeoutCap(conn, saved, time.Until(deadline)) }
	for {
		mode, err := walAttempt(ctx, conn, capBusy)
		if err == nil {
			if mode == "wal" || mode == "memory" {
				return nil
			}
			return fmt.Errorf("sqlitedsn: journal_mode is %q, want wal", mode)
		}
		var se *sqlite.Error
		if !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY || !time.Now().Before(deadline) {
			return err
		}
		if onBusy != nil {
			onBusy()
		}
		timer := time.NewTimer(time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// setBusyTimeoutCap sets conn's busy_timeout to the budget remaining, in
// whole milliseconds, never above saved. SQLite treats a negative timeout as
// zero, so a spent budget needs no clamp.
func setBusyTimeoutCap(conn *sql.Conn, saved int64, remaining time.Duration) error {
	ms := min(remaining.Milliseconds(), saved)
	if _, err := conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout = %d", ms)); err != nil {
		return fmt.Errorf("sqlitedsn: cap busy_timeout: %w", err)
	}
	return nil
}

// walAttempt runs one WAL switch with SQLite's lock waits capped to the time
// left. modernc prepares eagerly, and journal_mode loads the schema, which can
// wait for a read lock; execution can wait again for the commit's exclusive
// lock, so the cap is refreshed in between or one attempt could spend the
// remaining budget twice.
func walAttempt(ctx context.Context, conn *sql.Conn, capBusy func() error) (string, error) {
	if err := capBusy(); err != nil {
		return "", err
	}
	stmt, err := conn.PrepareContext(ctx, "PRAGMA journal_mode=WAL")
	if err != nil {
		return "", err
	}
	defer func() { _ = stmt.Close() }()
	if err := capBusy(); err != nil {
		return "", err
	}
	var mode string
	if err := stmt.QueryRowContext(ctx).Scan(&mode); err != nil {
		return "", err
	}
	return strings.ToLower(mode), nil
}
