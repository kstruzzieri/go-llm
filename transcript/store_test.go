// transcript/store_test.go
package transcript

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kstruzzieri/go-llm/conversation"
)

func TestOpen_RejectsEmptyPath(t *testing.T) {
	if _, err := Open(context.Background(), ""); err == nil {
		t.Fatal("Open(\"\") should error")
	}
}

func TestOpen_MemoryAndCloseIdempotent(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestOpen_FileUsesWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("query journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestRecord_CreatesCanonicalAndRawRow(t *testing.T) {
	s, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	in := RecordInput{
		Request:  []conversation.Message{{Role: "user", Content: "hello"}},
		Response: conversation.Message{Role: "assistant", Content: "hi there"},
		Model:    "qwen3:8b",
		Provider: "ollama",
	}
	if err := s.Record(context.Background(), in); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var (
		msgsJSON, status string
		msgCount         int
	)
	if err := s.db.QueryRow(
		`SELECT messages, message_count, stitch_status FROM conversations`,
	).Scan(&msgsJSON, &msgCount, &status); err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	if status != statusCreated || msgCount != 2 {
		t.Errorf("canonical status=%q count=%d, want created/2", status, msgCount)
	}
	var stored []conversation.Message
	if err := json.Unmarshal([]byte(msgsJSON), &stored); err != nil {
		t.Fatalf("unmarshal stored messages: %v", err)
	}
	if len(stored) != 2 || stored[1].Role != "assistant" || stored[1].Content != "hi there" {
		t.Errorf("stored messages = %+v", stored)
	}

	var model, provider, pstatus string
	if err := s.db.QueryRow(
		`SELECT model, provider, projection_status FROM raw_chat_calls`,
	).Scan(&model, &provider, &pstatus); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if model != "qwen3:8b" || provider != "ollama" || pstatus != "ok" {
		t.Errorf("raw row model=%q provider=%q status=%q, want qwen3:8b/ollama/ok", model, provider, pstatus)
	}
}

func recordTurn(t *testing.T, s *Store, req []conversation.Message, resp string) {
	t.Helper()
	if err := s.Record(context.Background(), RecordInput{
		Request:  req,
		Response: conversation.Message{Role: "assistant", Content: resp},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func countConversations(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestRecord_ExtendsAcrossTurns(t *testing.T) {
	s, _ := Open(context.Background(), ":memory:")
	defer func() { _ = s.Close() }()

	recordTurn(t, s, []conversation.Message{{Role: "user", Content: "hi"}}, "hello")
	recordTurn(t, s, []conversation.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
		{Role: "user", Content: "more"},
	}, "sure")

	if n := countConversations(t, s); n != 1 {
		t.Fatalf("conversation rows = %d, want 1 (extended, not forked)", n)
	}
	var msgCount int
	var status string
	if err := s.db.QueryRow(`SELECT message_count, stitch_status FROM conversations`).Scan(&msgCount, &status); err != nil {
		t.Fatal(err)
	}
	if msgCount != 4 || status != statusExtended {
		t.Errorf("got count=%d status=%q, want 4/extended", msgCount, status)
	}
}

func TestRecord_DivergentSessionsForkUnderSameKey(t *testing.T) {
	s, _ := Open(context.Background(), ":memory:")
	defer func() { _ = s.Close() }()

	recordTurn(t, s, []conversation.Message{
		{Role: "user", Content: "hi"}, {Role: "assistant", Content: "A"}, {Role: "user", Content: "x"},
	}, "ansA")
	recordTurn(t, s, []conversation.Message{
		{Role: "user", Content: "hi"}, {Role: "assistant", Content: "B"}, {Role: "user", Content: "y"},
	}, "ansB")

	if n := countConversations(t, s); n != 2 {
		t.Fatalf("conversation rows = %d, want 2 siblings", n)
	}
	var distinctKeys int
	if err := s.db.QueryRow(`SELECT COUNT(DISTINCT conversation_key) FROM conversations`).Scan(&distinctKeys); err != nil {
		t.Fatal(err)
	}
	if distinctKeys != 1 {
		t.Errorf("distinct conversation_key = %d, want 1 (siblings share the key)", distinctKeys)
	}
}

func TestRecord_ExtendsForkedSibling(t *testing.T) {
	s, _ := Open(context.Background(), ":memory:")
	defer func() { _ = s.Close() }()

	recordTurn(t, s, []conversation.Message{
		{Role: "user", Content: "hi"}, {Role: "assistant", Content: "A"}, {Role: "user", Content: "x"},
	}, "ansA")
	recordTurn(t, s, []conversation.Message{
		{Role: "user", Content: "hi"}, {Role: "assistant", Content: "B"}, {Role: "user", Content: "y"},
	}, "ansB") // diverges → fork
	// Next turn of the forked session: it resends its own full history + a turn.
	recordTurn(t, s, []conversation.Message{
		{Role: "user", Content: "hi"}, {Role: "assistant", Content: "B"}, {Role: "user", Content: "y"},
		{Role: "assistant", Content: "ansB"}, {Role: "user", Content: "z"},
	}, "ansB2")

	if n := countConversations(t, s); n != 2 {
		t.Fatalf("conversation rows = %d, want 2 (sibling extended, not a 3rd fork)", n)
	}
}

func TestRecord_PersistsRouteOutcomeJSON(t *testing.T) {
	s, _ := Open(context.Background(), ":memory:")
	defer func() { _ = s.Close() }()

	ro := json.RawMessage(`{"planned_model":{"provider":"ollama","model":"qwen3:8b"}}`)
	if err := s.Record(context.Background(), RecordInput{
		Request:      []conversation.Message{{Role: "user", Content: "hi"}},
		Response:     conversation.Message{Role: "assistant", Content: "yo"},
		RouteOutcome: ro,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	var got sql.NullString
	if err := s.db.QueryRow(`SELECT route_outcome_json FROM raw_chat_calls`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.String != string(ro) {
		t.Errorf("route_outcome_json = %v, want %s", got, ro)
	}
}
