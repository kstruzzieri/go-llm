package fingerprint

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

func TestMigrations_FreshDB(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM fingerprint_profiles").Scan(&count); err != nil {
		t.Fatalf("query profiles table: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM fingerprint_failures").Scan(&count); err != nil {
		t.Fatalf("query failures table: %v", err)
	}
}

func TestMigrations_Idempotent(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := runMigrations(t.Context(), db); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
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
	// Check before querying: with one pooled connection, a leaked migration
	// transaction would block the query instead of failing here.
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Fatalf("retained connections = %d", inUse)
	}
	assertMigrationSQL(t, db, migrationVersionsQuery, want)
}

const migrationVersionsQuery = "SELECT COALESCE(group_concat(version), '') FROM (SELECT version FROM fingerprint_schema_version ORDER BY version)"

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
	_, err := NewStore(ctx, db)
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
		assertMigrationVersions(t, db, "1,2,3,4")
	}
	assertMigrationSQL(t, a, "SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name IN ('fingerprint_profiles','fingerprint_failures','capability_probes','capability_evidence','store_meta')", "5")
	assertMigrationSQL(t, a, "SELECT COUNT(*) FROM store_meta", "1")
}

func TestMigrationClaimBeforeDDLAndStaleSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	execMigrationSQL(t, a, "CREATE TABLE fingerprint_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
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
	assertMigrationSQL(t, b, "SELECT description FROM fingerprint_schema_version WHERE version=7", "winner")
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
		_, err := tx.Exec("INSERT INTO fingerprint_failures VALUES ('p','m','digest','seed',123,2); CREATE INDEX custom_legacy_index ON fingerprint_failures(attempted_at)")
		return err
	}
	boom := errors.New("migration failed")
	list = append(list, migration{version: 7, description: "tentative", fn: func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE tentative (value TEXT); UPDATE fingerprint_failures SET last_error='lost'"); err != nil {
			return err
		}
		return boom
	}})
	if err := runMigrationsWith(t.Context(), db, list); !errors.Is(err, boom) {
		t.Fatalf("failed step = %v, want sentinel", err)
	}
	assertMigrationVersions(t, db, "1,2,3,4")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='tentative'", "0")
	assertMigrationSQL(t, db, "SELECT last_error FROM fingerprint_failures WHERE backend_id='p'", "seed")
	assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='custom_legacy_index'", "1")
	list[len(list)-1].fn = func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE tentative (value TEXT); INSERT INTO tentative VALUES ('retried')")
		return err
	}
	if err := runMigrationsWith(t.Context(), db, list); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "1,2,3,4,7")
	assertMigrationSQL(t, db, "SELECT value FROM tentative", "retried")
	assertMigrationSQL(t, db, "SELECT last_error FROM fingerprint_failures WHERE backend_id='p'", "seed")
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
	// A query waits for database/sql's asynchronous cancellation rollback, so
	// skip assertMigrationVersions' immediate retained-connection check.
	assertMigrationSQL(t, db, migrationVersionsQuery, "")
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
			assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name='fingerprint_schema_version'", "0")
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
	assertMigrationVersions(t, ro, "1,2,3,4")
	pending := migration{version: 7, description: "pending", fn: func(tx *sql.Tx) error { _, err := tx.Exec("CREATE TABLE forbidden (id INTEGER)"); return err }}
	assertMigrationSQLiteError(t, runMigrationsWith(t.Context(), ro, []migration{pending}), 8)
	assertMigrationVersions(t, ro, "1,2,3,4")
	assertMigrationSQL(t, ro, "SELECT COUNT(*) FROM sqlite_schema WHERE name='forbidden'", "0")
}

func TestMigrationLockAndIOErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "busy.db")
	a, b := openMigrationDB(t, path), openMigrationDB(t, path)
	execMigrationSQL(t, a, "CREATE TABLE fingerprint_schema_version (version INTEGER PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL)")
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
	err = newMigrationStore(t.Context(), missing)
	assertMigrationSQLiteError(t, err, 14)
	if !strings.Contains(err.Error(), "check version table") {
		t.Fatalf("missing directory = %v, want the version-table check to fail", err)
	}
}

