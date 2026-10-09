package mcp

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
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

// TestOpenRetrievalFeedbackWeighterWaitsForLockedWALFile pins busy_timeout on
// every connection the opener creates: the journal_mode PRAGMA on its first
// connection waits behind a held database lock, and a replacement connection
// keeps the timeout.
func TestOpenRetrievalFeedbackWeighterWaitsForLockedWALFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.db")
	seed, _, err := openRetrievalFeedbackWeighter(t.Context(), path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
	sqlitetest.LockFileFor(t, path, 200*time.Millisecond)
	db, _, err := openRetrievalFeedbackWeighter(t.Context(), path)
	if err != nil {
		t.Fatalf("open behind a held database lock: %v", err)
	}
	sqlitetest.AssertNewConnectionBusyTimeout(t, db, 5*time.Second)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestOpenRetrievalFeedbackWeighterResecuresLooseSidecars pins the 0600
// re-securing of sidecars that already exist with looser bits. SQLite gives a
// new sidecar the DB file's mode, so only a sidecar left by an earlier, looser
// DB distinguishes the opener's SecureDBFiles call from its absence.
func TestOpenRetrievalFeedbackWeighterResecuresLooseSidecars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not portable on Windows")
	}
	path := filepath.Join(t.TempDir(), "feedback.db")
	if err := prepareRetrievalFeedbackDB(path); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	// An open WAL connection keeps the sidecars on disk.
	holder, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	holder.SetMaxOpenConns(1)
	if _, err := holder.ExecContext(t.Context(),
		"PRAGMA journal_mode=WAL; CREATE TABLE x(a); INSERT INTO x VALUES(1)"); err != nil {
		t.Fatalf("seed holder: %v", err)
	}
	sidecars := []string{path + "-wal", path + "-shm"}
	for _, sidecar := range sidecars {
		if _, err := os.Stat(sidecar); err != nil {
			t.Fatalf("sidecar %s missing while the holder is open: %v", sidecar, err)
		}
		if err := os.Chmod(sidecar, 0o644); err != nil {
			t.Fatalf("loosen %s: %v", sidecar, err)
		}
	}
	db, _, err := openRetrievalFeedbackWeighter(t.Context(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, sidecar := range sidecars {
		info, err := os.Stat(sidecar)
		if err != nil {
			t.Fatalf("stat %s: %v", sidecar, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("sidecar %s mode = %o after open, want 0600", sidecar, got)
		}
	}
}
