package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/signing"
)

// MemoryRecordStore is the agent-memory record store. It shares the memory
// package's DB and migration chain with SQLiteStore but owns the memory_records
// tables. One implementation; consumers define their own narrow interfaces.
type MemoryRecordStore struct {
	db           *sql.DB
	signer       signing.Signer
	verifiers    *signing.Keyring
	origin       string
	createdKeyID string
}

// NewMemoryRecordStore runs the shared migrations on db and returns a record
// store with mandatory signing. db must already be opened and hardened by
// the caller; config must select injected credentials or a dedicated KeyDir.
func NewMemoryRecordStore(ctx context.Context, db *sql.DB, config RecordStoreConfig) (*MemoryRecordStore, error) {
	if ctx == nil {
		return nil, errors.New("memory: nil record store context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("memory: init record store: %w", err)
	}
	origin, _ := config.origin()
	store := &MemoryRecordStore{db: db, origin: origin}
	if err := store.initializeSigning(ctx, config); err != nil {
		return nil, err
	}
	return store, nil
}

// CreatedKeyID reports the first successfully initialized filesystem identity.
// It is empty on ordinary reopen and with injected credentials.
func (s *MemoryRecordStore) CreatedKeyID() string { return s.createdKeyID }

// visibilityClause is the shared WHERE fragment enforcing derived visibility.
// Bind args in order: workspaceID, sessionID.
const visibilityClause = `(workspace_id = '' OR workspace_id = ?) AND (session_id = '' OR session_id = ?)`

// recordColumns is the canonical SELECT column list, matching scanRecord.
const recordColumns = `id, kind, content, namespace, workspace_id, session_id,
	source_kind, source_id, source_start, source_end, source_hash, metadata,
	created_at, updated_at, expires_at, deleted_at,
	origin_tool, origin_session_id, trust_class, signature_alg, signature_key_id, signature`

// recordColumnsAlias mirrors recordColumns with an `mr.` table alias for joined
// queries (Search). Keep the two column lists in lockstep with scanRecord.
const recordColumnsAlias = `mr.id, mr.kind, mr.content, mr.namespace, mr.workspace_id, mr.session_id,
	mr.source_kind, mr.source_id, mr.source_start, mr.source_end, mr.source_hash, mr.metadata,
	mr.created_at, mr.updated_at, mr.expires_at, mr.deleted_at,
	mr.origin_tool, mr.origin_session_id, mr.trust_class, mr.signature_alg, mr.signature_key_id, mr.signature`

// visibilityClauseAlias is visibilityClause with the `mr.` alias, for joined
// queries. Bind args in order: workspaceID, sessionID.
const visibilityClauseAlias = `(mr.workspace_id = '' OR mr.workspace_id = ?) AND (mr.session_id = '' OR mr.session_id = ?)`

func toMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// normalizeMetadata trims raw, returns "{}" for empty, else validates it parses.
func normalizeMetadata(raw json.RawMessage) (string, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(s)) {
		return "", ErrBadMetadata
	}
	return s, nil
}

func validKind(k MemoryKind) bool {
	switch k {
	case KindWorking, KindSemantic, KindEpisodic:
		return true
	default:
		return false
	}
}

func scanRecord(r rowScanner) (MemoryRecord, error) {
	var (
		m                    MemoryRecord
		kind, metadata       string
		createdMs, updatedMs int64
		expiresMs, deletedMs int64
	)
	if err := r.Scan(
		&m.ID, &kind, &m.Content, &m.Namespace, &m.WorkspaceID, &m.SessionID,
		&m.Provenance.SourceKind, &m.Provenance.SourceID, &m.Provenance.Start, &m.Provenance.End, &m.Provenance.Hash,
		&metadata, &createdMs, &updatedMs, &expiresMs, &deletedMs,
		&m.Provenance.OriginTool, &m.Provenance.OriginSessionID, &m.Provenance.TrustClass, &m.Signature.Alg, &m.Signature.KeyID, &m.Signature.Bytes,
	); err != nil {
		return MemoryRecord{}, recordScanError(err)
	}
	m.Kind = MemoryKind(kind)
	m.Metadata = json.RawMessage(metadata)
	m.CreatedAt = fromMs(createdMs)
	m.UpdatedAt = fromMs(updatedMs)
	m.ExpiresAt = fromMs(expiresMs)
	m.DeletedAt = fromMs(deletedMs)
	normalizeRecordTimes(&m.MemoryRecordBody)
	return m, nil
}

