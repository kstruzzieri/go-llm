package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
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
