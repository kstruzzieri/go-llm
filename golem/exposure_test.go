package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// foreignTool is a read tool whose results come from outside the conversation.
type foreignTool struct{}

func (foreignTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "fetch_page", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (foreignTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (foreignTool) Origin() agent.Origin { return agent.OriginForeign }

func (foreignTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "outside text"}, nil
}

// exposureCaller calls tool once per turn when tool is set, then answers.
// A non-nil fail makes requests fail, but only after the tool round when tool
// is set, so the failed run has already admitted the tool's result. before, if
// set, runs once at the start of the first request.
type exposureCaller struct {
	tool   string
	answer string
	fail   error
	before func()
}

func (c *exposureCaller) Chat(_ context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	if c.before != nil {
		before := c.before
		c.before = nil
		before()
	}
	called := false
	for _, m := range req.Messages {
		called = called || m.Role == "tool"
	}
	if c.fail != nil && (c.tool == "" || called) {
		return agent.ModelResult{}, c.fail
	}
	if c.tool != "" {
		if !called {
			call := provider.ToolCall{ID: "call-1", Type: "function", Function: provider.ToolCallFunction{Name: c.tool, Arguments: json.RawMessage(`{}`)}}
			return agent.ModelResult{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{call}}}, nil
		}
	}
	if c.answer != "" {
		if err := onToken(provider.ChatResponse{Content: c.answer}); err != nil {
			return agent.ModelResult{}, err
		}
	}
	return agent.ModelResult{Response: provider.ChatResponse{Content: c.answer, Done: true}}, nil
}

func newExposureRuntime(t *testing.T, caller *exposureCaller, opts golem.Options) *golem.Runtime {
	t.Helper()
	opts.Profile = golem.ProfileTrusted
	opts.Tools = []agent.Tool{foreignTool{}}
	opts.Orchestrator = agent.New(caller, agent.ContextManager{})
	return newCompactionRuntime(t, opts)
}

var runSeq atomic.Int64

func exposureTurn(t *testing.T, rt *golem.Runtime, thread string) (agent.Result, error) {
	t.Helper()
	return rt.Run(context.Background(), golem.Turn{
		ThreadID: thread,
		RunID:    fmt.Sprintf("run-%d", runSeq.Add(1)),
		Message:  "go",
	}, func(golem.Event) error { return nil })
}

func mustExposure(t *testing.T, rt *golem.Runtime, thread string) agent.Exposure {
	t.Helper()
	got, err := rt.ThreadExposure(context.Background(), thread)
	if err != nil {
		t.Fatalf("ThreadExposure: %v", err)
	}
	return got
}

func TestExposureCarriedAcrossTurns(t *testing.T) {
	t.Parallel()
	caller := &exposureCaller{tool: "fetch_page", answer: "a1"}
	store := &mapSessionStore{}
	rt := newExposureRuntime(t, caller, golem.Options{SessionStore: store})
	if _, err := exposureTurn(t, rt, "t"); err != nil {
		t.Fatal(err)
	}
	want := agent.Exposure{Recorded: true, Foreign: true, First: "tool:fetch_page"}
	if got := mustExposure(t, rt, "t"); got != want {
		t.Fatalf("ThreadExposure after turn 1 = %+v, want %+v", got, want)
	}
	if got := store.conversations["t"].Exposure; got != want {
		t.Fatalf("stored = %+v, want %+v", got, want)
	}
	caller.tool = ""
	res, err := exposureTurn(t, rt, "t")
	if err != nil {
		t.Fatal(err)
	}
	if res.Exposure != want {
		t.Fatalf("turn 2 result exposure = %+v, want %+v", res.Exposure, want)
	}
}

