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
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// countingTool is a stub delegate that counts invocations.
type countingTool struct {
	calls   atomic.Int64
	content string
}

type blockingTool struct {
	content string
	entered chan struct{}
	release chan struct{}
}

func (b *blockingTool) Spec() agent.ToolSpec { return agenttools.Retrieve{}.Spec() }
func (b *blockingTool) Effect() agent.Effect { return agenttools.Retrieve{}.Effect() }
func (b *blockingTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	close(b.entered)
	<-b.release
	return agent.ToolResult{Content: b.content}, nil
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
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err == nil {
		t.Fatal("close() must close the retained feedback DB handle")
	}
	if err := r.close(); err != nil { // idempotent, nil-safe second close
		t.Fatal(err)
	}
}

// Shutdown can race the background job: close() may run while the wrapper is
// still warming, and markReady lands afterwards. The handle installed then can
// never be released by close() again, so markReady must release it itself.
func TestReadyRetrieve_MarkReadyAfterCloseReleases(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	r := newReadyRetrieve(warmingRetrieveMessage)
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	r.markReady(&countingTool{content: "chunks"}, "ready", &behavioralWeighterHandle{db: db})
	if err := db.Ping(); err == nil {
		t.Fatal("markReady after close() must release the incoming feedback handle")
	}
}

func TestReadyRetrieve_CloseNilSafe(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	if err := r.close(); err != nil { // no feedback handle retained; must not panic
		t.Fatal(err)
	}
}

// A markReady that loses the terminal-transition race to markFailed must not
// strand its feedback handle either — same ownership rule as the close race.
func TestReadyRetrieve_MarkReadyAfterFailedReleases(t *testing.T) {
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
			if err := r.close(); err != nil {
				t.Error(err)
			}
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

func TestReadyRetrieve_InstallSwapsLiveDelegate(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	oldClosed := make(chan struct{})
	r.install(newRetrievalReader(&countingTool{content: "old"}, func() error { close(oldClosed); return nil }), "old ready")

	before, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil || before.Content != "old" {
		t.Fatalf("before swap = %+v, %v", before, err)
	}
	r.install(newRetrievalReader(&countingTool{content: "new"}, nil), "new ready")
	after, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
	if err != nil || after.Content != "new" {
		t.Fatalf("after swap = %+v, %v", after, err)
	}
	select {
	case <-oldClosed:
	case <-time.After(time.Second):
		t.Fatal("replaced delegate did not close")
	}
}

func TestReadyRetrieve_OldDelegateDrainsWhileNewUsed(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	oldTool := &blockingTool{content: "old", entered: make(chan struct{}), release: make(chan struct{})}
	oldClosed := make(chan struct{})
	r.install(newRetrievalReader(oldTool, func() error { close(oldClosed); return nil }), "old ready")

	oldResult := make(chan agent.ToolResult, 1)
	go func() {
		res, _ := r.Invoke(context.Background(), json.RawMessage(`{"query":"old"}`))
		oldResult <- res
	}()
	<-oldTool.entered
	r.install(newRetrievalReader(&countingTool{content: "new"}, nil), "new ready")

	newResult, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"new"}`))
	if err != nil || newResult.Content != "new" {
		t.Fatalf("new call = %+v, %v", newResult, err)
	}
	select {
	case <-oldClosed:
		t.Fatal("old delegate closed before its in-flight call completed")
	default:
	}
	close(oldTool.release)
	if got := <-oldResult; got.Content != "old" {
		t.Fatalf("in-flight result = %q, want old", got.Content)
	}
	select {
	case <-oldClosed:
	case <-time.After(time.Second):
		t.Fatal("old delegate did not close after drain")
	}
}

func TestReadyRetrieve_CloseRejectsAndClosesLateInstall(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	if r.install(newRetrievalReader(&countingTool{content: "late"}, func() error { close(closed); return nil }), "late") {
		t.Fatal("install after close must be rejected")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("rejected delegate was not closed")
	}
}

func TestRetrievalReader_CloseIsIdempotent(t *testing.T) {
	var closes atomic.Int64
	reader := newRetrievalReader(&countingTool{content: "x"}, func() error { closes.Add(1); return nil })
	if err := reader.closeAfterDrain(); err != nil {
		t.Fatal(err)
	}
	if err := reader.closeAfterDrain(); err != nil {
		t.Fatal(err)
	}
	if got := closes.Load(); got != 1 {
		t.Fatalf("close calls = %d, want 1", got)
	}
}

func TestReadyRetrieve_RepeatedConcurrentInvokeAndSwap(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	r.install(newRetrievalReader(&countingTool{content: "seed"}, nil), "seed")
	stop := make(chan struct{})
	var invokes sync.WaitGroup
	for range 8 {
		invokes.Add(1)
		go func() {
			defer invokes.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		r.install(newRetrievalReader(&countingTool{content: "next"}, nil), "next")
	}
	close(stop)
	invokes.Wait()
	if err := r.close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadyRetrieve_CloseWaitsForCurrentDelegate(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	cur := &blockingTool{content: "cur", entered: make(chan struct{}), release: make(chan struct{})}
	curClosed := make(chan struct{})
	r.install(newRetrievalReader(cur, func() error { close(curClosed); return nil }), "cur")
	go func() { _, _ = r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`)) }()
	<-cur.entered
	closed := make(chan struct{})
	go func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned while the current delegate call was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(cur.release)
	select {
	case <-curClosed:
	case <-time.After(time.Second):
		t.Fatal("current delegate did not close")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("wrapper close did not wait for the current delegate")
	}
}

// TestReadyRetrieve_ConcurrentInvokeInstallClose races Invoke, install, and
// close together and pins exactly-once reader closure: retired readers are
// joined by close via retiring, rejected post-close installs close their
// reader synchronously, and the current reader is drained by close itself.
func TestReadyRetrieve_ConcurrentInvokeInstallClose(t *testing.T) {
	for range 50 {
		r := newReadyRetrieve(warmingRetrieveMessage)
		var opened, released atomic.Int64
		mk := func() *retrievalReader {
			opened.Add(1)
			return newRetrievalReader(&countingTool{content: "x"}, func() error { released.Add(1); return nil })
		}
		r.install(mk(), "seed")
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = r.Invoke(context.Background(), json.RawMessage(`{"query":"q"}`))
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.install(mk(), "swap")
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.close(); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		if opened.Load() != released.Load() {
			t.Fatalf("opened %d readers, closed %d", opened.Load(), released.Load())
		}
	}
}

func TestReadyRetrieve_CloseWaitsForRetiredDelegate(t *testing.T) {
	r := newReadyRetrieve(warmingRetrieveMessage)
	old := &blockingTool{content: "old", entered: make(chan struct{}), release: make(chan struct{})}
	oldClosed := make(chan struct{})
	r.install(newRetrievalReader(old, func() error { close(oldClosed); return nil }), "old")
	go func() { _, _ = r.Invoke(context.Background(), json.RawMessage(`{"query":"old"}`)) }()
	<-old.entered
	r.install(newRetrievalReader(&countingTool{content: "new"}, nil), "new")
	closed := make(chan struct{})
	go func() {
		if err := r.close(); err != nil {
			t.Error(err)
		}
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned while a retired delegate call was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(old.release)
	select {
	case <-oldClosed:
	case <-time.After(time.Second):
		t.Fatal("retired delegate did not close")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("wrapper close did not wait for retired delegate")
	}
}
