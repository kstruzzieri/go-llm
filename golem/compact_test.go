package golem_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/provider"
)

func assertCompactedNextRequest(t *testing.T, caller *captureCaller, original conversation.Conversation) {
	t.Helper()
	if len(caller.requests) != 1 {
		t.Fatalf("next Run produced %d model requests, want one", len(caller.requests))
	}
	want := []provider.ChatMessage{
		{Role: "system", Content: "test system\n\n" + agent.ToolTrustContract},
		{Role: "system", Content: agent.DurableSummaryPrompt("SUM")},
	}
	for _, message := range original.Messages[2:] {
		want = append(want, provider.ChatMessage{Role: message.Role, Content: message.Content})
	}
	want = append(want, provider.ChatMessage{Role: "user", Content: "next goal"})
	if got := caller.requests[0].Messages; !reflect.DeepEqual(got, want) {
		t.Errorf("next Run messages = %+v; want %+v", got, want)
	}
}

func TestCompactThreadNextRunReloadsSavedHistory(t *testing.T) {
	t.Parallel()
	current := compactionConversation(5)
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: cloneConversation(current)}}
	caller := &captureCaller{answer: "next answer"}
	runtime := newCompactionRuntime(t, golem.Options{System: "test system", SessionStore: store, Orchestrator: agent.New(caller, agent.ContextManager{}), Summarizer: func(context.Context, string, []conversation.Message) (string, error) { return "SUM", nil }})
	if _, err := runtime.CompactThread(context.Background(), current.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Run(context.Background(), golem.Turn{ThreadID: current.ID, RunID: "next", Message: "next goal"}, func(golem.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertCompactedNextRequest(t, caller, current)
	if store.loads != 2 {
		t.Errorf("store loads = %d, want compaction load and fresh turn load", store.loads)
	}
}

func TestCompactThreadInjectedStoreReuseAndNextRun(t *testing.T) {
	t.Parallel()
	current := compactionConversation(5)
	current.ID = strings.Repeat("x", 256) // exact supported ID boundary
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: cloneConversation(current)}}
	first := newCompactionRuntime(t, golem.Options{SessionStore: store, Summarizer: func(context.Context, string, []conversation.Message) (string, error) { return "SUM", nil }})
	if got, err := first.CompactThread(context.Background(), current.ID); err != nil || got != (golem.CompactionReport{TokensBefore: 100, TokensAfter: 121, Changed: true}) {
		t.Fatalf("CompactThread at 256-byte ID = %+v, %v; want 100 -> 121 changed", got, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	caller := &captureCaller{answer: "next answer"}
	second := newCompactionRuntime(t, golem.Options{System: "test system", SessionStore: store, Orchestrator: agent.New(caller, agent.ContextManager{})})
	if _, err := second.Run(context.Background(), golem.Turn{ThreadID: current.ID, RunID: "next", Message: "next goal"}, func(golem.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertCompactedNextRequest(t, caller, current)
	want := cloneConversation(current)
	want.Messages = append(want.Messages[2:], conversation.Message{Role: "user", Content: "next goal"}, conversation.Message{Role: "assistant", Content: "next answer"})
	want.Title = current.Messages[2].Content // ordinary turns apply the existing title policy
	want.DurableSummary = &conversation.DurableSummary{Content: "SUM", MessageCount: 2}
	if saved := store.conversations[current.ID]; !reflect.DeepEqual(saved, want) {
		t.Errorf("reused store after next Run = %+v; want %+v", saved, want)
	}
}

func TestCompactThreadSQLiteReopenAndSearch(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ctx := context.Background()
	path := filepath.Join(os.Getenv("XDG_DATA_HOME"), "golem", "sessions.db")
	db, err := memory.OpenHardenedDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := conversation.NewStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	current := compactionConversation(5)
	if err := store.Save(ctx, current); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	current = *loaded
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	first := newCompactionRuntime(t, golem.Options{Root: root, Summarizer: func(_ context.Context, prior string, old []conversation.Message) (string, error) {
		if prior != "" || !reflect.DeepEqual(old, current.Messages[:2]) {
			t.Errorf("SQLite summary inputs = %q, %+v; want oldest pair only", prior, old)
		}
		return "SUM", nil
	}})
	if got, err := first.CompactThread(ctx, current.ID); err != nil || got != (golem.CompactionReport{TokensBefore: 100, TokensAfter: 121, Changed: true}) {
		t.Fatalf("SQLite CompactThread = %+v, %v; want 100 -> 121 changed", got, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopenedDB, err := memory.OpenHardenedDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedDB.Close() })
	reopened, err := conversation.NewStore(ctx, reopenedDB)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reopened.Load(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := cloneConversation(current)
	want.Messages = want.Messages[2:]
	want.DurableSummary = &conversation.DurableSummary{Content: "SUM", MessageCount: 2}
	want.UpdatedAt = saved.UpdatedAt
	if !reflect.DeepEqual(*saved, want) || saved.UpdatedAt.Before(current.UpdatedAt) {
		t.Errorf("reopened snapshot = %+v; want %+v and nondecreasing update time", *saved, want)
	}
	found, err := reopened.Search(ctx, "SUM", conversation.SearchOptions{})
	if err != nil || len(found) != 1 || found[0].ID != current.ID || found[0].MessageCount != 8 {
		t.Errorf("summary search = %+v, %v; want persisted thread with eight raw messages", found, err)
	}
	found, err = reopened.Search(ctx, current.Messages[0].Content, conversation.SearchOptions{})
	if err != nil || len(found) != 0 {
		t.Errorf("evicted-message search = %+v, %v; want no stale index hit", found, err)
	}
	if err := reopenedDB.Close(); err != nil {
		t.Fatal(err)
	}
	caller := &captureCaller{answer: "next answer"}
	second := newCompactionRuntime(t, golem.Options{Root: root, System: "test system", Orchestrator: agent.New(caller, agent.ContextManager{})})
	if _, err := second.Run(ctx, golem.Turn{ThreadID: current.ID, RunID: "next", Message: "next goal"}, func(golem.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertCompactedNextRequest(t, caller, current)
}

type compactionResult struct {
	report golem.CompactionReport
	err    error
}

func receiveCompaction[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case result := <-ch:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not reach its test barrier within five seconds")
		var zero T
		return zero
	}
}

func TestCompactThreadConflictsWithActiveTurn(t *testing.T) {
	t.Parallel()
	current := compactionConversation(5)
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: current}}
	entered := make(chan string, 1)
	calls := 0
	runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Orchestrator: agent.New(barrierCaller{entered: entered}, agent.ContextManager{}), Summarizer: func(context.Context, string, []conversation.Message) (string, error) { calls++; return "SUM", nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Run(ctx, golem.Turn{ThreadID: current.ID, RunID: "active-turn", Message: "wait"}, func(golem.Event) error { return nil })
		done <- err
	}()
	receiveCompaction(t, entered)
	got, err := runtime.CompactThread(context.Background(), current.ID)
	if !errors.Is(err, golem.ErrRunConflict) || got != (golem.CompactionReport{}) || calls != 0 {
		t.Errorf("CompactThread during turn = %+v, %v, calls %d; want conflict before work", got, err, calls)
	}
	cancel()
	if err := receiveCompaction(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("active turn = %v, want canceled", err)
	}
	if _, err := runtime.CompactThread(context.Background(), current.ID); err != nil {
		t.Errorf("CompactThread after turn releases reservation = %v", err)
	}
}

func TestCompactThreadReservationAndRelease(t *testing.T) {
	t.Parallel()
	for _, cause := range []string{"canceled", "summary failure", "save failure"} {
		t.Run(cause, func(t *testing.T) {
			t.Parallel()
			current := compactionConversation(5)
			other := cloneConversation(current)
			other.ID = "other-thread"
			store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: current, other.ID: other}}
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			failure := errors.New("summary failure")
			var calls atomic.Int32
			runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Summarizer: func(ctx context.Context, _ string, _ []conversation.Message) (string, error) {
				if calls.Add(1) == 1 {
					close(started)
					select {
					case <-ctx.Done():
						return "", ctx.Err()
					case <-release:
					}
					if cause == "summary failure" {
						return "", failure
					}
				}
				return "SUM", nil
			}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan compactionResult, 1)
			go func() { report, err := runtime.CompactThread(ctx, current.ID); done <- compactionResult{report, err} }()
			receiveCompaction(t, started)
			if runtime.Cancel(current.ID) {
				t.Error("compaction entered public RunID cancellation namespace")
			}
			second, err := runtime.CompactThread(context.Background(), current.ID)
			if !errors.Is(err, golem.ErrRunConflict) || second != (golem.CompactionReport{}) {
				t.Errorf("second compaction = %+v, %v; want zero report and conflict", second, err)
			}
			_, err = runtime.Run(context.Background(), golem.Turn{ThreadID: current.ID, RunID: "conflicting-run", Message: "must refuse"}, func(golem.Event) error { return nil })
			if !errors.Is(err, golem.ErrRunConflict) {
				t.Errorf("Run during compaction = %v, want conflict", err)
			}
			if got, err := runtime.CompactThread(context.Background(), other.ID); err != nil || !got.Changed {
				t.Errorf("independent compaction = %+v, %v; want changed success", got, err)
			}
			// A run ID equal to the compacted thread ID remains available.
			if _, err := runtime.Run(context.Background(), golem.Turn{RunID: current.ID, Message: "independent turn"}, func(golem.Event) error { return nil }); err != nil {
				t.Errorf("independent Run = %v", err)
			}
			wantErr := failure
			if cause == "canceled" {
				wantErr = context.Canceled
				cancel()
			} else {
				if cause == "save failure" {
					store.mu.Lock()
					store.saveErr = failure
					store.mu.Unlock()
				}
				unblock()
			}
			first := receiveCompaction(t, done)
			if !errors.Is(first.err, wantErr) || first.report != (golem.CompactionReport{TokensBefore: 100, TokensAfter: 100}) {
				t.Errorf("first compaction = %+v, %v; want unchanged 100 and %v", first.report, first.err, wantErr)
			}
			store.mu.Lock()
			store.saveErr = nil
			store.mu.Unlock()
			if got, err := runtime.CompactThread(context.Background(), current.ID); err != nil || !got.Changed {
				t.Errorf("retry after reservation release = %+v, %v; want changed success", got, err)
			}
		})
	}
}

type compactionBlockingCaller struct {
	started  chan<- string
	canceled chan<- string
	release  <-chan struct{}
}

func (c compactionBlockingCaller) Chat(ctx context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (agent.ModelResult, error) {
	goal := req.Messages[len(req.Messages)-1].Content
	c.started <- goal
	<-ctx.Done()
	c.canceled <- goal
	<-c.release
	return agent.ModelResult{}, ctx.Err()
}

func TestCompactThreadCloseCancelsAndJoinsAllOperations(t *testing.T) {
	t.Parallel()
	current := compactionConversation(5)
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: current}}
	// Exactly three operations report each barrier; buffering avoids ordering
	// their acknowledgements relative to each other.
	started, canceled := make(chan string, 3), make(chan string, 3)
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Orchestrator: agent.New(compactionBlockingCaller{started: started, canceled: canceled, release: release}, agent.ContextManager{}), Summarizer: func(ctx context.Context, _ string, _ []conversation.Message) (string, error) {
		started <- "compact"
		<-ctx.Done()
		canceled <- "compact"
		<-release
		return "", ctx.Err()
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	compactDone := make(chan compactionResult, 1)
	go func() {
		report, err := runtime.CompactThread(ctx, current.ID)
		compactDone <- compactionResult{report, err}
	}()
	runsDone := make(chan error, 2) // one result for each of the two turns
	for _, turn := range []golem.Turn{{ThreadID: "other", RunID: "stateful", Message: "stateful"}, {RunID: "stateless", Message: "stateless"}} {
		go func() { _, err := runtime.Run(ctx, turn, func(golem.Event) error { return nil }); runsDone <- err }()
	}
	for range 3 {
		receiveCompaction(t, started)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close() }()
	seen := map[string]bool{}
	for range 3 {
		seen[receiveCompaction(t, canceled)] = true
	}
	if !seen["compact"] || !seen["stateful"] || !seen["stateless"] {
		t.Errorf("Close canceled %v, want all three operation types", seen)
	}
	select {
	case err := <-closeDone:
		t.Errorf("Close returned before callbacks exited: %v", err)
	default:
	}
	if _, err := runtime.CompactThread(context.Background(), "later"); !errors.Is(err, golem.ErrClosed) {
		t.Errorf("compaction during Close = %v, want ErrClosed", err)
	}
	unblock()
	if err := receiveCompaction(t, closeDone); err != nil {
		t.Errorf("Close = %v", err)
	}
	if got := receiveCompaction(t, compactDone); !errors.Is(got.err, context.Canceled) || got.report != (golem.CompactionReport{TokensBefore: 100, TokensAfter: 100}) {
		t.Errorf("closed compaction = %+v, %v; want unchanged 100 and canceled", got.report, got.err)
	}
	for range 2 {
		if err := receiveCompaction(t, runsDone); !errors.Is(err, context.Canceled) {
			t.Errorf("closed turn = %v, want canceled", err)
		}
	}
	if _, err := runtime.Run(context.Background(), golem.Turn{RunID: "later", Message: "later"}, func(golem.Event) error { return nil }); !errors.Is(err, golem.ErrClosed) {
		t.Errorf("Run after Close = %v, want ErrClosed", err)
	}
	if !reflect.DeepEqual(store.conversations[current.ID], current) || store.saves != 0 {
		t.Errorf("Close changed snapshot or saved: %+v, saves %d", store.conversations[current.ID], store.saves)
	}
}

func TestCompactThreadCloseCancelsSave(t *testing.T) {
	t.Parallel()
	current := compactionConversation(5)
	base := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: current}}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	store := compactionStoreHooks{SessionStore: base, save: func(ctx context.Context, _ conversation.Conversation) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	}}
	runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Summarizer: func(context.Context, string, []conversation.Message) (string, error) { return "SUM", nil }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan compactionResult, 1)
	go func() { report, err := runtime.CompactThread(ctx, current.ID); done <- compactionResult{report, err} }()
	receiveCompaction(t, started)
	closeDone := make(chan error, 1)
	go func() { closeDone <- runtime.Close() }()
	receiveCompaction(t, canceled)
	select {
	case err := <-closeDone:
		t.Errorf("Close returned before Save exited: %v", err)
	default:
	}
	unblock()
	if err := receiveCompaction(t, closeDone); err != nil {
		t.Errorf("Close = %v", err)
	}
	got := receiveCompaction(t, done)
	if !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, golem.ErrSessionPersistence) || got.report != (golem.CompactionReport{TokensBefore: 100, TokensAfter: 100}) || !reflect.DeepEqual(base.conversations[current.ID], current) {
		t.Errorf("canceled Save compaction = %+v, %v, snapshot %+v; want canceled persistence error and unchanged state", got.report, got.err, base.conversations[current.ID])
	}
}

