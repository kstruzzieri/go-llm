// mcp/tools_chat_transcript_test.go
package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
	"github.com/kstruzzieri/go-llm/transcript"

	_ "modernc.org/sqlite"
)

func TestHandleChat_PolicyFilteredToZeroStopsBeforeRouter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	ts, err := transcript.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	evaluator := &mcpPolicyEvaluatorSpy{
		decision:       rag.RetrievalPolicyDecision{Allow: true},
		resultDecision: []rag.RetrievalResultDecision{{Keep: false}},
	}
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{{
		SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "denied", Source: "denied.go", Content: "DENIED_SECRET"}, Score: 0.8, Distance: 0.2},
	}}}
	router := newRecordingRouteEngine("answer")
	s := &Server{
		router: router, transcriptStore: ts,
		retriever:                mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator)),
		retrievalPolicyEvaluator: evaluator,
	}

	result, err := s.handleChat(context.Background(), rawArgs(t, `{"model":"ollama/test","use_rag":true,"messages":[{"role":"user","content":"question"}]}`))
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if got := extractText(result); got != "policy_no_context: no permitted RAG context" {
		t.Fatalf("error text = %q", got)
	}
	if result.Meta == nil {
		t.Fatal("governed empty result omitted safe applied metadata")
	}
	if router.called {
		t.Fatal("router called after policy filtered all context")
	}
	_ = ts.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open read handle: %v", err)
	}
	defer func() { _ = db.Close() }()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM raw_chat_calls`).Scan(&count); err != nil {
		t.Fatalf("count transcript records: %v", err)
	}
	if count != 0 {
		t.Fatalf("transcript records = %d, want 0", count)
	}
}

func TestHandleChat_PolicyContentNeverReachesRenderedTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	ts, err := transcript.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	evaluator := &policyMetadataEvaluator{}
	store := &recordingMCPMultiStore{results: []rag.ScoredResult{
		{SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "denied", Source: "denied.go", Content: "DENIED_SECRET"}, Score: 0.9, Distance: 0.1}},
		{SearchResult: rag.SearchResult{Chunk: rag.Chunk{ID: "redacted", Source: "allowed.go", Content: "ORIGINAL_SECRET"}, Score: 0.8, Distance: 0.2}},
	}}
	s := &Server{
		router: newRecordingRouteEngine("answer"), transcriptStore: ts,
		retriever:                mcpTestRetriever(t, store, rag.WithRetrievalPolicyEvaluator(evaluator)),
		retrievalPolicyEvaluator: evaluator,
	}

	req := rawArgs(t, `{"model":"ollama/test","use_rag":true,"conversation_id":"policy-transcript","messages":[{"role":"user","content":"question"}]}`)
	req.Params.Meta = policyMetaFromJSON(t, `{"audit_labels":{"purpose":"support"}}`)
	result, err := s.handleChat(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	_ = ts.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open read handle: %v", err)
	}
	defer func() { _ = db.Close() }()
	var renderedJSON string
	if err := db.QueryRow(`SELECT rendered_messages FROM conversations WHERE id = ?`, "policy-transcript").Scan(&renderedJSON); err != nil {
		t.Fatalf("read rendered request: %v", err)
	}
	var record struct {
		RenderedRequest []conversation.Message `json:"rendered_request"`
	}
	if err := json.Unmarshal([]byte(`{"rendered_request":`+renderedJSON+`}`), &record); err != nil {
		t.Fatalf("decode rendered request: %v", err)
	}
	encoded, err := json.Marshal(record.RenderedRequest)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "DENIED_SECRET") || strings.Contains(text, "ORIGINAL_SECRET") || !strings.Contains(text, "REDACTED") {
		t.Fatalf("rendered transcript leaked policy content: %s", text)
	}
}

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

func TestPersistTranscript_KeepsRAGOutOfCanonicalButInRendered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	ts, err := transcript.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	s := &Server{transcriptStore: ts}

	args := chatArgs{
		ConversationID: "conv-rag",
		Messages: []ollama.ChatMessage{
			{Role: "system", Content: "original system"},
			{Role: "user", Content: "question"},
		},
	}
	effective := []ollama.ChatMessage{
		{Role: "system", Content: "Relevant context from the codebase:\n\nretrieved chunk"},
		{Role: "system", Content: "original system"},
		{Role: "user", Content: "question"},
	}
	s.persistTranscript(context.Background(), &gomcp.CallToolRequest{}, args, effective, &provider.ChatResponse{
		Content:  "answer",
		Model:    "qwen3:8b",
		Provider: "fake",
	})
	_ = ts.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open read handle: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	var messagesJSON, renderedJSON string
	if err := db.QueryRow(`SELECT messages, rendered_messages FROM conversations WHERE id = ?`, "conv-rag").Scan(&messagesJSON, &renderedJSON); err != nil {
		t.Fatalf("read canonical messages: %v", err)
	}

	// Canonical messages must stay RAG-free so identity and stitching are stable.
	var canonical []conversation.Message
	if err := json.Unmarshal([]byte(messagesJSON), &canonical); err != nil {
		t.Fatalf("unmarshal canonical messages: %v", err)
	}
	if len(canonical) != 3 {
		t.Fatalf("canonical messages len = %d; want 3 (RAG excluded) (%+v)", len(canonical), canonical)
	}
	if canonical[0].Role != "system" || canonical[0].Content != "original system" {
		t.Fatalf("canonical[0] = %+v, want original system prompt without RAG", canonical[0])
	}
	if canonical[2].Role != "assistant" || canonical[2].Content != "answer" {
		t.Fatalf("canonical final = %+v, want assistant answer", canonical[2])
	}

	// Rendered messages carry the effective RAG-injected prompt for replay.
	var rendered []conversation.Message
	if err := json.Unmarshal([]byte(renderedJSON), &rendered); err != nil {
		t.Fatalf("unmarshal rendered messages: %v", err)
	}
	if len(rendered) != 4 {
		t.Fatalf("rendered messages len = %d; want 4 (RAG included) (%+v)", len(rendered), rendered)
	}
	if rendered[0].Role != "system" || rendered[0].Content != "Relevant context from the codebase:\n\nretrieved chunk" {
		t.Fatalf("rendered[0] = %+v, want injected RAG system context", rendered[0])
	}
}

// TestPersistTranscript_MultiTurnRAGStitchesIntoOneConversation is the
// regression for the forked-session bug: when use_rag returns different chunks
// across turns of one session (no explicit conversation_id), persisting the
// pre-RAG canonical request must keep both turns in a single stitched
// conversation instead of forking on the per-turn RAG system message.
func TestPersistTranscript_MultiTurnRAGStitchesIntoOneConversation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	ts, err := transcript.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	s := &Server{transcriptStore: ts}

	// Turn 1: retrieval chunk A.
	s.persistTranscript(context.Background(), &gomcp.CallToolRequest{},
		chatArgs{Messages: []ollama.ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "q1"},
		}},
		[]ollama.ChatMessage{
			{Role: "system", Content: "Relevant context from the codebase:\n\nchunk A"},
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "q1"},
		},
		&provider.ChatResponse{Content: "a1", Model: "qwen3:8b", Provider: "fake"})

	// Turn 2: same session continues, retrieval now returns chunk B.
	s.persistTranscript(context.Background(), &gomcp.CallToolRequest{},
		chatArgs{Messages: []ollama.ChatMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "q2"},
		}},
		[]ollama.ChatMessage{
			{Role: "system", Content: "Relevant context from the codebase:\n\nchunk B"},
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "q2"},
		},
		&provider.ChatResponse{Content: "a2", Model: "qwen3:8b", Provider: "fake"})
	_ = ts.Close()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open read handle: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&count); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if count != 1 {
		t.Fatalf("conversations = %d; want 1 (RAG chunks must not fork the session)", count)
	}

	var messagesJSON, renderedJSON, status string
	if err := db.QueryRow(`SELECT messages, rendered_messages, stitch_status FROM conversations`).Scan(&messagesJSON, &renderedJSON, &status); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != "extended" {
		t.Fatalf("stitch_status = %q; want extended", status)
	}
	var stored []conversation.Message
	if err := json.Unmarshal([]byte(messagesJSON), &stored); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	if len(stored) != 5 {
		t.Fatalf("stitched messages len = %d; want 5 (sys,q1,a1,q2,a2) (%+v)", len(stored), stored)
	}
	for _, m := range stored {
		if strings.Contains(m.Content, "Relevant context from the codebase") {
			t.Fatalf("canonical history leaked RAG context: %+v", stored)
		}
	}

	var rendered []conversation.Message
	if err := json.Unmarshal([]byte(renderedJSON), &rendered); err != nil {
		t.Fatalf("unmarshal rendered messages: %v", err)
	}
	if len(rendered) != 6 {
		t.Fatalf("rendered messages len = %d; want 6 (RAG,q1,a1,q2,a2) (%+v)", len(rendered), rendered)
	}
	if rendered[0].Content != "Relevant context from the codebase:\n\nchunk B" {
		t.Fatalf("rendered[0] = %+v, want latest RAG context from extended turn", rendered[0])
	}
	if rendered[5].Role != "assistant" || rendered[5].Content != "a2" {
		t.Fatalf("rendered final = %+v, want second assistant answer", rendered[5])
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

// TestHandleChat_PersistsDespiteCanceledContext guards the best-effort
// persistence against request-context cancellation: an IDE client that cancels
// right after receiving the answer must not cause the trace to be dropped. The
// handler detaches the persistence write from the request context.
func TestHandleChat_PersistsDespiteCanceledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	ts, err := transcript.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	router := newRecordingRouteEngine("routed-answer")
	s := &Server{router: router, transcriptStore: ts}

	args, _ := json.Marshal(chatArgs{
		ConversationID: "conv-cancel",
		Model:          "ollama/qwen3:8b",
		Messages:       []ollama.ChatMessage{{Role: "user", Content: "hi"}},
	})

	// Cancel the request context before the call. The fake router ignores ctx,
	// so ExecuteChat still succeeds; persistence must survive the cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := s.handleChat(ctx, &gomcp.CallToolRequest{
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
	if err := db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE id = ?`, "conv-cancel").Scan(&convCount); err != nil {
		t.Fatalf("count conversations: %v", err)
	}
	if convCount != 1 {
		t.Errorf("conversations with id=conv-cancel = %d, want 1 (persistence must survive a canceled request context)", convCount)
	}
}
