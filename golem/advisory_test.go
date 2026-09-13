package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/golem"
)

func newAdvisoryRuntime(t *testing.T, store golem.SessionStore, orch *agent.Orchestrator) *golem.Runtime {
	t.Helper()
	rt, err := golem.New(context.Background(), golem.Options{
		Root:         t.TempDir(),
		SessionStore: store,
		Orchestrator: orch,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

const advisoryDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func stagedAdvisory() *agent.Advisory {
	return &agent.Advisory{
		Source:  "claude",
		Tool:    "claude 2.1.240",
		Model:   "opus",
		Digest:  advisoryDigest,
		Content: "SENTINEL",
		Origin:  agent.OriginModel,
	}
}

func TestRunPassesAdvisoryAndPersistsRawGoalOnly(t *testing.T) {
	caller := &captureCaller{answer: "fine"}
	rt := newAdvisoryRuntime(t, &mapSessionStore{}, agent.New(caller, agent.ContextManager{}))

	res, err := rt.Run(context.Background(), golem.Turn{
		ThreadID: "t1", RunID: "r1", Message: "goal", Advisory: stagedAdvisory(),
	}, func(golem.Event) error { return nil })
	if err != nil || res.Answer != "fine" {
		t.Fatalf("run: %v %+v", err, res)
	}
	first := caller.requests[0].Messages
	if !strings.Contains(first[len(first)-1].Content, "SENTINEL") {
		t.Fatalf("advisory not projected on the goal: %q", first[len(first)-1].Content)
	}

	// The next turn of the same thread replays persisted history: the raw goal
	// and the answer only, never the projection.
	if _, err := rt.Run(context.Background(), golem.Turn{ThreadID: "t1", RunID: "r2", Message: "next"},
		func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(caller.requests) < 2 {
		t.Fatalf("second run made no model call: %d requests", len(caller.requests))
	}
	replayed := false
	for _, m := range caller.requests[1].Messages {
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

	if _, err := rt.Run(context.Background(), golem.Turn{RunID: "r3", Message: "goal", Advisory: &agent.Advisory{}},
		func(golem.Event) error { return nil }); !errors.Is(err, golem.ErrInvalidRequest) {
		t.Fatalf("invalid advisory accepted: %v", err)
	}
}

// TestStatelessTurnProjectsAdvisory pins the pass-through for a turn with no
// thread: the advisory travels with the request, not with the session.
func TestStatelessTurnProjectsAdvisory(t *testing.T) {
	caller := &captureCaller{answer: "fine"}
	rt := newAdvisoryRuntime(t, &mapSessionStore{}, agent.New(caller, agent.ContextManager{}))

	if _, err := rt.Run(context.Background(), golem.Turn{
		RunID: "r1", Message: "goal", Advisory: stagedAdvisory(),
	}, func(golem.Event) error { return nil }); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(caller.requests) != 1 {
		t.Fatalf("model calls = %d, want 1", len(caller.requests))
	}
	last := caller.requests[0].Messages[len(caller.requests[0].Messages)-1]
	if !strings.Contains(last.Content, "SENTINEL") {
		t.Fatalf("advisory not projected on the stateless goal: %q", last.Content)
	}
}

func TestTurnAdvisoryIsNotStoredInSession(t *testing.T) {
	store := &mapSessionStore{}
	rt := newAdvisoryRuntime(t, store, agent.New(&captureCaller{answer: "fine"}, agent.ContextManager{}))

	if _, err := rt.Run(context.Background(), golem.Turn{
		ThreadID: "t1", RunID: "r1", Message: "goal", Advisory: stagedAdvisory(),
	}, func(golem.Event) error { return nil }); err != nil {
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
	for _, banned := range []string{"SENTINEL", "CONSULT_ADVICE", "claude 2.1.240", "opus", advisoryDigest} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("advisory %q reached the session store: %s", banned, raw)
		}
	}
}

// blockAdvisory refuses any inspected content carrying the marker.
type blockAdvisory struct{ marker string }

func (blockAdvisory) Name() string { return "blocker" }

func (b blockAdvisory) InspectInput(_ context.Context, in agent.InputInspection) ([]agent.Finding, error) {
	for _, m := range in.Messages {
		if strings.Contains(m.Content, b.marker) {
			return []agent.Finding{{
				Interceptor: "blocker", Rule: "advice-refused", Verdict: agent.VerdictBlock,
			}}, nil
		}
	}
	return nil, nil
}

func (blockAdvisory) InspectOutput(context.Context, agent.OutputInspection) ([]agent.Finding, error) {
	return nil, nil
}

func (blockAdvisory) InspectToolCall(context.Context, agent.ToolCallInspection) ([]agent.Finding, error) {
	return nil, nil
}

// TestBlockedAdvisoryFailsAsPolicy pins the classification: a refusal by the
// interceptor policy is the caller's problem to see as such, not an
// unexplained internal fault.
func TestBlockedAdvisoryFailsAsPolicy(t *testing.T) {
	caller := &captureCaller{answer: "fine"}
	rt := newAdvisoryRuntime(t, &mapSessionStore{},
		agent.New(caller, agent.ContextManager{}, agent.WithInterceptors(blockAdvisory{marker: "SENTINEL"})))

	var events []golem.Event
	_, err := rt.Run(context.Background(), golem.Turn{
		RunID: "r1", Message: "goal", Advisory: stagedAdvisory(),
	}, func(e golem.Event) error {
		events = append(events, e)
		return nil
	})
	if !errors.Is(err, agent.ErrAdvisoryBlocked) {
		t.Fatalf("Run error = %v, want ErrAdvisoryBlocked", err)
	}
	if len(caller.requests) != 0 {
		t.Fatalf("blocked advisory still reached the model: %d requests", len(caller.requests))
	}
	var failed struct {
		Code string `json:"code"`
	}
	last := events[len(events)-1]
	if last.Type != "run.failed" {
		t.Fatalf("terminal event = %q, want run.failed", last.Type)
	}
	if err := json.Unmarshal(last.Payload, &failed); err != nil {
		t.Fatalf("decode run.failed: %v", err)
	}
	if failed.Code != "policy_blocked" {
		t.Fatalf("run.failed code = %q, want policy_blocked", failed.Code)
	}
}