// Create validates and stores a new record.
func (s *MemoryRecordStore) Create(ctx context.Context, in CreateRecordParams) (MemoryRecord, error) {
	if err := validateRecordContent(in.Content); err != nil {
		return MemoryRecord{}, err
	}
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return MemoryRecord{}, ErrEmptyContent
	}
	if !validKind(in.Kind) {
		return MemoryRecord{}, ErrBadKind
	}
	if in.SessionID != "" && in.WorkspaceID == "" {
		return MemoryRecord{}, ErrSessionNeedsWorkspace
	}
	if in.Kind == KindWorking && in.SessionID == "" {
		return MemoryRecord{}, ErrWorkingNeedsSession
	}
	// End == 0 means "unset" (partial provenance, e.g. a known start with no end is
	// allowed). Only validate ordering when End is actually set.
	if in.Provenance.End != 0 && in.Provenance.End < in.Provenance.Start {
		return MemoryRecord{}, ErrBadProvenanceRange
	}
	now := time.Now().UnixMilli()
	in.Provenance.OriginTool = s.origin
	in.Provenance.OriginSessionID = in.SessionID
	in.Provenance.TrustClass = TrustAgentWritten
	m := MemoryRecord{MemoryRecordBody: MemoryRecordBody{
		ID:          newID(),
		Kind:        in.Kind,
		Content:     content,
		Namespace:   in.Namespace,
		WorkspaceID: in.WorkspaceID,
		SessionID:   in.SessionID,
		Provenance:  in.Provenance,
		Metadata:    in.Metadata,
		CreatedAt:   time.UnixMilli(now),
		UpdatedAt:   time.UnixMilli(now),
		ExpiresAt:   in.ExpiresAt,
	}}

	if err := validateRecordSize(m.MemoryRecordBody); err != nil {
		return MemoryRecord{}, err
	}
	metadata, err := normalizeMetadata(m.Metadata)
	if err != nil {
		return MemoryRecord{}, err
	}
	m.Metadata = json.RawMessage(metadata)
	if err := signRecord(ctx, s.signer, &m); err != nil {
		return MemoryRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_records (`+recordColumns+`)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, recordValues(m)...); err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: create: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_records_fts (id, content) VALUES (?, ?)`, m.ID, m.Content); err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: create: fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: create: commit: %w", err)
	}
	return m, nil
}

// Get returns the live record with the given id visible under acc. Expired records
// are still returned (direct fetch; expiry filtering is Search's concern). A miss
// or a record outside acc's scope returns ErrRecordNotFound.
func (s *MemoryRecordStore) Get(ctx context.Context, id string, acc RecordAccess) (MemoryRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+recordColumns+`
		   FROM memory_records
		  WHERE id = ? AND deleted_at = 0 AND `+visibilityClause,
		id, acc.WorkspaceID, acc.SessionID)
	return s.verifiedRecord(ctx, row)
}

// Search returns live records visible under opts (workspace + session), best match
// first. An empty/punctuation-only query lists by recency (updated_at DESC); a
// non-empty query ranks by FTS5 bm25. Expired records are excluded unless
// opts.IncludeExpired. Returns a non-nil empty slice on no match.
func (s *MemoryRecordStore) Search(ctx context.Context, query string, opts RecordSearchOptions) ([]MemoryRecord, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 8
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	var (
		where []string
		args  []any
	)
	where = append(where, `mr.deleted_at = 0`)
	where = append(where, visibilityClauseAlias)
	args = append(args, opts.WorkspaceID, opts.SessionID)
	if opts.Kind != "" {
		where = append(where, `mr.kind = ?`)
		args = append(args, string(opts.Kind))
	}
	if opts.Namespace != "" {
		where = append(where, `mr.namespace = ?`)
		args = append(args, opts.Namespace)
	}
	if !opts.IncludeExpired {
		where = append(where, `NOT (mr.expires_at != 0 AND mr.expires_at <= ?)`)
		args = append(args, now.UnixMilli())
	}

	var q string
	match := sanitizeFTS5Query(query)
	if match == "" {
		q = `SELECT ` + recordColumnsAlias + ` FROM memory_records mr WHERE ` + strings.Join(where, " AND ") +
			` ORDER BY mr.updated_at DESC, mr.id ASC LIMIT ?`
		args = append(args, limit)
	} else {
		q = `SELECT ` + recordColumnsAlias + ` FROM memory_records_fts f JOIN memory_records mr ON mr.id = f.id
			WHERE f.content MATCH ? AND ` + strings.Join(where, " AND ") +
			` ORDER BY bm25(memory_records_fts) ASC, mr.updated_at DESC, mr.id ASC LIMIT ?`
		args = append([]any{match}, args...)
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("memory: search records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []MemoryRecord{}
	for rows.Next() {
		m, err := s.verifiedRecord(ctx, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, recordScanError(err)
	}
	if err := rows.Close(); err != nil {
		return nil, recordScanError(err)
	}
	return out, nil
}

// SoftDelete marks the record deleted (deleted_at set) within acc's scope and
// removes its FTS row. An absent, already-deleted, or out-of-scope id returns
// ErrRecordNotFound (matches SQLiteStore.SoftDelete; not idempotent-silent).
func (s *MemoryRecordStore) SoftDelete(ctx context.Context, id string, acc RecordAccess) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: soft delete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	m, err := s.verifiedRecord(ctx, tx.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM memory_records WHERE id = ? AND deleted_at = 0 AND `+visibilityClause, id, acc.WorkspaceID, acc.SessionID))
	if err != nil {
		return err
	}
	m.DeletedAt = time.Now()
	m.UpdatedAt = m.DeletedAt
	if err := signRecord(ctx, s.signer, &m); err != nil {
		return err
	}
	if err := persistRecord(ctx, tx, m); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_records_fts WHERE id = ?`, id); err != nil {
		return fmt.Errorf("memory: soft delete: fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: soft delete: commit: %w", err)
	}
	return nil
}

// Promote converts a record to a durable kind (semantic or episodic) within acc's
// scope: it sheds the session binding (session_id cleared), clears expiry, keeps
// the workspace, and preserves content/provenance. to must be KindSemantic or
// KindEpisodic. A miss or out-of-scope record returns ErrRecordNotFound. The
// from-kind is deliberately unrestricted (the intended flow is working ->
// durable, but re-promoting an already-durable record is a harmless no-op).
func (s *MemoryRecordStore) Promote(ctx context.Context, id string, acc RecordAccess, to MemoryKind) (MemoryRecord, error) {
	if to != KindSemantic && to != KindEpisodic {
		return MemoryRecord{}, ErrBadPromotion
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: promote: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	m, err := s.verifiedRecord(ctx, tx.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM memory_records WHERE id = ? AND deleted_at = 0 AND `+visibilityClause, id, acc.WorkspaceID, acc.SessionID))
	if err != nil {
		return MemoryRecord{}, err
	}
	m.Kind, m.SessionID, m.ExpiresAt, m.UpdatedAt = to, "", time.Time{}, time.Now()
	if err := signRecord(ctx, s.signer, &m); err != nil {
		return MemoryRecord{}, err
	}
	if err := persistRecord(ctx, tx, m); err != nil {
		return MemoryRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: promote: commit: %w", err)
	}
	return m, nil
}

