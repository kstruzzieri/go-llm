package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
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

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open :memory: db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openMigrationDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	return openMigrationDBJournal(t, path, "WAL")
}

func openMigrationDBJournal(t *testing.T, path, journalMode string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	// Finish journal setup before racing migration runners, not journal-mode changes.
	for _, query := range []string{"PRAGMA busy_timeout=5000", "PRAGMA journal_mode=" + journalMode} {
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
	rows, err := db.Query(`SELECT version FROM conversation_schema_version ORDER BY version`)
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
	path := filepath.Join(t.TempDir(), "conversations.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	newStoresConcurrently(t, a, b)
	for _, db := range []*sql.DB{a, b} {
		assertMigrationVersions(t, db, "1,2,3,4,5")
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
	execMigrationSQL(t, a, "CREATE TABLE conversation_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
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
	assertMigrationSQL(t, b, "SELECT description FROM conversation_schema_version WHERE version=1", "first")
	execMigrationSQL(t, competitor, "BEGIN IMMEDIATE")
	execMigrationSQL(t, competitor, "ROLLBACK")
}

// The literal #543 criterion: a competing migrator submits its claim while the
// winner still holds the write lock mid-step, waits on the busy handler, then
// skips the committed step instead of failing or rerunning it. Both journal
// modes, since the runner does not depend on WAL.
func TestApplyMigration_WaitsForWriterThenSkips(t *testing.T) {
	for _, journalMode := range []string{"WAL", "DELETE"} {
		t.Run(journalMode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "contended.db")
			a, b := openMigrationDBJournal(t, path, journalMode), openMigrationDBJournal(t, path, journalMode)
			execMigrationSQL(t, a, "CREATE TABLE conversation_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			claimed, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			winner, loser := make(chan error, 1), make(chan error, 1)
			go func() {
				winner <- applyMigration(ctx, a, migration{version: 1, description: "winner", fn: func(tx *sql.Tx) error {
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
				t.Fatalf("applyMigration before gate: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			reran := errors.New("competing step reran")
			go func() {
				loser <- applyMigration(ctx, b, migration{version: 1, description: "loser", fn: func(*sql.Tx) error { return reran }})
			}()
			// The competitor must be blocked behind the uncommitted claim, not decided.
			select {
			case err := <-loser:
				t.Fatalf("competing applyMigration returned before the winner committed: %v", err)
			case <-time.After(500 * time.Millisecond):
			}
			unblock()
			for _, result := range []chan error{winner, loser} {
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			assertMigrationVersions(t, b, "1")
			assertMigrationSQL(t, b, "SELECT value FROM applied_once", "winner")
			assertMigrationSQL(t, b, "SELECT description FROM conversation_schema_version WHERE version=1", "winner")
		})
	}
}

func TestApplyMigration_ConcurrentSameVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "overlap.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	execMigrationSQL(t, a, "CREATE TABLE conversation_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
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
			assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='conversation_schema_version'", "0")
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
	assertMigrationVersions(t, ro, "1,2,3,4,5")
	// A real pending step requires a write, even though the old schema is readable.
	err := runMigrationsWith(t.Context(), ro, []migration{{version: 9, description: "pending", fn: func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE forbidden (id INTEGER)")
		return err
	}}})
	assertMigrationSQLiteError(t, err, 8)
	assertMigrationVersions(t, ro, "1,2,3,4,5")
	assertMigrationSQL(t, ro, "SELECT COUNT(*) FROM sqlite_schema WHERE name='forbidden'", "0")
}

func TestRunMigrations_NonconsecutiveVersions(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "versions.db"))
	execMigrationSQL(t, db, "CREATE TABLE conversation_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL); INSERT INTO conversation_schema_version VALUES (2, 'historical', 123)")
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
	execMigrationSQL(t, db, "INSERT INTO conversation_schema_version VALUES (9, 'future', 456)")
	if err := runMigrationsWith(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "2,7,9")
	assertMigrationSQL(t, db, "SELECT applied_at FROM conversation_schema_version WHERE version=2", "123")
}

func TestRunMigrations_MalformedVersionTable(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "malformed.db"))
	execMigrationSQL(t, db, "CREATE TABLE conversation_schema_version (version TEXT); INSERT INTO conversation_schema_version VALUES ('broken')")
	if _, err := NewStore(t.Context(), db); err == nil {
		t.Fatal("NewStore(malformed version) succeeded")
	}
	assertMigrationSQL(t, db, "SELECT version FROM conversation_schema_version", "broken")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='conversations'", "0")
}

func TestRunMigrations_FreshDB(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&count)
	if err != nil {
		t.Fatalf("conversations table missing: %v", err)
	}

	var version int
	err = db.QueryRow(`SELECT MAX(version) FROM conversation_schema_version`).Scan(&version)
	if err != nil {
		t.Fatalf("version query failed: %v", err)
	}
	if version != 5 {
		t.Fatalf("schema version = %d, want 5", version)
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("first runMigrations() error: %v", err)
	}
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("second runMigrations() error: %v", err)
	}

	var version int
	err := db.QueryRow(`SELECT MAX(version) FROM conversation_schema_version`).Scan(&version)
	if err != nil {
		t.Fatalf("version query failed: %v", err)
	}
	if version != 5 {
		t.Fatalf("schema version = %d, want 5", version)
	}
}

func TestRunMigrations_IndexExists(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_conversations_updated_id'`,
	).Scan(&count)
	if err != nil || count != 1 {
		t.Fatalf("expected idx_conversations_updated_id index, count=%d err=%v", count, err)
	}
}

