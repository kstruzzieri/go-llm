// mcp/tools_chat_transcript_test.go
package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/transcript"

	_ "modernc.org/sqlite"
)

func TestChatArgs_ParsesConversationID(t *testing.T) {
	var args chatArgs
	raw := `{"messages":[{"role":"user","content":"hi"}],"conversation_id":"conv-1"}`
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if args.ConversationID != "conv-1" {
		t.Errorf("ConversationID = %q, want conv-1", args.ConversationID)
	}
}

func TestHandleChat_PersistsTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	ts, err := transcript.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	router := newRecordingRouteEngine("routed-answer")
	s := &Server{router: router, transcriptStore: ts}

	args, _ := json.Marshal(chatArgs{
		ConversationID: "conv-1",
		Model:          "ollama/qwen3:8b",
		Messages:       []ollama.ChatMessage{{Role: "user", Content: "hi"}},
	})
	res, err := s.handleChat(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil {
		t.Fatalf("handleChat: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleChat returned tool error: %s", extractText(res))
	}
	_ = ts.Close() // flush WAL so a separate read handle sees the row

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open read handle: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	var convCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE id = ?`, "conv-1").Scan(&convCount); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if convCount != 1 {
		t.Errorf("conversations with id=conv-1 = %d, want 1", convCount)
	}

	var messagesJSON string
	if err := db.QueryRow(`SELECT messages FROM conversations WHERE id = ?`, "conv-1").Scan(&messagesJSON); err != nil {
		t.Fatalf("read canonical messages: %v", err)
	}
	var stored []conversation.Message
	if err := json.Unmarshal([]byte(messagesJSON), &stored); err != nil {
		t.Fatalf("unmarshal canonical messages: %v", err)
	}
	if len(stored) != 2 ||
		stored[0].Role != "user" || stored[0].Content != "hi" ||
		stored[1].Role != "assistant" || stored[1].Content != "routed-answer" {
		t.Errorf("stored messages = %+v, want original request + assistant response", stored)
	}

	var provider string
	var routeOutcome sql.NullString
	if err := db.QueryRow(`SELECT provider, route_outcome_json FROM raw_chat_calls`).Scan(&provider, &routeOutcome); err != nil {
		t.Fatalf("read raw provider: %v", err)
	}
	if provider != "fake" {
		t.Errorf("raw provider = %q, want fake (from the served ChatResponse)", provider)
	}
	if !routeOutcome.Valid || !json.Valid([]byte(routeOutcome.String)) {
		t.Errorf("route_outcome_json = %v, want valid JSON", routeOutcome)
	}
}

func TestHandleChat_NilTranscriptStoreStillSucceeds(t *testing.T) {
	router := newRecordingRouteEngine("ok")
	s := &Server{router: router} // transcriptStore nil

	args, _ := json.Marshal(chatArgs{
		Model:    "ollama/m",
		Messages: []ollama.ChatMessage{{Role: "user", Content: "hi"}},
	})
	res, err := s.handleChat(context.Background(), &gomcp.CallToolRequest{
		Params: &gomcp.CallToolParamsRaw{Arguments: args},
	})
	if err != nil || res.IsError {
		t.Fatalf("handleChat with nil store should succeed: err=%v isError=%v", err, res.IsError)
	}
}