// Update applies the non-nil fields of in to the record visible under acc, bumps
// updated_at, and re-syncs the FTS row when Content changed. Kind/workspace/session
// are not mutable here. A miss or out-of-scope record returns ErrRecordNotFound.
func (s *MemoryRecordStore) Update(ctx context.Context, id string, acc RecordAccess, in UpdateRecordParams) (MemoryRecord, error) {
	// Bound replacement content before opening the transaction. Metadata is
	// validated with the complete resulting body after the preimage is verified.
	var newContent *string
	if in.Content != nil {
		if err := validateRecordContent(*in.Content); err != nil {
			return MemoryRecord{}, err
		}
		c := strings.TrimSpace(*in.Content)
		if c == "" {
			return MemoryRecord{}, ErrEmptyContent
		}
		newContent = &c
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: update: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Resolve within scope; also gives us existence.
	row := tx.QueryRowContext(ctx,
		`SELECT `+recordColumns+` FROM memory_records
		  WHERE id = ? AND deleted_at = 0 AND `+visibilityClause,
		id, acc.WorkspaceID, acc.SessionID)
	cur, err := s.verifiedRecord(ctx, row)
	if err != nil {
		return MemoryRecord{}, err
	}

	if newContent != nil {
		cur.Content = *newContent
	}
	if in.Namespace != nil {
		cur.Namespace = *in.Namespace
	}
	if in.Metadata != nil {
		cur.Metadata = in.Metadata
	}
	if in.ExpiresAt != nil {
		cur.ExpiresAt = *in.ExpiresAt
	}
	if err := validateRecordSize(cur.MemoryRecordBody); err != nil {
		return MemoryRecord{}, err
	}
	if in.Metadata != nil {
		metadata, err := normalizeMetadata(cur.Metadata)
		if err != nil {
			return MemoryRecord{}, err
		}
		cur.Metadata = json.RawMessage(metadata)
	}
	cur.UpdatedAt = time.Now()
	if err := signRecord(ctx, s.signer, &cur); err != nil {
		return MemoryRecord{}, err
	}
	if err := persistRecord(ctx, tx, cur); err != nil {
		return MemoryRecord{}, err
	}

	if newContent != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_records_fts WHERE id = ?`, id); err != nil {
			return MemoryRecord{}, fmt.Errorf("memory: update: fts delete: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_records_fts (id, content) VALUES (?, ?)`, id, cur.Content); err != nil {
			return MemoryRecord{}, fmt.Errorf("memory: update: fts insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: update: commit: %w", err)
	}
	return cur, nil
}