func TestMigrations_StrictlyIncreasingVersions(t *testing.T) {
	t.Parallel()
	for i := 1; i < len(migrations); i++ {
		if got, previous := migrations[i].version, migrations[i-1].version; got <= previous {
			t.Errorf("migrations[%d].version = %d, want greater than migrations[%d].version (%d)", i, got, i-1, previous)
		}
	}
}

func TestMigrationMalformedVersionTable(t *testing.T) {
	for _, tc := range []struct{ name, ddl string }{
		{"missing columns", "CREATE TABLE fingerprint_schema_version (version TEXT); INSERT INTO fingerprint_schema_version VALUES ('broken')"},
		// Every column the claim needs exists, so only the version read
		// rejects the non-integer row.
		{"non-integer version", "CREATE TABLE fingerprint_schema_version (version PRIMARY KEY, description TEXT NOT NULL, applied_at INTEGER NOT NULL); INSERT INTO fingerprint_schema_version VALUES ('broken', 'corrupt', 0)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openMigrationDB(t, filepath.Join(t.TempDir(), "malformed.db"))
			execMigrationSQL(t, db, tc.ddl)
			if err := newMigrationStore(t.Context(), db); err == nil || !strings.Contains(err.Error(), "query schema version") {
				t.Fatalf("malformed version table = %v, want a version-read error", err)
			}
			assertMigrationSQL(t, db, "SELECT group_concat(version) FROM fingerprint_schema_version", "broken")
			assertMigrationSQL(t, db, "SELECT COUNT(*) FROM sqlite_schema WHERE name <> 'fingerprint_schema_version' AND name NOT LIKE 'sqlite_%'", "0")
		})
	}
}

func TestMigrationV3_OlderBinaryCoexists(t *testing.T) {
	ctx := t.Context()
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "coexist.db"))

	// An older binary made this file and saved a verdict.
	older := migrations[:2]
	if err := runMigrationsWith(ctx, db, older); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "1,2")
	oldStore := &SQLiteStore{db: db}
	saved := CapProbe{
		BackendID: "local", ModelName: "m", Capability: "tool_call", State: CapProbeYes,
		ModelDigest: "d", ProbeVersion: CurrentToolProbeVersion, TestedAt: time.UnixMilli(1_000),
	}
	if err := oldStore.SaveCapProbe(ctx, saved); err != nil {
		t.Fatal(err)
	}

	// A new binary upgrades the file in place, leaving the old table alone.
	if err := newMigrationStore(ctx, db); err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "1,2,3,4")

	// The older binary opens the newer schema, skips what it does not know, and
	// still reads its row and writes a newer one. So does one that stops at V3.
	if err := runMigrationsWith(ctx, db, older); err != nil {
		t.Fatalf("older binary on the new schema: %v", err)
	}
	if err := runMigrationsWith(ctx, db, migrations[:3]); err != nil {
		t.Fatalf("V3 binary on the new schema: %v", err)
	}
	assertMigrationVersions(t, db, "1,2,3,4")
	got, err := oldStore.GetCapProbe(ctx, "local", "m", "tool_call")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != CapProbeYes || got.ModelDigest != "d" || !got.TestedAt.Equal(saved.TestedAt) {
		t.Fatalf("older row after the upgrade = %+v", got)
	}
	saved.State, saved.TestedAt = CapProbeNo, time.UnixMilli(2_000)
	if err := oldStore.SaveCapProbe(ctx, saved); err != nil {
		t.Fatal(err)
	}
	got, err = oldStore.GetCapProbe(ctx, "local", "m", "tool_call")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != CapProbeNo || !got.TestedAt.Equal(saved.TestedAt) {
		t.Fatalf("older write on the new schema = %+v", got)
	}
}

