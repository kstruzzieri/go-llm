package fingerprint

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const evidenceColumns = `id, key_version, endpoint, adapter, model_name, model_digest,
	capability, evidence, kind, state, method, method_version, detail, http_status,
	observed_at, fresh_until, expires_at`

// modelEvidenceQuery reads one model's rows at an endpoint, for any adapter.
const modelEvidenceQuery = `SELECT ` + evidenceColumns + ` FROM capability_evidence
	WHERE key_version = ? AND endpoint = ? AND model_name = ? ORDER BY id`

// AppendEvidence appends rows in one transaction with one generation bump. Row
// IDs are assigned by the store; any set on the rows are ignored. Appending
// nothing writes nothing. One row that could never be read back rejects the whole
// batch before a transaction opens; see validateEvidenceRows.
func (s *SQLiteStore) AppendEvidence(ctx context.Context, rows ...EvidenceRow) error {
	if len(rows) == 0 {
		return nil
	}
	if err := validateEvidenceRows(rows); err != nil {
		return fmt.Errorf("fingerprint: append evidence: %w", err)
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		return insertEvidence(ctx, tx, rows)
	})
	if err != nil {
		return fmt.Errorf("fingerprint: append evidence: %w", err)
	}
	return nil
}

// EvidenceRows returns a model's rows at an endpoint for the current key
// version, through every adapter, in insertion order. Rows of unknown shape are
// included: ReadCapabilities decides what counts.
func (s *SQLiteStore) EvidenceRows(ctx context.Context, endpoint, model string) ([]EvidenceRow, error) {
	rows, err := s.db.QueryContext(ctx, modelEvidenceQuery, EvidenceKeyVersion, endpoint, model)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: evidence rows %q/%q: %w", endpoint, model, err)
	}
	out, err := scanEvidence(rows)
	if err != nil {
		return nil, fmt.Errorf("fingerprint: evidence rows %q/%q: %w", endpoint, model, err)
	}
	return out, nil
}

// Generation returns the store's write generation, which moves with every write
// transaction. A consumer that cached a decision re-reads when it has moved.
func (s *SQLiteStore) Generation(ctx context.Context) (int64, error) {
	var generation int64
	if err := s.db.QueryRowContext(ctx, `SELECT generation FROM store_meta`).Scan(&generation); err != nil {
		return 0, fmt.Errorf("fingerprint: read generation: %w", err)
	}
	return generation, nil
}

