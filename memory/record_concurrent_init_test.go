package memory

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/sqlitetest"
)

// Processes opening one fresh record-store path at once all succeed; none
// reports a false ErrIncompleteInitialization while another is initializing
// (#631).
func TestOpenRecordStoreConcurrentFirstOpens(t *testing.T) {
	sqlitetest.RunConcurrentFirstOpens(t, 4, 5, func(ctx context.Context, path string) (io.Closer, error) {
		return OpenRecordStore(ctx, path, RecordStoreConfig{Writer: WriterMCP})
	})
}

// A first-time open that meets another connection's write transaction waits
// for it under the busy timeout. Before #631 the import read first, so its
// first write was a read-to-write upgrade that SQLite does not retry through
// the busy handler: the open failed after publishing its key, leaving a real
// incomplete initialization.
func TestOpenRecordStoreFirstOpenWaitsForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	ctx := t.Context()
	db, err := OpenHardenedDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// A current schema with no signing marker: migrations on the next open
	// only read, so the held write lock reaches signing initialization.
	if _, err := NewStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		_, err := writer.ExecContext(context.Background(), "ROLLBACK")
		released <- err
	}()
	rt, err := OpenRecordStore(ctx, path, RecordStoreConfig{Writer: WriterMCP})
	if rerr := <-released; rerr != nil {
		t.Fatalf("release writer: %v", rerr)
	}
	if err != nil {
		t.Fatalf("first open beside a writer: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
}