// SQL scan errors can quote stored bytes; retain only safe cancellation/miss
// identities and one fixed malformed-row diagnosis.
func recordScanError(err error) error {
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w: malformed stored row", ErrRecordIntegrity)
}

func validateRecordMeaning(body MemoryRecordBody) error {
	switch {
	case body.ID == "":
		return errInvalidRecordBody
	case !validKind(body.Kind):
		return ErrBadKind
	case strings.TrimSpace(body.Content) == "":
		return ErrEmptyContent
	case body.SessionID != "" && body.WorkspaceID == "":
		return ErrSessionNeedsWorkspace
	case body.Kind == KindWorking && body.SessionID == "":
		return ErrWorkingNeedsSession
	case body.Provenance.End != 0 && body.Provenance.End < body.Provenance.Start:
		return ErrBadProvenanceRange
	}
	return nil
}

func (s *MemoryRecordStore) verifiedRecord(ctx context.Context, row rowScanner) (MemoryRecord, error) {
	m, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MemoryRecord{}, ErrRecordNotFound
	}
	if err != nil {
		return MemoryRecord{}, err
	}
	if err := validateRecordMeaning(m.MemoryRecordBody); err != nil {
		return MemoryRecord{}, fmt.Errorf("%w: %w", ErrRecordIntegrity, err)
	}
	if err := verifyRecord(ctx, s.verifiers, m); err != nil {
		return MemoryRecord{}, err
	}
	return m, nil
}

func recordValues(m MemoryRecord) []any {
	return []any{
		m.ID, string(m.Kind), m.Content, m.Namespace, m.WorkspaceID, m.SessionID,
		m.Provenance.SourceKind, m.Provenance.SourceID, m.Provenance.Start, m.Provenance.End, m.Provenance.Hash,
		string(m.Metadata), toMs(m.CreatedAt), toMs(m.UpdatedAt), toMs(m.ExpiresAt), toMs(m.DeletedAt),
		m.Provenance.OriginTool, m.Provenance.OriginSessionID, string(m.Provenance.TrustClass), string(m.Signature.Alg), m.Signature.KeyID, m.Signature.Bytes,
	}
}

// persistRecord writes the exact signed body through the transaction that read
// its preimage (or the authorized import transaction). IDs remain immutable.
func persistRecord(ctx context.Context, tx *sql.Tx, m MemoryRecord) error {
	values := recordValues(m)
	args := append(values[1:], m.ID)
	result, err := tx.ExecContext(ctx, `UPDATE memory_records SET
 kind = ?, content = ?, namespace = ?, workspace_id = ?, session_id = ?,
 source_kind = ?, source_id = ?, source_start = ?, source_end = ?, source_hash = ?, metadata = ?,
 created_at = ?, updated_at = ?, expires_at = ?, deleted_at = ?,
 origin_tool = ?, origin_session_id = ?, trust_class = ?, signature_alg = ?, signature_key_id = ?, signature = ? WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("memory: persist record: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: persist record count: %w", err)
	}
	if n != 1 {
		return ErrRecordNotFound
	}
	return nil
}