type compactionStoreHooks struct {
	golem.SessionStore
	load func(context.Context, string) (*conversation.Conversation, error)
	save func(context.Context, conversation.Conversation) error
}

func (s compactionStoreHooks) Load(ctx context.Context, id string) (*conversation.Conversation, error) {
	if s.load != nil {
		return s.load(ctx, id)
	}
	return s.SessionStore.Load(ctx, id)
}

func (s compactionStoreHooks) Save(ctx context.Context, current conversation.Conversation) error {
	if s.save != nil {
		return s.save(ctx, current)
	}
	return s.SessionStore.Save(ctx, current)
}

func TestCompactThreadNoOp(t *testing.T) {
	t.Parallel()
	for exchanges := -1; exchanges <= 4; exchanges++ {
		t.Run(fmt.Sprintf("exchanges_%d", exchanges), func(t *testing.T) {
			t.Parallel()
			current := compactionConversation(max(exchanges, 0))
			store := &mapSessionStore{conversations: map[string]conversation.Conversation{}}
			if exchanges >= 0 {
				store.conversations[current.ID] = current
			}
			calls := 0
			runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Summarizer: func(context.Context, string, []conversation.Message) (string, error) { calls++; return "SUM", nil }})
			got, err := runtime.CompactThread(context.Background(), current.ID)
			wantTokens := max(exchanges, 0) * 20
			if err != nil || got != (golem.CompactionReport{TokensBefore: wantTokens, TokensAfter: wantTokens}) || calls != 0 || store.saves != 0 {
				t.Errorf("CompactThread(%d exchanges) = %+v, %v, calls %d, saves %d; want %d unchanged with no work", exchanges, got, err, calls, store.saves, wantTokens)
			}
			if exchanges < 0 {
				if len(store.conversations) != 0 {
					t.Errorf("missing thread inserted: %+v", store.conversations)
				}
			} else if !reflect.DeepEqual(store.conversations[current.ID], current) {
				t.Errorf("no-op snapshot changed: %+v", store.conversations[current.ID])
			}
		})
	}
}

func TestCompactThreadProgressiveSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                            string
		exchanges, before, after, count int
		replacement                     string
	}{
		{name: "fold pair", exchanges: 5, before: 142, after: 122, count: 14, replacement: "REVISED"},
		{name: "equal cost rewrite", exchanges: 4, before: 122, after: 122, count: 12, replacement: "REVISED"},
		{name: "larger rewrite", exchanges: 4, before: 122, after: 123, count: 12, replacement: "REPLACEMENT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := compactionConversation(tc.exchanges)
			current.DurableSummary = &conversation.DurableSummary{Content: "EARLIER", MessageCount: 12}
			store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: cloneConversation(current)}}
			calls := 0
			runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Summarizer: func(_ context.Context, prior string, old []conversation.Message) (string, error) {
				calls++
				if prior != "EARLIER" || len(old) != max(0, tc.exchanges-4)*2 || (len(old) > 0 && !reflect.DeepEqual(old, current.Messages[:2])) {
					t.Errorf("summary inputs = %q, %+v; want EARLIER and newly evicted pair only", prior, old)
				}
				return tc.replacement, nil
			}})
			got, err := runtime.CompactThread(context.Background(), current.ID)
			want := cloneConversation(current)
			want.Messages = want.Messages[max(0, tc.exchanges-4)*2:]
			want.DurableSummary = &conversation.DurableSummary{Content: tc.replacement, MessageCount: tc.count}
			if err != nil || got != (golem.CompactionReport{TokensBefore: tc.before, TokensAfter: tc.after, Changed: true}) || calls != 1 || store.saves != 1 || !reflect.DeepEqual(store.conversations[current.ID], want) {
				t.Errorf("CompactThread(%s) = %+v, %v, calls %d, saves %d, snapshot %+v; want %d -> %d changed and %+v", tc.name, got, err, calls, store.saves, store.conversations[current.ID], tc.before, tc.after, want)
			}
		})
	}
}

