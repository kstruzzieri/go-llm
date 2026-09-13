package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// advisoryCaller records every wire request the orchestrator builds, so a test
// can read what the model actually saw rather than what the runtime intended.
type advisoryCaller struct{ reqs []provider.ChatRequest }

func (c *advisoryCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	c.reqs = append(c.reqs, req)
	if onToken != nil {
		if err := onToken(provider.ChatResponse{Content: "fine", Done: true}); err != nil {
			return agent.ModelResult{}, err
		}
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: "fine", Done: true}}, nil
}

func newAdvisoryRuntime(t *testing.T, store golem.SessionStore, caller agent.ModelCaller) *golem.Runtime {
	t.Helper()
	rt, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		SessionStore: store,
		Orchestrator: agent.New(caller, agent.ContextManager{}),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// discardEvents satisfies the required sink; these tests read the wire
// requests and the session store, not the event stream.
func discardEvents(golem.Event) error { return nil }

func stagedAdvisory() *agent.Advisory {
	return &agent.Advisory{
		Source:  "claude",
		Tool:    "claude 2.1.240",
		Model:   "opus",
		Digest:  strings.Repeat("b", 64),
		Content: "SENTINEL",
		Origin:  agent.OriginModel,
	}
}

func TestRunPassesAdvisoryAndPersistsRawGoalOnly(t *testing.T) {
	caller := &advisoryCaller{}
	rt := newAdvisoryRuntime(t, &mapSessionStore{}, caller)

	res, err := rt.Run(context.Background(), golem.Turn{
		ThreadID: "t1", RunID: "r1", Message: "goal", Advisory: stagedAdvisory(),
	}, discardEvents)
	if err != nil || res.Answer != "fine" {
		t.Fatalf("run: %v %+v", err, res)
	}
	first := caller.reqs[0].Messages
	if !strings.Contains(first[len(first)-1].Content, "SENTINEL") {
		t.Fatalf("advisory not projected on the goal: %q", first[len(first)-1].Content)
	}

	// The next turn of the same thread replays persisted history: the raw goal
	// and the answer only, never the projection.
	if _, err := rt.Run(context.Background(), golem.Turn{ThreadID: "t1", RunID: "r2", Message: "next"}, discardEvents); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(caller.reqs) < 2 {
		t.Fatalf("second run made no model call: %d requests", len(caller.reqs))
	}
	replayed := false
	for _, m := range caller.reqs[1].Messages {
		if m.Content == "goal" {
			replayed = true
		}
		if strings.Contains(m.Content, "SENTINEL") || strings.Contains(m.Content, "CONSULT_ADVICE") {
			t.Fatalf("advisory persisted into history: %q", m.Content)
		}
	}
	if !replayed {
		t.Fatal("second request did not replay the persisted goal; the history assertion is blind")
	}

	if _, err := rt.Run(context.Background(), golem.Turn{RunID: "r3", Message: "goal", Advisory: &agent.Advisory{}}, discardEvents); !errors.Is(err, golem.ErrInvalidRequest) {
		t.Fatalf("invalid advisory accepted: %v", err)
	}
}

func TestTurnAdvisoryIsNotStoredInSession(t *testing.T) {
	store := &mapSessionStore{}
	rt := newAdvisoryRuntime(t, store, &advisoryCaller{})

	if _, err := rt.Run(context.Background(), golem.Turn{
		ThreadID: "t1", RunID: "r1", Message: "goal", Advisory: stagedAdvisory(),
	}, discardEvents); err != nil {
		t.Fatalf("run: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	conv, ok := store.conversations["t1"]
	if !ok {
		t.Fatal("thread was never persisted; the assertion below would be vacuous")
	}
	raw, err := json.Marshal(conv)
	if err != nil {
		t.Fatalf("marshal stored conversation: %v", err)
	}
	if !strings.Contains(string(raw), "goal") {
		t.Fatalf("stored conversation lost the raw goal: %s", raw)
	}
	for _, banned := range []string{"SENTINEL", "CONSULT_ADVICE", "claude 2.1.240"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("advisory %q reached the session store: %s", banned, raw)
		}
	}
}
