// routing_feedback_sqlite_migration.go owns the routing-feedback schema.
// Each step claims its version before reads or DDL and commits both together.
// Failed steps roll back; earlier committed steps survive. Legacy v1 tables
// are validated and given any missing baseline indexes under the same claim.
// The package-local pattern mirrors conversation/migration.go (#543).
package provider

import (
	"context"
	"database/sql"
	"errors"
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

func runFeedbackMigrations(ctx context.Context, db *sql.DB) error {
	return runFeedbackMigrationsWith(ctx, db, feedbackMigrations)
}

func runFeedbackMigrationsWith(ctx context.Context, db *sql.DB, list []migration) error {
	if ctx == nil {
		return errors.New("provider: nil migration context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(list) == 0 {
		return nil
	}
	currentVersion, err := currentFeedbackSchemaVersion(ctx, db)
	if err != nil {
		return err
	}
	if currentVersion >= list[len(list)-1].version {
		return nil
	}
	const createVersionTable = `CREATE TABLE IF NOT EXISTS routing_feedback_schema_version (
		version     INTEGER PRIMARY KEY,
		description TEXT NOT NULL,
		applied_at  INTEGER NOT NULL
	)`
	if _, err := db.ExecContext(ctx, createVersionTable); err != nil {
		return fmt.Errorf("provider: create version table: %w", err)
	}
	for _, m := range list {
		if m.version <= currentVersion {
			continue
		}
		if err := applyFeedbackMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func applyFeedbackMigration(ctx context.Context, db *sql.DB, m migration) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("provider: begin migration v%d: %w", m.version, err)
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("provider: rollback migration v%d: %w", m.version, rbErr))
		}
	}()

	// Claim before any reads or DDL so SQLite waits for the writer without a
	// read-lock upgrade. The claim and migration commit together; a duplicate
	// version means another opener already committed this step.
	result, err := tx.ExecContext(ctx,
		`INSERT INTO routing_feedback_schema_version (version, description, applied_at)
		 VALUES (?, ?, ?) ON CONFLICT(version) DO NOTHING`,
		m.version, m.description, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("provider: claim migration v%d: %w", m.version, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("provider: claim migration v%d rows: %w", m.version, err)
	}
	if affected == 0 {
		return nil
	}
	if m.version == 1 {
		exists, err := tableExists(ctx, tx, "routing_feedback_signals")
		if err != nil {
			return fmt.Errorf("provider: probe routing_feedback_signals existence: %w", err)
		}
		if exists {
			if err := validateExistingSignalsSchema(ctx, tx); err != nil {
				return fmt.Errorf("provider: pre-existing routing_feedback_signals table is incompatible with v1 schema: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE routing_feedback_schema_version SET description = ? WHERE version = 1`,
				m.description+" (pre-existing)"); err != nil {
				return fmt.Errorf("provider: record pre-existing feedback version: %w", err)
			}
		}
	}
	if err := m.fn(tx); err != nil {
		return fmt.Errorf("provider: migration v%d (%s): %w", m.version, m.description, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("provider: commit migration v%d: %w", m.version, err)
	}
	return nil
}

func currentFeedbackSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var exists bool
	if err := db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'routing_feedback_schema_version')`,
	).Scan(&exists); err != nil {
		return 0, fmt.Errorf("provider: check version table: %w", err)
	}
	if !exists {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM routing_feedback_schema_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("provider: query schema version: %w", err)
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
func tableExists(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var count int
	if err := tx.QueryRowContext(ctx,
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
func validateExistingSignalsSchema(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `SELECT
		id, provider, model, use_case, kind, strength, at_ns,
		latency_ms, error_class, route_id, completion_id, meta
		FROM routing_feedback_signals
		WHERE 0 LIMIT 0`); err != nil {
		return fmt.Errorf("column check: %w", err)
	}
	var ddl string
	if err := tx.QueryRowContext(ctx,
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
