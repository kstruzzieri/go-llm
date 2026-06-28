package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// SQLiteStore is a memory store backed by SQLite. One implementation; consumers
// define their own narrow interfaces where they need a seam.
type SQLiteStore struct {
	db *sql.DB
}

// NewStore runs migrations on db and returns a store. db must already be opened
// and hardened by the caller (cmd/golem owns file path, mode, and PRAGMAs).
func NewStore(ctx context.Context, db *sql.DB) (*SQLiteStore, error) {
	_ = ctx
	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("memory: init store: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("memory: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}

type rowScanner interface{ Scan(dest ...any) error }

func scanMemory(r rowScanner) (Memory, error) {
	var m Memory
	var scope string
	var createdMs, updatedMs int64
	if err := r.Scan(&m.ID, &m.Text, &scope, &m.WorkspaceID, &m.SourceSessionID, &createdMs, &updatedMs); err != nil {
		return Memory{}, fmt.Errorf("memory: scan: %w", err)
	}
	m.Scope = Scope(scope)
	m.CreatedAt = time.UnixMilli(createdMs)
	m.UpdatedAt = time.UnixMilli(updatedMs)
	return m, nil
}

// Add stores a new memory. Text is trimmed and required. ScopeWorkspace requires
// WorkspaceID; ScopeGlobal forces WorkspaceID="".
func (s *SQLiteStore) Add(ctx context.Context, in AddParams) (Memory, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return Memory{}, ErrEmptyText
	}
	ws := in.WorkspaceID
	switch in.Scope {
	case ScopeGlobal:
		ws = ""
	case ScopeWorkspace:
		if strings.TrimSpace(ws) == "" {
			return Memory{}, fmt.Errorf("memory: workspace scope requires workspace_id")
		}
	default:
		return Memory{}, ErrBadScope
	}
	now := time.Now().UnixMilli()
	m := Memory{
		ID:              newID(),
		Text:            text,
		Scope:           in.Scope,
		WorkspaceID:     ws,
		SourceSessionID: in.SourceSessionID,
		CreatedAt:       time.UnixMilli(now),
		UpdatedAt:       time.UnixMilli(now),
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Memory{}, fmt.Errorf("memory: add: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memories (id, text, scope, workspace_id, source_session_id, created_at, updated_at, deleted_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		m.ID, m.Text, string(m.Scope), m.WorkspaceID, m.SourceSessionID, now, now); err != nil {
		return Memory{}, fmt.Errorf("memory: add: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memories_fts (id, text) VALUES (?, ?)`, m.ID, m.Text); err != nil {
		return Memory{}, fmt.Errorf("memory: add: fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Memory{}, fmt.Errorf("memory: add: commit: %w", err)
	}
	return m, nil
}

// Search returns live, visible memories whose text matches query (FTS5/bm25,
// porter-stemmed), best match first. Empty/punctuation-only query => empty slice.
func (s *SQLiteStore) Search(ctx context.Context, query string, opts SearchOptions) ([]Memory, error) {
	match := sanitizeFTS5Query(query)
	if match == "" {
		return []Memory{}, nil
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 8
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT m.id, m.text, m.scope, m.workspace_id, m.source_session_id, m.created_at, m.updated_at
		   FROM memories_fts
		   JOIN memories m ON m.id = memories_fts.id
		  WHERE memories_fts MATCH ?
		    AND m.deleted_at = 0
		    AND (m.scope = 'global' OR m.workspace_id = ?)
		  ORDER BY bm25(memories_fts) ASC, m.updated_at DESC, m.id ASC
		  LIMIT ?`,
		match, opts.WorkspaceID, limit)
	if err != nil {
		return nil, fmt.Errorf("memory: search: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: search: iterate: %w", err)
	}
	return out, nil
}

// List returns live memories visible to opts.WorkspaceID (global + that
// workspace), newest first. Returns a non-nil empty slice when none match.
func (s *SQLiteStore) List(ctx context.Context, opts ListOptions) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, text, scope, workspace_id, source_session_id, created_at, updated_at
		   FROM memories
		  WHERE deleted_at = 0 AND (scope = 'global' OR workspace_id = ?)
		  ORDER BY updated_at DESC, id ASC`,
		opts.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("memory: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: list: iterate: %w", err)
	}
	return out, nil
}
