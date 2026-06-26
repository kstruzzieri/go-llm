package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Store defines conversation persistence operations.
type Store interface {
	Save(ctx context.Context, conv Conversation) error
	Load(ctx context.Context, id string) (*Conversation, error)
	List(ctx context.Context) ([]Summary, error)
	Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error)
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
	summaryContent, summaryMessageCount := durableSummaryValues(conv.DurableSummary)

	now := time.Now().UnixMilli()

	searchBody := searchText(msgs)
	if summaryContent != "" {
		searchBody += "\n" + summaryContent
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("conversation: save %q: begin: %w", conv.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO conversations (id, title, messages, summary_content, summary_message_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			title                 = excluded.title,
			messages              = excluded.messages,
			summary_content       = excluded.summary_content,
			summary_message_count = excluded.summary_message_count,
			updated_at            = excluded.updated_at`,
		conv.ID, conv.Title, string(messagesJSON), summaryContent, summaryMessageCount, now, now,
	)
	if err != nil {
		return fmt.Errorf("conversation: save %q: %w", conv.ID, err)
	}
	if err := s.saveSearchIndex(ctx, tx, conv.ID, conv.Title, searchBody, len(msgs), now, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("conversation: save %q: commit: %w", conv.ID, err)
	}
	return nil
}

// Load retrieves a conversation by ID. Returns ErrNotFound if not found.
func (s *SQLiteStore) Load(ctx context.Context, id string) (*Conversation, error) {
	var conv Conversation
	var messagesJSON string
	var summaryContent string
	var summaryMessageCount int
	var createdMs, updatedMs int64

	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, messages, summary_content, summary_message_count, created_at, updated_at FROM conversations WHERE id = ?`,
		id,
	).Scan(&conv.ID, &conv.Title, &messagesJSON, &summaryContent, &summaryMessageCount, &createdMs, &updatedMs)
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
	if summaryContent != "" || summaryMessageCount > 0 {
		conv.DurableSummary = &DurableSummary{
			Content:      summaryContent,
			MessageCount: summaryMessageCount,
		}
	}

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

func (s *SQLiteStore) Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error) {
	match := sanitizeFTS5Query(query)
	if match == "" {
		return []SearchResult{}, nil
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, s.title,
		        snippet(conversation_fts, 2, '', '', '...', 12) AS snippet,
		        s.message_count, s.created_at, s.updated_at,
		        bm25(conversation_fts) AS score
		   FROM conversation_fts
		   JOIN conversation_search s ON s.id = conversation_fts.id
		  WHERE conversation_fts MATCH ?
		    AND (? = '' OR s.id LIKE ? || '%')
		  ORDER BY score ASC, s.updated_at DESC, s.id ASC
		  LIMIT ?`,
		match, opts.IDPrefix, opts.IDPrefix, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("conversation: search: %w", err)
	}
	defer func() { _ = rows.Close() }()

	results := []SearchResult{}
	for rows.Next() {
		var r SearchResult
		var createdMs, updatedMs int64
		if err := rows.Scan(&r.ID, &r.Title, &r.Snippet, &r.MessageCount, &createdMs, &updatedMs, &r.Score); err != nil {
			return nil, fmt.Errorf("conversation: search: scan: %w", err)
		}
		r.CreatedAt = time.UnixMilli(createdMs)
		r.UpdatedAt = time.UnixMilli(updatedMs)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("conversation: search: iterate: %w", err)
	}
	return results, nil
}

// Delete removes a conversation by ID. Returns nil if not found (idempotent).
func (s *SQLiteStore) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("conversation: delete %q: begin: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = ?`, id); err != nil {
		return fmt.Errorf("conversation: delete %q: %w", id, err)
	}
	if err := s.deleteSearchIndex(ctx, tx, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("conversation: delete %q: commit: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) saveSearchIndex(ctx context.Context, tx *sql.Tx, id, title, body string, msgCount int, createdMs, updatedMs int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_fts WHERE id = ?`, id); err != nil {
		return fmt.Errorf("conversation: save %q: delete search fts: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversation_search (id, title, body, message_count, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			title         = excluded.title,
			body          = excluded.body,
			message_count = excluded.message_count,
			updated_at    = excluded.updated_at`,
		id, title, body, msgCount, createdMs, updatedMs,
	); err != nil {
		return fmt.Errorf("conversation: save %q: upsert search metadata: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversation_fts (id, title, body) VALUES (?, ?, ?)`,
		id, title, body,
	); err != nil {
		return fmt.Errorf("conversation: save %q: insert search fts: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) deleteSearchIndex(ctx context.Context, tx *sql.Tx, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_fts WHERE id = ?`, id); err != nil {
		return fmt.Errorf("conversation: delete %q: delete search fts: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_search WHERE id = ?`, id); err != nil {
		return fmt.Errorf("conversation: delete %q: delete search metadata: %w", id, err)
	}
	return nil
}

func durableSummaryValues(s *DurableSummary) (string, int) {
	if s == nil {
		return "", 0
	}
	return s.Content, s.MessageCount
}

func searchText(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role != "" {
			b.WriteString(m.Role)
			b.WriteString(": ")
		}
		b.WriteString(m.Content)
		if m.ToolName != "" {
			b.WriteByte(' ')
			b.WriteString(m.ToolName)
		}
		if m.ToolCallID != "" {
			b.WriteByte(' ')
			b.WriteString(m.ToolCallID)
		}
		if len(m.ToolCalls) > 0 {
			b.WriteByte(' ')
			b.Write(m.ToolCalls)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func sanitizeFTS5Query(query string) string {
	var tokens []string
	var current strings.Builder
	for _, r := range query {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			current.WriteRune(r)
		} else if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	if len(tokens) == 0 {
		return ""
	}
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	return strings.Join(quoted, " ")
}
