// Package sqlitedsn builds modernc.org/sqlite DSNs for go-llm's store openers.
package sqlitedsn

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// WithBusyTimeout returns a DSN for path whose every connection starts with
// the given busy_timeout, including connections database/sql opens to replace
// a discarded one. A one-off PRAGMA configures only the connection that ran
// it, and database/sql discards a modernc connection after a
// context-cancelled statement. "" and ":memory:" are returned unchanged. A
// "file:" URI keeps its query parameters, including its own busy_timeout
// pragma, which then replaces timeout (modernc leaves the order of two
// busy_timeout pragmas undefined). Any other path is made absolute and
// escaped, so '?', '#', and '%' in a file name stay part of the path.
func WithBusyTimeout(path string, timeout time.Duration) (string, error) {
	if path == "" || path == ":memory:" {
		return path, nil
	}
	var u *url.URL
	if strings.HasPrefix(path, "file:") {
		parsed, err := url.Parse(path)
		if err != nil {
			return "", fmt.Errorf("sqlitedsn: parse URI %q: %w", path, err)
		}
		u = parsed
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("sqlitedsn: resolve path %q: %w", path, err)
		}
		u = &url.URL{Scheme: "file", Path: abs}
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
