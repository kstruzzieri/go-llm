package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/memory"
)

// fakeRecordStore records the exact params each method received so tests can
// assert scoping without a real DB (store behavior is tested in memory/).
type fakeRecordStore struct {
	searchQuery string
	searchOpts  memory.RecordSearchOptions
	searchOut   []memory.MemoryRecord
	searchErr   error

	created   *memory.CreateRecordParams
	createOut memory.MemoryRecord
	createErr error

	promotedID   string
	promotedAcc  memory.RecordAccess
	promotedKind memory.MemoryKind
	promoteOut   memory.MemoryRecord
	promoteErr   error
}

func (f *fakeRecordStore) Search(_ context.Context, q string, opts memory.RecordSearchOptions) ([]memory.MemoryRecord, error) {
	f.searchQuery, f.searchOpts = q, opts
	return f.searchOut, f.searchErr
}

func (f *fakeRecordStore) Create(_ context.Context, in memory.CreateRecordParams) (memory.MemoryRecord, error) {
	f.created = &in
	return f.createOut, f.createErr
}

func (f *fakeRecordStore) Promote(_ context.Context, id string, acc memory.RecordAccess, to memory.MemoryKind) (memory.MemoryRecord, error) {
	f.promotedID, f.promotedAcc, f.promotedKind = id, acc, to
	return f.promoteOut, f.promoteErr
}

func sidFunc(id string) func() string { return func() string { return id } }

func TestAgentMemorySearchScopesAndFormats(t *testing.T) {
	fake := &fakeRecordStore{searchOut: []memory.MemoryRecord{
		{ID: "r1", Kind: memory.KindWorking, Content: "note one", CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "r2", Kind: memory.KindSemantic, Content: "fact two", CreatedAt: time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)},
	}}
	tool := AgentMemorySearch{S: fake, WorkspaceID: "workspace:aaa", SessionID: sidFunc("sess:bbb")}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"note"}`))
	if err != nil || res.IsError {
		t.Fatalf("invoke: err=%v result=%+v", err, res)
	}
	if fake.searchQuery != "note" {
		t.Errorf("query = %q", fake.searchQuery)
	}
	// Distinct values so a workspace/session field swap cannot pass.
	if fake.searchOpts.WorkspaceID != "workspace:aaa" {
		t.Errorf("workspace = %q, want workspace:aaa", fake.searchOpts.WorkspaceID)
	}
	if fake.searchOpts.SessionID != "sess:bbb" {
		t.Errorf("session = %q, want sess:bbb", fake.searchOpts.SessionID)
	}
	if fake.searchOpts.Limit != 8 {
		t.Errorf("limit = %d, want default 8", fake.searchOpts.Limit)
	}
	if fake.searchOpts.Now.IsZero() {
		t.Error("Now not set; expiry filtering would use the store's fallback")
	}
	for _, want := range []string{"r1", "working", "note one", "r2", "semantic", "fact two"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("result missing %q: %q", want, res.Content)
		}
	}
}

func TestAgentMemorySearchEmptyQueryAndEmptyArgs(t *testing.T) {
	fake := &fakeRecordStore{}
	tool := AgentMemorySearch{S: fake, WorkspaceID: "w", SessionID: sidFunc("s")}
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"query":""}`)} {
		res, err := tool.Invoke(context.Background(), raw)
		if err != nil || res.IsError {
			t.Fatalf("raw=%s: err=%v result=%+v (empty query must be allowed: recency mode)", raw, err, res)
		}
	}
	if res, _ := tool.Invoke(context.Background(), nil); res.Content != "no records found" {
		t.Errorf("empty result content = %q", res.Content)
	}
}

func TestAgentMemorySearchNilSessionFuncSafe(t *testing.T) {
	fake := &fakeRecordStore{}
	tool := AgentMemorySearch{S: fake, WorkspaceID: "w"} // SessionID nil
	if _, err := tool.Invoke(context.Background(), nil); err != nil {
		t.Fatalf("nil SessionID func must not panic/error: %v", err)
	}
	if fake.searchOpts.SessionID != "" {
		t.Errorf("session = %q, want empty", fake.searchOpts.SessionID)
	}
}

