package feedback

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestRunMigrations(t *testing.T) {
	db := openTestDB(t)

	if err := runMigrations(db); err != nil {
		t.Fatalf("runMigrations: %v", err)
	}

	// Verify schema version was recorded.
	ver, err := currentSchemaVersion(db)
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
		if err := runMigrations(db); err != nil {
			t.Fatalf("runMigrations (pass %d): %v", i+1, err)
		}
	}

	ver, err := currentSchemaVersion(db)
	if err != nil {
		t.Fatalf("currentSchemaVersion: %v", err)
	}
	if ver != 1 {
		t.Errorf("schema version = %d, want 1", ver)
	}
}
