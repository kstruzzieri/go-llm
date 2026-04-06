package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Store defines conversation persistence operations.
type Store interface {
	Save(ctx context.Context, conv Conversation) error
	Load(ctx context.Context, id string) (*Conversation, error)
	List(ctx context.Context) ([]Summary, error)
	Delete(ctx context.Context, id string) error
}

// SQLiteStore is a conversation Store backed by SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewStore creates a conversation store on the given database, running
// migrations if needed.
func NewStore(ctx context.Context, db *sql.DB) (*SQLiteStore, error) {
	if err := runMigrations(db); err != nil {
		return nil, fmt.Errorf("conversation: init store: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Save persists a conversation using upsert semantics.
func (s *SQLiteStore) Save(ctx context.Context, conv Conversation) error {
	if conv.ID == "" {
		return fmt.Errorf("conversation: save: id is required")
	}

	msgs := conv.Messages
	if msgs == nil {
		msgs = []Message{}
	}

	messagesJSON, err := json.Marshal(msgs)
	if err != nil {
		return fmt.Errorf("conversation: save: marshal messages: %w", err)
	}

	now := time.Now().UnixMilli()

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, title, messages, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			title      = excluded.title,
			messages   = excluded.messages,
			updated_at = excluded.updated_at`,
		conv.ID, conv.Title, string(messagesJSON), now, now,
	)
	if err != nil {
		return fmt.Errorf("conversation: save %q: %w", conv.ID, err)
	}
	return nil
}

// Load retrieves a conversation by ID. Returns ErrNotFound if not found.
func (s *SQLiteStore) Load(ctx context.Context, id string) (*Conversation, error) {
	var conv Conversation
	var messagesJSON string
	var createdMs, updatedMs int64

	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, messages, created_at, updated_at FROM conversations WHERE id = ?`,
		id,
	).Scan(&conv.ID, &conv.Title, &messagesJSON, &createdMs, &updatedMs)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("conversation: load %q: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("conversation: load %q: %w", id, err)
	}

	if err := json.Unmarshal([]byte(messagesJSON), &conv.Messages); err != nil {
		return nil, fmt.Errorf("conversation: load %q: unmarshal messages: %w", id, err)
	}

	conv.CreatedAt = time.UnixMilli(createdMs)
	conv.UpdatedAt = time.UnixMilli(updatedMs)

	return &conv, nil
}

// List returns summaries ordered by most recently updated first.
func (s *SQLiteStore) List(ctx context.Context) ([]Summary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, json_array_length(messages), created_at, updated_at
		 FROM conversations
		 ORDER BY updated_at DESC, id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("conversation: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var summaries []Summary
	for rows.Next() {
		var s Summary
		var createdMs, updatedMs int64
		if err := rows.Scan(&s.ID, &s.Title, &s.MessageCount, &createdMs, &updatedMs); err != nil {
			return nil, fmt.Errorf("conversation: list: scan: %w", err)
		}
		s.CreatedAt = time.UnixMilli(createdMs)
		s.UpdatedAt = time.UnixMilli(updatedMs)
		summaries = append(summaries, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversation: list: iterate: %w", err)
	}

	if summaries == nil {
		summaries = []Summary{}
	}
	return summaries, nil
}

// Delete removes a conversation by ID. Returns nil if not found (idempotent).
func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("conversation: delete %q: %w", id, err)
	}
	return nil
}