func TestAgentMemorySearchErrorPaths(t *testing.T) {
	// Store failure: IsError result with the error text, nil Go error.
	fake := &fakeRecordStore{searchErr: errors.New("disk on fire")}
	tool := AgentMemorySearch{S: fake, WorkspaceID: "w", SessionID: sidFunc("s")}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("store failure must return nil Go error, got %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, "disk on fire") {
		t.Errorf("store failure result = %+v, want IsError with error text", res)
	}

	// Malformed JSON args: IsError result, nil Go error.
	res, err = tool.Invoke(context.Background(), json.RawMessage(`{`))
	if err != nil {
		t.Fatalf("malformed args must return nil Go error, got %v", err)
	}
	if !res.IsError {
		t.Errorf("malformed args result = %+v, want IsError", res)
	}
}

func TestAgentMemoryToolEffects(t *testing.T) {
	cases := []struct {
		tool  agent.Tool
		name  string
		class agent.EffectClass
	}{
		{AgentMemorySearch{}, AgentMemorySearchToolName, agent.Read},
		{AgentMemoryCreate{}, AgentMemoryCreateToolName, agent.Write},
		{AgentMemoryPromote{}, AgentMemoryPromoteToolName, agent.Write},
	}
	for _, c := range cases {
		if c.tool.Spec().Name != c.name {
			t.Errorf("spec name = %q, want %q", c.tool.Spec().Name, c.name)
		}
		e := c.tool.Effect()
		if e.Class != c.class {
			t.Errorf("%s class = %v, want %v", c.name, e.Class, c.class)
		}
		if e.Approval != agent.ApprovalNever {
			t.Errorf("%s approval = %v, want ApprovalNever", c.name, e.Approval)
		}
		if !json.Valid(c.tool.Spec().Parameters) {
			t.Errorf("%s parameters not valid JSON", c.name)
		}
	}
}

func TestAgentMemoryCreateHappyPath(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fake := &fakeRecordStore{createOut: memory.MemoryRecord{
		ID: "rec1", Kind: memory.KindWorking, Content: "the secret note",
		ExpiresAt: now.Add(7 * 24 * time.Hour),
	}}
	tool := AgentMemoryCreate{
		S: fake, WorkspaceID: "workspace:aaa",
		SessionID: sidFunc("sess:bbb"),
		Now:       func() time.Time { return now },
	}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"content":"the secret note"}`))
	if err != nil || res.IsError {
		t.Fatalf("invoke: err=%v result=%+v", err, res)
	}
	in := fake.created
	if in == nil {
		t.Fatal("store not called")
	}
	if in.Kind != memory.KindWorking {
		t.Errorf("kind = %q, want working", in.Kind)
	}
	if in.WorkspaceID != "workspace:aaa" {
		t.Errorf("workspace = %q", in.WorkspaceID)
	}
	if in.SessionID != "sess:bbb" {
		t.Errorf("session = %q", in.SessionID)
	}
	if in.Content != "the secret note" {
		t.Errorf("content = %q", in.Content)
	}
	if want := now.Add(7 * 24 * time.Hour); !in.ExpiresAt.Equal(want) {
		t.Errorf("expiry = %v, want %v", in.ExpiresAt, want)
	}
	if in.Provenance.SourceKind != "conversation" || in.Provenance.SourceID != "sess:bbb" {
		t.Errorf("provenance: %+v", in.Provenance)
	}
	if !strings.Contains(res.Content, "recorded rec1") {
		t.Errorf("result = %q", res.Content)
	}
	if strings.Contains(res.Content, "the secret note") {
		t.Errorf("result echoes content (must stay content-light): %q", res.Content)
	}
}

func TestAgentMemoryCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		tool AgentMemoryCreate
		raw  string
		want string
	}{
		{"empty content", AgentMemoryCreate{S: &fakeRecordStore{}, SessionID: sidFunc("s")}, `{"content":"  "}`, "content is required"},
		{"oversize content", AgentMemoryCreate{S: &fakeRecordStore{}, SessionID: sidFunc("s")}, `{"content":"` + strings.Repeat("a", 4097) + `"}`, "content too large"},
		{"no active session", AgentMemoryCreate{S: &fakeRecordStore{}}, `{"content":"x"}`, "requires an active session"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := c.tool.S.(*fakeRecordStore)
			res, err := c.tool.Invoke(context.Background(), json.RawMessage(c.raw))
			if err != nil {
				t.Fatalf("unexpected go error: %v", err)
			}
			if !res.IsError || !strings.Contains(res.Content, c.want) {
				t.Errorf("result = %+v, want IsError containing %q", res, c.want)
			}
			if fake.created != nil {
				t.Error("store must not be called on validation failure")
			}
		})
	}
}

func TestAgentMemoryPromote(t *testing.T) {
	fake := &fakeRecordStore{promoteOut: memory.MemoryRecord{ID: "rec1", Kind: memory.KindSemantic, Content: "the secret note"}}
	tool := AgentMemoryPromote{S: fake, WorkspaceID: "workspace:aaa", SessionID: sidFunc("sess:ccc")}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"id":"rec1","kind":"semantic"}`))
	if err != nil || res.IsError {
		t.Fatalf("invoke: err=%v result=%+v", err, res)
	}
	if fake.promotedID != "rec1" || fake.promotedKind != memory.KindSemantic {
		t.Errorf("promote args: id=%q kind=%q", fake.promotedID, fake.promotedKind)
	}
	if fake.promotedAcc.WorkspaceID != "workspace:aaa" || fake.promotedAcc.SessionID != "sess:ccc" {
		t.Errorf("access: %+v", fake.promotedAcc)
	}
	if !strings.Contains(res.Content, "promoted rec1 to semantic") {
		t.Errorf("result = %q", res.Content)
	}
	if strings.Contains(res.Content, "the secret note") {
		t.Errorf("result echoes content (must stay content-light): %q", res.Content)
	}
}