func TestExposureSurvivesCompaction(t *testing.T) {
	t.Parallel()
	current := compactionConversation(5)
	current.Exposure = agent.Exposure{Recorded: true, Foreign: true, First: "tool:fetch_page"}
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: cloneConversation(current)}}
	rt := newExposureRuntime(t, &exposureCaller{answer: "x"}, golem.Options{SessionStore: store, Summarizer: func(context.Context, string, []conversation.Message) (string, error) { return "SUM", nil }})
	rep, err := rt.CompactThread(context.Background(), current.ID)
	if err != nil || !rep.Changed {
		t.Fatalf("CompactThread = %+v, %v; want changed", rep, err)
	}
	if got := mustExposure(t, rt, current.ID); got != current.Exposure {
		t.Fatalf("ThreadExposure after compaction = %+v, want %+v", got, current.Exposure)
	}
}

func TestExposureFailedRunIsDropped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		caller *exposureCaller
	}{
		{"error", &exposureCaller{tool: "fetch_page", fail: errors.New("boom")}},
		{"empty answer", &exposureCaller{tool: "fetch_page"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &mapSessionStore{conversations: map[string]conversation.Conversation{
				"t": {ID: "t", Revision: 1, Exposure: agent.Exposure{Recorded: true}, Messages: []conversation.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}},
			}}
			rt := newExposureRuntime(t, tc.caller, golem.Options{SessionStore: store})
			res, _ := exposureTurn(t, rt, "t")
			if !res.Exposure.Foreign {
				t.Fatalf("failed run exposure = %+v, want Foreign (the tool round must have run)", res.Exposure)
			}
			if got, want := mustExposure(t, rt, "t"), (agent.Exposure{Recorded: true}); got != want {
				t.Fatalf("ThreadExposure = %+v, want %+v", got, want)
			}
		})
	}
}

func TestExposureLostRevisionRaceKeepsStored(t *testing.T) {
	t.Parallel()
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{
		"t": {ID: "t", Revision: 1, Exposure: agent.Exposure{Recorded: true}, Messages: []conversation.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}},
	}}
	racer := agent.Exposure{Recorded: true, Foreign: true, First: "tool:racer"}
	caller := &exposureCaller{tool: "fetch_page", answer: "a"}
	// A second writer commits at the revision this turn loaded, so the turn's
	// own save is stale.
	caller.before = func() {
		won := cloneConversation(store.conversations["t"])
		won.Exposure = racer
		if _, err := store.Save(context.Background(), won); err != nil {
			t.Errorf("racing save: %v", err)
		}
	}
	rt := newExposureRuntime(t, caller, golem.Options{SessionStore: store})
	if _, err := exposureTurn(t, rt, "t"); !errors.Is(err, conversation.ErrConflict) {
		t.Fatalf("turn error = %v, want conflict", err)
	}
	if got := mustExposure(t, rt, "t"); got != racer {
		t.Fatalf("ThreadExposure = %+v, want the racer's %+v", got, racer)
	}
}

// droppingStore zeroes Exposure on every Save, like a custom store that
// predates the field.
type droppingStore struct{ mapSessionStore }

func (s *droppingStore) Save(ctx context.Context, conv conversation.Conversation) (int64, error) {
	conv.Exposure = agent.Exposure{}
	return s.mapSessionStore.Save(ctx, conv)
}

func TestExposureCustomStoreDroppingFieldReadsUnknown(t *testing.T) {
	t.Parallel()
	caller := &exposureCaller{answer: "a"}
	rt := newExposureRuntime(t, caller, golem.Options{SessionStore: &droppingStore{}})
	if _, err := exposureTurn(t, rt, "t"); err != nil {
		t.Fatal(err)
	}
	if got := mustExposure(t, rt, "t"); !got.Exposed() || got.Recorded {
		t.Fatalf("ThreadExposure = %+v, want unrecorded and exposed", got)
	}
	res, err := exposureTurn(t, rt, "t")
	if err != nil {
		t.Fatal(err)
	}
	if want := (agent.Exposure{Recorded: true, Unknown: true, First: "history"}); res.Exposure != want || !res.Exposure.Exposed() {
		t.Fatalf("turn 2 exposure = %+v, want %+v", res.Exposure, want)
	}
}

