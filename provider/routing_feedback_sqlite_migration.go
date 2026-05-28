// routing_feedback_sqlite_migration.go owns the schema migrations for the
// SQLite-backed routing-feedback store. Schema lives in a version-tracking
// table (routing_feedback_schema_version).
//
// Atomicity guarantee, per migration: each entry in feedbackMigrations
// runs its DDL and version-record INSERT inside one transaction, so a
// crash between those two cannot leave a half-applied state for that
// migration. Across migrations the guarantee is only "no partial
// individual step" — a crash between v1 and v2 commits leaves v1 applied
// and the next startup picks up at v2. The pre-migration-detection path
// (recording v1 for a DB that already has routing_feedback_signals)
// uses its own transaction; a crash between that and v2's commit also
// safely resumes at v2.
//
// Pattern mirrors rag/migration.go so future migrations follow the same
// shape; intentional duplication beats coupling provider/ to rag/ types.
package provider

import (
	"database/sql"
	"fmt"
	"strings"
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

	exists, err := tableExists(db, "routing_feedback_signals")
	if err != nil {
		return fmt.Errorf("provider: probe routing_feedback_signals existence: %w", err)
	}
	if currentVersion == 0 && exists {
		// Database predates the migration runner. Validate the existing
		// schema matches v1 before marking it as applied; blessing an
		// incompatible table would surface as a confusing "no such
		// column" error far from the actual cause — or worse, silently
		// accept rows that v1's CHECK constraints would have rejected.
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

// tableExists checks whether a table by the given name exists. Returns
// (false, nil) when the table is absent; (false, err) when the
// underlying probe fails so callers can distinguish "not present" from
// "could not determine" — a closed or corrupt DB would otherwise be
// silently treated as "table missing" and cascade into a worse error
// later in the migration loop. Mirrors the rag helper of the same name;
// provider-local copy to avoid an upward dependency on rag/.
func tableExists(db *sql.DB, name string) (bool, error) {
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// validateExistingSignalsSchema checks that a pre-existing
// routing_feedback_signals table both has every v1 column AND carries
// the kind-specific composite CHECK constraint that enforces v1's
// payload invariants. Used by runFeedbackMigrations when the table is
// detected without a schema_version row.
//
// Two checks:
//  1. A SELECT 0 LIMIT 0 against every v1 column — SQLite reports "no
//     such column" if any column is missing or renamed.
//  2. A sqlite_master.sql lookup to verify the composite kind/latency/
//     error_class CHECK is present. Without this, a pre-existing table
//     that had the right columns but lacked the CHECK would be blessed
//     as v1 and silently accept rows v1 would have rejected.
func validateExistingSignalsSchema(db *sql.DB) error {
	if _, err := db.Exec(`SELECT
		id, provider, model, use_case, kind, strength, at_ns,
		latency_ms, error_class, route_id, completion_id, meta
		FROM routing_feedback_signals
		WHERE 0 LIMIT 0`); err != nil {
		return fmt.Errorf("column check: %w", err)
	}
	var ddl string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='routing_feedback_signals'`,
	).Scan(&ddl); err != nil {
		return fmt.Errorf("sqlite_master lookup: %w", err)
	}
	// The composite CHECK fingerprint — case-insensitive, whitespace-
	// tolerant. We only require the disjunction's distinguishing tokens
	// be present, not byte-for-byte identical, so a DBA-normalised DDL
	// (different newlines, different quoting) still passes.
	lower := strings.ToLower(ddl)
	for _, want := range []string{
		"kind = 'success'", "kind = 'failure'", "kind = 'latency'",
		"latency_ms = 0", "latency_ms > 0", "error_class",
	} {
		if !strings.Contains(lower, want) {
			return fmt.Errorf("missing v1 CHECK fingerprint %q in pre-existing DDL", want)
		}
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