func TestCompactThreadFailurePreservesSnapshot(t *testing.T) {
	t.Parallel()
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name                 string
		before, calls, saves int
		wantErr              error
	}{
		{name: "load", wantErr: failure},
		{name: "nil load"},
		{name: "wrong ID"},
		{name: "summary", before: 142, calls: 1, wantErr: failure},
		{name: "blank summary", before: 142, calls: 1, wantErr: conversation.ErrEmptySummary},
		{name: "save", before: 142, calls: 1, saves: 1, wantErr: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := compactionConversation(5)
			current.DurableSummary = &conversation.DurableSummary{Content: "EARLIER", MessageCount: 12}
			base := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: cloneConversation(current)}}
			store := compactionStoreHooks{SessionStore: base}
			switch tc.name {
			case "load":
				base.loadErr = failure
			case "nil load":
				store.load = func(context.Context, string) (*conversation.Conversation, error) { return nil, nil }
			case "wrong ID":
				store.load = func(context.Context, string) (*conversation.Conversation, error) {
					return &conversation.Conversation{ID: "wrong"}, nil
				}
			case "save":
				base.saveErr = failure
			}
			calls := 0
			runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Summarizer: func(context.Context, string, []conversation.Message) (string, error) {
				calls++
				if tc.name == "summary" {
					return "", failure
				}
				if tc.name == "blank summary" {
					return " \n\t ", nil
				}
				return "SUM", nil
			}})
			got, err := runtime.CompactThread(context.Background(), current.ID)
			if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) || got != (golem.CompactionReport{TokensBefore: tc.before, TokensAfter: tc.before}) || calls != tc.calls || base.saves != tc.saves {
				t.Errorf("CompactThread(%s) = %+v, %v, calls %d, saves %d; want unchanged %d, %v, calls %d, saves %d", tc.name, got, err, calls, base.saves, tc.before, tc.wantErr, tc.calls, tc.saves)
			}
			if tc.name == "save" && !errors.Is(err, golem.ErrSessionPersistence) {
				t.Errorf("save failure = %v, want ErrSessionPersistence", err)
			}
			if !reflect.DeepEqual(base.conversations[current.ID], current) {
				t.Errorf("failure mutated snapshot: %+v", base.conversations[current.ID])
			}
		})
	}
}

