package mcp

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

func TestOpenRetrievalFeedbackWeighterConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		db, _, err := openRetrievalFeedbackWeighter(ctx, path)
		if err != nil {
			return nil, err
		}
		return db, nil
	})
}

// A connection that cannot use WAL must fail the open, not run the store with
// a rollback journal: nolock=1 turns locking off, and SQLite then reports
// journal mode "delete". The working directory is empty so a decoy named
// after the raw URI would show (#648).
func TestOpenRetrievalFeedbackWeighterRejectsNonWAL(t *testing.T) {
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "x.db"))
	cwd := t.TempDir()
	t.Chdir(cwd)
	db, _, err := openRetrievalFeedbackWeighter(t.Context(), "file:"+path+"?nolock=1")
	if err == nil {
		_ = db.Close()
		t.Fatal("open of a nolock database succeeded; want a WAL-mode error")
	}
	if !strings.Contains(err.Error(), `journal_mode is "delete", want wal`) {
		t.Fatalf("err = %v, want the WAL-mode error", err)
	}
	if entries, err := os.ReadDir(cwd); err != nil || len(entries) != 0 {
		t.Fatalf("working directory after the open: %v, %v; want it empty", entries, err)
	}
}

// TestOpenRetrievalFeedbackWeighterHardensTheFileSQLiteOpens: retrieval
// feedback is behavioral telemetry; the 0600 promise covers the file SQLite
// opens for every input form (#648).
func TestOpenRetrievalFeedbackWeighterHardensTheFileSQLiteOpens(t *testing.T) {
	sqlitetest.RunHardenedPaths(t, sqlitetest.HardenedPathsOptions{
		URI:      func(name string) string { return "file:" + name },
		MemoryOK: true,
		Open: func(ctx context.Context, input string) (io.Closer, error) {
			db, _, err := openRetrievalFeedbackWeighter(ctx, input)
			if err != nil {
				return nil, err
			}
			return db, nil
		},
	})
}