// TestMigrationV3_Schema pins each column's type and constraints from the design.
func TestMigrationV3_Schema(t *testing.T) {
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "schema.db"))
	if err := runMigrationsWith(t.Context(), db, migrations[:3]); err != nil {
		t.Fatal(err)
	}
	// Each column: name, type, NOT NULL, position in the primary key.
	want := map[string]string{
		"capability_evidence": "id INTEGER 0 1, key_version INTEGER 1 0, endpoint TEXT 1 0, adapter TEXT 1 0, " +
			"model_name TEXT 1 0, model_digest TEXT 1 0, capability TEXT 1 0, evidence TEXT 1 0, kind TEXT 1 0, " +
			"state TEXT 1 0, method TEXT 1 0, method_version INTEGER 1 0, detail TEXT 1 0, http_status INTEGER 0 0, " +
			"observed_at INTEGER 1 0, fresh_until INTEGER 0 0, expires_at INTEGER 0 0",
		"listing_snapshots": "id INTEGER 0 1, key_version INTEGER 1 0, endpoint TEXT 1 0, adapter TEXT 1 0, account TEXT 1 0, " +
			"started_at INTEGER 1 0, committed_at INTEGER 1 0",
		"listing_members": "snapshot_id INTEGER 1 1, model_name TEXT 1 2, owned_by TEXT 1 0, model_digest TEXT 1 0",
		"named_models":    "key_version INTEGER 1 1, endpoint TEXT 1 2, model_name TEXT 1 3, added_at INTEGER 1 0",
		"model_access": "id INTEGER 0 1, key_version INTEGER 1 0, endpoint TEXT 1 0, account TEXT 1 0, " +
			"model_name TEXT 1 0, state TEXT 1 0, http_status INTEGER 0 0, observed_at INTEGER 1 0, expires_at INTEGER 0 0",
		"store_meta": "id INTEGER 0 1, generation INTEGER 1 0, account_secret BLOB 1 0",
	}
	for table, columns := range want {
		assertMigrationSQL(t, db, "SELECT group_concat(name || ' ' || type || ' ' || \"notnull\" || ' ' || pk, ', ') FROM pragma_table_info('"+table+"')", columns)
	}
	assertMigrationSQL(t, db, "SELECT group_concat(name) FROM pragma_index_info('idx_capability_evidence_lookup')",
		"key_version,endpoint,adapter,model_name,capability,observed_at")
}

