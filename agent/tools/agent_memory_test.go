package tools

import (
	"context"
	"encoding/json"
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
	tool := AgentMemorySearch{S: fake, WorkspaceID: "workspace:aaa", SessionID: sidFunc("workspace:aaa")}
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"note"}`))
	if err != nil || res.IsError {
		t.Fatalf("invoke: err=%v result=%+v", err, res)
	}
	if fake.searchQuery != "note" {
		t.Errorf("query = %q", fake.searchQuery)
	}
	if fake.searchOpts.WorkspaceID != "workspace:aaa" || fake.searchOpts.SessionID != "workspace:aaa" {
		t.Errorf("scope not passed: %+v", fake.searchOpts)
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

func TestAgentMemoryToolEffects(t *testing.T) {
	cases := []struct {
		tool  agent.Tool
		name  string
		class agent.EffectClass
	}{
		{AgentMemorySearch{}, AgentMemorySearchToolName, agent.Read},
		// enabled in task 2:
		// {AgentMemoryCreate{}, AgentMemoryCreateToolName, agent.Write},
		// {AgentMemoryPromote{}, AgentMemoryPromoteToolName, agent.Write},
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
