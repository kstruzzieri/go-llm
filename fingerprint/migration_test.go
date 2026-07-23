package fingerprint

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrations_FreshDB(t *testing.T) {
	db := openTestDB(t)
	if err := runMigrations(db); err != nil {
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
	if err := runMigrations(db); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := runMigrations(db); err != nil {
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
