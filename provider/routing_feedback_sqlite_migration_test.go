package provider

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
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
	if err := runFeedbackMigrations(db); err != nil {
		t.Fatalf("runFeedbackMigrations: %v", err)
	}
	v, err := currentFeedbackSchemaVersion(db)
	if err != nil {
		t.Fatalf("currentFeedbackSchemaVersion: %v", err)
	}
	if v != len(feedbackMigrations) {
		t.Errorf("schema version = %d, want %d", v, len(feedbackMigrations))
	}
	if !tableExists(db, "routing_feedback_signals") {
		t.Errorf("routing_feedback_signals table missing after migration")
	}
}

func TestRunFeedbackMigrationsIdempotent(t *testing.T) {
	db := newMigrationTestDB(t)
	if err := runFeedbackMigrations(db); err != nil {
		t.Fatalf("first run: %v", err)
	}
	v1, _ := currentFeedbackSchemaVersion(db)
	if err := runFeedbackMigrations(db); err != nil {
		t.Fatalf("second run: %v", err)
	}
	v2, _ := currentFeedbackSchemaVersion(db)
	if v1 != v2 {
		t.Errorf("version drifted on second run: %d -> %d", v1, v2)
	}
}

func TestRunFeedbackMigrationsPreMigrationCompatibleDB(t *testing.T) {
	db := newMigrationTestDB(t)
	// Simulate a DB that has the full v1 routing_feedback_signals schema
	// but no schema_version row (e.g., created by a future tool before
	// the migration runner existed). Must mark v1 as applied so the
	// runner doesn't try to re-CREATE the table.
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
	if err := runFeedbackMigrations(db); err != nil {
		t.Fatalf("runFeedbackMigrations: %v", err)
	}
	v, _ := currentFeedbackSchemaVersion(db)
	if v < 1 {
		t.Errorf("compatible pre-migration DB should be recorded at >=v1, got %d", v)
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
	err := runFeedbackMigrations(db)
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
	err := runFeedbackMigrations(db)
	if err == nil {
		t.Fatalf("runFeedbackMigrations accepted corrupt schema_version table; want error")
	}
}

func TestCurrentFeedbackSchemaVersionEmpty(t *testing.T) {
	db := newMigrationTestDB(t)
	// Without the version table, currentFeedbackSchemaVersion should
	// return 0 cleanly (no rows means "version 0").
	if _, err := db.Exec(`CREATE TABLE routing_feedback_schema_version (
		version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("seed version table: %v", err)
	}
	v, err := currentFeedbackSchemaVersion(db)
	if err != nil {
		t.Fatalf("currentFeedbackSchemaVersion: %v", err)
	}
	if v != 0 {
		t.Errorf("empty version table = %d, want 0", v)
	}
}

func TestFeedbackSignalsTableCheckConstraints(t *testing.T) {
	db := newMigrationTestDB(t)
	if err := runFeedbackMigrations(db); err != nil {
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
