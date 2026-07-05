package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// countingTool is a stub delegate that counts invocations.
type countingTool struct {
	calls   atomic.Int64
	content string
}

func (c *countingTool) Spec() agent.ToolSpec { return agenttools.Retrieve{}.Spec() }
func (c *countingTool) Effect() agent.Effect { return agenttools.Retrieve{}.Effect() }
func (c *countingTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	c.calls.Add(1)
	return agent.ToolResult{Content: c.content}, nil
}

func assertNamesFileTools(t *testing.T, content string) {
	t.Helper()
	for _, name := range []string{"read_file", "search", "glob", "list"} {
		if !strings.Contains(content, name) {
			t.Fatalf("response %q does not name file tool %q", content, name)
		}
	}
}

func TestReadyRetrieve_SpecEffectMirrorRetrieve(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	if !reflect.DeepEqual(r.Spec(), agenttools.Retrieve{}.Spec()) {
		t.Fatal("Spec() must mirror agenttools.Retrieve so the model-facing schema is stable across the swap")
	}
	// agent.Effect holds a slice field (Scope.Paths); compare with DeepEqual.
	if !reflect.DeepEqual(r.Effect(), agenttools.Retrieve{}.Effect()) {
		t.Fatal("Effect() must mirror agenttools.Retrieve")
	}
}

func TestReadyRetrieve_WarmingResponse(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	res, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal("warming response must be non-error")
	}
	if !strings.Contains(res.Content, "warming") {
		t.Fatalf("warming response %q should mention warming", res.Content)
	}
	assertNamesFileTools(t, res.Content)
}

func TestReadyRetrieve_FailedResponse(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	r.markFailed("retrieve unavailable: embedder down; use read_file, search, glob, and list instead")
	res, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatal("failed response must be non-error")
	}
	if !strings.Contains(res.Content, "embedder down") {
		t.Fatalf("failed response %q should carry the failure reason", res.Content)
	}
	assertNamesFileTools(t, res.Content)
}

func TestReadyRetrieve_ReadyDelegatesExactlyOnce(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	inner := &countingTool{content: "chunks"}
	r.markReady(inner, "ready", nil)
	res, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "chunks" {
		t.Fatalf("ready must delegate, got %q", res.Content)
	}
	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("delegate invoked %d times, want 1", got)
	}
}

func TestReadyRetrieve_FirstTerminalTransitionWins(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	r.markFailed("first failure; use read_file, search, glob, and list instead")
	r.markReady(&countingTool{content: "chunks"}, "ready", nil)
	res, _ := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if res.Content == "chunks" {
		t.Fatal("markReady after markFailed must be ignored (first terminal transition wins)")
	}

	r2 := newReadyRetrieve(warmingRetrieveMessage)
	r2.markReady(&countingTool{content: "chunks"}, "ready", nil)
	r2.markFailed("late failure")
	res2, _ := r2.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if res2.Content != "chunks" {
		t.Fatal("markFailed after markReady must be ignored (first terminal transition wins)")
	}
}

func TestReadyRetrieve_CloseReleasesFeedbackHandle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	r := newReadyRetrieve(warmingRetrieveMessage)
	r.markReady(&countingTool{content: "chunks"}, "ready", &behavioralWeighterHandle{db: db})
	r.close()
	if err := db.Ping(); err == nil {
		t.Fatal("close() must close the retained feedback DB handle")
	}
	r.close() // idempotent, nil-safe second close
}

// Shutdown can race the background job: close() may run while the wrapper is
// still warming, and markReady lands afterwards. The handle installed then can
// never be released by close() again, so markReady must release it itself.
func TestReadyRetrieve_MarkReadyAfterCloseReleasesHandle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	r := newReadyRetrieve(warmingRetrieveMessage)
	r.close()
	r.markReady(&countingTool{content: "chunks"}, "ready", &behavioralWeighterHandle{db: db})
	if err := db.Ping(); err == nil {
		t.Fatal("markReady after close() must release the incoming feedback handle")
	}
}

func TestReadyRetrieve_CloseNilSafe(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	r.close() // no feedback handle retained; must not panic
}

// A markReady that loses the terminal-transition race to markFailed must not
// strand its feedback handle either — same ownership rule as the close race.
func TestReadyRetrieve_MarkReadyAfterFailedReleasesHandle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	r := newReadyRetrieve(warmingRetrieveMessage)
	r.markFailed("boom; use read_file, search, glob, and list instead")
	r.markReady(&countingTool{content: "chunks"}, "ready", &behavioralWeighterHandle{db: db})
	if err := db.Ping(); err == nil {
		t.Fatal("markReady losing the transition race must release its feedback handle")
	}
}

// Concurrent close/markReady: exactly one side must end up releasing the
// handle, with no race reported and no strand.
func TestReadyRetrieve_ConcurrentCloseAndMarkReady(t *testing.T) {
	for i := 0; i < 50; i++ {
		dbPath := filepath.Join(t.TempDir(), "feedback.db")
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		r := newReadyRetrieve(warmingRetrieveMessage)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.markReady(&countingTool{content: "chunks"}, "ready", &behavioralWeighterHandle{db: db})
		}()
		go func() {
			defer wg.Done()
			r.close()
		}()
		wg.Wait()
		// Whichever side lost the race must have released the handle already;
		// no sweep needed.
		if err := db.Ping(); err == nil {
			t.Fatal("handle stranded after concurrent close/markReady")
		}
	}
}

// Invoke may run from parallel read-only dispatch goroutines (#235) while the
// background job flips state; the wrapper must be race-clean.
func TestReadyRetrieve_ConcurrentMarkReadyAndInvoke(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	inner := &countingTool{content: "chunks"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if _, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	r.markReady(inner, "ready", nil)
	wg.Wait()
	res, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "chunks" {
		t.Fatalf("post-swap invoke = %q", res.Content)
	}
}
