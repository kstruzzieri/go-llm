package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func TestMigrations_StrictlyIncreasingVersions(t *testing.T) {
	t.Parallel()
	for i := 1; i < len(migrations); i++ {
		if got, previous := migrations[i].version, migrations[i-1].version; got <= previous {
			t.Errorf("migrations[%d].version = %d, want greater than migrations[%d].version (%d)", i, got, i-1, previous)
		}
	}
}

func openMigrationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	// Finish WAL setup before racing migration runners, not journal-mode changes.
	for _, query := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=WAL"} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func newStoresConcurrently(t *testing.T, dbs ...*sql.DB) []*SQLiteStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, len(dbs))
	stores := make([]*SQLiteStore, len(dbs))
	for i, db := range dbs {
		go func() {
			<-start
			store, err := NewStore(ctx, db)
			stores[i] = store
			errs <- err
		}()
	}
	close(start)
	for range dbs {
		select {
		case err := <-errs:
			if err != nil {
				t.Errorf("concurrent NewStore: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if t.Failed() {
		t.FailNow()
	}
	return stores
}

func assertMigrationVersions(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM memory_schema_version ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var versions []string
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, fmt.Sprint(v))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(versions, ","); got != want {
		t.Fatalf("migration versions = %q, want %q", got, want)
	}
}

func TestNewStore_ConcurrentMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	newStoresConcurrently(t, a, b)
	for _, db := range []*sql.DB{a, b} {
		assertMigrationVersions(t, db, "1,2,3")
	}
}

func execMigrationSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("Exec(%q): %v", query, err)
	}
}

func assertMigrationSQL(t *testing.T, db *sql.DB, query, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatalf("Query(%q): %v", query, err)
	}
	if got != want {
		t.Fatalf("Query(%q) = %q, want %q", query, got, want)
	}
}

func assertMigrationSQLiteError(t *testing.T, err error, want int) {
	t.Helper()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&255 != want {
		t.Fatalf("SQLite error = %v, want primary code %d", err, want)
	}
}

func TestApplyMigration_ClaimBeforeDDLAndStaleSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.db")
	a, b, competitor := openMigrationDB(t, path), openMigrationDB(t, path), openMigrationDB(t, path)
	execMigrationSQL(t, a, "CREATE TABLE memory_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
	execMigrationSQL(t, competitor, "PRAGMA busy_timeout=0")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	claimed, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	result := make(chan error, 1)
	go func() {
		result <- applyMigration(ctx, a, migration{version: 1, description: "first", fn: func(tx *sql.Tx) error {
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
	case err := <-result:
		t.Fatalf("applyMigration before gate: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	_, err := competitor.ExecContext(ctx, "BEGIN IMMEDIATE")
	if err == nil {
		execMigrationSQL(t, competitor, "ROLLBACK")
	}
	assertMigrationSQLiteError(t, err, 5)
	// Reading on another WAL connection cannot see the uncommitted claim.
	assertMigrationVersions(t, b, "")
	unblock()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Submit the already-selected version directly, bypassing any fast path.
	duplicate := errors.New("duplicate migration executed")
	if err := applyMigration(ctx, b, migration{version: 1, description: "loser", fn: func(*sql.Tx) error { return duplicate }}); err != nil {
		t.Fatalf("applyMigration(stale version 1): %v", err)
	}
	assertMigrationVersions(t, b, "1")
	assertMigrationSQL(t, b, "SELECT value FROM applied_once", "winner")
	assertMigrationSQL(t, b, "SELECT description FROM memory_schema_version WHERE version=1", "first")
	execMigrationSQL(t, competitor, "BEGIN IMMEDIATE")
	execMigrationSQL(t, competitor, "ROLLBACK")
}

func TestApplyMigration_ConcurrentSameVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlap.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	execMigrationSQL(t, a, "CREATE TABLE memory_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	m := migration{version: 1, description: "once", fn: func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE applied_once (value TEXT); INSERT INTO applied_once VALUES ('once')")
		return err
	}}
	for _, db := range []*sql.DB{a, b} {
		go func() {
			<-start
			results <- applyMigration(ctx, db, m)
		}()
	}
	close(start)
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("concurrent applyMigration: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	assertMigrationVersions(t, a, "1")
	assertMigrationSQL(t, b, "SELECT value FROM applied_once", "once")
}

func TestRunMigrations_RollbackStepAndRetry(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "rollback.db"))
	boom := errors.New("migration failed")
	list := []migration{
		{version: 1, description: "durable", fn: func(tx *sql.Tx) error {
			_, err := tx.Exec("CREATE TABLE durable (value TEXT); INSERT INTO durable VALUES ('preserved')")
			return err
		}},
		{version: 2, description: "failed", fn: func(tx *sql.Tx) error {
			if _, err := tx.Exec("CREATE TABLE tentative (value TEXT)"); err != nil {
				return err
			}
			return boom
		}},
	}
	if err := runMigrationsWith(t.Context(), db, list); !errors.Is(err, boom) {
		t.Fatalf("runMigrationsWith(failing step) = %v, want sentinel", err)
	}
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Fatalf("connections retained after failed migration = %d, want 0", inUse)
	}
	assertMigrationVersions(t, db, "1")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='tentative'", "0")
	assertMigrationSQL(t, db, "SELECT value FROM durable", "preserved")
	list[1].fn = func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE tentative (value TEXT); INSERT INTO tentative VALUES ('retried')")
		return err
	}
	if err := runMigrationsWith(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "1,2")
	assertMigrationSQL(t, db, "SELECT value FROM tentative", "retried")
}

