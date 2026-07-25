package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/internal/datadir"
	"github.com/kstruzzieri/go-llm/internal/pathguard"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/provider"
	_ "modernc.org/sqlite"
)

// validSessionName restricts explicit -session ids so display, DB keys, and any
// future session commands stay boring.
var validSessionName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validNamespacedSessionID accepts session ids that golem itself prints.
// Bare names are still mapped to user:<name> for CLI ergonomics.
var validNamespacedSessionID = regexp.MustCompile(`^(golem|user|workspace):[A-Za-z0-9._-]+$`)

// sessionIDOpts selects which session id to use. Precedence (highest first):
// fresh, explicit, then the per-workspace default.
type sessionIDOpts struct {
	fresh    bool
	explicit string // raw -session value
	root     string // canonical absolute workspace root
}

// resolveSessionID derives the keyed session id. fresh => a new unique
// "golem:<uuid>"; explicit => either a printed namespaced id or validated
// "user:<id>"; default => a stable "workspace:<sha256(root) prefix>".
func resolveSessionID(o sessionIDOpts) (string, error) {
	if o.fresh {
		return "golem:" + conversation.NewID(), nil
	}
	if o.explicit != "" {
		e := strings.TrimSpace(o.explicit)
		if e == "" {
			return "", fmt.Errorf("golem: -session must not be blank")
		}
		if strings.Contains(e, ":") {
			if !validNamespacedSessionID.MatchString(e) {
				return "", fmt.Errorf("golem: invalid -session %q: use a printed golem:, workspace:, or user: id, or a bare name with characters A-Z a-z 0-9 . _ -", e)
			}
			return e, nil
		}
		if !validSessionName.MatchString(e) {
			return "", fmt.Errorf("golem: invalid -session %q: allowed characters are A-Z a-z 0-9 . _ -", e)
		}
		return "user:" + e, nil
	}
	return workspaceID(o.root), nil
}

// dataDirBase resolves the per-user data dir base. Delegates to the shared
// resolver so the CLI and the embeddable runtime agree on the same paths.
func dataDirBase(getenv func(string) string) (string, error) {
	return datadir.Base(getenv)
}

// sessionDBPath locates the session DB OUTSIDE the repo, under the per-user data
// dir ($XDG_DATA_HOME/golem/sessions.db, else ~/.local/share/golem/sessions.db).
func sessionDBPath(getenv func(string) string) (string, error) {
	return datadir.SessionDBPath(getenv)
}

func sessionDBPathForWorkspace(getenv func(string) string, root string) (string, error) {
	dbPath, err := sessionDBPath(getenv)
	if err != nil {
		return "", err
	}
	if err := validateSessionDBOutsideWorkspace(dbPath, root); err != nil {
		return "", err
	}
	return dbPath, nil
}

func validateSessionDBOutsideWorkspace(dbPath, root string) error {
	return validatePathOutsideWorkspace(dbPath, root)
}

// validatePathOutsideWorkspace rejects a path that is the workspace root itself
// or nested inside it. Used for both the session DB and the index DB so neither
// can land inside the indexed/edited tree.
func validatePathOutsideWorkspace(p, root string) error {
	return pathguard.ValidateOutside(p, root)
}

const (
	sessionDirMode  os.FileMode = 0o700
	sessionFileMode os.FileMode = 0o600
)

// session is golem's per-process view of one persistent conversation. The store
// is the source of truth; the in-memory buffer is loaded once at startup and
// re-saved after each successful turn. Single-writer: B3 does not merge
// concurrent writers — two golem processes on the same id are last-save-wins.
type session struct {
	db      *sql.DB
	dbPath  string // retained so record/clear can re-secure sidecars after each write
	store   conversation.Store
	id      string
	msgs    []conversation.Message
	summary *conversation.DurableSummary
}

// sessionInfo is the startup disclosure for one resolved session.
type sessionInfo struct {
	id        string
	resumed   bool
	msgCount  int
	updatedAt time.Time
}

// line renders the human-facing startup notice for the session.
func (i sessionInfo) line() string {
	if !i.resumed {
		return "session: " + i.id + " (new)"
	}
	return fmt.Sprintf("session: %s resumed, %s, updated %s",
		i.id, plural(i.msgCount, "message", "messages"), i.updatedAt.Format("2006-01-02 15:04"))
}

// openSession prepares the hardened DB file, opens it WAL-mode, runs migrations,
// and loads the keyed conversation. A missing row is a new session (not an
// error); any other load error is surfaced so the caller can disable + report.
func openSession(ctx context.Context, dbPath, id string) (*session, sessionInfo, error) {
	if err := prepareDBFile(dbPath); err != nil {
		return nil, sessionInfo{}, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, sessionInfo{}, fmt.Errorf("golem: open session db %q: %w", dbPath, err)
	}
	// modernc.org/sqlite applies PRAGMAs per-connection; clamp the pool to one so
	// WAL + busy_timeout hold for every write.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, sessionInfo{}, fmt.Errorf("golem: session db %s: %w", pragma, err)
		}
	}
	store, err := conversation.NewStore(ctx, db) // runs migrations (may create -wal/-shm)
	if err != nil {
		_ = db.Close()
		return nil, sessionInfo{}, fmt.Errorf("golem: init session store: %w", err)
	}
	if err := chmodDBFiles(dbPath); err != nil {
		_ = db.Close()
		return nil, sessionInfo{}, err
	}

	s := &session{db: db, dbPath: dbPath, store: store, id: id}
	info := sessionInfo{id: id}
	conv, lerr := store.Load(ctx, id)
	switch {
	case lerr == nil:
		s.msgs = conv.Messages
		s.summary = cloneDurableSummary(conv.DurableSummary)
		info.resumed = len(conv.Messages) > 0
		info.msgCount = len(conv.Messages)
		info.updatedAt = conv.UpdatedAt
	case errors.Is(lerr, conversation.ErrNotFound):
		// new session; nothing to load
	default:
		_ = db.Close()
		return nil, sessionInfo{}, fmt.Errorf("golem: load session %q: %w", id, lerr)
	}
	return s, info, nil
}

