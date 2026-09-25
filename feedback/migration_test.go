package feedback

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRunMigrations(t *testing.T) {
	db := openTestDB(t)

	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	// Verify schema version was recorded.
	ver, err := currentSchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatalf("currentSchemaVersion: %v", err)
	}
	if ver != 1 {
		t.Errorf("schema version = %d, want 1", ver)
	}

	// Verify tables exist by inserting a row into each.
	if _, err := db.Exec(
		`INSERT INTO feedback_retrievals (retrieval_id, query, chunk_keys, created_at)
		 VALUES ('r1', 'test query', 'c1', 1000)`,
	); err != nil {
		t.Errorf("insert into feedback_retrievals: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO feedback_signals (retrieval_id, chunk_key, signal_kind, strength, created_at)
		 VALUES ('r1', 'c1', 'completion_accepted', 0.8, 1000)`,
	); err != nil {
		t.Errorf("insert into feedback_signals: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO feedback_aggregates (chunk_key) VALUES ('c1')`,
	); err != nil {
		t.Errorf("insert into feedback_aggregates: %v", err)
	}
}

func TestRunMigrationsIdempotent(t *testing.T) {
	db := openTestDB(t)

	for i := 0; i < 3; i++ {
		if err := runMigrations(t.Context(), db); err != nil {
			t.Fatalf("runMigrations (pass %d): %v", i+1, err)
		}
	}

	ver, err := currentSchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatalf("currentSchemaVersion: %v", err)
	}
	if ver != 1 {
		t.Errorf("schema version = %d, want 1", ver)
	}
}

func openMigrationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	// Configure WAL sequentially, before racing migration calls. The timeout
	// belongs in the DSN so replacement connections inherit it too.
	execMigrationSQL(t, db, "PRAGMA journal_mode=WAL")
	return db
}

func execMigrationSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatalf("Exec(%q): %v", query, err)
	}
}

func assertMigrationSQL(t *testing.T, db *sql.DB, query, want string) {
	t.Helper()
	var got string
	if err := db.QueryRowContext(t.Context(), query).Scan(&got); err != nil {
		t.Fatalf("Query(%q): %v", query, err)
	}
	if got != want {
		t.Fatalf("Query(%q) = %q, want %q", query, got, want)
	}
}

func assertMigrationVersions(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	assertMigrationSQL(t, db, "SELECT COALESCE(group_concat(version), '') FROM (SELECT version FROM feedback_schema_version ORDER BY version)", want)
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Fatalf("retained connections = %d", inUse)
	}
}

func assertMigrationSQLiteError(t *testing.T, err error, want int) {
	t.Helper()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&255 != want {
		t.Fatalf("SQLite error = %v, want primary code %d", err, want)
	}
}

func migrationResult(t *testing.T, ctx context.Context, results <-chan error) error {
	t.Helper()
	select {
	case err := <-results:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return ctx.Err()
	}
}

func newMigrationStore(ctx context.Context, db *sql.DB) error {
	_, err := NewSignalStore(ctx, db)
	return err
}

func TestMigrationConcurrentFreshStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start, results := make(chan struct{}), make(chan error, 2)
	for _, db := range []*sql.DB{a, b} {
		go func() { <-start; results <- newMigrationStore(ctx, db) }()
	}
	close(start)
	for range 2 {
		if err := migrationResult(t, ctx, results); err != nil {
			t.Errorf("concurrent store: %v", err)
		}
	}
	for _, db := range []*sql.DB{a, b} {
		assertMigrationVersions(t, db, "1")
	}
	assertMigrationSQL(t, a, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name IN ('feedback_retrievals','feedback_signals','feedback_aggregates')", "3")
}