func TestRunMigrations_CanceledStep(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "canceled.db"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := runMigrationsWith(ctx, db, []migration{{version: 1, description: "cancel", fn: func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE tentative (value TEXT)"); err != nil {
			return err
		}
		cancel()
		return nil
	}}})
	if !errors.Is(err, context.Canceled) && !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("runMigrationsWith(canceled step) = %v, want cancellation", err)
	}
	assertMigrationVersions(t, db, "")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='tentative'", "0")
	execMigrationSQL(t, db, "CREATE TABLE usable (value TEXT); INSERT INTO usable VALUES ('after cancel')")
	assertMigrationSQL(t, db, "SELECT value FROM usable", "after cancel")
}

func TestNewStore_InvalidContext(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{{"nil", nil}, {"canceled", canceled}} {
		t.Run(tc.name, func(t *testing.T) {
			db := openMigrationDB(t, filepath.Join(t.TempDir(), "context.db"))
			_, err := NewStore(tc.ctx, db)
			if err == nil || (tc.ctx != nil && !errors.Is(err, context.Canceled)) {
				t.Fatalf("NewStore(%s context) = %v, want context error", tc.name, err)
			}
			assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='memory_schema_version'", "0")
		})
	}
}

func TestRunMigrations_FastPathAndReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "current.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	if _, err := NewStore(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	execMigrationSQL(t, b, "PRAGMA busy_timeout=0")
	execMigrationSQL(t, a, "BEGIN IMMEDIATE")
	_, openErr := NewStore(t.Context(), b)
	execMigrationSQL(t, a, "ROLLBACK")
	if openErr != nil {
		t.Fatalf("NewStore(current schema, competing writer): %v", openErr)
	}
	uri := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	ro := openMigrationDB(t, uri.String())
	if _, err := NewStore(t.Context(), ro); err != nil {
		t.Fatalf("NewStore(current mode=ro): %v", err)
	}
	assertMigrationVersions(t, ro, "1,2,3")
	// A real pending step requires a write, even though the old schema is readable.
	err := runMigrationsWith(t.Context(), ro, []migration{{version: 9, description: "pending", fn: func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE forbidden (id INTEGER)")
		return err
	}}})
	assertMigrationSQLiteError(t, err, 8)
	assertMigrationVersions(t, ro, "1,2,3")
	assertMigrationSQL(t, ro, "SELECT COUNT(*) FROM sqlite_schema WHERE name='forbidden'", "0")
}

func TestRunMigrations_NonconsecutiveVersions(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "versions.db"))
	execMigrationSQL(t, db, "CREATE TABLE memory_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL); INSERT INTO memory_schema_version VALUES (2, 'historical', 123)")
	list := []migration{
		{version: 1, fn: func(*sql.Tx) error { return errors.New("historical step reran") }},
		{version: 7, description: "seven", fn: func(tx *sql.Tx) error {
			_, err := tx.Exec("CREATE TABLE seventh (value TEXT); INSERT INTO seventh VALUES ('seven')")
			return err
		}},
	}
	if err := runMigrationsWith(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "2,7")
	assertMigrationSQL(t, db, "SELECT value FROM seventh", "seven")
	execMigrationSQL(t, db, "INSERT INTO memory_schema_version VALUES (9, 'future', 456)")
	if err := runMigrationsWith(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "2,7,9")
	assertMigrationSQL(t, db, "SELECT applied_at FROM memory_schema_version WHERE version=2", "123")
}

