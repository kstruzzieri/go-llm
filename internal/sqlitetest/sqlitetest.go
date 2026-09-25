// Package sqlitetest provides SQLite test helpers shared by go-llm's store
// packages.
package sqlitetest

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// LockFileFor holds path's database file lock from another connection and
// releases it after d, which must stay below the busy_timeout of the opener
// under test. EXCLUSIVE locking mode set before the first WAL access keeps the
// file locked until that connection closes. Scheduling delay can only hide a
// regression: an opener that reaches its first lock after the release passes
// whether or not it waits.
func LockFileFor(t testing.TB, path string, d time.Duration) {
	t.Helper()
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	var tables int
	if _, err := holder.Exec("PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatalf("lock holder locking_mode: %v", err)
	}
	if err := holder.QueryRow("SELECT COUNT(*) FROM sqlite_schema").Scan(&tables); err != nil {
		t.Fatalf("lock holder read: %v", err)
	}
	release := time.AfterFunc(d, func() { _ = holder.Close() })
	t.Cleanup(func() { release.Stop() })
}

// AssertNewConnectionBusyTimeout checks that a connection db opens after
// discarding its current one starts with busy_timeout = want. database/sql
// discards a modernc connection after a context-cancelled statement, so a
// one-off PRAGMA on the first connection does not survive; the timeout must
// come from the DSN. It stops db from keeping idle connections.
func AssertNewConnectionBusyTimeout(t testing.TB, db *sql.DB, want time.Duration) {
	t.Helper()
	// With no idle connections kept, every statement runs on a new connection.
	db.SetMaxIdleConns(0)
	for i := range 2 {
		var got int64
		if err := db.QueryRow("PRAGMA busy_timeout").Scan(&got); err != nil {
			t.Fatalf("read busy_timeout on new connection %d: %v", i+1, err)
		}
		if got != want.Milliseconds() {
			t.Fatalf("busy_timeout on new connection %d = %dms, want %dms", i+1, got, want.Milliseconds())
		}
	}
}
