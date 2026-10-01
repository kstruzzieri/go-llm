// transcript/migration_test.go
package transcript

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/kstruzzieri/go-llm/internal/sqlitedsn"
)

func openMem(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1) // :memory: opens a private DB per connection otherwise
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func columns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		out[name] = true
	}
	return out
}

func TestMigrate_FreshCreatesBothTables(t *testing.T) {
	db := openMem(t)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	raw := columns(t, db, "raw_chat_calls")
	for _, c := range []string{
		"call_id", "conversation_key", "conversation_id", "identity_source",
		"request_messages", "response_message", "model", "provider", "route_outcome_json",
		"created_at", "projection_status", "projection_error",
	} {
		if !raw[c] {
			t.Errorf("raw_chat_calls missing column %q", c)
		}
	}
	conv := columns(t, db, "conversations")
	for _, c := range []string{
		"id", "title", "messages", "created_at", "updated_at",
		"conversation_key", "identity_source", "latest_call_id", "message_count", "stitch_status",
		"rendered_messages",
	} {
		if !conv[c] {
			t.Errorf("conversations missing column %q", c)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	db := openMem(t)
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestMigrate_LegacyFiveColumnTableGetsAuditColumns(t *testing.T) {
	db := openMem(t)
	// Simulate a conversation/ store DB: exact five-column baseline + one row.
	if _, err := db.Exec(`CREATE TABLE conversations (
		id         TEXT PRIMARY KEY,
		title      TEXT NOT NULL DEFAULT '',
		messages   TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, messages, created_at, updated_at) VALUES (?,?,?,?,?)`,
		"legacy-1", "old", `[{"role":"user","content":"hi"}]`, int64(1), int64(2),
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	conv := columns(t, db, "conversations")
	for _, c := range []string{
		"conversation_key", "identity_source", "latest_call_id", "message_count", "stitch_status",
		"rendered_messages",
	} {
		if !conv[c] {
			t.Errorf("legacy conversations missing audit column %q after migrate", c)
		}
	}

	// Pre-existing row survives and stays capture-readable (five base columns).
	var (
		id, title, messages string
		created, updated    int64
	)
	if err := db.QueryRow(
		`SELECT id, title, messages, created_at, updated_at FROM conversations WHERE id = ?`, "legacy-1",
	).Scan(&id, &title, &messages, &created, &updated); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if id != "legacy-1" || title != "old" || messages != `[{"role":"user","content":"hi"}]` {
		t.Errorf("legacy row mutated: id=%q title=%q messages=%q", id, title, messages)
	}

	// Backfilled audit defaults are usable.
	var convKey string
	var msgCount int
	if err := db.QueryRow(
		`SELECT conversation_key, message_count FROM conversations WHERE id = ?`, "legacy-1",
	).Scan(&convKey, &msgCount); err != nil {
		t.Fatalf("read audit cols: %v", err)
	}
	if convKey != "" || msgCount != 0 {
		t.Errorf("legacy audit defaults = (%q,%d), want (\"\",0)", convKey, msgCount)
	}
}

// openUpgradeDB opens path with a 5s busy_timeout on every connection and one
// pooled connection, and sets WAL before any racing call.
func openUpgradeDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn, err := sqlitedsn.WithBusyTimeout(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	execUpgradeSQL(t, db, "PRAGMA journal_mode=WAL")
	return db
}

func execUpgradeSQL(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), query); err != nil {
		t.Fatalf("Exec(%q): %v", query, err)
	}
}

// seedLegacyConversations creates the five-column conversation/ baseline plus
// the named audit columns, one legacy row, and the objects migrate creates
// before the audit upgrade, so a racing migrate needs no lock before its probe.
func seedLegacyConversations(t *testing.T, db *sql.DB, present ...string) {
	t.Helper()
	execUpgradeSQL(t, db, `CREATE TABLE conversations (
		id         TEXT PRIMARY KEY,
		title      TEXT NOT NULL DEFAULT '',
		messages   TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	for _, name := range present {
		for _, col := range auditColumns {
			if col.name == name {
				execUpgradeSQL(t, db, fmt.Sprintf("ALTER TABLE conversations ADD COLUMN %s %s", col.name, col.ddl))
			}
		}
	}
	execUpgradeSQL(t, db, `INSERT INTO conversations (id, title, messages, created_at, updated_at) VALUES ('legacy-1', 'old', '[]', 1, 2)`)
	execUpgradeSQL(t, db, createRawTable)
	execUpgradeSQL(t, db, createRawIndex)
}

// assertAuditUpgrade checks every audit column exists and the legacy row kept
// its data with the declared defaults.
func assertAuditUpgrade(t *testing.T, db *sql.DB) {
	t.Helper()
	conv := columns(t, db, "conversations")
	for _, col := range auditColumns {
		if !conv[col.name] {
			t.Errorf("conversations missing audit column %q", col.name)
		}
	}
	var title, messages, convKey, identity, latest, stitch, rendered string
	var created, updated, msgCount int64
	if err := db.QueryRow(`
		SELECT title, messages, created_at, updated_at,
		       conversation_key, identity_source, latest_call_id,
		       message_count, stitch_status, rendered_messages
		FROM conversations WHERE id = 'legacy-1'`,
	).Scan(&title, &messages, &created, &updated, &convKey,
		&identity, &latest, &msgCount, &stitch, &rendered); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if title != "old" || messages != "[]" || created != 1 || updated != 2 {
		t.Errorf("legacy data = (%q,%q,%d,%d), want (\"old\",\"[]\",1,2)", title, messages, created, updated)
	}
	if convKey != "" || identity != "" || latest != "" || msgCount != 0 || stitch != "" || rendered != "" {
		t.Errorf("audit defaults = (%q,%q,%q,%d,%q,%q), want all empty and 0", convKey, identity, latest, msgCount, stitch, rendered)
	}
}

func assertSQLiteCode(t *testing.T, err error, want int) {
	t.Helper()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code()&255 != want {
		t.Fatalf("SQLite error = %v, want primary code %d", err, want)
	}
}

func upgradeResult(t *testing.T, ctx context.Context, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return ctx.Err()
	}
}

// TestAuditUpgradeOwnsWriteLockBeforeAltering: between its probe and its
// ALTERs the upgrade must hold SQLite's write lock, so a racing opener waits
// and then re-probes instead of acting on a stale column list.
func TestAuditUpgradeOwnsWriteLockBeforeAltering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.db")
	a, b := openUpgradeDB(t, path), openUpgradeDB(t, path)
	seedLegacyConversations(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	locked, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	done := make(chan error, 1)
	go func() {
		done <- addMissingAuditColumnsWith(ctx, a, auditUpgradeHooks{afterLock: func() {
			close(locked)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}})
	}()
	select {
	case <-locked:
	case err := <-done:
		t.Fatalf("upgrade finished before its lock gate: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	execUpgradeSQL(t, b, "PRAGMA busy_timeout=0")
	_, lockErr := b.ExecContext(ctx, "BEGIN IMMEDIATE")
	if lockErr == nil {
		execUpgradeSQL(t, b, "ROLLBACK")
	}
	unblock()
	if err := upgradeResult(t, ctx, done); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	assertSQLiteCode(t, lockErr, sqlite3.SQLITE_BUSY)
	assertAuditUpgrade(t, a)
}

// TestAuditUpgradeRechecksColumnsUnderLock: an upgrade that saw missing
// columns must re-check under the lock and skip columns another opener added
// meanwhile, instead of failing with "duplicate column name".
func TestAuditUpgradeRechecksColumnsUnderLock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present []string
	}{
		{"legacy", nil},
		{"partial", []string{"identity_source", "stitch_status"}},
		{"newest-missing", []string{"conversation_key", "identity_source", "latest_call_id", "message_count", "stitch_status"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "transcript.db")
			a, b := openUpgradeDB(t, path), openUpgradeDB(t, path)
			seedLegacyConversations(t, a, tc.present...)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			probed, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			done := make(chan error, 1)
			go func() {
				done <- addMissingAuditColumnsWith(ctx, a, auditUpgradeHooks{afterProbe: func() {
					close(probed)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}})
			}()
			select {
			case <-probed:
			case err := <-done:
				t.Fatalf("upgrade finished before its probe gate: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// A has seen missing columns and holds no lock; B upgrades first.
			if err := migrate(ctx, b); err != nil {
				t.Fatalf("competing migrate: %v", err)
			}
			unblock()
			if err := upgradeResult(t, ctx, done); err != nil {
				t.Fatalf("upgrade after a competing upgrade: %v", err)
			}
			assertAuditUpgrade(t, a)
		})
	}
}

// TestAuditUpgradeWaitsForConcurrentUpgrade: two openers both on the slow
// path. While A holds the write lock, B must wait on busy_timeout instead of
// failing at once, then find A's columns and succeed.
func TestAuditUpgradeWaitsForConcurrentUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.db")
	a, b := openUpgradeDB(t, path), openUpgradeDB(t, path)
	seedLegacyConversations(t, a)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	aLocked, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	aDone := make(chan error, 1)
	go func() {
		aDone <- addMissingAuditColumnsWith(ctx, a, auditUpgradeHooks{afterLock: func() {
			close(aLocked)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}})
	}()
	select {
	case <-aLocked:
	case err := <-aDone:
		t.Fatalf("A finished before its lock gate: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	bProbed := make(chan struct{})
	bDone := make(chan error, 1)
	go func() {
		bDone <- addMissingAuditColumnsWith(ctx, b, auditUpgradeHooks{afterProbe: func() { close(bProbed) }})
	}()
	select {
	case <-bProbed:
	case err := <-bDone:
		t.Fatalf("B finished before its probe gate: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-bDone:
		t.Fatalf("B returned while A held the write lock: %v; want it to wait on busy_timeout", err)
	case <-time.After(200 * time.Millisecond):
	}
	unblock()
	if err := upgradeResult(t, ctx, aDone); err != nil {
		t.Fatalf("A: %v", err)
	}
	if err := upgradeResult(t, ctx, bDone); err != nil {
		t.Fatalf("B after A's upgrade: %v", err)
	}
	assertAuditUpgrade(t, a)
	assertAuditUpgrade(t, b)
}

// TestAuditUpgradeIsAtomic: a failure mid-upgrade leaves no column behind,
// and a retry completes the upgrade.
func TestAuditUpgradeIsAtomic(t *testing.T) {
	db := openMem(t)
	seedLegacyConversations(t, db)
	injected := errors.New("injected ALTER failure")
	err := addMissingAuditColumnsWith(t.Context(), db, auditUpgradeHooks{beforeAlter: func(col string) error {
		if col == auditColumns[1].name {
			return injected
		}
		return nil
	}})
	if !errors.Is(err, injected) {
		t.Fatalf("upgrade err = %v, want the injected failure", err)
	}
	if columns(t, db, "conversations")[auditColumns[0].name] {
		t.Fatalf("column %q survived a failed upgrade", auditColumns[0].name)
	}
	if err := addMissingAuditColumns(t.Context(), db); err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertAuditUpgrade(t, db)
}

// TestMigrateCurrentSchemaTakesNoWriteLock: on an up-to-date database the
// real Open only reads, so it returns at once while another connection holds
// the write lock. Existing-object CREATE ... IF NOT EXISTS and journal_mode=WAL
// on a WAL database take no write lock under modernc v1.46.1 (SQLite 3.51.2);
// this test pins that.
func TestMigrateCurrentSchemaTakesNoWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.db")
	a := openUpgradeDB(t, path)
	if err := migrate(t.Context(), a); err != nil {
		t.Fatal(err)
	}
	execUpgradeSQL(t, a, "BEGIN IMMEDIATE")
	start := time.Now()
	store, err := Open(t.Context(), path)
	elapsed := time.Since(start)
	execUpgradeSQL(t, a, "ROLLBACK")
	if err != nil {
		t.Fatalf("current-schema Open behind a writer: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if elapsed >= time.Second {
		t.Fatalf("current-schema Open took %v, want under 1s", elapsed)
	}
}