func TestRunMigrations_MalformedVersionTable(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "malformed.db"))
	execMigrationSQL(t, db, "CREATE TABLE memory_schema_version (version TEXT); INSERT INTO memory_schema_version VALUES ('broken')")
	if _, err := NewStore(t.Context(), db); err == nil {
		t.Fatal("NewStore(malformed version) succeeded")
	}
	assertMigrationSQL(t, db, "SELECT version FROM memory_schema_version", "broken")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='memories'", "0")
}

func TestNewStore_ConcurrentSchemaV2Fixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	a := legacyRecordDB(t, path, 2)
	// Complete the literal v2 fixture with the original v1 memory schema.
	execMigrationSQL(t, a, `
        PRAGMA busy_timeout=5000;
        PRAGMA journal_mode=WAL;
        INSERT INTO memory_schema_version VALUES (1, 'literal baseline', 1);
        CREATE TABLE memories (
            id TEXT PRIMARY KEY, text TEXT NOT NULL, scope TEXT NOT NULL,
            workspace_id TEXT NOT NULL DEFAULT '', source_session_id TEXT NOT NULL DEFAULT '',
            created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, deleted_at INTEGER NOT NULL DEFAULT 0);
        CREATE INDEX idx_memories_live ON memories(scope, workspace_id) WHERE deleted_at=0;
        CREATE VIRTUAL TABLE memories_fts USING fts5(id UNINDEXED, text, tokenize='porter');
        INSERT INTO memories VALUES ('prior', 'historical memory', 'global', '', 'session', 123, 456, 0);
        INSERT INTO memories_fts VALUES ('prior', 'historical memory');`)
	b := openMigrationDB(t, path)
	newStoresConcurrently(t, a, b)
	for _, db := range []*sql.DB{a, b} {
		assertMigrationVersions(t, db, "1,2,3")
		assertMigrationSQL(t, db, "SELECT COUNT(*) FROM memory_records", "2")
		// Pin raw metadata, provenance, timestamps, tombstones and signature
		// defaults without going through record scanning or signing helpers.
		for _, tc := range []struct{ id, want string }{
			{"legacy-0000", `["legacy-0000","working","legacy needle","","workspace","historical-session","user","historical-claim",0,0,"","  ",1234,5678,9,0,"","","","","",""]`},
			{"legacy-0001", `["legacy-0001","working","legacy needle","","workspace","historical-session","user","historical-claim",0,0,"","  ",1234,5678,9,1,"","","","","",""]`},
		} {
			var got string
			err := db.QueryRow(`SELECT json_array(id,kind,content,namespace,workspace_id,session_id,source_kind,source_id,
                source_start,source_end,source_hash,metadata,created_at,updated_at,expires_at,deleted_at,
                origin_tool,origin_session_id,trust_class,signature_alg,signature_key_id,hex(signature))
                FROM memory_records WHERE id=?`, tc.id).Scan(&got)
			if err != nil || got != tc.want {
				t.Fatalf("record %s = %q, err=%v; want %q", tc.id, got, err, tc.want)
			}
		}
		assertMigrationSQL(t, db, "SELECT COUNT(*) FROM memory_records_fts", "1")
		assertMigrationSQL(t, db, "SELECT json_array(id,content) FROM memory_records_fts WHERE memory_records_fts MATCH 'needle'", `["legacy-0000","legacy needle"]`)
		assertMigrationSQL(t, db, "SELECT json_array(id,text,scope,workspace_id,source_session_id,created_at,updated_at,deleted_at) FROM memories", `["prior","historical memory","global","","session",123,456,0]`)
		assertMigrationSQL(t, db, "SELECT json_array(id,text) FROM memories_fts WHERE memories_fts MATCH 'historical'", `["prior","historical memory"]`)
		assertMigrationSQL(t, db, "SELECT COUNT(*) FROM memory_record_signing", "0")
		assertMigrationSQL(t, db, "SELECT COUNT(*) FROM memory_schema_version WHERE version IN (1,2) AND applied_at=1", "2")
	}
}
