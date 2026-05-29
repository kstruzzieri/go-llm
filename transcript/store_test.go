// transcript/store_test.go
package transcript

import (
	"context"
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
