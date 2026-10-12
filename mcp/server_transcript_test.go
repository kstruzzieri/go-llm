// mcp/server_transcript_test.go
package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// newMockedServer builds a server with a permissive routing-fallback mock so
// NewServer succeeds without a live Ollama, plus the given extra options.
func newMockedServer(t *testing.T, extra ...Option) (*Server, func()) {
	t.Helper()
	mock := httptest.NewServer(routingFallbackHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})))
	opts := append([]Option{WithOllamaURL(mock.URL), WithRAGDisabled()}, extra...)
	s, err := NewServer(context.Background(), opts...)
	if err != nil {
		mock.Close()
		t.Fatalf("NewServer: %v", err)
	}
	return s, func() { _ = s.Close(); mock.Close() }
}

func TestWithTranscriptStore_OpensAndClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	s, cleanup := newMockedServer(t, WithTranscriptStore(path))
	defer cleanup()

	if s.transcriptStoreSnapshot() == nil {
		t.Fatal("transcript store was not opened")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if s.transcriptStoreSnapshot() != nil {
		t.Error("transcript store not cleared after Close")
	}
}

func TestWithTranscriptStore_OpenFailureIsFatal(t *testing.T) {
	mock := httptest.NewServer(routingFallbackHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})))
	defer mock.Close()
	// A path that is an existing directory cannot be opened as a SQLite file.
	dir := t.TempDir()
	s, err := NewServer(context.Background(), WithOllamaURL(mock.URL), WithRAGDisabled(), WithTranscriptStore(dir))
	if err == nil {
		_ = s.Close()
		t.Fatal("NewServer should fail when the transcript DB path cannot be opened")
	}
}

// TestWithTranscriptStore_FileURICreatesNoDecoy: go-llm-mcp -conversation-db
// accepts a file: URI; startup must create the database's directory, not a
// directory named after the raw URI in the working directory (#648).
func TestWithTranscriptStore_FileURICreatesNoDecoy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "conv.db")
	cwd := t.TempDir()
	t.Chdir(cwd)
	_, cleanup := newMockedServer(t, WithTranscriptStore("file:"+filepath.ToSlash(path)))
	defer cleanup()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("transcript database not at %s: %v", path, err)
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
		t.Fatalf("working directory after startup: %v, %v; want it empty", entries, err)
	}
}