func TestMigrationV3_SeedsOneMetaRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.db")
	db := openMigrationDB(t, path)
	if err := newMigrationStore(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	assertMigrationSQL(t, db, "SELECT COUNT(*) || ' ' || MIN(generation) || ' ' || MIN(length(account_secret)) || ' ' || MIN(typeof(account_secret)) FROM store_meta", "1 0 32 blob")
	var secret string
	if err := db.QueryRowContext(t.Context(), "SELECT hex(account_secret) FROM store_meta").Scan(&secret); err != nil {
		t.Fatal(err)
	}
	if secret == strings.Repeat("00", 32) {
		t.Fatal("account secret is all zeroes")
	}

	// A second row is refused, and a second opener does not re-seed.
	if _, err := db.ExecContext(t.Context(), "INSERT INTO store_meta (id, generation, account_secret) VALUES (2, 0, x'00')"); err == nil {
		t.Fatal("store_meta accepted a second row")
	}
	if err := newMigrationStore(t.Context(), openMigrationDB(t, path)); err != nil {
		t.Fatal(err)
	}
	assertMigrationSQL(t, db, "SELECT hex(account_secret) FROM store_meta", secret)

	// Another file gets its own secret.
	other := openMigrationDB(t, filepath.Join(t.TempDir(), "other.db"))
	if err := newMigrationStore(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	var otherSecret string
	if err := other.QueryRowContext(t.Context(), "SELECT hex(account_secret) FROM store_meta").Scan(&otherSecret); err != nil {
		t.Fatal(err)
	}
	if otherSecret == secret {
		t.Fatal("two files share one account secret")
	}
}

// earlierV3DDL is what V3 created before listing snapshots were keyed by
// adapter. Stores migrated by that build exist, at version 3, and V3 never runs
// on them again.
var earlierV3DDL = []string{
	`CREATE TABLE capability_evidence (
		id             INTEGER PRIMARY KEY,
		key_version    INTEGER NOT NULL,
		endpoint       TEXT NOT NULL,
		adapter        TEXT NOT NULL,
		model_name     TEXT NOT NULL,
		model_digest   TEXT NOT NULL,
		capability     TEXT NOT NULL,
		evidence       TEXT NOT NULL,
		kind           TEXT NOT NULL,
		state          TEXT NOT NULL,
		method         TEXT NOT NULL,
		method_version INTEGER NOT NULL,
		detail         TEXT NOT NULL DEFAULT '',
		http_status    INTEGER,
		observed_at    INTEGER NOT NULL,
		fresh_until    INTEGER,
		expires_at     INTEGER
	)`,
	`CREATE INDEX idx_capability_evidence_lookup
		ON capability_evidence (key_version, endpoint, adapter, model_name, capability, observed_at)`,
	`CREATE TABLE listing_snapshots (
		id           INTEGER PRIMARY KEY,
		key_version  INTEGER NOT NULL,
		endpoint     TEXT NOT NULL,
		account      TEXT NOT NULL,
		started_at   INTEGER NOT NULL,
		committed_at INTEGER NOT NULL
	)`,
	`CREATE TABLE listing_members (
		snapshot_id  INTEGER NOT NULL,
		model_name   TEXT NOT NULL,
		owned_by     TEXT NOT NULL DEFAULT '',
		model_digest TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (snapshot_id, model_name)
	)`,
	`CREATE TABLE named_models (
		key_version INTEGER NOT NULL,
		endpoint    TEXT NOT NULL,
		model_name  TEXT NOT NULL,
		added_at    INTEGER NOT NULL,
		PRIMARY KEY (key_version, endpoint, model_name)
	)`,
	`CREATE TABLE model_access (
		id          INTEGER PRIMARY KEY,
		key_version INTEGER NOT NULL,
		endpoint    TEXT NOT NULL,
		account     TEXT NOT NULL,
		model_name  TEXT NOT NULL,
		state       TEXT NOT NULL,
		http_status INTEGER,
		observed_at INTEGER NOT NULL,
		expires_at  INTEGER
	)`,
	`CREATE TABLE store_meta (
		id             INTEGER PRIMARY KEY CHECK (id = 1),
		generation     INTEGER NOT NULL,
		account_secret BLOB NOT NULL
	)`,
	`INSERT INTO store_meta (id, generation, account_secret) VALUES (1, 7, zeroblob(32))`,
	`INSERT INTO listing_snapshots (key_version, endpoint, account, started_at, committed_at)
		VALUES (1, 'http://127.0.0.1:8080', '', 1000, 1000)`,
	`INSERT INTO fingerprint_schema_version (version, description, applied_at)
		VALUES (3, 'append-only capability evidence log, listings, named models, model access and store_meta', 1000)`,
}

func TestMigrationV4_RepairsEarlierV3Store(t *testing.T) {
	ctx := t.Context()
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "earlier.db"))
	if err := runMigrationsWith(ctx, db, migrations[:2]); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range earlierV3DDL {
		execMigrationSQL(t, db, stmt)
	}
	assertMigrationVersions(t, db, "1,2,3")

	s, err := NewStore(ctx, db)
	if err != nil {
		t.Fatalf("NewStore on an earlier V3 store: %v", err)
	}
	assertMigrationVersions(t, db, "1,2,3,4")
	assertMigrationSQL(t, db, "SELECT group_concat(name || ' ' || type || ' ' || \"notnull\", ', ') FROM pragma_table_info('listing_snapshots')",
		"id INTEGER 0, key_version INTEGER 1, endpoint TEXT 1, account TEXT 1, started_at INTEGER 1, committed_at INTEGER 1, adapter TEXT 1")
	assertMigrationSQL(t, db, "SELECT group_concat(name) FROM pragma_index_info('idx_capability_evidence_lookup')", "key_version,endpoint,model_name,observed_at")
	assertMigrationSQL(t, db, "SELECT group_concat(name) FROM pragma_index_info('idx_model_access_lookup')", "key_version,endpoint,account,model_name,observed_at")
	// The repair keeps the store's identity and counts as no write.
	assertMigrationSQL(t, db, "SELECT generation || ' ' || hex(account_secret) FROM store_meta", "7 "+strings.Repeat("00", 32))

	const key = "secret-key"
	started := time.Now()
	if err := s.PublishListings(ctx, []ListingPublish{{
		Endpoint: evEndpoint, Adapter: AdapterOllama, APIKey: key, StartedAt: started,
		Members: []ListingMember{{Model: "m", Digest: "d"}},
	}}); err != nil {
		t.Fatalf("PublishListings: %v", err)
	}
	listing, err := s.CurrentListing(ctx, evEndpoint, AdapterOllama, key)
	if err != nil {
		t.Fatalf("CurrentListing: %v", err)
	}
	if listing == nil || len(listing.Members) != 1 || listing.Members[0] != (ListingMember{Model: "m", Digest: "d"}) {
		t.Fatalf("CurrentListing = %+v", listing)
	}
	k := key
	snap, err := s.ReadSnapshot(ctx, evEndpoint, AdapterOllama, &k, "", []string{"m"}, time.Now())
	if err != nil {
		t.Fatalf("keyed ReadSnapshot: %v", err)
	}
	if snap.Listing == nil || len(snap.Listing.Members) != 1 {
		t.Fatalf("keyed ReadSnapshot listing = %+v", snap.Listing)
	}
	// A row from before the repair has no adapter, so no route reads it.
	none := ""
	if old, err := s.CurrentListing(ctx, evEndpoint, AdapterOllama, none); err != nil || old != nil {
		t.Fatalf("pre-repair listing = %+v, %v; want none", old, err)
	}

	// Reopening changes nothing.
	if _, err := NewStore(ctx, db); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	assertMigrationVersions(t, db, "1,2,3,4")
}

