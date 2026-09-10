package conversation

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

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

func TestRunMigrations_FreshDB(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(db); err != nil {
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
	if version != 4 {
		t.Fatalf("schema version = %d, want 4", version)
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(db); err != nil {
		t.Fatalf("first runMigrations() error: %v", err)
	}
	if err := runMigrations(db); err != nil {
		t.Fatalf("second runMigrations() error: %v", err)
	}

	var version int
	err := db.QueryRow(`SELECT MAX(version) FROM conversation_schema_version`).Scan(&version)
	if err != nil {
		t.Fatalf("version query failed: %v", err)
	}
	if version != 4 {
		t.Fatalf("schema version = %d, want 4", version)
	}
}

func TestRunMigrations_IndexExists(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(db); err != nil {
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
	if err := runMigrations(db); err != nil {
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
	if err := runMigrations(db); err != nil {
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

	// Only v3 and v4 should apply now (v1/v2 already stamped).
	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations() error: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT MAX(version) FROM conversation_schema_version`).Scan(&version); err != nil {
		t.Fatalf("version query failed: %v", err)
	}
	if version != 4 {
		t.Fatalf("schema version = %d, want 4", version)
	}

	// The pre-v3 row loads with a nil DurableSummary and preserved content.
	// NewStore re-runs migrations idempotently (already at v4).
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

func TestNewStore_MigratesSchemaV3Fixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "schema-v3.sql"))
	if err != nil {
		t.Fatalf("ReadFile(schema-v3.sql) error: %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "conversation.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(%q) error: %v", dbPath, err)
	}
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
		wantSchemaVersion  = 4
		wantVersionRecords = 4
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

	store, err := NewStore(context.Background(), db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewStore(first open) error: %v", err)
	}
	assertState("first open", db, store)
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
}
