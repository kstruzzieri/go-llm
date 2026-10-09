package mcp

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
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
// journal mode "delete". The working directory is a temp dir because the
// opener prepares the raw path string, which for a file: URI is a relative
// path (#648).
func TestOpenRetrievalFeedbackWeighterRejectsNonWAL(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("#648: the opener prepares the raw file: string, which Windows rejects as a file name")
	}
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "x.db"))
	t.Chdir(t.TempDir())
	db, _, err := openRetrievalFeedbackWeighter(t.Context(), "file:"+path+"?nolock=1")
	if err == nil {
		_ = db.Close()
		t.Fatal("open of a nolock database succeeded; want a WAL-mode error")
	}
	if !strings.Contains(err.Error(), `journal_mode is "delete", want wal`) {
		t.Fatalf("err = %v, want the WAL-mode error", err)
	}
}