func TestCompactThreadCancellationBoundaries(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"before load", "after load", "after summary", "save", "committed save"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			current := compactionConversation(5)
			base := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: cloneConversation(current)}}
			loads, calls, saves := 0, 0, 0
			store := compactionStoreHooks{SessionStore: base,
				load: func(context.Context, string) (*conversation.Conversation, error) {
					loads++
					if phase == "after load" {
						cancel()
					}
					loaded := cloneConversation(current)
					return &loaded, nil
				},
				save: func(saveCtx context.Context, candidate conversation.Conversation) error {
					saves++
					if phase == "save" {
						cancel()
						return saveCtx.Err()
					}
					// Ignore cancellation deliberately: the runtime must refuse to
					// call Save when cancellation happened before this boundary.
					if err := base.Save(context.Background(), candidate); err != nil {
						return err
					}
					if phase == "committed save" {
						cancel()
					}
					return nil
				},
			}
			runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Summarizer: func(context.Context, string, []conversation.Message) (string, error) {
				calls++
				if phase == "after summary" {
					cancel()
				}
				return "SUM", nil
			}})
			if phase == "before load" {
				cancel()
			}
			got, err := runtime.CompactThread(ctx, current.ID)
			if phase == "committed save" {
				if err != nil || got != (golem.CompactionReport{TokensBefore: 100, TokensAfter: 121, Changed: true}) || loads != 1 || calls != 1 || saves != 1 || base.saves != 1 {
					t.Errorf("committed CompactThread = %+v, %v, loads %d, calls %d, saves %d, commits %d; want changed success with one of each", got, err, loads, calls, saves, base.saves)
				}
				return
			}
			before, wantLoads, wantCalls, wantSaves := 100, 1, 1, 0
			if phase == "before load" {
				before, wantLoads, wantCalls = 0, 0, 0
			}
			if phase == "after load" {
				wantCalls = 0
			}
			if phase == "save" {
				wantSaves = 1
			}
			if !errors.Is(err, context.Canceled) || got != (golem.CompactionReport{TokensBefore: before, TokensAfter: before}) || loads != wantLoads || calls != wantCalls || saves != wantSaves || !reflect.DeepEqual(base.conversations[current.ID], current) {
				t.Errorf("CompactThread canceled %s = %+v, %v, loads %d, calls %d, saves %d; want unchanged %d, canceled, counts %d/%d/%d", phase, got, err, loads, calls, saves, before, wantLoads, wantCalls, wantSaves)
			}
		})
	}
}

