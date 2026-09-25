package provider

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

// newMigrationTestDB opens an isolated in-memory database that survives
// across multiple connections (file::memory: with cache=shared) and is
// scoped to the test via t.Cleanup.
func newMigrationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRunFeedbackMigrationsFreshDB(t *testing.T) {
	db := newMigrationTestDB(t)
	if err := runFeedbackMigrations(t.Context(), db); err != nil {
		t.Fatalf("runFeedbackMigrations: %v", err)
	}
	v, err := currentFeedbackSchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatalf("currentFeedbackSchemaVersion: %v", err)
	}
	if v != len(feedbackMigrations) {
		t.Errorf("schema version = %d, want %d", v, len(feedbackMigrations))
	}
	var exists bool
	err = db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='routing_feedback_signals')`).Scan(&exists)
	if err != nil {
		t.Fatalf("tableExists: %v", err)
	}
	if !exists {
		t.Errorf("routing_feedback_signals table missing after migration")
	}
}

func TestRunFeedbackMigrationsIdempotent(t *testing.T) {
	db := newMigrationTestDB(t)
	if err := runFeedbackMigrations(t.Context(), db); err != nil {
		t.Fatalf("first run: %v", err)
	}
	v1, _ := currentFeedbackSchemaVersion(t.Context(), db)
	if err := runFeedbackMigrations(t.Context(), db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	v2, _ := currentFeedbackSchemaVersion(t.Context(), db)
	if v1 != v2 {
		t.Errorf("version drifted on second run: %d -> %d", v1, v2)
	}
}

func TestRunFeedbackMigrationsPreMigrationCompatibleDB(t *testing.T) {
	db := newMigrationTestDB(t)
	// Simulate a DB that has the full v1 routing_feedback_signals schema
	// including the composite CHECK but no schema_version row (e.g.,
	// created by a future tool before the migration runner existed).
	// Validate and stamp v1, creating its missing baseline indexes.
	if _, err := db.Exec(legacySignalsSchema); err != nil {
		t.Fatalf("seed signals table: %v", err)
	}
	if err := runFeedbackMigrations(t.Context(), db); err != nil {
		t.Fatalf("runFeedbackMigrations: %v", err)
	}
	v, _ := currentFeedbackSchemaVersion(t.Context(), db)
	if v < 1 {
		t.Errorf("compatible pre-migration DB should be recorded at >=v1, got %d", v)
	}
}

// TestRunFeedbackMigrationsRejectsPreExistingTableMissingChecks proves
// validateExistingSignalsSchema is not satisfied by columns alone — a
// pre-existing table with the right columns but lacking v1's composite
// CHECK is rejected. Otherwise the migration runner would bless a
// schema that silently accepts rows the in-memory store would reject.
func TestRunFeedbackMigrationsRejectsPreExistingTableMissingChecks(t *testing.T) {
	db := newMigrationTestDB(t)
	if _, err := db.Exec(`CREATE TABLE routing_feedback_signals (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL,
		model TEXT NOT NULL,
		use_case TEXT NOT NULL,
		kind TEXT NOT NULL,
		strength REAL,
		at_ns INTEGER NOT NULL,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		error_class TEXT NOT NULL DEFAULT '',
		route_id TEXT NOT NULL DEFAULT '',
		completion_id TEXT NOT NULL DEFAULT '',
		meta TEXT NOT NULL DEFAULT '{}'
	)`); err != nil {
		t.Fatalf("seed signals table: %v", err)
	}
	err := runFeedbackMigrations(t.Context(), db)
	if err == nil {
		t.Fatalf("accepted pre-existing table missing v1 CHECK fingerprint; want error")
	}
}

func TestRunFeedbackMigrationsRejectsIncompatiblePreExistingTable(t *testing.T) {
	db := newMigrationTestDB(t)
	// Pre-existing table is missing required columns/checks. The
	// runner must refuse to mark v1 as applied; otherwise a future
	// Record() would fail with a confusing "no such column" error
	// far from the actual cause.
	if _, err := db.Exec(`CREATE TABLE routing_feedback_signals (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("seed signals table: %v", err)
	}
	err := runFeedbackMigrations(t.Context(), db)
	if err == nil {
		t.Fatalf("runFeedbackMigrations accepted incompatible pre-existing table; want error")
	}
}

