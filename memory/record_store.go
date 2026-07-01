package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MemoryRecordStore is the agent-memory record store. It shares the memory
// package's DB and migration chain with SQLiteStore but owns the memory_records
// tables. One implementation; consumers define their own narrow interfaces.
type MemoryRecordStore struct {
	db *sql.DB
}

// NewMemoryRecordStore runs the shared migrations on db and returns a record
// store. db must already be opened and hardened by the caller.
func NewMemoryRecordStore(ctx context.Context, db *sql.DB) (*MemoryRecordStore, error) {
	_ = ctx
	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("memory: init record store: %w", err)
	}
	return &MemoryRecordStore{db: db}, nil
}

// visibilityClause is the shared WHERE fragment enforcing derived visibility.
// Bind args in order: workspaceID, sessionID.
const visibilityClause = `(workspace_id = '' OR workspace_id = ?) AND (session_id = '' OR session_id = ?)`

// recordColumns is the canonical SELECT column list, matching scanRecord.
const recordColumns = `id, kind, content, namespace, workspace_id, session_id,
	source_kind, source_id, source_start, source_end, source_hash, metadata,
	created_at, updated_at, expires_at, deleted_at`

// recordColumnsAlias mirrors recordColumns with an `mr.` table alias for joined
// queries (Search). Keep the two column lists in lockstep with scanRecord.
const recordColumnsAlias = `mr.id, mr.kind, mr.content, mr.namespace, mr.workspace_id, mr.session_id,
	mr.source_kind, mr.source_id, mr.source_start, mr.source_end, mr.source_hash, mr.metadata,
	mr.created_at, mr.updated_at, mr.expires_at, mr.deleted_at`

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
	); err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: scan record: %w", err)
	}
	m.Kind = MemoryKind(kind)
	m.Metadata = json.RawMessage(metadata)
	m.CreatedAt = fromMs(createdMs)
	m.UpdatedAt = fromMs(updatedMs)
	m.ExpiresAt = fromMs(expiresMs)
	m.DeletedAt = fromMs(deletedMs)
	return m, nil
}

// Create validates and stores a new record.
func (s *MemoryRecordStore) Create(ctx context.Context, in CreateRecordParams) (MemoryRecord, error) {
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
	metadata, err := normalizeMetadata(in.Metadata)
	if err != nil {
		return MemoryRecord{}, err
	}

	now := time.Now().UnixMilli()
	m := MemoryRecord{
		ID:          newID(),
		Kind:        in.Kind,
		Content:     content,
		Namespace:   in.Namespace,
		WorkspaceID: in.WorkspaceID,
		SessionID:   in.SessionID,
		Provenance:  in.Provenance,
		Metadata:    json.RawMessage(metadata),
		CreatedAt:   time.UnixMilli(now),
		UpdatedAt:   time.UnixMilli(now),
		ExpiresAt:   in.ExpiresAt,
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: create: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memory_records (id, kind, content, namespace, workspace_id, session_id,
			source_kind, source_id, source_start, source_end, source_hash, metadata,
			created_at, updated_at, expires_at, deleted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		m.ID, string(m.Kind), m.Content, m.Namespace, m.WorkspaceID, m.SessionID,
		m.Provenance.SourceKind, m.Provenance.SourceID, m.Provenance.Start, m.Provenance.End, m.Provenance.Hash,
		metadata, now, now, toMs(m.ExpiresAt)); err != nil {
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
	m, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryRecord{}, ErrRecordNotFound
		}
		return MemoryRecord{}, err
	}
	return m, nil
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
		m, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: search records: iterate: %w", err)
	}
	return out, nil
}

// SoftDelete marks the record deleted (deleted_at set) within acc's scope and
// removes its FTS row. An absent, already-deleted, or out-of-scope id returns
// ErrRecordNotFound (matches SQLiteStore.SoftDelete; not idempotent-silent).
func (s *MemoryRecordStore) SoftDelete(ctx context.Context, id string, acc RecordAccess) error {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: soft delete: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE memory_records SET deleted_at = ?, updated_at = ?
		  WHERE id = ? AND deleted_at = 0 AND `+visibilityClause,
		now, now, id, acc.WorkspaceID, acc.SessionID)
	if err != nil {
		return fmt.Errorf("memory: soft delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: soft delete: rows: %w", err)
	}
	if n == 0 {
		return ErrRecordNotFound
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
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: promote: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE memory_records SET kind = ?, session_id = '', expires_at = 0, updated_at = ?
		  WHERE id = ? AND deleted_at = 0 AND `+visibilityClause,
		string(to), now, id, acc.WorkspaceID, acc.SessionID)
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: promote: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: promote: rows: %w", err)
	}
	if n == 0 {
		return MemoryRecord{}, ErrRecordNotFound
	}
	// Re-read the mutated row within the tx. FTS is untouched: content is
	// preserved, only kind/session/expiry changed.
	row := tx.QueryRowContext(ctx, `SELECT `+recordColumns+` FROM memory_records WHERE id = ?`, id)
	m, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryRecord{}, ErrRecordNotFound
		}
		return MemoryRecord{}, fmt.Errorf("memory: promote: reselect: %w", err)
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
	// Validate inputs before opening the tx (matches Create's validate-first pattern).
	var newContent *string
	if in.Content != nil {
		c := strings.TrimSpace(*in.Content)
		if c == "" {
			return MemoryRecord{}, ErrEmptyContent
		}
		newContent = &c
	}
	var metadata *string
	if in.Metadata != nil {
		norm, err := normalizeMetadata(in.Metadata)
		if err != nil {
			return MemoryRecord{}, err
		}
		metadata = &norm
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
	cur, err := scanRecord(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MemoryRecord{}, ErrRecordNotFound
		}
		return MemoryRecord{}, err
	}

	if newContent != nil {
		cur.Content = *newContent
	}
	if in.Namespace != nil {
		cur.Namespace = *in.Namespace
	}
	if metadata != nil {
		cur.Metadata = json.RawMessage(*metadata)
	}
	if in.ExpiresAt != nil {
		cur.ExpiresAt = *in.ExpiresAt
	}
	now := time.Now().UnixMilli()
	cur.UpdatedAt = time.UnixMilli(now)

	if _, err := tx.ExecContext(ctx,
		`UPDATE memory_records SET content = ?, namespace = ?, metadata = ?, expires_at = ?, updated_at = ?
		  WHERE id = ?`,
		cur.Content, cur.Namespace, string(cur.Metadata), toMs(cur.ExpiresAt), now, id); err != nil {
		return MemoryRecord{}, fmt.Errorf("memory: update: %w", err)
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
