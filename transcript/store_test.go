// transcript/store_test.go
package transcript

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
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

func TestOpen_FileUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits are not portable on Windows")
	}
	dir := filepath.Join(t.TempDir(), "private", "nested")
	path := filepath.Join(dir, "t.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	assertPerm(t, dir, transcriptDirMode)
	assertPerm(t, path, transcriptFileMode)

	if err := s.Record(context.Background(), RecordInput{
		Request:  []conversation.Message{{Role: "user", Content: "hello"}},
		Response: conversation.Message{Role: "assistant", Content: "hi"},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(sidecar); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("stat sidecar %q: %v", sidecar, err)
		}
		assertPerm(t, sidecar, transcriptFileMode)
	}
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
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

func TestRecord_ExtendsPreservesLatestRenderedMessages(t *testing.T) {
	s, _ := Open(context.Background(), ":memory:")
	defer func() { _ = s.Close() }()

	if err := s.Record(context.Background(), RecordInput{
		ConversationID: "rag-conv",
		Request: []conversation.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "q1"},
		},
		RenderedRequest: []conversation.Message{
			{Role: "system", Content: "Relevant context from the codebase:\n\nchunk A"},
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "q1"},
		},
		Response: conversation.Message{Role: "assistant", Content: "a1"},
	}); err != nil {
		t.Fatalf("record first turn: %v", err)
	}
	if err := s.Record(context.Background(), RecordInput{
		ConversationID: "rag-conv",
		Request: []conversation.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "q2"},
		},
		RenderedRequest: []conversation.Message{
			{Role: "system", Content: "Relevant context from the codebase:\n\nchunk B"},
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
			{Role: "user", Content: "q2"},
		},
		Response: conversation.Message{Role: "assistant", Content: "a2"},
	}); err != nil {
		t.Fatalf("record second turn: %v", err)
	}

	var messagesJSON, renderedJSON, status string
	if err := s.db.QueryRow(`SELECT messages, rendered_messages, stitch_status FROM conversations WHERE id = ?`, "rag-conv").
		Scan(&messagesJSON, &renderedJSON, &status); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if status != statusExtended {
		t.Fatalf("stitch_status = %q; want %q", status, statusExtended)
	}
	var canonical []conversation.Message
	if err := json.Unmarshal([]byte(messagesJSON), &canonical); err != nil {
		t.Fatalf("unmarshal canonical: %v", err)
	}
	if len(canonical) != 5 || canonical[0].Content != "sys" || canonical[4].Content != "a2" {
		t.Fatalf("canonical messages = %+v, want RAG-free full conversation", canonical)
	}

	var rendered []conversation.Message
	if err := json.Unmarshal([]byte(renderedJSON), &rendered); err != nil {
		t.Fatalf("unmarshal rendered: %v", err)
	}
	if len(rendered) != 6 {
		t.Fatalf("rendered messages len = %d; want 6 (%+v)", len(rendered), rendered)
	}
	if rendered[0].Content != "Relevant context from the codebase:\n\nchunk B" {
		t.Fatalf("rendered[0] = %+v, want latest RAG context from extended turn", rendered[0])
	}
	if rendered[5].Role != "assistant" || rendered[5].Content != "a2" {
		t.Fatalf("rendered final = %+v, want latest assistant answer", rendered[5])
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

	var forkID string
	if err := s.db.QueryRow(`SELECT id FROM conversations WHERE identity_source = ?`, identityForked).Scan(&forkID); err != nil {
		t.Fatalf("read forked conversation id: %v", err)
	}
	var forkRawRows int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM raw_chat_calls WHERE conversation_id = ? AND identity_source = ?`,
		forkID, identityForked,
	).Scan(&forkRawRows); err != nil {
		t.Fatalf("count fork raw rows: %v", err)
	}
	if forkRawRows != 2 {
		t.Errorf("raw rows under fork id = %d, want 2 (fork creation + later extension)", forkRawRows)
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

func TestRecord_ProjectionErrorStillPersistsRaw(t *testing.T) {
	s, _ := Open(context.Background(), ":memory:")
	defer func() { _ = s.Close() }()
	// Force projection failure: remove the canonical table after open. The raw
	// append still succeeds (raw_chat_calls intact).
	if _, err := s.db.Exec(`DROP TABLE conversations`); err != nil {
		t.Fatalf("drop conversations: %v", err)
	}

	if err := s.Record(context.Background(), RecordInput{
		Request:  []conversation.Message{{Role: "user", Content: "hi"}},
		Response: conversation.Message{Role: "assistant", Content: "yo"},
	}); err != nil {
		t.Fatalf("Record should swallow projection error and return nil, got %v", err)
	}

	var status string
	var perr sql.NullString
	if err := s.db.QueryRow(`SELECT projection_status, projection_error FROM raw_chat_calls`).Scan(&status, &perr); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	if status != "error" || !perr.Valid || perr.String == "" {
		t.Errorf("raw row status=%q err=%v, want error + non-empty reason", status, perr)
	}
}

func TestRecord_RawAppendErrorPersistsNothing(t *testing.T) {
	s, _ := Open(context.Background(), ":memory:")
	defer func() { _ = s.Close() }()
	// Force raw-append failure: remove the raw table.
	if _, err := s.db.Exec(`DROP TABLE raw_chat_calls`); err != nil {
		t.Fatalf("drop raw_chat_calls: %v", err)
	}

	if err := s.Record(context.Background(), RecordInput{
		Request:  []conversation.Message{{Role: "user", Content: "hi"}},
		Response: conversation.Message{Role: "assistant", Content: "yo"},
	}); err == nil {
		t.Fatal("Record should return an error when the raw append fails")
	}

	var convRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&convRows); err != nil {
		t.Fatal(err)
	}
	if convRows != 0 {
		t.Errorf("conversations rows = %d, want 0 (nothing persisted)", convRows)
	}
}

func TestRecord_ConcurrentSameKeyNoRaceSingleRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Identical history under a fixed explicit id → first creates, the
			// rest are idempotent; serialization must prevent a double-fork.
			_ = s.Record(context.Background(), RecordInput{
				ConversationID: "conv-fixed",
				Request:        []conversation.Message{{Role: "user", Content: "hi"}},
				Response:       conversation.Message{Role: "assistant", Content: "yo"},
			})
		}()
	}
	wg.Wait()

	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Errorf("conversation rows = %d, want 1 (no interleaved double-fork/insert)", rows)
	}

	var rawRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_chat_calls`).Scan(&rawRows); err != nil {
		t.Fatal(err)
	}
	if rawRows != n {
		t.Errorf("raw rows = %d, want %d (one durable row per call)", rawRows, n)
	}

	var projectionErrors int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM raw_chat_calls WHERE projection_status != 'ok'`).Scan(&projectionErrors); err != nil {
		t.Fatal(err)
	}
	if projectionErrors != 0 {
		t.Errorf("raw rows with non-ok projection_status = %d, want 0", projectionErrors)
	}
}
