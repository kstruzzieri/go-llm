package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/conversation"
)

type fakeConversationStore struct {
	summaries     []conversation.Summary
	conversations map[string]conversation.Conversation
	loaded        *[]string
}

func (s fakeConversationStore) List(context.Context) ([]conversation.Summary, error) {
	out := make([]conversation.Summary, len(s.summaries))
	copy(out, s.summaries)
	return out, nil
}

func (s fakeConversationStore) Load(_ context.Context, id string) (*conversation.Conversation, error) {
	if s.loaded != nil {
		*s.loaded = append(*s.loaded, id)
	}
	conv, ok := s.conversations[id]
	if !ok {
		return nil, errors.New("missing conversation")
	}
	return &conv, nil
}

func TestConversationToTraceRedactsAndConvertsToolCalls(t *testing.T) {
	conv := conversation.Conversation{
		ID:        "abc-123",
		CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 5, 1, 12, 5, 0, 0, time.UTC),
		Messages: []conversation.Message{
			{Role: "system", Content: "Use workspace /Users/keith/project and token=system-secret."},
			{Role: "user", Content: "Read /Users/keith/project/secret.go for user@example.com"},
			{
				Role: "assistant",
				ToolCalls: json.RawMessage(`[
					{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "read_file",
							"arguments": {
								"path": "/Users/keith/project/secret.go",
								"api_key": "abc123",
								"notes": ["email user@example.com"]
							}
						}
					}
				]`),
			},
			{
				Role:       "tool",
				ToolName:   "read_file",
				ToolCallID: "call_1",
				Content:    `{"path":"/Users/keith/project/secret.go","token":"tool-secret"}`,
			},
			{Role: "assistant", Content: "Done from /tmp/out.txt with password=hunter2."},
		},
	}

	trace, err := conversationToTrace(conv, "test-source", "", defaultRedactor(), nil, false)
	if err != nil {
		t.Fatalf("conversationToTrace() error: %v", err)
	}

	if trace.ID != "conversation-abc-123" {
		t.Fatalf("trace ID = %q", trace.ID)
	}
	if trace.Source != "test-source" {
		t.Fatalf("trace source = %q", trace.Source)
	}
	assertClean(t, trace.System)
	if strings.Contains(trace.System, "system-secret") {
		t.Fatalf("system was not redacted: %q", trace.System)
	}
	if len(trace.Turns) != 3 {
		t.Fatalf("turn count = %d, want 3", len(trace.Turns))
	}
	assertClean(t, trace.Turns[0].Content)
	assertClean(t, trace.Turns[2].Content)
	assertClean(t, trace.Golden.FinalAnswerSubstring)

	if len(trace.Turns[1].ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v", trace.Turns[1].ToolCalls)
	}
	call := trace.Turns[1].ToolCalls[0]
	if call.Name != "read_file" {
		t.Fatalf("tool name = %q", call.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(call.Arguments, &args); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if args["path"] != "[REDACTED_PATH]/secret.go" {
		t.Fatalf("path arg = %#v", args["path"])
	}
	if args["api_key"] != "[REDACTED_SECRET]" {
		t.Fatalf("api_key arg = %#v", args["api_key"])
	}
	notes := args["notes"].([]any)
	if notes[0] != "email [REDACTED_EMAIL]" {
		t.Fatalf("notes = %#v", notes)
	}
	if len(trace.Golden.ToolCalls) != 1 || trace.Golden.ToolCalls[0] != "read_file" {
		t.Fatalf("golden tool calls = %#v", trace.Golden.ToolCalls)
	}
}

func TestConversationToTraceUsesFallbackSystem(t *testing.T) {
	conv := conversation.Conversation{
		ID: "no-system",
		Messages: []conversation.Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
	}

	trace, err := conversationToTrace(conv, "test-source", "Fallback system", defaultRedactor(), nil, false)
	if err != nil {
		t.Fatalf("conversationToTrace() error: %v", err)
	}
	if trace.System != "Fallback system" {
		t.Fatalf("system = %q", trace.System)
	}
}

func TestConversationToTraceCombinesMultipleSystemMessages(t *testing.T) {
	conv := conversation.Conversation{
		ID: "multi-system",
		Messages: []conversation.Message{
			{Role: "system", Content: "Relevant context from the codebase:\n\nretrieved chunk"},
			{Role: "system", Content: "original system"},
			{Role: "user", Content: "question"},
			{Role: "assistant", Content: "answer"},
		},
	}

	trace, err := conversationToTrace(conv, "test-source", "", defaultRedactor(), nil, false)
	if err != nil {
		t.Fatalf("conversationToTrace() error = %v", err)
	}
	want := "Relevant context from the codebase:\n\nretrieved chunk\n\noriginal system"
	if trace.System != want {
		t.Fatalf("trace.System = %q; want %q", trace.System, want)
	}
	if len(trace.Turns) != 1 || trace.Turns[0].Role != "user" || trace.Turns[0].Content != "question" {
		t.Fatalf("trace.Turns = %#v, want single user turn", trace.Turns)
	}
}

func TestConversationToTraceDeduplicatesRepeatedSystemMessages(t *testing.T) {
	conv := conversation.Conversation{
		ID: "dup-system",
		Messages: []conversation.Message{
			{Role: "system", Content: "shared preamble"},
			{Role: "system", Content: "shared preamble"},
			{Role: "system", Content: "original system"},
			{Role: "user", Content: "question"},
			{Role: "assistant", Content: "answer"},
		},
	}

	trace, err := conversationToTrace(conv, "test-source", "", defaultRedactor(), nil, false)
	if err != nil {
		t.Fatalf("conversationToTrace() error = %v", err)
	}
	want := "shared preamble\n\noriginal system"
	if trace.System != want {
		t.Fatalf("trace.System = %q; want %q (verbatim duplicate system messages must collapse)", trace.System, want)
	}
}

func TestConversationToTraceRejectsMalformedConversations(t *testing.T) {
	tests := []struct {
		name    string
		conv    conversation.Conversation
		wantErr error
	}{
		{
			name:    "missing id",
			conv:    conversation.Conversation{Messages: []conversation.Message{{Role: "system", Content: "sys"}}},
			wantErr: errConversationMissingID,
		},
		{
			name: "missing system",
			conv: conversation.Conversation{
				ID:       "missing-system",
				Messages: []conversation.Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
			},
			wantErr: errConversationMissingSystem,
		},
		{
			name: "missing user",
			conv: conversation.Conversation{
				ID:       "missing-user",
				Messages: []conversation.Message{{Role: "system", Content: "sys"}, {Role: "assistant", Content: "hello"}},
			},
			wantErr: errConversationMissingUser,
		},
		{
			name: "missing assistant",
			conv: conversation.Conversation{
				ID:       "missing-assistant",
				Messages: []conversation.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}},
			},
			wantErr: errConversationMissingAssistant,
		},
		{
			name: "invalid tool calls",
			conv: conversation.Conversation{
				ID: "bad-tool",
				Messages: []conversation.Message{
					{Role: "system", Content: "sys"},
					{Role: "user", Content: "hi"},
					{Role: "assistant", ToolCalls: json.RawMessage(`{"not":"an array"}`)},
					{Role: "assistant", Content: "done"},
				},
			},
			wantErr: errConversationInvalidToolCall,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := conversationToTrace(tt.conv, "source", "", defaultRedactor(), nil, false)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCaptureConversationsSkipsMalformedAndWritesDeterministicOutput(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	store := fakeConversationStore{
		summaries: []conversation.Summary{
			{ID: "bad", UpdatedAt: now.Add(2 * time.Minute)},
			{ID: "good", UpdatedAt: now.Add(time.Minute)},
		},
		conversations: map[string]conversation.Conversation{
			"bad": {
				ID:       "bad",
				Messages: []conversation.Message{{Role: "user", Content: "hi"}},
			},
			"good": {
				ID:        "good",
				UpdatedAt: now,
				Messages: []conversation.Message{
					{Role: "system", Content: "sys"},
					{Role: "user", Content: "hello"},
					{Role: "assistant", Content: "world"},
				},
			},
		},
	}

	dir1 := t.TempDir()
	dir2 := t.TempDir()
	result1, err := captureConversations(context.Background(), store, captureOptions{OutputDir: dir1, Source: "test"})
	if err != nil {
		t.Fatalf("captureConversations first run: %v", err)
	}
	result2, err := captureConversations(context.Background(), store, captureOptions{OutputDir: dir2, Source: "test"})
	if err != nil {
		t.Fatalf("captureConversations second run: %v", err)
	}

	if len(result1.Written) != 1 || len(result1.Skipped) != 1 {
		t.Fatalf("first result = %#v", result1)
	}
	if len(result2.Written) != 1 || len(result2.Skipped) != 1 {
		t.Fatalf("second result = %#v", result2)
	}

	file1 := filepath.Join(dir1, "conversation-good.json")
	file2 := filepath.Join(dir2, "conversation-good.json")
	data1, err := os.ReadFile(file1)
	if err != nil {
		t.Fatalf("read first trace: %v", err)
	}
	data2, err := os.ReadFile(file2)
	if err != nil {
		t.Fatalf("read second trace: %v", err)
	}
	if string(data1) != string(data2) {
		t.Fatalf("capture output differed:\n%s\n---\n%s", data1, data2)
	}
}

func TestCaptureLimitCountsWrittenTraces(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	var loaded []string
	store := fakeConversationStore{
		summaries: []conversation.Summary{
			{ID: "bad", UpdatedAt: now.Add(3 * time.Minute)},
			{ID: "good-1", UpdatedAt: now.Add(2 * time.Minute)},
			{ID: "good-2", UpdatedAt: now.Add(time.Minute)},
		},
		conversations: map[string]conversation.Conversation{
			"bad": {
				ID:       "bad",
				Messages: []conversation.Message{{Role: "user", Content: "hi"}},
			},
			"good-1": validTestConversation("good-1", "first", now),
			"good-2": validTestConversation("good-2", "second", now),
		},
		loaded: &loaded,
	}

	dir := t.TempDir()
	result, err := captureConversations(context.Background(), store, captureOptions{
		OutputDir: dir,
		Source:    "test",
		Limit:     1,
	})
	if err != nil {
		t.Fatalf("captureConversations: %v", err)
	}
	if len(result.Written) != 1 {
		t.Fatalf("written = %#v, want 1 trace", result.Written)
	}
	if len(result.Skipped) != 1 {
		t.Fatalf("skipped = %#v, want 1 skipped malformed conversation", result.Skipped)
	}
	if filepath.Base(result.Written[0]) != "conversation-good-1.json" {
		t.Fatalf("written file = %q", result.Written[0])
	}
	if got, want := strings.Join(loaded, ","), "bad,good-1"; got != want {
		t.Fatalf("loaded conversations = %s, want %s", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "conversation-good-2.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("good-2 should not be written after limit, stat err=%v", err)
	}
}

func TestRunCaptureDoesNotMigrateSourceDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "source.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open source db: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE conversations (
			id         TEXT PRIMARY KEY,
			title      TEXT NOT NULL DEFAULT '',
			messages   TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
	); err != nil {
		t.Fatalf("create conversations table: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO conversations (id, title, messages, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"conv-1",
		"test",
		`[
			{"role":"system","content":"sys"},
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"world"}
		]`,
		int64(1000),
		int64(2000),
	); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close source db: %v", err)
	}

	result, err := runCapture(context.Background(), captureOptions{
		DBPath:    dbPath,
		OutputDir: t.TempDir(),
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("runCapture: %v", err)
	}
	if len(result.Written) != 1 {
		t.Fatalf("written = %#v, want 1 trace", result.Written)
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen source db: %v", err)
	}
	defer func() { _ = db.Close() }()

	var versionTables int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'conversation_schema_version'`,
	).Scan(&versionTables); err != nil {
		t.Fatalf("query schema version table: %v", err)
	}
	if versionTables != 0 {
		t.Fatalf("capture created conversation_schema_version table")
	}
}

func TestRedactorRedactsObviousSensitiveValues(t *testing.T) {
	input := `Path /Users/keith/src/private.txt token=abc123 Authorization: Bearer bearer-secret user@example.com
OPENAI_API_KEY=sk-1234567890abcdefghi GITHUB_TOKEN=ghp_1234567890abcdefABCD ANTHROPIC_API_KEY='sk-ant-1234567890abcdef'
{"apiKey":"json-secret","authToken":"json-token","privateKey":"json-private-key"}
//registry.npmjs.org/:_authToken=npm_1234567890abcdef
https://user:pass@example.com
-----BEGIN OPENSSH PRIVATE KEY-----
private key body
-----END OPENSSH PRIVATE KEY-----`
	got := defaultRedactor().Redact(input)

	assertClean(t, got)
	for _, want := range []string{
		"[REDACTED_PATH]/private.txt",
		"token=[REDACTED_SECRET]",
		"Authorization: [REDACTED_SECRET]",
		"[REDACTED_EMAIL]",
		"OPENAI_API_KEY=[REDACTED_SECRET]",
		"GITHUB_TOKEN=[REDACTED_SECRET]",
		`ANTHROPIC_API_KEY='[REDACTED_SECRET]'`,
		`"apiKey":"[REDACTED_SECRET]"`,
		`"authToken":"[REDACTED_SECRET]"`,
		`"privateKey":"[REDACTED_SECRET]"`,
		"//registry.npmjs.org/:_authToken=[REDACTED_SECRET]",
		"https://[REDACTED_SECRET]@example.com",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("redacted text %q missing %q", got, want)
		}
	}
}

func validTestConversation(id, answer string, updatedAt time.Time) conversation.Conversation {
	return conversation.Conversation{
		ID:        id,
		UpdatedAt: updatedAt,
		Messages: []conversation.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: answer},
		},
	}
}

func assertClean(t *testing.T, text string) {
	t.Helper()
	for _, forbidden := range []string{
		"/Users/keith",
		"/tmp/",
		"abc123",
		"bearer-secret",
		"sk-1234567890abcdefghi",
		"ghp_1234567890abcdefABCD",
		"sk-ant-1234567890abcdef",
		"json-secret",
		"json-token",
		"json-private-key",
		"npm_1234567890abcdef",
		"user:pass@",
		"private key body",
		"hunter2",
		"tool-secret",
		"user@example.com",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("text %q still contains %q", text, forbidden)
		}
	}
}

func TestCaptureConversationsAttachesSnapshotTools(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"name":"read_file","inputSchema":{"type":"object"}}`)}
	conv := conversation.Conversation{
		ID: "c1",
		Messages: []conversation.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "go"},
			{Role: "assistant", Content: "done"},
		},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	store := fakeConversationStore{
		summaries:     []conversation.Summary{{ID: "c1", UpdatedAt: conv.UpdatedAt}},
		conversations: map[string]conversation.Conversation{"c1": conv},
	}
	outDir := t.TempDir()
	res, err := captureConversations(context.Background(), store, captureOptions{
		OutputDir:        outDir,
		ToolSchemaSource: staticToolSchemaSource{tools: tools},
	})
	if err != nil {
		t.Fatalf("captureConversations: %v", err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("Written=%v; want 1 trace", res.Written)
	}
	data, err := os.ReadFile(res.Written[0])
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var trace Trace
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}
	if len(trace.Tools) != 1 {
		t.Fatalf("trace.Tools len=%d; want 1", len(trace.Tools))
	}
	if !strings.Contains(string(trace.Tools[0]), "read_file") {
		t.Fatalf("trace.Tools[0]=%q; want a read_file entry", string(trace.Tools[0]))
	}
}

