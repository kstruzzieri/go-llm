package main

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

type auditDiagnostic struct{ code, target, message string }

type auditResult struct {
	scope, assurance, outcome string
	checked                   int64
	total                     *int64
	paths, sources            int64
	diagnostics               []auditDiagnostic
	earlyStop                 bool
}

// withAuditStore requires stopped writers and a checkpointed database. The
// observations detect ordinary concurrent changes, not an atomic snapshot.
// Callers validate workspace placement and their supported schema in scan.
func withAuditStore(ctx context.Context, path string, scan func(*sql.DB) auditResult) (result auditResult) {
	uri, err := auditStoreURI(path)
	if err != nil {
		return auditStoreFailure(path, "incomplete", "store-unavailable", "Store is unavailable or unsafe.")
	}
	before, err := observeAuditStore(path)
	if err != nil {
		return auditStoreFailure(path, "incomplete", "store-unavailable", "Store is unavailable or unsafe.")
	}
	if before[0] == nil {
		return auditStoreFailure(path, "not-present", "store-not-present", "Store is not present.")
	}
	for _, i := range []int{1, 3} {
		if before[i] != nil && before[i].Size() > 0 {
			return auditStoreFailure(path, "incomplete", "store-active-journal", "Close the writer cleanly and retry; a nonempty journal is present.")
		}
	}
	defer func() {
		// Registered first so this comparison runs after the connection closes.
		after, err := observeAuditStore(path)
		if err != nil || !auditStoreUnchanged(before, after) {
			result = auditStoreFailure(path, "incomplete", "store-changed", "Store changed during verification; close the writer cleanly and retry.")
		}
	}()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return auditStoreReadFailure(path, err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err := db.Close(); err != nil {
			result = auditStoreReadFailure(path, err)
		}
	}()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check(1)").Scan(&integrity); err != nil {
		return auditStoreReadFailure(path, err)
	}
	if integrity != "ok" {
		return auditStoreFailure(path, "violation", "store-corrupt", "SQLite integrity verification failed.")
	}
	result = scan(db)
	if result.outcome == "valid" && ctx.Err() != nil {
		return auditStoreReadFailure(path, ctx.Err())
	}
	return result
}

func auditStoreReadFailure(path string, err error) auditResult {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
			return auditStoreFailure(path, "violation", "store-corrupt", "SQLite integrity verification failed.")
		}
	}
	return auditStoreFailure(path, "incomplete", "store-unavailable", "Store is unavailable or unsafe.")
}

func auditStoreURI(path string) (string, error) {
	if strings.HasPrefix(path, "//") || strings.HasPrefix(path, `\\`) {
		return "", os.ErrInvalid
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	normalized := filepath.ToSlash(absolute)
	if strings.HasPrefix(normalized, "//") {
		return "", os.ErrInvalid
	}
	if volume := filepath.VolumeName(absolute); len(volume) == 2 && volume[1] == ':' {
		normalized = "/" + normalized
	}
	u := url.URL{Scheme: "file", Path: normalized}
	q := url.Values{"cache": {"private"}, "immutable": {"1"}, "mode": {"ro"}}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func observeAuditStore(path string) ([4]os.FileInfo, error) {
	var observations [4]os.FileInfo
	for i, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return observations, err
		}
		if !info.Mode().IsRegular() {
			return observations, os.ErrInvalid
		}
		observations[i] = info
	}
	return observations, nil
}

func auditStoreUnchanged(before, after [4]os.FileInfo) bool {
	for i, old := range before {
		now := after[i]
		if old == nil || now == nil {
			if old != nil || now != nil {
				return false
			}
			continue
		}
		if !os.SameFile(old, now) || old.Size() != now.Size() || old.Mode() != now.Mode() || !old.ModTime().Equal(now.ModTime()) {
			return false
		}
	}
	return true
}

func auditStoreFailure(path, outcome, code, message string) auditResult {
	return auditResult{outcome: outcome, diagnostics: []auditDiagnostic{{code: code, target: path, message: message}}}
}
