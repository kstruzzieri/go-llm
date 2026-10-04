// Package sqlitedsn builds modernc.org/sqlite DSNs for go-llm's store openers.
package sqlitedsn

import (
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// FileURL returns the file: URL that opens path. A plain path is made
// absolute; a "file:" URI is parsed and kept, query included. Callers add
// their own query parameters (mode=ro, immutable=1, _pragma=..., _txlock=...).
// On Windows a drive-letter path renders as file:///C:/dir/name.db, and UNC
// or device paths are rejected in either form, because SQLite WAL does not
// work on network filesystems; the only URI authority accepted is localhost.
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
			if err := windowsURIFilename(u); err != nil {
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
// or device path on Windows: any authority but localhost, or a decoded
// filename (Opaque when set, else Path) that starts with two separators.
// That covers file:%5C%5Cserver%5Cshare, file:/%5C%5C%3F%5CUNC%5C...,
// file:////server/share and file://server/share.
func windowsURIFilename(u *url.URL) error {
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
