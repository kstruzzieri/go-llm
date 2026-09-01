package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func newChangesFixture(t *testing.T) (*scratchRuntime, ScratchChanges) {
	t.Helper()
	rt, _ := newTestScratchRuntime(t, ScratchConfig{Enabled: true})
	return rt, ScratchChanges{rt: rt}
}

func TestScratchChangesEffectIsReadOnly(t *testing.T) {
	_, tool := newChangesFixture(t)
	e := tool.Effect()
	if e.Class != agent.Read || e.Approval != agent.ApprovalNever {
		t.Fatalf("scratch_changes must be Read/ApprovalNever, got %+v", e)
	}
	if tool.Spec().Name != "scratch_changes" {
		t.Fatalf("tool name = %q", tool.Spec().Name)
	}
}

func TestScratchChangesStatuses(t *testing.T) {
	rt, tool := newChangesFixture(t)
	rt.store.beginPending("scr-live")
	rt.store.beginPending("scr-done")
	rt.store.completePending("scr-done", scratchOutcome{
		id: "scr-done",
		changes: []scratchChange{
			{path: "evil\nname.txt", kind: scratchChangeCreate, size: 4, hash: "abcd", promotable: true, data: []byte("secret-bytes"), preview: "+ preview"},
			{path: "mod.txt", kind: scratchChangeUpdate, size: 9, hash: "ef01", reason: "updates are report-only in MVP"},
		},
	})

	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"id":"scr-live"}`))
	if err != nil || res.IsError || !strings.Contains(res.Content, "pending") {
		t.Fatalf("pending render: %+v err=%v", res, err)
	}

	res, err = tool.Invoke(context.Background(), json.RawMessage(`{"id":"scr-done"}`))
	if err != nil || res.IsError {
		t.Fatalf("captured render: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Content, `"evil\nname.txt"`) {
		t.Fatalf("paths must be %%q-escaped:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "promotable") || !strings.Contains(res.Content, "updates are report-only") {
		t.Fatalf("render must carry promotability and reasons:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "secret-bytes") || strings.Contains(res.Content, "+ preview") {
		t.Fatalf("query tool must never leak artifact content:\n%s", res.Content)
	}

	res, err = tool.Invoke(context.Background(), json.RawMessage(`{"id":"scr-missing"}`))
	if err != nil || !res.IsError {
		t.Fatalf("unknown id must be a tool error: %+v err=%v", res, err)
	}
	res, err = tool.Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil || !res.IsError {
		t.Fatalf("missing id must be a tool error: %+v err=%v", res, err)
	}
}

func TestScratchChangesTruncatedAndErrors(t *testing.T) {
	rt, tool := newChangesFixture(t)
	rt.store.beginPending("scr-t")
	rt.store.completePending("scr-t", scratchOutcome{
		id: "scr-t", truncated: true, captureErr: "changeset exceeds 2 entries", cleanupErr: "abandoned",
	})
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"id":"scr-t"}`))
	if err != nil || res.IsError {
		t.Fatalf("truncated render: %+v err=%v", res, err)
	}
	for _, want := range []string{"truncated", "changeset exceeds 2 entries", "abandoned"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("render missing %q:\n%s", want, res.Content)
		}
	}
}