func TestExposureStatelessTurnCarriesNothing(t *testing.T) {
	t.Parallel()
	caller := &exposureCaller{tool: "fetch_page", answer: "a"}
	store := &mapSessionStore{}
	rt := newExposureRuntime(t, caller, golem.Options{SessionStore: store})
	first, err := exposureTurn(t, rt, "")
	if err != nil || !first.Exposure.Foreign {
		t.Fatalf("stateless foreign turn = %+v, %v", first.Exposure, err)
	}
	caller.tool = ""
	second, err := exposureTurn(t, rt, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Exposure != (agent.Exposure{Recorded: true}) || store.saves != 0 {
		t.Fatalf("second stateless exposure = %+v, saves %d; want clean and no save", second.Exposure, store.saves)
	}
}

func TestExposureTrackedWithoutStrict(t *testing.T) {
	t.Parallel()
	// No option for a strict approval mode exists yet: the record is always on.
	store := &mapSessionStore{}
	rt := newExposureRuntime(t, &exposureCaller{tool: "fetch_page", answer: "a"}, golem.Options{SessionStore: store})
	if _, err := exposureTurn(t, rt, "t"); err != nil {
		t.Fatal(err)
	}
	if got, want := store.conversations["t"].Exposure, (agent.Exposure{Recorded: true, Foreign: true, First: "tool:fetch_page"}); got != want {
		t.Fatalf("stored = %+v, want %+v", got, want)
	}
}

func TestExposureLegacyRow(t *testing.T) {
	t.Parallel()
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{
		"t": {ID: "t", Revision: 1, Messages: []conversation.Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}},
	}}
	rt := newExposureRuntime(t, &exposureCaller{answer: "a"}, golem.Options{SessionStore: store})
	if got := mustExposure(t, rt, "t"); got.Recorded || !got.Exposed() {
		t.Fatalf("legacy ThreadExposure = %+v, want unrecorded and exposed", got)
	}
	if _, err := exposureTurn(t, rt, "t"); err != nil {
		t.Fatal(err)
	}
	if got, want := store.conversations["t"].Exposure, (agent.Exposure{Recorded: true, Unknown: true, First: "history"}); got != want {
		t.Fatalf("stored after first turn = %+v, want %+v", got, want)
	}
}

func TestExposureCleanStaysClean(t *testing.T) {
	t.Parallel()
	store := &mapSessionStore{}
	rt := newExposureRuntime(t, &exposureCaller{answer: "a"}, golem.Options{SessionStore: store})
	if _, err := exposureTurn(t, rt, "t"); err != nil {
		t.Fatal(err)
	}
	if got := store.conversations["t"].Exposure; got != (agent.Exposure{Recorded: true}) {
		t.Fatalf("stored after turn 1 = %+v, want clean record", got)
	}
	res, err := exposureTurn(t, rt, "t")
	if err != nil {
		t.Fatal(err)
	}
	if res.Exposure.Exposed() || store.conversations["t"].Exposure.Exposed() {
		t.Fatalf("turn 2 result %+v / stored %+v, want not exposed", res.Exposure, store.conversations["t"].Exposure)
	}
}

