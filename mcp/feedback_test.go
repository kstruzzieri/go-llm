package mcp

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWithRetrievalFeedbackSetsPath(t *testing.T) {
	s := &Server{}
	WithRetrievalFeedback("/tmp/fb.db")(s)
	if s.feedbackDBPath != "/tmp/fb.db" {
		t.Errorf("feedbackDBPath = %q, want /tmp/fb.db", s.feedbackDBPath)
	}
}

func TestWithRetrievalFeedbackOpensAndCloses(t *testing.T) {
	mock := httptest.NewServer(routingFallbackHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})))
	defer mock.Close()

	ragPath := filepath.Join(t.TempDir(), "rag.db")
	feedbackPath := filepath.Join(t.TempDir(), "feedback", "feedback.db")
	s, err := NewServer(context.Background(),
		WithOllamaURL(mock.URL),
		WithRAGPath(ragPath),
		WithRetrievalFeedback(feedbackPath),
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if s.feedbackDB == nil {
		_ = s.Close()
		t.Fatal("feedbackDB = nil, want opened feedback DB")
	}
	// WAL/SHM sidecars (if present while the DB is open) must be 0600: telemetry
	// must never leak through a sidecar. They may legitimately not exist yet.
	for _, sidecar := range []string{feedbackPath + "-wal", feedbackPath + "-shm"} {
		if info, err := os.Stat(sidecar); err == nil {
			if info.Mode().Perm() != 0o600 {
				_ = s.Close()
				t.Fatalf("sidecar %s mode = %o, want 0600", sidecar, info.Mode().Perm())
			}
		} else if !os.IsNotExist(err) {
			_ = s.Close()
			t.Fatalf("stat sidecar %s: %v", sidecar, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if s.feedbackDB != nil {
		t.Fatal("feedbackDB not cleared after Close")
	}
	if info, err := os.Stat(feedbackPath); err != nil {
		t.Fatalf("feedback DB not created: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("feedback DB mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestWithRetrievalFeedbackBadPathIsNonFatal(t *testing.T) {
	mock := httptest.NewServer(routingFallbackHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})))
	defer mock.Close()

	ragPath := filepath.Join(t.TempDir(), "rag.db")
	blockingParent := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blockingParent, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := NewServer(context.Background(),
		WithOllamaURL(mock.URL),
		WithRAGPath(ragPath),
		WithRetrievalFeedback(filepath.Join(blockingParent, "feedback.db")),
	)
	if err != nil {
		t.Fatalf("NewServer() error = %v, want feedback-open failure to be non-fatal", err)
	}
	defer func() { _ = s.Close() }()
	if s.store == nil {
		t.Fatal("RAG store = nil, want server to continue with RAG store")
	}
	if s.feedbackDB != nil {
		t.Fatal("feedbackDB = non-nil, want bad feedback path to stay disabled")
	}
}

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

// TestOpenRetrievalFeedbackWeighterWaitsForLockedWALFile pins the PRAGMA
// order: busy_timeout must precede journal_mode. On an existing WAL file, the
// journal_mode PRAGMA reads the database header and otherwise fails at once
// while another connection holds the database lock. Scheduling delay can only
// hide a regression: an opener that reaches the PRAGMA after the release
// passes under either order.
func TestOpenRetrievalFeedbackWeighterWaitsForLockedWALFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	seed, _, err := openRetrievalFeedbackWeighter(t.Context(), path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	lockSQLiteFileFor(t, path, 200*time.Millisecond)
	db, _, err := openRetrievalFeedbackWeighter(t.Context(), path)
	if err != nil {
		t.Fatalf("open behind a held database lock: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
