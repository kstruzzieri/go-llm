package conversation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

// Store defines conversation persistence operations. Revision zero creates only
// an absent ID; positive revisions replace only that exact stored revision and
// advance it by one. A recreated ID must receive a revision greater than any of
// its deleted snapshots. Save returns the committed revision without modifying
// its input; retained callers must assign that result after success. Every error
// returns revision zero and leaves storage unchanged. Negative and maximum int64
// revisions are invalid. List and Search projections are not save tokens.
type Store interface {
	Save(ctx context.Context, conv Conversation) (int64, error)
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
// migrations if needed. Concurrent migrations coordinate through SQLite's write
// lock. Callers must configure busy_timeout on every connection that may migrate
// (for example with the DSN _pragma=busy_timeout(5000)); a zero or expired timeout
// can return a busy error, and cancellation is observed between statements, not
// during a lock wait. Opening a current schema requires only reads.
func NewStore(ctx context.Context, db *sql.DB) (*SQLiteStore, error) {
	if err := runMigrations(ctx, db); err != nil {
		return nil, fmt.Errorf("conversation: init store: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Save atomically persists a snapshot and its search projection. Revision zero
// creates at the durable store-wide revision floor plus one; positive revisions
// update only a matching stored revision, committing r+1. It returns the committed
// revision without modifying conv. Conflicts return *ConflictError. Invalid input
// revisions and an exhausted creation floor return non-conflict errors.
func (s *SQLiteStore) Save(ctx context.Context, conv Conversation) (int64, error) {
	if conv.ID == "" {
		return 0, fmt.Errorf("conversation: save: id is required")
	}

	if conv.Revision < 0 || conv.Revision == math.MaxInt64 {
		return 0, fmt.Errorf("conversation: save %q: invalid revision %d", conv.ID, conv.Revision)
	}

	msgs := conv.Messages
	if msgs == nil {
		msgs = []Message{}
	}

	messagesJSON, err := json.Marshal(msgs)
	if err != nil {
		return 0, fmt.Errorf("conversation: save: marshal messages: %w", err)
	}
	summaryContent, summaryMessageCount := durableSummaryValues(conv.DurableSummary)

	now := time.Now().UnixMilli()

	searchBody := searchText(msgs)
	if summaryContent != "" {
		searchBody += "\n" + summaryContent
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("conversation: save %q: begin: %w", conv.ID, err)
	}
	defer func() { _ = tx.Rollback() }()

	var revision int64
	if conv.Revision == 0 {
		// Write first: reading the floor before this statement would allow a
		// WAL read-to-write upgrade to fail with SQLITE_BUSY_SNAPSHOT.
		err = tx.QueryRowContext(ctx,
			`INSERT INTO conversations (id, title, messages, summary_content, summary_message_count, revision, created_at, updated_at)
			 SELECT ?, ?, ?, ?, ?, value + 1, ?, ? FROM conversation_revision_floor
			 WHERE id = 1 AND value < ?
			 ON CONFLICT(id) DO NOTHING RETURNING revision`,
			conv.ID, conv.Title, string(messagesJSON), summaryContent, summaryMessageCount, now, now, int64(math.MaxInt64),
		).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			// Even a no-row INSERT holds the writer lock. Prefer a duplicate-ID
			// conflict over exhaustion, and never repair missing metadata.
			var exists bool
			var floor int64
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM conversations WHERE id = ?), value
				 FROM conversation_revision_floor WHERE id = 1`, conv.ID,
			).Scan(&exists, &floor); err != nil {
				return 0, fmt.Errorf("conversation: save %q: read revision floor: %w", conv.ID, err)
			}
			if !exists {
				if floor == math.MaxInt64 {
					return 0, fmt.Errorf("conversation: save %q: revision exhausted", conv.ID)
				}
				return 0, fmt.Errorf("conversation: save %q: no revision allocated", conv.ID)
			}
		}
	} else {
		err = tx.QueryRowContext(ctx,
			`UPDATE conversations SET title = ?, messages = ?, summary_content = ?, summary_message_count = ?,
			 revision = revision + 1, updated_at = ?
			 WHERE id = ? AND revision = ? RETURNING revision`,
			conv.Title, string(messagesJSON), summaryContent, summaryMessageCount, now, conv.ID, conv.Revision,
		).Scan(&revision)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, &ConflictError{ID: conv.ID, ExpectedRevision: conv.Revision}
	}
	if err != nil {
		return 0, fmt.Errorf("conversation: save %q: %w", conv.ID, err)
	}
	if err := s.saveSearchIndex(ctx, tx, conv.ID, conv.Title, searchBody, len(msgs), now, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("conversation: save %q: commit: %w", conv.ID, err)
	}
	return revision, nil
}

// Load retrieves a conversation by ID. Returns ErrNotFound if not found.
func (s *SQLiteStore) Load(ctx context.Context, id string) (*Conversation, error) {
	var conv Conversation
	var messagesJSON string
	var summaryContent string
	var summaryMessageCount int
	var createdMs, updatedMs int64

	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, messages, summary_content, summary_message_count, revision, created_at, updated_at FROM conversations WHERE id = ?`,
		id,
	).Scan(&conv.ID, &conv.Title, &messagesJSON, &summaryContent, &summaryMessageCount, &conv.Revision, &createdMs, &updatedMs)
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

// Delete unconditionally removes a conversation by ID. Its revision is captured
// by the schema trigger in the same transaction as the search deletion. It returns
// nil if not found (idempotent); a stale caller can still delete a newer snapshot.
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