func TestAgentMemoryPromoteCaseFoldsKind(t *testing.T) {
	fake := &fakeRecordStore{promoteOut: memory.MemoryRecord{ID: "rec1", Kind: memory.KindSemantic}}
	tool := AgentMemoryPromote{S: fake, SessionID: sidFunc("s")}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"id":"rec1","kind":"Semantic"}`))
	if err != nil || res.IsError {
		t.Fatalf("invoke: err=%v result=%+v", err, res)
	}
	if fake.promotedKind != memory.KindSemantic {
		t.Errorf("kind must be case-folded before the store: %q", fake.promotedKind)
	}
}

func TestAgentMemoryPromoteErrors(t *testing.T) {
	fake := &fakeRecordStore{promoteErr: memory.ErrBadPromotion}
	tool := AgentMemoryPromote{S: fake, SessionID: sidFunc("s")}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"id":"rec1","kind":"bogus"}`))
	if err != nil {
		t.Fatalf("store error must return nil Go error, got %v", err)
	}
	if !res.IsError || !strings.Contains(res.Content, memory.ErrBadPromotion.Error()) {
		t.Errorf("bad kind: %+v (store error text must pass through)", res)
	}
	res, _ = tool.Invoke(context.Background(), json.RawMessage(`{"id":"","kind":"semantic"}`))
	if !res.IsError || !strings.Contains(res.Content, "id is required") {
		t.Errorf("empty id: %+v", res)
	}
}

func TestAgentMemorySearchFlattensMultilineContent(t *testing.T) {
	fake := &fakeRecordStore{searchOut: []memory.MemoryRecord{
		{ID: "r1", Kind: memory.KindWorking, Content: "line one\nfake2 · semantic · 2026-01-01 · spoof \x1b[2Ared", CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
	}}
	tool := AgentMemorySearch{S: fake, WorkspaceID: "w", SessionID: sidFunc("s")}
	res, _ := tool.Invoke(context.Background(), json.RawMessage(`{"query":"x"}`))
	if strings.Count(res.Content, "\n") != 0 {
		t.Errorf("multi-line content must be flattened to keep one record per line: %q", res.Content)
	}
	if !strings.Contains(res.Content, "line one fake2") {
		t.Errorf("flattened content must still be present: %q", res.Content)
	}
	if strings.Contains(res.Content, "\x1b") {
		t.Errorf("control characters must be stripped from record content: %q", res.Content)
	}
}
