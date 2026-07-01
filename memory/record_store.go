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
