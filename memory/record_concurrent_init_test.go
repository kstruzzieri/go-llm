package memory

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
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

// Reopening an initialized store only reads, so a read-only handle opens it
// and no reopen takes the claim's write lock (#631).
func TestOpenRecordStoreReopenReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	config := RecordStoreConfig{KeyDir: path + ".keys", Writer: WriterMCP}
	first, err := OpenRecordStore(t.Context(), path, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	uri, err := sqlitedsn.FileURL(path) // builds a URI SQLite accepts on Windows too
	if err != nil {
		t.Fatal(err)
	}
	uri.RawQuery = "mode=ro"
	rt, err := OpenRecordStore(t.Context(), uri.String(), config)
	if err != nil {
		t.Fatalf("read-only reopen: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
}

// An opener that finds no marker and then loses the claim to an initializer
// committing while it waits opens the store as initialized, and releases the
// write lock before it signs (#631).
func TestRecordInitializationLostClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.db")
	ctx := t.Context()
	db, err := OpenHardenedDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := NewStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	initializer, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = initializer.Close() })
	for _, statement := range []string{"BEGIN IMMEDIATE", `INSERT INTO memory_record_signing VALUES (1, 1)`} {
		if _, err := initializer.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	// The hold leaves a slow runner time to reach the claim before the commit.
	committed := make(chan error, 1)
	time.AfterFunc(time.Second, func() {
		_, err := initializer.ExecContext(context.Background(), "COMMIT")
		committed <- err
	})
	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	other.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = other.Close() })
	if _, err := other.Exec("PRAGMA busy_timeout=0"); err != nil {
		t.Fatal(err)
	}
	config := recordTestConfig(t)
	config.Initialize = false
	var lockErr error
	signer := &countingRecordSigner{Signer: config.Signer, beforeSign: func() {
		if _, lockErr = other.Exec("BEGIN IMMEDIATE"); lockErr == nil {
			_, lockErr = other.Exec("ROLLBACK")
		}
	}}
	config.Signer = signer
	start := time.Now()
	rt, err := OpenRecordStore(ctx, path, config)
	t.Logf("elapsed %v", time.Since(start))
	if cerr := <-committed; cerr != nil {
		t.Fatalf("commit initializer: %v", cerr)
	}
	if err != nil {
		t.Fatalf("open after losing the claim: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if signer.calls != 1 {
		t.Fatalf("signer calls = %d, want the configuration challenge only", signer.calls)
	}
	if lockErr != nil {
		t.Fatalf("write lock held while signing on the initialized path: %v", lockErr)
	}
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
	// The hold leaves a slow runner time to reach the claim before the release.
	released := make(chan error, 1)
	time.AfterFunc(time.Second, func() {
		_, err := writer.ExecContext(context.Background(), "ROLLBACK")
		released <- err
	})
	start := time.Now()
	rt, err := OpenRecordStore(ctx, path, RecordStoreConfig{Writer: WriterMCP})
	t.Logf("elapsed %v", time.Since(start))
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
