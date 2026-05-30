// transcript/store.go
package transcript

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kstruzzieri/go-llm/conversation"
)

// Store persists MCP chat calls: an immutable raw_chat_calls row (source of
// truth) followed by a best-effort canonical conversations projection. Safe for
// concurrent use — a store-level mutex serializes Record so concurrent same-key
// calls cannot interleave their stitch decisions.
type Store struct {
	db    *sql.DB
	ownDB bool
	path  string

	mu             sync.Mutex // serializes Record (preserves the stitch invariant)
	now            func() time.Time
	leaseWindow    time.Duration
	shortThreshold int

	closeOnce sync.Once
}

// Open opens (or creates) a transcript database at path and runs migrations.
// Pass ":memory:" explicitly for in-process tests (empty string is rejected to
// avoid the default-string footgun). The returned store owns the *sql.DB and
// closes it on Close.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("transcript: Open requires non-empty path; pass \":memory:\" explicitly")
	}
	if path != ":memory:" {
		if err := prepareTranscriptDBFile(path); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("transcript: open sqlite %q: %w", path, err)
	}
	// modernc.org/sqlite applies PRAGMAs per-connection; clamp the pool to one
	// connection so the WAL + busy_timeout settings hold for every write, and so
	// :memory: does not open a fresh private DB per connection.
	db.SetMaxOpenConns(1)
	if path != ":memory:" {
		for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
			if _, err := db.ExecContext(ctx, pragma); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("transcript: %s: %w", pragma, err)
			}
		}
		if err := chmodTranscriptDBFiles(path); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if path != ":memory:" {
		if err := chmodTranscriptDBFiles(path); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return &Store{
		db:             db,
		ownDB:          true,
		path:           dbPath(path),
		now:            time.Now,
		leaseWindow:    defaultLeaseWindow,
		shortThreshold: defaultShortThreshold,
	}, nil
}

const (
	transcriptDirMode  os.FileMode = 0o700
	transcriptFileMode os.FileMode = 0o600
)

func prepareTranscriptDBFile(path string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, transcriptDirMode); err != nil {
			return fmt.Errorf("transcript: create directory %q: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, transcriptFileMode)
	if err != nil {
		return fmt.Errorf("transcript: create sqlite %q: %w", path, err)
	}
	if err := f.Chmod(transcriptFileMode); err != nil {
		_ = f.Close()
		return fmt.Errorf("transcript: chmod sqlite %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("transcript: close sqlite %q: %w", path, err)
	}
	return nil
}

func dbPath(path string) string {
	if path == ":memory:" {
		return ""
	}
	return path
}

func chmodTranscriptDBFiles(path string) error {
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if err := chmodTranscriptFileIfExists(file); err != nil {
			return err
		}
	}
	return nil
}

func chmodTranscriptFileIfExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("transcript: stat %q: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("transcript: %q is a directory", path)
	}
	if err := os.Chmod(path, transcriptFileMode); err != nil {
		return fmt.Errorf("transcript: chmod %q: %w", path, err)
	}
	return nil
}

// Close releases the underlying *sql.DB. Safe to call multiple times.
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		if s.ownDB {
			err = s.db.Close()
		}
	})
	return err
}