func TestCompactThreadCanceledIdenticalRewrite(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := compactionConversation(4)
	current.DurableSummary = &conversation.DurableSummary{Content: "SUM", MessageCount: 2}
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: cloneConversation(current)}}
	calls := 0
	runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Summarizer: func(context.Context, string, []conversation.Message) (string, error) {
		calls++
		cancel()
		return "SUM", nil
	}})
	got, err := runtime.CompactThread(ctx, current.ID)
	if !errors.Is(err, context.Canceled) || got != (golem.CompactionReport{TokensBefore: 121, TokensAfter: 121}) || calls != 1 || store.saves != 0 || !reflect.DeepEqual(store.conversations[current.ID], current) {
		t.Errorf("canceled identical rewrite = %+v, %v, calls %d, saves %d; want 121 unchanged, canceled, one call and no save", got, err, calls, store.saves)
	}
}

func compactionConversation(exchanges int) conversation.Conversation {
	current := conversation.Conversation{ID: "compact-thread", Title: "Original title", CreatedAt: time.Unix(10, 0), UpdatedAt: time.Unix(20, 0)}
	for i := range exchanges {
		current.Messages = append(current.Messages,
			conversation.Message{Role: "user", Content: strings.Repeat(string(rune('a'+i)), 40)},
			conversation.Message{Role: "assistant", Content: strings.Repeat(string(rune('A'+i)), 40)})
	}
	return current
}

