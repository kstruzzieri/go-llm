package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

// TestOpenSessionWaitsForLockedWALFile pins busy_timeout on every connection
// the opener creates: the journal_mode PRAGMA on its first connection waits
// behind a held database lock, and a replacement connection keeps the timeout.
func TestOpenSessionWaitsForLockedWALFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	seed, _, err := openSession(t.Context(), path, "workspace:lock")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	sqlitetest.LockFileFor(t, path, 200*time.Millisecond)
	s, _, err := openSession(t.Context(), path, "workspace:lock")
	if err != nil {
		t.Fatalf("open behind a held database lock: %v", err)
	}
	sqlitetest.AssertNewConnectionBusyTimeout(t, s.db, 5*time.Second)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestOpenFeedbackServiceWaitsForLockedWALFile pins busy_timeout on every
// connection the opener creates: the journal_mode PRAGMA on its first
// connection waits behind a held database lock, and a replacement connection
// keeps the timeout. openFeedbackService's 1s busy_timeout still exceeds the
// 200ms hold.
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
	sqlitetest.LockFileFor(t, path, 200*time.Millisecond)
	svc, err := openFeedbackService(t.Context(), root, path, feedbackTestWarn(t))
	if err != nil {
		t.Fatalf("open behind a held database lock: %v", err)
	}
	sqlitetest.AssertNewConnectionBusyTimeout(t, svc.writer, time.Second)
	if _, err := svc.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