// Record persists one chat call. It appends the immutable raw row first
// (returning an error only when that append fails — meaning nothing was
// persisted) then projects the canonical conversation best-effort; a projection
// failure is logged, recorded on the raw row, and swallowed so a non-nil return
// always means "nothing was persisted."
func (s *Store) Record(ctx context.Context, in RecordInput) error {
	if s.path != "" {
		if err := chmodTranscriptDBFiles(s.path); err != nil {
			return err
		}
		defer func() {
			if err := chmodTranscriptDBFiles(s.path); err != nil {
				log.Printf("transcript: failed to secure sqlite files: %v", err)
			}
		}()
	}
	callID, err := newCallID()
	if err != nil {
		return fmt.Errorf("transcript: generate call id: %w", err)
	}
	key, source := conversationKey(in)

	reqJSON, err := json.Marshal(nonNilMessages(in.Request))
	if err != nil {
		return fmt.Errorf("transcript: marshal request: %w", err)
	}
	respJSON, err := json.Marshal(in.Response)
	if err != nil {
		return fmt.Errorf("transcript: marshal response: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UnixMilli()

	if err := s.insertRaw(ctx, rawRow{
		callID:         callID,
		key:            key,
		conversationID: key,
		identitySource: source,
		requestJSON:    string(reqJSON),
		responseJSON:   string(respJSON),
		model:          in.Model,
		provider:       in.Provider,
		routeOutcome:   in.RouteOutcome,
		createdAt:      now,
	}); err != nil {
		return fmt.Errorf("transcript: append raw call: %w", err)
	}

	if perr := s.project(ctx, callID, key, source, in, now); perr != nil {
		log.Printf("transcript: canonical projection failed for call %s: %v", callID, perr)
		s.finalizeRawError(ctx, callID, perr)
	}
	return nil
}

type rawRow struct {
	callID         string
	key            string
	conversationID string
	identitySource string
	requestJSON    string
	responseJSON   string
	model          string
	provider       string
	routeOutcome   json.RawMessage
	createdAt      int64
}

func (s *Store) insertRaw(ctx context.Context, r rawRow) error {
	var routeOutcome any // nil → SQL NULL
	if len(r.routeOutcome) > 0 {
		routeOutcome = string(r.routeOutcome)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO raw_chat_calls
		   (call_id, conversation_key, conversation_id, identity_source,
		    request_messages, response_message, model, provider, route_outcome_json,
		    created_at, projection_status, projection_error)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL)`,
		r.callID, r.key, r.conversationID, r.identitySource,
		r.requestJSON, r.responseJSON, r.model, r.provider, routeOutcome,
		r.createdAt, "pending",
	)
	return err
}

// project loads the candidate set, decides the stitch, upserts the canonical
// row, and finalizes the raw row's projection columns. Any returned error is
// swallowed by Record (the raw row remains the durable source of truth).
func (s *Store) project(ctx context.Context, callID, key, source string, in RecordInput, now int64) error {
	incoming := append(append([]conversation.Message{}, in.Request...), in.Response)

	candidates, err := s.loadCandidates(ctx, key)
	if err != nil {
		return err
	}
	dec := decideStitch(key, incoming, candidates, time.UnixMilli(now), s.leaseWindow, s.shortThreshold)
	if err := s.upsertCanonical(ctx, dec, key, source, callID, incoming, now); err != nil {
		return err
	}
	return s.finalizeRawOK(ctx, callID, dec, key)
}

// loadCandidates returns every canonical row sharing the base key (base + forked
// siblings) plus any row whose id == key (the legacy-migrated explicit-id row,
// whose conversation_key is still ”). message_count is recomputed from the
// stored messages so a stale audit column never skews the stitch decision.
func (s *Store) loadCandidates(ctx context.Context, key string) ([]candidate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, messages, updated_at FROM conversations
		   WHERE conversation_key = ? OR id = ?`,
		key, key,
	)
	if err != nil {
		return nil, fmt.Errorf("transcript: load candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []candidate
	for rows.Next() {
		var (
			id        string
			msgsJSON  string
			updatedMs int64
		)
		if err := rows.Scan(&id, &msgsJSON, &updatedMs); err != nil {
			return nil, fmt.Errorf("transcript: scan candidate: %w", err)
		}
		var msgs []conversation.Message
		if err := json.Unmarshal([]byte(msgsJSON), &msgs); err != nil {
			return nil, fmt.Errorf("transcript: unmarshal candidate %q: %w", id, err)
		}
		out = append(out, candidate{id: id, messages: msgs, messageCount: len(msgs), updatedAt: updatedMs})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("transcript: iterate candidates: %w", err)
	}
	return out, nil
}

// upsertCanonical writes the canonical row for the decision. created/forked
// INSERT a new row; extended overwrites messages (longer history); idempotent
// refreshes recency/provenance only (never shrinks). Legacy rows (audit columns
// defaulted to ”) are backfilled when touched.
func (s *Store) upsertCanonical(ctx context.Context, dec stitchDecision, key, source, callID string, incoming []conversation.Message, now int64) error {
	switch dec.status {
	case statusCreated, statusForked:
		msgsJSON, err := json.Marshal(incoming)
		if err != nil {
			return fmt.Errorf("transcript: marshal canonical messages: %w", err)
		}
		identity := source
		if dec.status == statusForked {
			identity = identityForked
		}
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO conversations
			   (id, title, messages, created_at, updated_at,
			    conversation_key, identity_source, latest_call_id, message_count, stitch_status)
			 VALUES (?,?,?,?,?,?,?,?,?,?)`,
			dec.targetID, conversationTitle(incoming), string(msgsJSON), now, now,
			key, identity, callID, len(incoming), dec.status,
		); err != nil {
			return fmt.Errorf("transcript: insert canonical: %w", err)
		}
		return nil

	case statusExtended:
		msgsJSON, err := json.Marshal(incoming)
		if err != nil {
			return fmt.Errorf("transcript: marshal canonical messages: %w", err)
		}
		if _, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET
			    messages = ?, updated_at = ?, latest_call_id = ?,
			    message_count = ?, stitch_status = ?,
			    conversation_key = ?,
			    identity_source = CASE WHEN identity_source = '' THEN ? ELSE identity_source END,
			    title = CASE WHEN title = '' THEN ? ELSE title END
			  WHERE id = ?`,
			string(msgsJSON), now, callID, len(incoming), statusExtended,
			key, source, conversationTitle(incoming), dec.targetID,
		); err != nil {
			return fmt.Errorf("transcript: extend canonical: %w", err)
		}
		return nil

	case statusIdempotent:
		// No shrink: refresh recency + provenance, keep the stored messages.
		if _, err := s.db.ExecContext(ctx,
			`UPDATE conversations SET
			    updated_at = ?, latest_call_id = ?, stitch_status = ?,
			    conversation_key = CASE WHEN conversation_key = '' THEN ? ELSE conversation_key END,
			    identity_source = CASE WHEN identity_source = '' THEN ? ELSE identity_source END
			  WHERE id = ?`,
			now, callID, statusIdempotent, key, source, dec.targetID,
		); err != nil {
			return fmt.Errorf("transcript: idempotent update: %w", err)
		}
		return nil

	default:
		return fmt.Errorf("transcript: unknown stitch status %q", dec.status)
	}
}

func (s *Store) finalizeRawOK(ctx context.Context, callID string, dec stitchDecision, key string) error {
	if dec.status == statusForked && dec.targetID != key {
		_, err := s.db.ExecContext(ctx,
			`UPDATE raw_chat_calls SET projection_status = 'ok',
			    conversation_id = ?, identity_source = ? WHERE call_id = ?`,
			dec.targetID, identityForked, callID,
		)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE raw_chat_calls SET projection_status = 'ok' WHERE call_id = ?`, callID,
	)
	return err
}

func (s *Store) finalizeRawError(ctx context.Context, callID string, cause error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE raw_chat_calls SET projection_status = 'error', projection_error = ? WHERE call_id = ?`,
		cause.Error(), callID,
	); err != nil {
		log.Printf("transcript: failed to mark projection error for call %s: %v", callID, err)
	}
}

func newCallID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func nonNilMessages(msgs []conversation.Message) []conversation.Message {
	if msgs == nil {
		return []conversation.Message{}
	}
	return msgs
}

// conversationTitle returns the first user message content trimmed to 80 runes.
func conversationTitle(msgs []conversation.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			return trimRunes(m.Content, 80)
		}
	}
	return ""
}

func trimRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
