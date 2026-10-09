package golem_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// sentinelToken is the synthetic, non-secret marker inside every fixture
// credential file below. The credential_path guard blocks the read before
// invocation, so it must never reach the session store.
const sentinelToken = "guard-probe-575"

// seqCaller returns queued responses across runs, then "done".
type seqCaller struct{ responses []agent.ModelResult }

func (s *seqCaller) Chat(_ context.Context, _ provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if len(s.responses) == 0 {
		return agent.ModelResult{Response: provider.ChatResponse{Content: "done"}}, nil
	}
	r := s.responses[0]
	s.responses = s.responses[1:]
	return r, nil
}

func readCall(id, path string) agent.ModelResult {
	return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
		ID: id, Type: "function", Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"` + path + `"}`)},
	}}}}
}

// TestGuardBlockedObservationsPersistAsToday (#575): pins the runtime-level
// behavior of the always-on guards with a locally composed guard chain: an
// answered run saves its blocked observations raw with the call arguments,
// nothing but the conversation's own fields is persisted, risk resets per
// run, and a capped empty-answer turn is not saved. The chain is composed
// here, not taken from the CLI; persistence under the CLI's own composition
// is pinned by TestDefaultGuardsBlockedObservationPersists in cmd/golem.
func TestGuardBlockedObservationsPersistAsToday(t *testing.T) {
	inv, err := interceptor.NewInvariants(interceptor.DefaultInvariants())
	if err != nil {
		t.Fatal(err)
	}
	chain := agent.WithInterceptors(inv, interceptor.Egress{}, interceptor.ChildScopeDenials{})
	caller := &seqCaller{responses: []agent.ModelResult{
		readCall("b1", ".env"), {Response: provider.ChatResponse{Content: "answered"}},
		{Response: provider.ChatResponse{Content: "second"}},
		readCall("c1", ".env"), readCall("c2", ".ssh/id_ed25519"), readCall("c3", ".aws/credentials"),
	}}
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{}}
	// golem.New installs the built-in file tools (read_file included) and
	// rejects duplicate names, so no extra tool is registered here. Every
	// credential file exists, so an unguarded read would succeed and put the
	// sentinel into the stored transcript.
	root := t.TempDir()
	for _, p := range []string{".env", ".ssh/id_ed25519", ".aws/credentials"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, p), []byte("TOKEN="+sentinelToken+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := golem.New(context.Background(), golem.Options{
		Root: root, Orchestrator: agent.New(caller, agent.ContextManager{}, chain), SessionStore: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	sink := func(golem.Event) error { return nil }
	first, err := rt.Run(context.Background(), golem.Turn{ThreadID: "t", RunID: "r1", Message: "q1"}, sink)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := rt.Run(context.Background(), golem.Turn{ThreadID: "t", RunID: "r2", Message: "q2"}, sink)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}

	store.mu.Lock()
	conv := store.conversations["t"]
	store.mu.Unlock()
	wantRoles := []string{"user", "assistant", "tool", "assistant", "user", "assistant"}
	var roles []string
	for _, m := range conv.Messages {
		roles = append(roles, m.Role)
		if strings.Contains(m.Content, sentinelToken) || strings.Contains(string(m.ToolCalls), sentinelToken) {
			t.Fatalf("stored message leaked the sentinel: %+v", m)
		}
	}
	if !slices.Equal(roles, wantRoles) {
		t.Fatalf("stored roles = %v, want %v: %+v", roles, wantRoles, conv.Messages)
	}
	// The answered turn persists its tool call with the arguments the model
	// sent, and the raw blocked observation (not the framed wire copy).
	var calls []provider.ToolCall
	if err := json.Unmarshal(conv.Messages[1].ToolCalls, &calls); err != nil {
		t.Fatalf("stored tool calls = %s: %v", conv.Messages[1].ToolCalls, err)
	}
	if len(calls) != 1 || calls[0].ID != "b1" || calls[0].Function.Name != "read_file" || string(calls[0].Function.Arguments) != `{"path":".env"}` {
		t.Fatalf("stored tool calls = %+v, want read_file b1 with its arguments", calls)
	}
	const blocked = "tool call blocked by interceptor invariants (credential_path)"
	if m := conv.Messages[2]; m.Content != blocked || m.ToolName != "read_file" || m.ToolCallID != "b1" {
		t.Fatalf("stored observation = %+v, want the raw blocked observation for b1", m)
	}
	// Risk, blocked and native metadata are not persisted: a stored message
	// carries only the conversation.Message keys, and the conversation itself
	// is exactly its identity, title, revision and messages. So a risk or
	// blocked field added to either type or to the save path fails here. The
	// blocked state survives only as the observation text above.
	for i, m := range conv.Messages {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatal(err)
		}
		for k := range keys {
			if !slices.Contains([]string{"role", "content", "tool_calls", "tool_name", "tool_call_id"}, k) {
				t.Fatalf("stored message %d carries unexpected key %q: %s", i, k, raw)
			}
		}
	}
	// Revision 2 is one save per answered turn. mapSessionStore stamps no
	// wall-clock fields, so CreatedAt and UpdatedAt are zero and compared as
	// such. The messages are pinned above; comparing them to themselves here
	// leaves every other field of the conversation under test.
	wantConv := conversation.Conversation{ID: "t", Title: "q1", Messages: conv.Messages, Revision: 2}
	if !reflect.DeepEqual(conv, wantConv) {
		t.Fatalf("stored conversation = %+v, want only id, title, revision and messages: %+v", conv, wantConv)
	}

	if first.Risk == nil || first.Risk.Score != 30 {
		t.Fatalf("first run risk = %+v, want 30", first.Risk)
	}
	// Risk is per run: each Run starts a fresh report, so the blocked call in
	// r1 must not surface on r2.
	if second.Risk != nil {
		t.Fatalf("second run risk = %+v, want per-run reset", second.Risk)
	}

	capped, err := rt.Run(context.Background(), golem.Turn{ThreadID: "capped", RunID: "r3", Message: "q3"}, sink)
	if err != nil || capped.StopReason != agent.ToolErrorCapReached || capped.Answer != "" {
		t.Fatalf("capped run = %v %q %v", capped.StopReason, capped.Answer, err)
	}
	if len(capped.ToolCalls) != 3 {
		t.Fatalf("capped tool calls = %+v, want 3 guard blocks", capped.ToolCalls)
	}
	for i, r := range capped.ToolCalls {
		if !r.Blocked || r.Invoked {
			t.Fatalf("capped record %d = %+v, want a guard block", i, r)
		}
	}
	store.mu.Lock()
	_, saved := store.conversations["capped"]
	saves := store.saves
	store.mu.Unlock()
	if saved || saves != 2 {
		t.Fatalf("capped empty-answer turn saved = %v after %d saves, want not saved and the 2 answered turns only", saved, saves)
	}
}