func TestMigrationClaimBeforeDDLAndStaleSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	execMigrationSQL(t, a, "CREATE TABLE feedback_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	claimed, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	winner, loser := make(chan error, 1), make(chan error, 1)
	go func() {
		winner <- applyMigration(ctx, a, migration{version: 7, description: "winner", fn: func(tx *sql.Tx) error {
			close(claimed)
			select {
			case <-release:
				_, err := tx.Exec("CREATE TABLE applied_once (value TEXT); INSERT INTO applied_once VALUES ('winner')")
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
	}()
	select {
	case <-claimed:
	case err := <-winner:
		t.Fatalf("before migration gate: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Before the first DDL statement, the winner must already own the write
	// lock, and its version claim must still be invisible to another handle.
	execMigrationSQL(t, b, "PRAGMA busy_timeout=0")
	_, err := b.ExecContext(ctx, "BEGIN IMMEDIATE")
	if err == nil {
		execMigrationSQL(t, b, "ROLLBACK")
	}
	assertMigrationSQLiteError(t, err, 5)
	assertMigrationVersions(t, b, "")
	execMigrationSQL(t, b, "PRAGMA busy_timeout=5000")
	duplicate := errors.New("duplicate migration executed")
	stale := migration{version: 7, description: "loser", fn: func(*sql.Tx) error { return duplicate }}
	go func() { loser <- applyMigration(ctx, b, stale) }()
	// Wait for the competing operation to acquire its sole pool connection
	// while the writer is held. No sleep decides whether the test succeeds.
	for b.Stats().InUse == 0 {
		select {
		case err := <-loser:
			t.Fatalf("competing migration returned early: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
			runtime.Gosched()
		}
	}
	unblock()
	for _, results := range []chan error{winner, loser} {
		if err := migrationResult(t, ctx, results); err != nil {
			t.Fatal(err)
		}
	}
	// Bypass the runner's version fast path: a stale decision must skip DDL.
	if err := applyMigration(ctx, b, stale); err != nil {
		t.Fatalf("stale selection: %v", err)
	}
	assertMigrationVersions(t, b, "7")
	assertMigrationSQL(t, b, "SELECT COUNT(*) FROM applied_once", "1")
	assertMigrationSQL(t, b, "SELECT value FROM applied_once", "winner")
	assertMigrationSQL(t, b, "SELECT description FROM feedback_schema_version WHERE version=7", "winner")
	execMigrationSQL(t, b, "BEGIN IMMEDIATE; ROLLBACK")
}

func TestMigrationRollbackAndRetry(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "rollback.db"))
	list := append([]migration(nil), migrations...)
	first := list[0].fn
	list[0].fn = func(tx *sql.Tx) error {
		if err := first(tx); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO feedback_retrievals VALUES ('old','seed','chunk',123); CREATE INDEX custom_legacy_index ON feedback_retrievals(query)")
		return err
	}
	boom := errors.New("migration failed")
	list = append(list, migration{version: 7, description: "tentative", fn: func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE tentative (value TEXT); UPDATE feedback_retrievals SET query='lost'"); err != nil {
			return err
		}
		return boom
	}})
	if err := runMigrationsWith(t.Context(), db, list); !errors.Is(err, boom) {
		t.Fatalf("failed step = %v, want sentinel", err)
	}
	assertMigrationVersions(t, db, "1")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='tentative'", "0")
	assertMigrationSQL(t, db, "SELECT query FROM feedback_retrievals WHERE retrieval_id='old'", "seed")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='custom_legacy_index'", "1")
	list[len(list)-1].fn = func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE tentative (value TEXT); INSERT INTO tentative VALUES ('retried')")
		return err
	}
	if err := runMigrationsWith(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "1,7")
	assertMigrationSQL(t, db, "SELECT value FROM tentative", "retried")
	assertMigrationSQL(t, db, "SELECT query FROM feedback_retrievals WHERE retrieval_id='old'", "seed")
}

func TestMigrationCanceledStep(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "cancel.db"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := runMigrationsWith(ctx, db, []migration{{version: 7, description: "cancel", fn: func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE tentative (id INTEGER)"); err != nil {
			return err
		}
		cancel()
		return nil
	}}})
	if !errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("canceled migration = %v", err)
	}
	// A query waits for database/sql's asynchronous cancellation rollback.
	assertMigrationVersions(t, db, "")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='tentative'", "0")
	if err := newMigrationStore(t.Context(), db); err != nil {
		t.Fatalf("retry after cancellation: %v", err)
	}
}

func TestMigrationInvalidContext(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{{"nil", nil}, {"canceled", canceled}} {
		t.Run(tc.name, func(t *testing.T) {
			db := openMigrationDB(t, filepath.Join(t.TempDir(), "context.db"))
			err := newMigrationStore(tc.ctx, db)
			if err == nil || (tc.ctx != nil && !errors.Is(err, context.Canceled)) {
				t.Fatalf("invalid context = %v", err)
			}
			assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='feedback_schema_version'", "0")
		})
	}
}

func TestMigrationCurrentSchemaReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	if err := newMigrationStore(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	execMigrationSQL(t, b, "PRAGMA busy_timeout=0")
	execMigrationSQL(t, a, "BEGIN IMMEDIATE")
	err := newMigrationStore(t.Context(), b)
	execMigrationSQL(t, a, "ROLLBACK")
	if err != nil {
		t.Fatalf("current schema behind writer: %v", err)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	ro, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	ro.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = ro.Close() })
	if err := newMigrationStore(t.Context(), ro); err != nil {
		t.Fatalf("current read-only schema: %v", err)
	}
	assertMigrationVersions(t, ro, "1")
	pending := migration{version: 7, description: "pending", fn: func(tx *sql.Tx) error { _, err := tx.Exec("CREATE TABLE forbidden (id INTEGER)"); return err }}
	assertMigrationSQLiteError(t, runMigrationsWith(t.Context(), ro, []migration{pending}), 8)
	assertMigrationVersions(t, ro, "1")
	assertMigrationSQL(t, ro, "SELECT COUNT(*) FROM sqlite_schema WHERE name='forbidden'", "0")
}

func TestMigrationLockAndIOErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	execMigrationSQL(t, a, "CREATE TABLE feedback_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
	execMigrationSQL(t, a, "BEGIN IMMEDIATE")
	execMigrationSQL(t, b, "PRAGMA busy_timeout=0")
	err := applyMigration(t.Context(), b, migration{version: 7, description: "blocked", fn: func(*sql.Tx) error { return errors.New("must not run") }})
	assertMigrationSQLiteError(t, err, 5)
	assertMigrationVersions(t, b, "")
	// Keep the writer held through a real, positive busy timeout. This tests
	// SQLite waiting itself, without a sleep deciding when to release a writer.
	execMigrationSQL(t, b, "PRAGMA busy_timeout=50")
	started := time.Now()
	err = applyMigration(t.Context(), b, migration{version: 7, description: "timed out", fn: func(*sql.Tx) error { return errors.New("must not run") }})
	elapsed := time.Since(started)
	execMigrationSQL(t, a, "ROLLBACK")
	assertMigrationSQLiteError(t, err, 5)
	if elapsed < 25*time.Millisecond {
		t.Fatalf("claim returned in %v without waiting for the positive busy timeout", elapsed)
	}
	assertMigrationVersions(t, b, "")
	if err := newMigrationStore(t.Context(), b); err != nil {
		t.Fatalf("retry after lock: %v", err)
	}
	missing, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "missing-directory", "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = missing.Close() })
	assertMigrationSQLiteError(t, newMigrationStore(t.Context(), missing), 14)
}