func TestRunMigrations_SearchTablesExist(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	for _, name := range []string{"conversation_search", "conversation_fts"} {
		var count int
		err := db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE name = ?`,
			name,
		).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("expected %s table, count=%d err=%v", name, count, err)
		}
	}
}

func TestRunMigrations_DurableSummaryColumnsExist(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}

	for _, name := range []string{"summary_content", "summary_message_count"} {
		var count int
		err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('conversations') WHERE name = ?`, name).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("expected conversations.%s column, count=%d err=%v", name, count, err)
		}
	}
}

// TestRunMigrations_V2BackwardCompat verifies that a database created at
// schema v2 (no summary columns) upgrades in place and that pre-v3 rows
// load with a nil DurableSummary. This is the spec's backward-compatibility
// requirement: later migrations are additive and never rewrite existing data.
func TestRunMigrations_V2BackwardCompat(t *testing.T) {
	db := openTestDB(t)

	// Build a v2-state database: version table + v1/v2 schema, stamped at v2,
	// with one pre-v3 conversation row (conversations has no summary columns yet).
	if _, err := db.Exec(`CREATE TABLE conversation_schema_version (
		version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create version table: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateV1(tx); err != nil {
		t.Fatalf("migrateV1: %v", err)
	}
	if err := migrateV2(tx); err != nil {
		t.Fatalf("migrateV2: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO conversation_schema_version (version, description, applied_at)
		VALUES (1, 'v1', 0), (2, 'v2', 0)`); err != nil {
		t.Fatalf("stamp versions: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, messages, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"old1", "old title", `[{"role":"user","content":"hi"}]`, 1, 2,
	); err != nil {
		t.Fatalf("insert pre-v3 row: %v", err)
	}

	// Only v3, v4 and v5 should apply now (v1/v2 already stamped).
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM conversation_schema_version`).Scan(&version); err != nil {
		t.Fatalf("version query failed: %v", err)
	}
	if version != 5 {
		t.Fatalf("schema version = %d, want 5", version)
	}

	// The pre-v3 row loads with a nil DurableSummary and preserved content.
	// NewStore re-runs migrations idempotently (already at v5).
	store, err := NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
	conv, err := store.Load(context.Background(), "old1")
	if err != nil {
		t.Fatalf("Load pre-v3 row: %v", err)
	}
	if conv.DurableSummary != nil {
		t.Fatalf("pre-v3 row must load with nil DurableSummary, got %+v", conv.DurableSummary)
	}
	if conv.Title != "old title" || len(conv.Messages) != 1 {
		t.Fatalf("pre-v3 row content not preserved: title=%q msgs=%d", conv.Title, len(conv.Messages))
	}
}

func TestNewStore_ConcurrentSchemaV3Fixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "schema-v3.sql"))
	if err != nil {
		t.Fatalf("ReadFile(schema-v3.sql) error: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "conversation.db")
	db := openMigrationDB(t, dbPath)
	if _, err := db.Exec(string(fixture)); err != nil {
		_ = db.Close()
		t.Fatalf("Exec(schema-v3.sql) error: %v", err)
	}

	const (
		id                 = "workspace:schema-v3-fixture"
		wantTitle          = "Prior schema fixture"
		wantMessages       = `[{"role":"user","content":"Inspect the calibrationtoken file."},{"role":"assistant","content":"","tool_calls":[{"id":"call_fixture","type":"function","function":{"name":"read_file","arguments":{"path":"fixture.go"}}}]},{"role":"tool","content":"package fixture","tool_name":"read_file","tool_call_id":"call_fixture"}]`
		wantSummary        = "Earlier messages established the fixture contract."
		wantSummaryCount   = 7
		wantSearchBody     = "user: Inspect the calibrationtoken file.\nassistant:  [{\"id\":\"call_fixture\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":{\"path\":\"fixture.go\"}}}]\ntool: package fixture read_file call_fixture\nEarlier messages established the fixture contract."
		wantMessageCount   = 3
		wantCreatedMillis  = int64(1700000000123)
		wantUpdatedMillis  = int64(1700000010456)
		wantSchemaVersion  = 5
		wantVersionRecords = 5
	)

	assertState := func(phase string, db *sql.DB, store *SQLiteStore) {
		t.Helper()

		var version, versionRecords int
		if err := db.QueryRow(`SELECT MAX(version), COUNT(*) FROM conversation_schema_version`).Scan(&version, &versionRecords); err != nil {
			t.Fatalf("%s schema version query error: %v", phase, err)
		}
		if version != wantSchemaVersion || versionRecords != wantVersionRecords {
			t.Errorf("%s schema versions = (max=%d, count=%d), want (max=%d, count=%d)", phase, version, versionRecords, wantSchemaVersion, wantVersionRecords)
		}

		var title, messages, summary string
		var summaryCount int
		var createdMillis, updatedMillis, revision int64
		if err := db.QueryRow(`SELECT title, messages, summary_content, summary_message_count, created_at, updated_at, revision FROM conversations WHERE id = ?`, id).
			Scan(&title, &messages, &summary, &summaryCount, &createdMillis, &updatedMillis, &revision); err != nil {
			t.Fatalf("%s raw conversation query error: %v", phase, err)
		}
		if title != wantTitle || messages != wantMessages || summary != wantSummary || summaryCount != wantSummaryCount || createdMillis != wantCreatedMillis || updatedMillis != wantUpdatedMillis || revision != 1 {
			t.Errorf("%s raw conversation = (title=%q, messages=%q, summary=%q, summary_count=%d, created_at=%d, updated_at=%d, revision=%d), want (%q, %q, %q, %d, %d, %d, 1)", phase, title, messages, summary, summaryCount, createdMillis, updatedMillis, revision, wantTitle, wantMessages, wantSummary, wantSummaryCount, wantCreatedMillis, wantUpdatedMillis)
		}

		var searchTitle, searchBody string
		var searchCount int
		var searchCreatedMillis, searchUpdatedMillis int64
		if err := db.QueryRow(`SELECT title, body, message_count, created_at, updated_at FROM conversation_search WHERE id = ?`, id).
			Scan(&searchTitle, &searchBody, &searchCount, &searchCreatedMillis, &searchUpdatedMillis); err != nil {
			t.Fatalf("%s raw search query error: %v", phase, err)
		}
		if searchTitle != wantTitle || searchBody != wantSearchBody || searchCount != wantMessageCount || searchCreatedMillis != wantCreatedMillis || searchUpdatedMillis != wantUpdatedMillis {
			t.Errorf("%s raw search row = (title=%q, body=%q, message_count=%d, created_at=%d, updated_at=%d), want (%q, %q, %d, %d, %d)", phase, searchTitle, searchBody, searchCount, searchCreatedMillis, searchUpdatedMillis, wantTitle, wantSearchBody, wantMessageCount, wantCreatedMillis, wantUpdatedMillis)
		}

		var ftsTitle, ftsBody string
		if err := db.QueryRow(`SELECT title, body FROM conversation_fts WHERE id = ?`, id).Scan(&ftsTitle, &ftsBody); err != nil {
			t.Fatalf("%s raw FTS query error: %v", phase, err)
		}
		if ftsTitle != wantTitle || ftsBody != wantSearchBody {
			t.Errorf("%s raw FTS row = (title=%q, body=%q), want (%q, %q)", phase, ftsTitle, ftsBody, wantTitle, wantSearchBody)
		}

		loaded, err := store.Load(context.Background(), id)
		if err != nil {
			t.Fatalf("%s Load(%q) error: %v", phase, id, err)
		}
		if loaded.Revision != 1 {
			t.Errorf("%s Load(%q).Revision = %d, want 1", phase, id, loaded.Revision)
		}
		if loaded.Title != wantTitle || len(loaded.Messages) != wantMessageCount || loaded.DurableSummary == nil || loaded.DurableSummary.Content != wantSummary || loaded.DurableSummary.MessageCount != wantSummaryCount || !loaded.CreatedAt.Equal(time.UnixMilli(wantCreatedMillis)) || !loaded.UpdatedAt.Equal(time.UnixMilli(wantUpdatedMillis)) {
			t.Errorf("%s Load(%q) = %+v, want preserved v3 fixture", phase, id, loaded)
		}
		if len(loaded.Messages) == wantMessageCount {
			if got := string(loaded.Messages[1].ToolCalls); got != `[{"id":"call_fixture","type":"function","function":{"name":"read_file","arguments":{"path":"fixture.go"}}}]` {
				t.Errorf("%s Load(%q) tool calls = %s, want literal fixture payload", phase, id, got)
			}
			if loaded.Messages[2].ToolName != "read_file" || loaded.Messages[2].ToolCallID != "call_fixture" {
				t.Errorf("%s Load(%q) tool response = %+v, want literal fixture metadata", phase, id, loaded.Messages[2])
			}
		}

		results, err := store.Search(context.Background(), "calibrationtoken", SearchOptions{Limit: 10})
		if err != nil {
			t.Fatalf("%s Search(calibrationtoken) error: %v", phase, err)
		}
		if len(results) != 1 {
			t.Fatalf("%s Search(calibrationtoken) len = %d, want 1", phase, len(results))
		}
		got := results[0]
		if got.ID != id || got.Title != wantTitle || got.MessageCount != wantMessageCount || !strings.Contains(got.Snippet, "calibrationtoken") || !got.CreatedAt.Equal(time.UnixMilli(wantCreatedMillis)) || !got.UpdatedAt.Equal(time.UnixMilli(wantUpdatedMillis)) {
			t.Errorf("%s Search(calibrationtoken)[0] = %+v, want preserved fixture search projection", phase, got)
		}
	}

	other := openMigrationDB(t, dbPath)
	stores := newStoresConcurrently(t, db, other)
	store := stores[0]
	assertMigrationVersions(t, db, "1,2,3,4,5")
	assertMigrationVersions(t, other, "1,2,3,4,5")
	assertState("first open", db, store)
	assertState("other open", other, stores[1])
	if err := db.Close(); err != nil {
		t.Fatalf("Close(first open) error: %v", err)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(%q, reopen) error: %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err = NewStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewStore(reopen) error: %v", err)
	}
	assertState("reopen", db, store)

	loaded, err := store.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(context.Background(), *loaded); err != nil {
		t.Fatalf("Save(v3 loaded snapshot): %v", err)
	}
	saved, err := store.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := json.Marshal(saved.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Revision != 2 || saved.Title != wantTitle || string(messages) != wantMessages || saved.DurableSummary == nil || saved.DurableSummary.Content != wantSummary || saved.DurableSummary.MessageCount != wantSummaryCount || saved.CreatedAt.UnixMilli() != wantCreatedMillis {
		t.Errorf("Save/Load(v3 fixture) = %+v, messages=%s, want revision 2 and pinned fixture content", saved, messages)
	}
	for _, term := range []string{"calibrationtoken", "fixture.go", "Earlier"} {
		hits, err := store.Search(context.Background(), term, SearchOptions{})
		if err != nil || len(hits) != 1 {
			t.Fatalf("Search(%q) after save = %+v, %v, want fixture hit", term, hits, err)
		}
		if hits[0].ID != id || hits[0].Title != wantTitle || hits[0].MessageCount != wantMessageCount || hits[0].CreatedAt.UnixMilli() != wantCreatedMillis || !hits[0].UpdatedAt.Equal(saved.UpdatedAt) {
			t.Errorf("Search(%q) after save = %+v, want pinned fixture projection", term, hits[0])
		}
	}
}

func TestNewStore_ConcurrentSchemaV4RevisionFloor(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "v4.db")
	db := openMigrationDB(t, path)
	if err := runMigrationsWith(ctx, db, migrations[:4]); err != nil {
		t.Fatal(err)
	}
	// Deliberately noncanonical JSON and distinct revisions: migration must not
	// rewrite conversation bytes, timestamps, summaries, or either projection.
	execMigrationSQL(t, db, `INSERT INTO conversations (id,title,messages,summary_content,summary_message_count,revision,created_at,updated_at)
        VALUES ('high','fixturetitle','[ { "role": "user", "content": "fixturetoken" } ]','fixturesummary',7,17,1234,2345),
               ('low','lowtitle','[]','',0,3,1234,2345)`)
	execMigrationSQL(t, db, `INSERT INTO conversation_search(id,title,body,message_count,created_at,updated_at)
        VALUES ('high','fixturetitle','fixturetoken fixturesummary',1,1234,2345)`)
	snapshot := func(db *sql.DB) string {
		t.Helper()
		var state strings.Builder
		for _, query := range []string{
			`SELECT json_group_array(json_array(id,title,messages,summary_content,summary_message_count,revision,created_at,updated_at)) FROM (SELECT * FROM conversations ORDER BY id)`,
			`SELECT json_group_array(json_array(id,title,body,message_count,created_at,updated_at)) FROM (SELECT * FROM conversation_search ORDER BY id)`,
			`SELECT json_group_array(json_array(id,title,body)) FROM (SELECT * FROM conversation_fts ORDER BY id)`,
		} {
			var data string
			if err := db.QueryRow(query).Scan(&data); err != nil {
				t.Fatal(err)
			}
			state.WriteString(data)
		}
		return state.String()
	}
	execMigrationSQL(t, db, `INSERT INTO conversation_fts(id,title,body) SELECT id,title,body FROM conversation_search`)
	before := snapshot(db)
	other := openMigrationDB(t, path)
	newStoresConcurrently(t, db, other)
	for _, handle := range []*sql.DB{db, other} {
		assertMigrationVersions(t, handle, "1,2,3,4,5")
		assertMigrationSQL(t, handle, `SELECT value FROM conversation_revision_floor`, "17")
		if snapshot(handle) != before {
			t.Fatal("v5 migration rewrote v4 data")
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	db = openMigrationDB(t, path)
	store, err := NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot(db) != before {
		t.Fatal("reopen changed data")
	}
	assertMigrationSQL(t, db, `SELECT value FROM conversation_revision_floor`, "17")
	// The seed also blocks a released unconditional upsert before any deletion.
	if _, err := db.Exec(legacyV02Save, "high", "obsolete", `[]`, "", 0, 1234, 3456); err == nil {
		t.Fatal("legacy upsert was accepted after migration")
	}
	if snapshot(db) != before {
		t.Fatal("legacy upsert changed migrated data")
	}
	loaded, err := store.Load(ctx, "high")
	if err != nil {
		t.Fatal(err)
	}
	if revision, err := store.Save(ctx, *loaded); err != nil || revision != 18 {
		t.Fatalf("save migrated snapshot = %d, %v; want 18", revision, err)
	}
	saved, err := store.Load(ctx, "high")
	if err != nil {
		t.Fatal(err)
	}
	loaded.Revision, loaded.UpdatedAt = saved.Revision, saved.UpdatedAt
	if !reflect.DeepEqual(loaded, saved) {
		t.Fatalf("saved content = %+v, want %+v", saved, loaded)
	}
	for _, term := range []string{"fixturetitle", "fixturetoken", "fixturesummary"} {
		hits, err := store.Search(ctx, term, SearchOptions{})
		if err != nil || len(hits) != 1 {
			t.Fatalf("Search(%s) = %v, %v", term, hits, err)
		}
	}
}
