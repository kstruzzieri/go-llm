// routing_feedback_sqlite_migration.go owns the schema migrations for the
// SQLite-backed routing-feedback store. Schema lives in a version-tracking
// table (routing_feedback_schema_version) that the migration runner
// inserts into atomically inside each migration's own transaction, so a
// crash between DDL and version-record cannot leave a half-applied state.
//
// Pattern mirrors rag/migration.go so future migrations follow the same
// shape; intentional duplication beats coupling provider/ to rag/ types.
package provider

import (
	"database/sql"
	"fmt"
	"time"
)

// migration is one ordered schema step.
type migration struct {
	version     int
	description string
	fn          func(tx *sql.Tx) error
}

// feedbackMigrations is the ordered list of routing-feedback-store schema
// migrations. v1 is the baseline single-table schema with indexes and
// CHECK constraints matching the in-memory store's validation rules.
var feedbackMigrations = []migration{
	{
		version:     1,
		description: "baseline routing_feedback_signals table + indexes",
		fn:          migrateFeedbackV1,
	},
}

func migrateFeedbackV1(tx *sql.Tx) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS routing_feedback_signals (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			provider      TEXT NOT NULL,
			model         TEXT NOT NULL,
			use_case      TEXT NOT NULL,
			kind          TEXT NOT NULL CHECK (kind IN ('success', 'failure', 'latency')),
			strength      REAL,
			at_ns         INTEGER NOT NULL,
			latency_ms    INTEGER NOT NULL DEFAULT 0 CHECK (latency_ms >= 0),
			error_class   TEXT NOT NULL DEFAULT '',
			route_id      TEXT NOT NULL DEFAULT '',
			completion_id TEXT NOT NULL DEFAULT '',
			meta          TEXT NOT NULL DEFAULT '{}',
			CHECK (
				(kind = 'success' AND latency_ms = 0 AND error_class = '') OR
				(kind = 'failure' AND latency_ms = 0 AND error_class <> '') OR
				(kind = 'latency' AND latency_ms > 0 AND error_class = '')
			)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_rfs_key_at
			ON routing_feedback_signals(provider, model, use_case, at_ns, id)`,
		`CREATE INDEX IF NOT EXISTS idx_rfs_key_kind
			ON routing_feedback_signals(provider, model, use_case, kind)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("provider: feedback migrate v1: %w", err)
		}
	}
	return nil
}

// runFeedbackMigrations applies all pending migrations. Handles three
// scenarios identical to rag/runMigrations:
//  1. Fresh DB: creates routing_feedback_schema_version, runs all migrations.
//  2. Existing DB at version N: runs only versions > N.
//  3. Pre-migration DB (routing_feedback_signals table exists, no version
//     row): records v1 as already-applied, then runs v2+.
func runFeedbackMigrations(db *sql.DB) error {
	const createVersionTable = `CREATE TABLE IF NOT EXISTS routing_feedback_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.Exec(createVersionTable); err != nil {
		return fmt.Errorf("provider: create feedback version table: %w", err)
	}

	currentVersion, err := currentFeedbackSchemaVersion(db)
	if err != nil {
		return err
	}

	if currentVersion == 0 && tableExists(db, "routing_feedback_signals") {
		// Database predates the migration runner. Validate the existing
		// schema matches v1 before marking it as applied; blessing an
		// incompatible table would surface as a confusing "no such
		// column" error far from the actual cause.
		if err := validateExistingSignalsSchema(db); err != nil {
			return fmt.Errorf("provider: pre-existing routing_feedback_signals table is incompatible with v1 schema: %w", err)
		}
		if err := recordFeedbackVersionsUpTo(db, 1); err != nil {
			return err
		}
		currentVersion = 1
	}

	for _, m := range feedbackMigrations {
		if m.version <= currentVersion {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("provider: begin feedback migration v%d: %w", m.version, err)
		}
		if err := m.fn(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("provider: feedback migration v%d (%s): %w", m.version, m.description, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO routing_feedback_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
			m.version, m.description, time.Now().Unix(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("provider: record feedback version %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("provider: commit feedback migration v%d: %w", m.version, err)
		}
	}
	return nil
}

// currentFeedbackSchemaVersion returns the highest applied version, or 0
// when no version rows exist.
func currentFeedbackSchemaVersion(db *sql.DB) (int, error) {
	var version int
	err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM routing_feedback_schema_version`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("provider: query feedback schema version: %w", err)
	}
	return version, nil
}

// tableExists checks whether a table by the given name exists. Mirrors
// the rag helper of the same name; provider-local copy to avoid an
// upward dependency on rag/.
func tableExists(db *sql.DB, name string) bool {
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&count)
	return err == nil && count > 0
}

// validateExistingSignalsSchema runs a no-op SELECT against every column
// the v1 schema declares. SQLite reports "no such column" if any column
// is missing or named differently. Used by runFeedbackMigrations when a
// pre-existing routing_feedback_signals table is detected without a
// schema_version row.
//
// The SELECT 0 LIMIT 0 form scans no rows; we only care about the column
// resolution at prepare time.
func validateExistingSignalsSchema(db *sql.DB) error {
	_, err := db.Exec(`SELECT
		id, provider, model, use_case, kind, strength, at_ns,
		latency_ms, error_class, route_id, completion_id, meta
		FROM routing_feedback_signals
		WHERE 0 LIMIT 0`)
	if err != nil {
		return err
	}
	return nil
}

// recordFeedbackVersionsUpTo inserts version records for every migration
// up to and including upTo. Used for pre-migration DB detection so a
// crash cannot leave a partial version state.
func recordFeedbackVersionsUpTo(db *sql.DB, upTo int) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("provider: begin feedback version recording: %w", err)
	}
	now := time.Now().Unix()
	for _, m := range feedbackMigrations {
		if m.version > upTo {
			break
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO routing_feedback_schema_version (version, description, applied_at) VALUES (?, ?, ?)`,
			m.version, m.description+" (pre-existing)", now,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("provider: record feedback version %d: %w", m.version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("provider: commit feedback version recording: %w", err)
	}
	return nil
}