// record appends the user line + final answer and persists the conversation.
// It only mutates the in-memory buffer after Save succeeds, so a failed save
// cannot leak an unsaved turn into future history() output or a later successful save.
func (s *session) record(ctx context.Context, userLine, answer string) error {
	return s.recordMessages(ctx, []conversation.Message{
		conversation.Message{Role: "user", Content: userLine},
		conversation.Message{Role: "assistant", Content: answer},
	})
}

func (s *session) recordMessages(ctx context.Context, msgs []conversation.Message) error {
	next := append(append([]conversation.Message{}, s.msgs...), msgs...)
	if err := s.store.Save(ctx, conversation.Conversation{
		ID:             s.id,
		Title:          sessionTitle(next),
		Messages:       next,
		DurableSummary: cloneDurableSummary(s.summary),
	}); err != nil {
		return err
	}
	s.msgs = next
	// SQLite may have (re)created the -wal/-shm sidecars honoring the umask on
	// this write; re-secure them (the WAL can hold un-checkpointed message text).
	_ = chmodDBFiles(s.dbPath)
	return nil
}

// history maps the persisted conversation to real-role chat messages for the
// agent runtime's Request.History seam. No trimming: ContextManager.Assemble is
// the single authority that bounds model-visible context. Rows outside the
// runtime allowlist (non user/assistant role or empty content) are skipped
// defensively, so a foreign or corrupt stored turn cannot brick the session.
// Nil-safe: a nil session (e.g. --no-session) yields nil.
func (s *session) history() []provider.ChatMessage {
	if s == nil || len(s.msgs) == 0 {
		return nil
	}
	out := make([]provider.ChatMessage, 0, len(s.msgs))
	for _, m := range s.msgs {
		// Skip any row the agent runtime's allowlist would reject (non
		// user/assistant role, or empty content) so a single foreign or corrupt
		// stored turn cannot brick the session by failing validateHistory on
		// every future run. record() only writes valid turns; this is defensive.
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		if m.Content == "" {
			continue
		}
		out = append(out, provider.ChatMessage{Role: m.Role, Content: m.Content})
	}
	return out
}

func (s *session) historySummary() string {
	if s == nil || s.summary == nil {
		return ""
	}
	return s.summary.Content
}

// sessionTitle is the first user line, truncated, for nicer future listings.
func sessionTitle(msgs []conversation.Message) string {
	for _, m := range msgs {
		if m.Role == "user" {
			return truncateRunes(strings.TrimSpace(m.Content), 60)
		}
	}
	return ""
}

func truncateRunes(s string, max int) string {
	rs := []rune(s)
	if len(rs) <= max {
		return s
	}
	return string(rs[:max])
}

// clear deletes the persisted row and empties the in-memory buffer.
func (s *session) clear(ctx context.Context) error {
	if err := s.store.Delete(ctx, s.id); err != nil {
		return err
	}
	s.msgs = nil
	s.summary = nil
	_ = chmodDBFiles(s.dbPath)
	return nil
}

// renew switches to a fresh persistent session id and clears the buffer.
func (s *session) renew() {
	s.id = "golem:" + conversation.NewID()
	s.msgs = nil
	s.summary = nil
}

func (s *session) switchTo(ctx context.Context, id string) (sessionInfo, error) {
	conv, err := s.store.Load(ctx, id)
	if err != nil {
		return sessionInfo{}, err
	}
	s.id = id
	s.msgs = conv.Messages
	s.summary = cloneDurableSummary(conv.DurableSummary)
	return sessionInfo{
		id:        id,
		resumed:   len(conv.Messages) > 0,
		msgCount:  len(conv.Messages),
		updatedAt: conv.UpdatedAt,
	}, nil
}

func cloneDurableSummary(in *conversation.DurableSummary) *conversation.DurableSummary {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// Close releases the DB. Nil-safe and idempotent enough for defer.
func (s *session) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// prepareDBFile creates the DB's parent dir (0700) and file (0600).
// Delegates to the shared hardened-open primitives in memory/ (#237
// invariants live once).
func prepareDBFile(path string) error {
	return memory.PrepareDBFile(path)
}

// chmodDBFiles re-secures the DB file and -wal/-shm sidecars (0600).
// Delegates to memory.SecureDBFiles.
func chmodDBFiles(path string) error {
	return memory.SecureDBFiles(path)
}