func TestCaptureConversationsRedactsSnapshotTools(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{
		"name":"read_file",
		"description":"Read /Users/keith/project/secret.go for user@example.com with api_key=abc123",
		"inputSchema":{
			"type":"object",
			"properties":{
				"path":{
					"type":"string",
					"description":"Local path such as /Users/keith/project/secret.go",
					"pattern":"^/Users/keith/project/.*"
				},
				"token":{
					"type":"string",
					"default":"ghp_1234567890abcdefABCD"
				},
					"api_key":{
						"type":"string",
						"default":"abc123",
						"examples":["abc123"],
						"enum":["abc123"]
					}
				},
				"patternProperties":{
					"^/Users/keith/project/.*$":{"type":"string"}
				},
				"additionalProperties":false
			}
		}`)}
	conv := validTestConversation("schema-redaction", "done", time.Now())
	store := fakeConversationStore{
		summaries:     []conversation.Summary{{ID: "schema-redaction", UpdatedAt: conv.UpdatedAt}},
		conversations: map[string]conversation.Conversation{"schema-redaction": conv},
	}
	res, err := captureConversations(context.Background(), store, captureOptions{
		OutputDir:        t.TempDir(),
		ToolSchemaSource: staticToolSchemaSource{tools: tools},
	})
	if err != nil {
		t.Fatalf("captureConversations: %v", err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("Written=%v; want 1 trace", res.Written)
	}
	data, err := os.ReadFile(res.Written[0])
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var trace Trace
	if err := json.Unmarshal(data, &trace); err != nil {
		t.Fatalf("unmarshal trace: %v", err)
	}
	if len(trace.Tools) != 1 {
		t.Fatalf("trace.Tools len=%d; want 1", len(trace.Tools))
	}
	toolJSON := string(trace.Tools[0])
	assertClean(t, toolJSON)
	if strings.Contains(toolJSON, "abc123") {
		t.Fatalf("schema default/example/enum kept low-entropy secret: %q", toolJSON)
	}
	for _, want := range []string{"[REDACTED_PATH]/secret.go", "[REDACTED_EMAIL]", "[REDACTED_SECRET]"} {
		if !strings.Contains(toolJSON, want) {
			t.Fatalf("redacted tool schema %q missing %q", toolJSON, want)
		}
	}
	schemas, err := toolSchemaByName(trace.Tools)
	if err != nil {
		t.Fatalf("toolSchemaByName: %v", err)
	}
	if err := schemas["read_file"].Validate(map[string]any{
		"path":                     "[REDACTED_PATH]/secret.go",
		"[REDACTED_PATH]/metadata": "ok",
	}); err != nil {
		t.Fatalf("redacted pattern should match redacted path placeholder: %v\nschema=%s", err, toolJSON)
	}
}

func TestCaptureConversationsSkipsConversationWithUndeclaredTool(t *testing.T) {
	tools := []json.RawMessage{json.RawMessage(`{"name":"read_file","inputSchema":{"type":"object"}}`)}
	conv := conversation.Conversation{
		ID:        "bad",
		Messages:  buildToolCallConvo("undeclared_tool"),
		UpdatedAt: time.Now(),
	}
	store := fakeConversationStore{
		summaries:     []conversation.Summary{{ID: "bad", UpdatedAt: conv.UpdatedAt}},
		conversations: map[string]conversation.Conversation{"bad": conv},
	}
	res, err := captureConversations(context.Background(), store, captureOptions{
		OutputDir:        t.TempDir(),
		ToolSchemaSource: staticToolSchemaSource{tools: tools},
	})
	if err != nil {
		t.Fatalf("captureConversations: %v", err)
	}
	if len(res.Written) != 0 {
		t.Fatalf("Written=%v; want 0", res.Written)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "not in MCP snapshot") {
		t.Fatalf("Skipped=%v; want one entry citing 'not in MCP snapshot'", res.Skipped)
	}
}

func TestCaptureConversationsConfiguredEmptySnapshotSkipsToolConversation(t *testing.T) {
	conv := conversation.Conversation{
		ID:        "empty-snapshot",
		Messages:  buildToolCallConvo("read_file"),
		UpdatedAt: time.Now(),
	}
	store := fakeConversationStore{
		summaries:     []conversation.Summary{{ID: "empty-snapshot", UpdatedAt: conv.UpdatedAt}},
		conversations: map[string]conversation.Conversation{"empty-snapshot": conv},
	}
	res, err := captureConversations(context.Background(), store, captureOptions{
		OutputDir:        t.TempDir(),
		ToolSchemaSource: staticToolSchemaSource{}, // configured source, authoritative empty snapshot
	})
	if err != nil {
		t.Fatalf("captureConversations: %v", err)
	}
	if len(res.Written) != 0 {
		t.Fatalf("Written=%v; want 0", res.Written)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0], "not in MCP snapshot") {
		t.Fatalf("Skipped=%v; want one entry citing 'not in MCP snapshot'", res.Skipped)
	}
}

// buildToolCallConvo returns a minimal valid conversation whose assistant
// turn calls a single named tool. Used by skip-on-undeclared tests.
func buildToolCallConvo(toolName string) []conversation.Message {
	args := []byte(`{}`)
	tc := []byte(`[{"name":"` + toolName + `","arguments":` + string(args) + `}]`)
	return []conversation.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "go"},
		{Role: "assistant", Content: "", ToolCalls: tc},
		{Role: "tool", Content: "result", ToolCallID: "1", ToolName: toolName},
		{Role: "assistant", Content: "done"},
	}
}

func TestCaptureConversationsNilSourceLeavesEmptyTools(t *testing.T) {
	conv := conversation.Conversation{
		ID:        "c1",
		Messages:  []conversation.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}, {Role: "assistant", Content: "a"}},
		UpdatedAt: time.Now(),
	}
	store := fakeConversationStore{
		summaries:     []conversation.Summary{{ID: "c1", UpdatedAt: conv.UpdatedAt}},
		conversations: map[string]conversation.Conversation{"c1": conv},
	}
	outDir := t.TempDir()
	res, err := captureConversations(context.Background(), store, captureOptions{
		OutputDir: outDir, // ToolSchemaSource left nil
	})
	if err != nil {
		t.Fatalf("captureConversations: %v", err)
	}
	if len(res.Written) != 1 {
		t.Fatalf("Written=%v; want 1", res.Written)
	}
	data, _ := os.ReadFile(res.Written[0])
	var trace Trace
	_ = json.Unmarshal(data, &trace)
	if len(trace.Tools) != 0 {
		t.Fatalf("trace.Tools=%v; want empty when no source configured", trace.Tools)
	}
}

func TestCaptureConversationsAppliesSampleAfterEnrichment(t *testing.T) {
	// 4 conversations, one of them with an undeclared tool that will be
	// dropped during enrichment. Spec asks for n=2 — the manifest must
	// list exactly the 2 written traces (not 2 of the 4 original
	// candidates).
	tools := []json.RawMessage{json.RawMessage(`{"name":"read_file","inputSchema":{"type":"object"}}`)}
	store := makeStoreWithOneUndeclared("undeclared_tool", 4)
	outDir := t.TempDir()
	spec, _ := parseCaptureSample("n=2,stratify=turn-count")
	res, err := captureConversations(context.Background(), store, captureOptions{
		OutputDir:        outDir,
		ToolSchemaSource: staticToolSchemaSource{tools: tools},
		SampleSpec:       &spec,
		SampleSeed:       42,
		Now:              time.Now,
	})
	if err != nil {
		t.Fatalf("captureConversations: %v", err)
	}
	if len(res.Written) != 2 {
		t.Fatalf("Written=%d; want 2 (post-sample)", len(res.Written))
	}
	manifestPath := filepath.Join(outDir, "_sample-manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest sampleManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(manifest.SampledIDs) != 2 {
		t.Fatalf("manifest.SampledIDs=%v; want 2", manifest.SampledIDs)
	}
	for _, id := range manifest.SampledIDs {
		if strings.Contains(id, "undeclared") {
			t.Fatalf("manifest lists dropped trace %q", id)
		}
	}
}

// makeStoreWithOneUndeclared returns a fakeConversationStore with N
// conversations. Each is a valid 5-message tool-call convo using
// "read_file"; the last conversation instead uses the supplied
// undeclared tool name (so it will fail validateTrace under any
// snapshot that doesn't declare it).
func makeStoreWithOneUndeclared(undeclared string, n int) fakeConversationStore {
	now := time.Now()
	summaries := make([]conversation.Summary, n)
	conversations := make(map[string]conversation.Conversation, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c-%03d", i)
		toolName := "read_file"
		if i == n-1 {
			toolName = undeclared
		}
		conv := conversation.Conversation{
			ID:        id,
			Messages:  buildToolCallConvo(toolName),
			UpdatedAt: now.Add(-time.Duration(i) * time.Minute),
		}
		summaries[i] = conversation.Summary{ID: id, UpdatedAt: conv.UpdatedAt}
		conversations[id] = conv
	}
	return fakeConversationStore{summaries: summaries, conversations: conversations}
}
