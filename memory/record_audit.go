package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kstruzzieri/go-llm/signing"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

var (
	// ErrRecordAuditViolation means stable stored record evidence failed validation.
	ErrRecordAuditViolation = errors.New("memory: record audit violation")
	// ErrRecordAuditIncomplete means record coverage could not be completed.
	ErrRecordAuditIncomplete = errors.New("memory: record audit incomplete")
	errUnsignedRecordHistory = errors.New("memory: unsigned record history")
	errSignedWithoutWitness  = errors.New("memory: signed record without initialization marker")
)

// RecordAuditReport is the content-free result of verifying stored agent memory.
type RecordAuditReport struct {
	Verified    int64
	TotalExtant *int64
	Configured  bool
}

type recordAuditError struct {
	tag   error
	cause error
	id    string
}

func (e *recordAuditError) Error() string {
	if e.id != "" {
		return fmt.Sprintf("%s: record %q", e.tag, e.id)
	}
	return e.tag.Error()
}

func (e *recordAuditError) Unwrap() []error { return []error{e.tag, e.cause} }

func auditRecordError(tag, cause error, id string) error {
	return &recordAuditError{tag: tag, cause: cause, id: id}
}

func auditStorageTag(err error, requiredSchema bool) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrRecordAuditIncomplete
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
			return ErrRecordAuditViolation
		case sqlite3.SQLITE_ERROR: // Absent required table or column for these fixed queries.
			if requiredSchema {
				return ErrRecordAuditViolation
			}
		}
	}
	return ErrRecordAuditIncomplete
}

// AuditRecords verifies every physical agent-memory row through one caller-owned
// database snapshot. It neither opens nor closes db and never returns record data.
func AuditRecords(ctx context.Context, db *sql.DB, keyDir string) (report RecordAuditReport, err error) {
	if ctx == nil {
		return report, auditRecordError(ErrRecordAuditIncomplete, errors.New("memory: nil audit context"), "")
	}
	if db == nil {
		return report, auditRecordError(ErrRecordAuditIncomplete, errors.New("memory: nil audit database"), "")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report, auditRecordError(auditStorageTag(err, false), err, "")
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM memory_schema_version`).Scan(&version); err != nil {
		return report, auditRecordError(auditStorageTag(err, false), err, "")
	}
	if version == 1 {
		total := int64(0)
		report.TotalExtant = &total
		return report, nil
	}
	if version != 2 && version != 3 {
		return report, auditRecordError(ErrRecordAuditIncomplete, fmt.Errorf("memory: unsupported schema version %d", version), "")
	}
	var total int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_records`).Scan(&total); err != nil {
		return report, auditRecordError(auditStorageTag(err, version == 3), err, "")
	}
	report.TotalExtant = &total
	if version == 2 {
		if total == 0 {
			return report, nil
		}
		return report, auditRecordError(ErrRecordAuditIncomplete, errUnsignedRecordHistory, "")
	}
	for _, query := range []string{
		`SELECT version, description, applied_at FROM memory_schema_version LIMIT 0`,
		`SELECT ` + recordColumns + ` FROM memory_records LIMIT 0`,
		`SELECT id, initialized_at FROM memory_record_signing LIMIT 0`,
	} {
		preflight, err := tx.QueryContext(ctx, query)
		if err != nil {
			return report, auditRecordError(auditStorageTag(err, true), err, "")
		}
		if err := preflight.Close(); err != nil {
			return report, auditRecordError(auditStorageTag(err, false), err, "")
		}
	}
	if err := tx.QueryRowContext(ctx, recordInitializedQuery).Scan(&report.Configured); err != nil {
		return report, auditRecordError(auditStorageTag(err, true), err, "")
	}
	if !report.Configured {
		if total == 0 {
			return report, nil
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, signature_alg, signature_key_id, signature FROM memory_records ORDER BY id`)
		if err != nil {
			return report, auditRecordError(auditStorageTag(err, true), err, "")
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, algorithm, keyID string
			var signature []byte
			if err := rows.Scan(&id, &algorithm, &keyID, &signature); err != nil {
				tag := ErrRecordAuditViolation
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					tag = ErrRecordAuditIncomplete
				}
				return report, auditRecordError(tag, err, "")
			}
			if algorithm != "" || keyID != "" || len(signature) != 0 {
				return report, auditRecordError(ErrRecordAuditViolation, errSignedWithoutWitness, id)
			}
		}
		if err := rows.Err(); err != nil {
			return report, auditRecordError(auditStorageTag(err, false), err, "")
		}
		return report, auditRecordError(ErrRecordAuditIncomplete, errUnsignedRecordHistory, "")
	}

	// Producer constraint: current.pem is the writer's existing private-key
	// artifact, so audit immediately derives and retains only its verifier.
	current, err := signing.LoadEd25519(filepath.Join(keyDir, "current.pem"))
	if err != nil {
		return report, auditRecordError(ErrRecordAuditIncomplete, err, "")
	}
	currentVerifier := current.Verifier()
	current = nil
	ring, err := loadRecordVerifiers(keyDir, currentVerifier)
	if err != nil {
		return report, auditRecordError(ErrRecordAuditIncomplete, err, "")
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+recordColumns+` FROM memory_records ORDER BY id`)
	if err != nil {
		return report, auditRecordError(auditStorageTag(err, true), err, "")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		record, scanErr := scanRecord(rows)
		if scanErr != nil {
			tag := ErrRecordAuditViolation
			if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
				tag = ErrRecordAuditIncomplete
			}
			return report, auditRecordError(tag, scanErr, "")
		}
		if meaningErr := validateRecordMeaning(record.MemoryRecordBody); meaningErr != nil {
			return report, auditRecordError(ErrRecordAuditViolation, fmt.Errorf("%w: %w", ErrRecordIntegrity, meaningErr), record.ID)
		}
		if record.Signature.Alg == "" || record.Signature.KeyID == "" || len(record.Signature.Bytes) == 0 {
			return report, auditRecordError(ErrRecordAuditViolation, fmt.Errorf("%w: %w", ErrRecordIntegrity, errMissingRecordSignature), record.ID)
		}
		if verifyErr := verifyRecord(ctx, ring, record); verifyErr != nil {
			tag := ErrRecordAuditViolation
			if errors.Is(verifyErr, signing.ErrUnknownKey) || errors.Is(verifyErr, context.Canceled) || errors.Is(verifyErr, context.DeadlineExceeded) {
				tag = ErrRecordAuditIncomplete
			}
			return report, auditRecordError(tag, verifyErr, record.ID)
		}
		report.Verified++
	}
	if err := rows.Err(); err != nil {
		return report, auditRecordError(auditStorageTag(err, false), err, "")
	}
	if err := rows.Close(); err != nil {
		return report, auditRecordError(auditStorageTag(err, false), err, "")
	}
	return report, nil
}
