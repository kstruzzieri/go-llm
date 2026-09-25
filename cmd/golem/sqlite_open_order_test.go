package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// lockSQLiteFileFor holds path's database file lock from another connection
// and releases it after d, which must stay below the opener's busy_timeout.
// EXCLUSIVE locking mode set before the first WAL access keeps the file
// locked until that connection closes.
func lockSQLiteFileFor(t *testing.T, path string, d time.Duration) {
	t.Helper()
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open lock holder: %v", err)
	}
	holder.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = holder.Close() })
	var tables int
	if _, err := holder.ExecContext(t.Context(), "PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatalf("lock holder locking_mode: %v", err)
	}
	if err := holder.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sqlite_schema").Scan(&tables); err != nil {
		t.Fatalf("lock holder read: %v", err)
	}
	release := time.AfterFunc(d, func() { _ = holder.Close() })
	t.Cleanup(func() { release.Stop() })
}

// TestOpenSessionWaitsForLockedWALFile pins the PRAGMA order: busy_timeout
// must precede journal_mode. On an existing WAL file, the journal_mode PRAGMA
// reads the database header and otherwise fails at once while another
// connection holds the database lock. Scheduling delay can only hide a
// regression: an opener that reaches the PRAGMA after the release passes under
// either order.
func TestOpenSessionWaitsForLockedWALFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	seed, _, err := openSession(t.Context(), path, "workspace:lock")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	lockSQLiteFileFor(t, path, 200*time.Millisecond)
	s, _, err := openSession(t.Context(), path, "workspace:lock")
	if err != nil {
		t.Fatalf("open behind a held database lock: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestOpenFeedbackServiceWaitsForLockedWALFile pins the same order for the
// feedback opener, whose 1s busy_timeout still exceeds the 200ms hold.
func TestOpenFeedbackServiceWaitsForLockedWALFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "feedback.db")
	seed, err := openFeedbackService(t.Context(), root, path, feedbackTestWarn(t))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := seed.close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	lockSQLiteFileFor(t, path, 200*time.Millisecond)
	svc, err := openFeedbackService(t.Context(), root, path, feedbackTestWarn(t))
	if err != nil {
		t.Fatalf("open behind a held database lock: %v", err)
	}
	if _, err := svc.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
