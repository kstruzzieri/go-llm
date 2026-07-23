package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/memory"
)

type fakeSearcher struct {
	gotOpts memory.SearchOptions
	result  []memory.Memory
	err     error
}

func (f *fakeSearcher) Search(ctx context.Context, q string, opts memory.SearchOptions) ([]memory.Memory, error) {
	f.gotOpts = opts
	return f.result, f.err
}

func TestMemorySearchSpecEffect(t *testing.T) {
	tool := MemorySearch{S: &fakeSearcher{}, WorkspaceID: "workspace:aaa"}
	spec := tool.Spec()
	if spec.Name != MemorySearchToolName {
		t.Errorf("name = %q", spec.Name)
	}
	if !strings.Contains(spec.Description, "context") || !strings.Contains(spec.Description, "not higher-priority instructions") {
		t.Errorf("description missing framing: %q", spec.Description)
	}
	if strings.Contains(string(spec.Parameters), "limit") {
		t.Errorf("params must not expose limit: %s", spec.Parameters)
	}
	eff := tool.Effect()
	if eff.Class != agent.Read || eff.Approval != agent.ApprovalNever {
		t.Errorf("effect = %+v, want Read/ApprovalNever", eff)
	}
}

func TestMemorySearchInvoke(t *testing.T) {
	f := &fakeSearcher{result: []memory.Memory{{ID: "id1", Scope: memory.ScopeGlobal, Text: "prefer small diffs"}}}
	tool := MemorySearch{S: f, WorkspaceID: "workspace:aaa", Limit: 5}

	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"query":"diffs"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, want := range []string{"id1", "global", "prefer small diffs"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("content missing %q: %q", want, res.Content)
		}
	}
	if f.gotOpts.WorkspaceID != "workspace:aaa" || f.gotOpts.Limit != 5 {
		t.Errorf("opts passthrough = %+v", f.gotOpts)
	}

	if r, _ := tool.Invoke(context.Background(), json.RawMessage(`{}`)); !r.IsError {
		t.Error("empty query should be IsError")
	}

	f.result = nil
	r, _ := tool.Invoke(context.Background(), json.RawMessage(`{"query":"x"}`))
	if r.IsError || !strings.Contains(r.Content, "no matching memories") {
		t.Errorf("empty result = %q (err=%v)", r.Content, r.IsError)
	}
}