func TestThreadExposureValidationAndMissing(t *testing.T) {
	t.Parallel()
	store := &mapSessionStore{}
	rt := newExposureRuntime(t, &exposureCaller{answer: "a"}, golem.Options{SessionStore: store})
	for _, id := range []string{"", strings.Repeat("x", 257)} {
		if got, err := rt.ThreadExposure(context.Background(), id); !errors.Is(err, golem.ErrInvalidRequest) || got != (agent.Exposure{}) {
			t.Errorf("ThreadExposure(len %d) = %+v, %v; want zero, ErrInvalidRequest", len(id), got, err)
		}
	}
	if got, err := rt.ThreadExposure(context.Background(), "missing"); err != nil || got != (agent.Exposure{Recorded: true}) || store.saves != 0 {
		t.Errorf("missing thread = %+v, %v, saves %d; want clean, nil, no save", got, err, store.saves)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := rt.ThreadExposure(context.Background(), "missing"); !errors.Is(err, golem.ErrClosed) || got != (agent.Exposure{}) {
		t.Errorf("after Close = %+v, %v; want zero, ErrClosed", got, err)
	}
}

func TestExposureForeignTurnThatCompresses(t *testing.T) {
	t.Parallel()
	current := compactionConversation(5)
	current.Exposure = agent.Exposure{Recorded: true}
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: cloneConversation(current)}}
	rt := newExposureRuntime(t, &exposureCaller{tool: "fetch_page", answer: strings.Repeat("a", 10000)}, golem.Options{
		SessionStore: store,
		Budget:       agent.Budget{InputCeiling: 4096},
		Summarizer:   func(context.Context, string, []conversation.Message) (string, error) { return "SUM", nil },
	})
	if _, err := exposureTurn(t, rt, current.ID); err != nil {
		t.Fatal(err)
	}
	stored := store.conversations[current.ID]
	if stored.DurableSummary == nil {
		t.Fatal("turn did not compress; the second save is untested")
	}
	if want := (agent.Exposure{Recorded: true, Foreign: true, First: "tool:fetch_page"}); stored.Exposure != want {
		t.Fatalf("stored after compression = %+v, want %+v", stored.Exposure, want)
	}
}

func TestExposureCustomStoreRecordedWithoutHistory(t *testing.T) {
	t.Parallel()
	want := agent.Exposure{Recorded: true, Foreign: true, First: "tool:fetch_page"}
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{"t": {ID: "t", Revision: 1, Exposure: want}}}
	rt := newExposureRuntime(t, &exposureCaller{answer: "a"}, golem.Options{SessionStore: store})
	res, err := exposureTurn(t, rt, "t")
	if err != nil {
		t.Fatal(err)
	}
	if res.Exposure != want {
		t.Fatalf("result exposure = %+v, want %+v", res.Exposure, want)
	}
}

// blockingLoadStore blocks Load until its context is done.
type blockingLoadStore struct {
	mapSessionStore
	started chan struct{}
}

func (s *blockingLoadStore) Load(ctx context.Context, _ string) (*conversation.Conversation, error) {
	close(s.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestThreadExposureCanceledByClose(t *testing.T) {
	t.Parallel()
	store := &blockingLoadStore{started: make(chan struct{})}
	rt := newExposureRuntime(t, &exposureCaller{answer: "a"}, golem.Options{SessionStore: store})
	type outcome struct {
		got agent.Exposure
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		got, err := rt.ThreadExposure(context.Background(), "t")
		done <- outcome{got, err}
	}()
	select {
	case <-store.started:
	case <-time.After(10 * time.Second):
		t.Fatal("ThreadExposure never reached Load")
	}
	closed := make(chan error, 1)
	go func() { closed <- rt.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return: it cannot cancel an in-flight ThreadExposure")
	}
	select {
	case o := <-done:
		if o.err == nil || o.got != (agent.Exposure{}) {
			t.Fatalf("ThreadExposure = %+v, %v; want zero and an error", o.got, o.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ThreadExposure did not return")
	}
}

func TestThreadExposureDuringRun(t *testing.T) {
	t.Parallel()
	caller := &exposureCaller{tool: "fetch_page", answer: "a"}
	var rt *golem.Runtime
	var during agent.Exposure
	var duringErr error
	caller.before = func() { during, duringErr = rt.ThreadExposure(context.Background(), "t") }
	rt = newExposureRuntime(t, caller, golem.Options{SessionStore: &mapSessionStore{}})
	if _, err := exposureTurn(t, rt, "t"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if duringErr != nil || during != (agent.Exposure{Recorded: true}) {
		t.Fatalf("ThreadExposure during Run = %+v, %v; want clean, nil", during, duringErr)
	}
}