func TestCompactThreadValidationAndAvailability(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for _, tc := range []struct {
		name              string
		id                string
		disabled          bool
		missingSummarizer bool
		closed            bool
		wantErr           error
	}{
		{name: "empty ID", wantErr: golem.ErrInvalidRequest},
		{name: "oversized ID", id: strings.Repeat("x", 257), wantErr: golem.ErrInvalidRequest},
		{name: "multibyte ID exceeds bytes", id: strings.Repeat("雪", 86), wantErr: golem.ErrInvalidRequest},
		{name: "disabled with summarizer", id: "compact-thread", disabled: true, wantErr: golem.ErrCompressionUnavailable},
		{name: "missing summarizer", id: "compact-thread", missingSummarizer: true, wantErr: golem.ErrCompressionUnavailable},
		{name: "closed", id: "compact-thread", closed: true, wantErr: golem.ErrClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := compactionConversation(5)
			store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: current}}
			calls := 0
			opts := golem.Options{SessionStore: store, DisableCompression: tc.disabled}
			if !tc.missingSummarizer {
				opts.Summarizer = func(context.Context, string, []conversation.Message) (string, error) { calls++; return "SUM", nil }
			}
			runtime := newCompactionRuntime(t, opts)
			if tc.closed {
				if err := runtime.Close(); err != nil {
					t.Fatal(err)
				}
			}
			got, err := runtime.CompactThread(context.Background(), tc.id)
			if !errors.Is(err, tc.wantErr) || got != (golem.CompactionReport{}) || calls != 0 || store.loads != 0 || store.saves != 0 {
				t.Errorf("CompactThread(%s) = %+v, %v, calls %d, loads %d, saves %d; want zero report, %v, no work", tc.name, got, err, calls, store.loads, store.saves, tc.wantErr)
			}
		})
	}
}

func newCompactionRuntime(t *testing.T, opts golem.Options) *golem.Runtime {
	t.Helper()
	if opts.Root == "" {
		opts.Root = t.TempDir()
	}
	if opts.Orchestrator == nil {
		opts.Orchestrator = agent.New(&captureCaller{answer: "next answer"}, agent.ContextManager{})
	}
	runtime, err := golem.New(context.Background(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return runtime
}

func TestCompactThreadPersistsAndRepeats(t *testing.T) {
	t.Parallel()
	current := compactionConversation(5)
	store := &mapSessionStore{conversations: map[string]conversation.Conversation{current.ID: cloneConversation(current)}}
	calls := 0
	runtime := newCompactionRuntime(t, golem.Options{SessionStore: store, Summarizer: func(_ context.Context, prior string, old []conversation.Message) (string, error) {
		calls++
		if calls == 1 {
			if prior != "" || !reflect.DeepEqual(old, current.Messages[:2]) {
				t.Errorf("first summary inputs = %q, %+v; want empty prior and oldest pair %+v", prior, old, current.Messages[:2])
			}
		} else if prior != "SUM" || len(old) != 0 {
			t.Errorf("second summary inputs = %q, %+v; want SUM and no new messages", prior, old)
		}
		return "SUM", nil
	}})
	got, err := runtime.CompactThread(context.Background(), current.ID)
	if err != nil || got != (golem.CompactionReport{TokensBefore: 100, TokensAfter: 121, Changed: true}) {
		t.Fatalf("CompactThread = %+v, %v; want 100 -> 121 changed", got, err)
	}
	want := cloneConversation(current)
	want.Messages = want.Messages[2:]
	want.DurableSummary = &conversation.DurableSummary{Content: "SUM", MessageCount: 2}
	if saved := store.conversations[current.ID]; !reflect.DeepEqual(saved, want) || store.saves != 1 {
		t.Fatalf("stored compaction = %+v, saves %d; want %+v, one save", saved, store.saves, want)
	}
	got, err = runtime.CompactThread(context.Background(), current.ID)
	if err != nil || got != (golem.CompactionReport{TokensBefore: 121, TokensAfter: 121}) || calls != 2 || store.saves != 1 {
		t.Errorf("second CompactThread = %+v, %v, calls %d, saves %d; want 121 -> 121 unchanged, two calls, one save", got, err, calls, store.saves)
	}
}