// Prune deletes evidence and access rows no reader can select at now or later, keeping 90 days
// of history and every older row that could still win. It decides from a read
// outside the transaction, so a prune with nothing to do opens no write
// transaction and bumps no generation. The decision stays safe against a
// concurrent writer, because a row added meanwhile cannot make a deleted row
// selectable, and a concurrent pruner, because dominance is transitive.
//
// ponytail: scans the whole log at every call. Measured at 31,500 rows (50
// models x 7 capabilities x 90 daily rows) an idle call takes about 55 ms; one
// that deletes 31,500 old rows takes about 200 ms. Restrict to partitions
// holding an old row if that ever shows up at open.
func (s *SQLiteStore) Prune(ctx context.Context, now time.Time) error {
	rows, err := s.db.QueryContext(ctx, `SELECT `+evidenceColumns+` FROM capability_evidence`)
	if err != nil {
		return fmt.Errorf("fingerprint: prune: read evidence: %w", err)
	}
	stored, err := scanEvidence(rows)
	if err != nil {
		return fmt.Errorf("fingerprint: prune: read evidence: %w", err)
	}
	ids := prunableEvidenceIDs(stored, now)
	access, err := s.readAccessRows(ctx)
	if err != nil {
		return fmt.Errorf("fingerprint: prune: read access: %w", err)
	}
	accessIDs := prunableAccessIDs(access, now)
	if len(ids) == 0 && len(accessIDs) == 0 {
		return nil
	}
	// The decision was made outside the transaction, and SQLite may reuse a
	// deleted row's id for a later insert. Matching observed_at as well keeps a
	// row appended meanwhile under a reused id from being deleted in its place.
	observed := make(map[int64]int64, len(stored)+len(access))
	for _, r := range stored {
		observed[r.ID] = r.ObservedAt.UnixMilli()
	}
	accessObserved := make(map[int64]int64, len(access))
	for _, r := range access {
		accessObserved[r.id] = r.observedAt
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		for table, doomed := range map[string][]int64{"capability_evidence": ids, "model_access": accessIDs} {
			at := observed
			if table == "model_access" {
				at = accessObserved
			}
			for _, id := range doomed {
				if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ? AND observed_at = ?`, id, at[id]); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("fingerprint: prune: %w", err)
	}
	return nil
}

// writeTx runs fn in a transaction whose first statement bumps the generation.
// Writing first makes SQLite wait for another writer within the busy timeout,
// where a transaction that read first would fail a read-lock upgrade.
func (s *SQLiteStore) writeTx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		// A failure such as a full disk makes SQLite roll back by itself, and
		// database/sql does not know: the rollback then finds no transaction.
		rbErr := tx.Rollback()
		if rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) && !strings.Contains(rbErr.Error(), "no transaction is active") {
			err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
	}()
	if _, err := tx.ExecContext(ctx, `UPDATE store_meta SET generation = generation + 1`); err != nil {
		return fmt.Errorf("bump generation: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// validateEvidenceRows names the first field that would keep a row from ever
// being read back: a KeyVersion other than EvidenceKeyVersion, an empty Endpoint,
// Adapter, Model, Capability or Method, or a zero ObservedAt. The error carries
// the field name and no row values. Unknown methods and states pass, because
// readers ignore them and other binaries may write them.
func validateEvidenceRows(rows []EvidenceRow) error {
	for _, r := range rows {
		var field string
		switch {
		case r.KeyVersion != EvidenceKeyVersion:
			field = "KeyVersion"
		case r.Endpoint == "":
			field = "Endpoint"
		case r.Adapter == "":
			field = "Adapter"
		case r.Model == "":
			field = "Model"
		case r.Capability == "":
			field = "Capability"
		case r.Method == "":
			field = "Method"
		case r.ObservedAt.IsZero():
			field = "ObservedAt"
		default:
			continue
		}
		return fmt.Errorf("row has invalid %s", field)
	}
	return nil
}

func insertEvidence(ctx context.Context, tx *sql.Tx, rows []EvidenceRow) error {
	for _, r := range rows {
		var httpStatus any // nil => NULL
		if r.HTTPStatus != 0 {
			httpStatus = r.HTTPStatus
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO capability_evidence
				(key_version, endpoint, adapter, model_name, model_digest,
				 capability, evidence, kind, state, method, method_version, detail, http_status,
				 observed_at, fresh_until, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.KeyVersion, r.Endpoint, r.Adapter, r.Model, r.Digest,
			r.Capability, string(r.Source), string(r.Kind), string(r.State), r.Method, r.MethodVersion, r.Detail, httpStatus,
			r.ObservedAt.UnixMilli(), millisOrNull(r.FreshUntil), millisOrNull(r.ExpiresAt))
		if err != nil {
			return fmt.Errorf("insert %q/%q/%q: %w", r.Endpoint, r.Model, r.Capability, err)
		}
	}
	return nil
}

// scanEvidence drains and closes rows selected with evidenceColumns.
func scanEvidence(rows *sql.Rows) (out []EvidenceRow, err error) {
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var r EvidenceRow
		var observedAt int64
		var httpStatus, freshUntil, expiresAt sql.NullInt64
		if err := rows.Scan(&r.ID, &r.KeyVersion, &r.Endpoint, &r.Adapter, &r.Model, &r.Digest,
			&r.Capability, &r.Source, &r.Kind, &r.State, &r.Method, &r.MethodVersion, &r.Detail, &httpStatus,
			&observedAt, &freshUntil, &expiresAt); err != nil {
			return nil, err
		}
		r.HTTPStatus = int(httpStatus.Int64)
		r.ObservedAt = time.UnixMilli(observedAt)
		r.FreshUntil = timeOrZero(freshUntil)
		r.ExpiresAt = timeOrZero(expiresAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

func millisOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func timeOrZero(ms sql.NullInt64) time.Time {
	if !ms.Valid {
		return time.Time{}
	}
	return time.UnixMilli(ms.Int64)
}