func TestMigrationV4_FreshStoreHasAdapter(t *testing.T) {
	ctx := t.Context()
	db := openMigrationDB(t, filepath.Join(t.TempDir(), "fresh.db"))
	s, err := NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	assertMigrationVersions(t, db, "1,2,3,4")
	// The adapter column stays where V3 put it: V4 did not add a second one.
	assertMigrationSQL(t, db, "SELECT group_concat(name) FROM pragma_table_info('listing_snapshots')", "id,key_version,endpoint,adapter,account,started_at,committed_at")
	assertMigrationSQL(t, db, "SELECT group_concat(name) FROM pragma_index_info('idx_capability_evidence_lookup')", "key_version,endpoint,model_name,observed_at")
	assertMigrationSQL(t, db, "SELECT group_concat(name) FROM pragma_index_info('idx_model_access_lookup')", "key_version,endpoint,account,model_name,observed_at")
	if err := s.PublishListings(ctx, []ListingPublish{{Endpoint: evEndpoint, Adapter: AdapterOllama, StartedAt: time.Now()}}); err != nil {
		t.Fatalf("PublishListings: %v", err)
	}
}

// TestLookupQueriesUseTheirIndexes pins the plans of the two per-model reads
// ReadSnapshot repeats for every model. A plan that scans turns a read of a few
// hundred models into seconds behind the store's one connection.
func TestLookupQueriesUseTheirIndexes(t *testing.T) {
	s := newTestStore(t)
	for _, tc := range []struct {
		name, query string
		args        []any
		want        string
	}{
		{"evidence", modelEvidenceQuery, []any{EvidenceKeyVersion, evEndpoint, evModel},
			"SEARCH capability_evidence USING INDEX idx_capability_evidence_lookup (key_version=? AND endpoint=? AND model_name=?)"},
		{"access", latestAccessQuery, []any{EvidenceKeyVersion, evEndpoint, "", evModel, int64(1)},
			"SEARCH model_access USING INDEX idx_model_access_lookup (key_version=? AND endpoint=? AND account=? AND model_name=? AND observed_at<?)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(plan) == 0 || plan[0] != tc.want {
				t.Fatalf("plan = %q, want first step %q", plan, tc.want)
			}
		})
	}
}