func TestRunFeedbackMigrationsCorruptVersionTable(t *testing.T) {
	db := newMigrationTestDB(t)
	// Create the version table with a wrong column name. The runner
	// will not detect this at CREATE TABLE IF NOT EXISTS time (no-op
	// when the table exists) but will fail on the version SELECT.
	// Surface as a clean error, not a panic.
	if _, err := db.Exec(`CREATE TABLE routing_feedback_schema_version (foo TEXT)`); err != nil {
		t.Fatalf("seed bad version table: %v", err)
	}
	err := runFeedbackMigrations(t.Context(), db)
	if err == nil {
		t.Fatalf("runFeedbackMigrations accepted corrupt schema_version table; want error")
	}
}

// TestCurrentFeedbackSchemaVersionEmptyVersionTable confirms that an
// existing but empty routing_feedback_schema_version table returns 0
// cleanly (no rows means "version 0"). The "no version table at all"
// case is covered transitively by TestRunFeedbackMigrationsFreshDB.
func TestCurrentFeedbackSchemaVersionEmptyVersionTable(t *testing.T) {
	db := newMigrationTestDB(t)
	if _, err := db.Exec(`CREATE TABLE routing_feedback_schema_version (
		version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("seed version table: %v", err)
	}
	v, err := currentFeedbackSchemaVersion(t.Context(), db)
	if err != nil {
		t.Fatalf("currentFeedbackSchemaVersion: %v", err)
	}
	if v != 0 {
		t.Errorf("empty version table = %d, want 0", v)
	}
}

func TestFeedbackSignalsTableCheckConstraints(t *testing.T) {
	db := newMigrationTestDB(t)
	if err := runFeedbackMigrations(t.Context(), db); err != nil {
		t.Fatalf("runFeedbackMigrations: %v", err)
	}
	// Success with non-zero latency must be rejected by the CHECK constraint.
	_, err := db.Exec(`INSERT INTO routing_feedback_signals
		(provider, model, use_case, kind, at_ns, latency_ms, error_class, route_id, completion_id, meta)
		VALUES ('p','m','chat','success',1,500,'','','','{}')`)
	if err == nil {
		t.Errorf("expected CHECK violation for success+latency_ms>0, got nil")
	}
	// Failure with empty error_class must be rejected.
	_, err = db.Exec(`INSERT INTO routing_feedback_signals
		(provider, model, use_case, kind, at_ns, latency_ms, error_class, route_id, completion_id, meta)
		VALUES ('p','m','chat','failure',1,0,'','','','{}')`)
	if err == nil {
		t.Errorf("expected CHECK violation for failure with empty error_class, got nil")
	}
	// Latency with zero latency_ms must be rejected.
	_, err = db.Exec(`INSERT INTO routing_feedback_signals
		(provider, model, use_case, kind, at_ns, latency_ms, error_class, route_id, completion_id, meta)
		VALUES ('p','m','chat','latency',1,0,'','','','{}')`)
	if err == nil {
		t.Errorf("expected CHECK violation for latency with latency_ms=0, got nil")
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
	assertMigrationSQL(t, db, "SELECT COALESCE(group_concat(version), '') FROM (SELECT version FROM routing_feedback_schema_version ORDER BY version)", want)
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
	_, err := NewSQLiteFeedbackStore(ctx, db, SQLiteFeedbackStoreConfig{})
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
	assertMigrationSQL(t, a, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name IN ('routing_feedback_signals')", "1")
}

func TestMigrationClaimBeforeDDLAndStaleSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	execMigrationSQL(t, a, "CREATE TABLE routing_feedback_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	claimed, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	winner, loser := make(chan error, 1), make(chan error, 1)
	go func() {
		winner <- applyFeedbackMigration(ctx, a, migration{version: 7, description: "winner", fn: func(tx *sql.Tx) error {
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
	go func() { loser <- applyFeedbackMigration(ctx, b, stale) }()
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
	if err := applyFeedbackMigration(ctx, b, stale); err != nil {
		t.Fatalf("stale selection: %v", err)
	}
	assertMigrationVersions(t, b, "7")
	assertMigrationSQL(t, b, "SELECT COUNT(*) FROM applied_once", "1")
	assertMigrationSQL(t, b, "SELECT value FROM applied_once", "winner")
	assertMigrationSQL(t, b, "SELECT description FROM routing_feedback_schema_version WHERE version=7", "winner")
	execMigrationSQL(t, b, "BEGIN IMMEDIATE; ROLLBACK")
}

func TestMigrationRollbackAndRetry(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "rollback.db"))
	list := append([]migration(nil), feedbackMigrations...)
	first := list[0].fn
	list[0].fn = func(tx *sql.Tx) error {
		if err := first(tx); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO routing_feedback_signals(provider,model,use_case,kind,at_ns) VALUES ('p','m','chat','success',123); CREATE INDEX custom_legacy_index ON routing_feedback_signals(route_id)")
		return err
	}
	boom := errors.New("migration failed")
	list = append(list, migration{version: 7, description: "tentative", fn: func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE tentative (value TEXT); UPDATE routing_feedback_signals SET model='lost'"); err != nil {
			return err
		}
		return boom
	}})
	if err := runFeedbackMigrationsWith(t.Context(), db, list); !errors.Is(err, boom) {
		t.Fatalf("failed step = %v, want sentinel", err)
	}
	assertMigrationVersions(t, db, "1")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='tentative'", "0")
	assertMigrationSQL(t, db, "SELECT model FROM routing_feedback_signals WHERE provider='p'", "m")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='custom_legacy_index'", "1")
	list[len(list)-1].fn = func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE tentative (value TEXT); INSERT INTO tentative VALUES ('retried')")
		return err
	}
	if err := runFeedbackMigrationsWith(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "1,7")
	assertMigrationSQL(t, db, "SELECT value FROM tentative", "retried")
	assertMigrationSQL(t, db, "SELECT model FROM routing_feedback_signals WHERE provider='p'", "m")
}

func TestMigrationCanceledStep(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "cancel.db"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := runFeedbackMigrationsWith(ctx, db, []migration{{version: 7, description: "cancel", fn: func(tx *sql.Tx) error {
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
			assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='routing_feedback_schema_version'", "0")
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
	assertMigrationSQLiteError(t, runFeedbackMigrationsWith(t.Context(), ro, []migration{pending}), 8)
	assertMigrationVersions(t, ro, "1")
	assertMigrationSQL(t, ro, "SELECT COUNT(*) FROM sqlite_schema WHERE name='forbidden'", "0")
}

func TestMigrationLockAndIOErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	execMigrationSQL(t, a, "CREATE TABLE routing_feedback_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
	execMigrationSQL(t, a, "BEGIN IMMEDIATE")
	execMigrationSQL(t, b, "PRAGMA busy_timeout=0")
	err := applyFeedbackMigration(t.Context(), b, migration{version: 7, description: "blocked", fn: func(*sql.Tx) error { return errors.New("must not run") }})
	assertMigrationSQLiteError(t, err, 5)
	assertMigrationVersions(t, b, "")
	// Keep the writer held through a real, positive busy timeout. This tests
	// SQLite waiting itself, without a sleep deciding when to release a writer.
	execMigrationSQL(t, b, "PRAGMA busy_timeout=50")
	started := time.Now()
	err = applyFeedbackMigration(t.Context(), b, migration{version: 7, description: "timed out", fn: func(*sql.Tx) error { return errors.New("must not run") }})
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

const legacySignalsSchema = `CREATE TABLE routing_feedback_signals (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		provider TEXT NOT NULL,
		model TEXT NOT NULL,
		use_case TEXT NOT NULL,
		kind TEXT NOT NULL CHECK (kind IN ('success', 'failure', 'latency')),
		strength REAL,
		at_ns INTEGER NOT NULL,
		latency_ms INTEGER NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
		error_class TEXT NOT NULL DEFAULT '',
		route_id TEXT NOT NULL DEFAULT '',
		completion_id TEXT NOT NULL DEFAULT '',
		meta TEXT NOT NULL DEFAULT '{}',
		CHECK (
			(kind = 'success' AND latency_ms = 0 AND error_class = '') OR
			(kind = 'failure' AND latency_ms = 0 AND error_class <> '') OR
			(kind = 'latency' AND latency_ms > 0 AND error_class = '')
		)
	)`

func TestMigrationConcurrentLegacySignals(t *testing.T) {
	for _, tc := range []struct {
		name, ddl  string
		compatible bool
	}{
		{"compatible", legacySignalsSchema, true},
		{"missing columns", "CREATE TABLE routing_feedback_signals (id INTEGER PRIMARY KEY)", false},
		{"missing checks", `CREATE TABLE routing_feedback_signals AS SELECT
   1 AS id, '' AS provider, '' AS model, '' AS use_case, '' AS kind,
   0 AS strength, 0 AS at_ns, 0 AS latency_ms, '' AS error_class,
   '' AS route_id, '' AS completion_id, '' AS meta WHERE 0`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			a, b := openMigrationDB(t, path), openMigrationDB(t, path)
			execMigrationSQL(t, a, tc.ddl)
			if tc.compatible {
				execMigrationSQL(t, a, `INSERT INTO routing_feedback_signals(id,provider,model,use_case,kind,at_ns,meta)
     VALUES (17,'p','m','chat','success',123,'{"seed":true}')`)
			} else {
				execMigrationSQL(t, a, "INSERT INTO routing_feedback_signals(id) VALUES (17)")
			}
			execMigrationSQL(t, a, "CREATE INDEX custom_legacy_index ON routing_feedback_signals(id)")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			start, results := make(chan struct{}), make(chan error, 2)
			for _, db := range []*sql.DB{a, b} {
				go func() { <-start; results <- newMigrationStore(ctx, db) }()
			}
			close(start)
			for range 2 {
				err := migrationResult(t, ctx, results)
				if tc.compatible && err != nil {
					t.Errorf("compatible legacy store: %v", err)
				}
				if !tc.compatible && (err == nil || !strings.Contains(err.Error(), "incompatible")) {
					t.Errorf("incompatible legacy store: %v", err)
				}
			}
			for _, db := range []*sql.DB{a, b} {
				versions := ""
				if tc.compatible {
					versions = "1"
				}
				assertMigrationVersions(t, db, versions)
				assertMigrationSQL(t, db, "SELECT id FROM routing_feedback_signals", "17")
				assertMigrationSQL(t, db, "SELECT COUNT(*) FROM routing_feedback_signals", "1")
				assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='custom_legacy_index'", "1")
			}
			if tc.compatible {
				assertMigrationSQL(t, b, "SELECT meta FROM routing_feedback_signals", `{"seed":true}`)
				assertMigrationSQL(t, b, "SELECT group_concat(name) FROM pragma_index_info('idx_rfs_key_at')", "provider,model,use_case,at_ns,id")
				assertMigrationSQL(t, b, "SELECT group_concat(name) FROM pragma_index_info('idx_rfs_key_kind')", "provider,model,use_case,kind")
				assertMigrationSQL(t, b, "SELECT description FROM routing_feedback_schema_version", "baseline routing_feedback_signals table + indexes (pre-existing)")
			} else {
				assertMigrationSQL(t, b, "SELECT COUNT(*) FROM sqlite_schema WHERE name IN ('idx_rfs_key_at','idx_rfs_key_kind')", "0")
			}
		})
	}
}

func TestMigrationLegacyIndexFailureRollsBack(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "legacy-retry.db"))
	execMigrationSQL(t, db, legacySignalsSchema)
	execMigrationSQL(t, db, "INSERT INTO routing_feedback_signals(provider,model,use_case,kind,at_ns) VALUES ('p','m','chat','success',123)")
	// A conflicting table name forces failure after the first index was made.
	execMigrationSQL(t, db, "CREATE TABLE idx_rfs_key_kind (id INTEGER)")
	if err := newMigrationStore(t.Context(), db); err == nil {
		t.Fatal("legacy index failure was ignored")
	}
	assertMigrationVersions(t, db, "")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='idx_rfs_key_at'", "0")
	assertMigrationSQL(t, db, "SELECT model FROM routing_feedback_signals", "m")
	execMigrationSQL(t, db, "DROP TABLE idx_rfs_key_kind")
	if err := newMigrationStore(t.Context(), db); err != nil {
		t.Fatalf("legacy retry: %v", err)
	}
	assertMigrationVersions(t, db, "1")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name IN ('idx_rfs_key_at','idx_rfs_key_kind')", "2")
	assertMigrationSQL(t, db, "SELECT model FROM routing_feedback_signals", "m")
}
