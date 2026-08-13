package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	feedbackpkg "github.com/kstruzzieri/go-llm/feedback"
	"github.com/kstruzzieri/go-llm/provider"
)

func feedbackCall(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Function: provider.ToolCallFunction{Name: name, Arguments: json.RawMessage(args)}}
}

func feedbackTestWarn(t *testing.T) func(string) {
	return func(line string) { t.Log(line) }
}

type feedbackBlockingStore struct {
	started      chan struct{}
	release      chan struct{}
	once         sync.Once
	fail         error
	recomputeErr error
	signals      atomic.Int64
	kindsMu      sync.Mutex
	kinds        map[feedbackpkg.SignalKind]int
}

type feedbackGateStore struct {
	feedbackBlockingStore
	retrievalStarted chan struct{}
	retrievalRelease chan struct{}
	retrievalOnce    sync.Once
	retrievalErr     error
}

func (s *feedbackGateStore) InsertRetrievalWithCounts(ctx context.Context, _ string, _ string, _ []string, _ time.Time) error {
	s.retrievalOnce.Do(func() { close(s.retrievalStarted) })
	select {
	case <-s.retrievalRelease:
		return s.retrievalErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *feedbackBlockingStore) InsertRetrievalWithCounts(ctx context.Context, _ string, _ string, _ []string, _ time.Time) error {
	return nil
}
func (*feedbackBlockingStore) InsertRetrieval(context.Context, string, string, []string) error {
	return nil
}
func (s *feedbackBlockingStore) InsertSignals(ctx context.Context, _ string, keys []string, kind feedbackpkg.SignalKind, _ float64, _ time.Time) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.fail != nil {
		return s.fail
	}
	s.signals.Add(int64(len(keys)))
	s.kindsMu.Lock()
	if s.kinds == nil {
		s.kinds = make(map[feedbackpkg.SignalKind]int)
	}
	s.kinds[kind] += len(keys)
	s.kindsMu.Unlock()
	return nil
}
func (*feedbackBlockingStore) InsertSignal(context.Context, string, string, feedbackpkg.SignalKind, float64, time.Time) error {
	return nil
}
func (s *feedbackBlockingStore) SignalCount(context.Context) (int, error) {
	return int(s.signals.Load()), nil
}
func (*feedbackBlockingStore) GetAggregate(context.Context, string) (feedbackpkg.Aggregate, error) {
	return feedbackpkg.Aggregate{}, nil
}
func (*feedbackBlockingStore) GetAggregatesBatch(context.Context, []string) (map[string]feedbackpkg.Aggregate, error) {
	return map[string]feedbackpkg.Aggregate{}, nil
}
func (s *feedbackBlockingStore) RecomputeAggregates(context.Context, float64) error {
	return s.recomputeErr
}
func (*feedbackBlockingStore) IncrementRetrievalCount(context.Context, []string) error { return nil }
func (*feedbackBlockingStore) PruneSignals(context.Context, time.Time) (int, error)    { return 0, nil }
func (*feedbackBlockingStore) PruneRetrievals(context.Context) (int, error)            { return 0, nil }

func feedbackServiceForStore(root string, store feedbackpkg.AtomicSignalStore, warn func(string)) *feedbackService {
	return feedbackServiceForStoreConfigured(root, store, warn, nil)
}

func feedbackServiceForStoreConfigured(root string, store feedbackpkg.AtomicSignalStore, warn func(string), configure func(*feedbackService)) *feedbackService {
	ctx, cancel := context.WithCancel(context.Background())
	s := &feedbackService{
		root: root, collector: feedbackpkg.NewManualCollector(store, feedbackpkg.DefaultConfig()),
		events: make(chan feedbackEvent, feedbackQueueSize), stop: make(chan struct{}), done: make(chan struct{}),
		cancel: cancel, now: time.Now, warn: newFeedbackNotifier(warn), report: feedbackReport{reasons: make(map[string]int)},
	}
	if configure != nil {
		configure(s)
	}
	go s.work(ctx)
	return s
}

func feedbackKindCount(store *feedbackBlockingStore, kind feedbackpkg.SignalKind) int {
	store.kindsMu.Lock()
	defer store.kindsMu.Unlock()
	return store.kinds[kind]
}

func waitFeedbackCompleted(t *testing.T, svc *feedbackService, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		svc.admissionMu.Lock()
		got := svc.report.completed
		svc.admissionMu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("completed = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFeedbackPathNormalization(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "Pkg", "file.go")
	nativeSeparatorInput, nativeSeparatorWant := `Pkg\file.go`, `Pkg\file.go`
	if runtime.GOOS == "windows" {
		nativeSeparatorWant = "Pkg/file.go"
	}
	for _, tt := range []struct {
		name, input, want string
		ok                bool
	}{
		{"relative", "Pkg/file.go", "Pkg/file.go", true},
		{"absolute equivalent", abs, "Pkg/file.go", true},
		{"clean", "Pkg/./sub/../file.go", "Pkg/file.go", true},
		{"native separator conversion", nativeSeparatorInput, nativeSeparatorWant, true},
		{"case preserved", "pkg/File.go", "pkg/File.go", true},
		{"empty", "", "", false},
		{"nul", "Pkg/\x00file.go", "", false},
		{"dot", ".", "", false},
		{"traversal", "../file.go", "", false},
		{"outside absolute", filepath.Join(filepath.Dir(root), "file.go"), "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeFeedbackPath(root, tt.input)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("normalizeFeedbackPath(%q) = %q, %v; want %q, %v", tt.input, got, ok, tt.want, tt.ok)
			}
		})
	}

	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Dir(root), alias); err != nil {
		t.Fatal(err)
	}
	if got, ok := normalizeFeedbackPath(root, "alias/file.go"); !ok || got != "alias/file.go" {
		t.Fatalf("textual symlink alias = %q, %v", got, ok)
	}
}

func TestFeedbackObserverDoesNotCreditDistinctPOSIXBackslashPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslash is a native separator on Windows")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "pkg"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{`pkg\file.go`, "pkg/file.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backslashInfo, err := os.Stat(filepath.Join(root, `pkg\file.go`))
	if err != nil {
		t.Fatal(err)
	}
	slashInfo, err := os.Stat(filepath.Join(root, "pkg/file.go"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(backslashInfo, slashInfo) {
		t.Fatal("POSIX test files unexpectedly identify the same file")
	}

	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	svc, err := openFeedbackService(context.Background(), root, dbPath, feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	observer := svc.observer("run")
	results := observer.(agent.ToolResultObserver)
	presentations := observer.(agent.RetrievalPresentationObserver)
	_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("retrieve", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = presentations.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "retrieve", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "backslash-key", Source: `pkg\file.go`}}}})
	_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("read", "read_file", `{"path":"pkg/file.go"}`), Invoked: true})
	if _, err := svc.close(); err != nil {
		t.Fatal(err)
	}
	assertFeedbackSignalCount(t, dbPath, string(feedbackpkg.SignalFileOpened), 0)
}

func TestFeedbackObserverRecordsFirstPresentationAndLaterRead(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	svc, err := openFeedbackService(context.Background(), root, dbPath, feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	obs := svc.observer("run-1")
	tro := obs.(agent.ToolResultObserver)
	rpo := obs.(agent.RetrievalPresentationObserver)

	if err := tro.OnToolResult(context.Background(), agent.ToolResultEvent{
		Step: 1, Call: feedbackCall("retrieve-1", "retrieve", `{"query":"locks"}`), Invoked: true,
		Result: agent.ToolResult{Attrib: &agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "ignored-result-copy", Source: "ignored.go"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := rpo.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{
		Step: 2, ToolCallID: "retrieve-1", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{
			{StableKey: "key-a", Source: "pkg/a.go"},
			{StableKey: "key-b", Source: "pkg/a.go"},
			{StableKey: "key-b", Source: "pkg/a.go"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// Duplicate presentation and repeated open must not duplicate positives.
	_ = rpo.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 3, ToolCallID: "retrieve-1", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "key-a", Source: "pkg/a.go"}}}})
	read := agent.ToolResultEvent{Step: 3, Call: feedbackCall("read-1", "read_file", `{"path":"pkg/a.go"}`), Invoked: true}
	_ = tro.OnToolResult(context.Background(), read)
	_ = tro.OnToolResult(context.Background(), read)

	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if report.attempted != 5 || report.completed != 5 || report.dropped != 0 {
		t.Fatalf("report = %+v", report)
	}
	if report.presentationDuplicates != 1 || report.presentationJoinMisses != 0 {
		t.Fatalf("presentation accounting = %+v", report)
	}
	_ = tro.OnToolResult(context.Background(), read)
	again, againErr := svc.close()
	if againErr != err || again.attempted != report.attempted || again.completed != report.completed || again.dropped != report.dropped {
		t.Fatalf("repeated close changed report: first=%+v second=%+v errors=%v/%v", report, again, err, againErr)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, err := db.Query(`SELECT chunk_key, signal_kind FROM feedback_signals ORDER BY chunk_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var got []string
	for rows.Next() {
		var key, kind string
		if err := rows.Scan(&key, &kind); err != nil {
			t.Fatal(err)
		}
		got = append(got, key+":"+kind)
	}
	if strings.Join(got, ",") != "key-a:file_opened,key-b:file_opened" {
		t.Fatalf("signals = %v", got)
	}
	var retrievals int
	if err := db.QueryRow(`SELECT COUNT(*) FROM feedback_retrievals`).Scan(&retrievals); err != nil || retrievals != 1 {
		t.Fatalf("retrievals=%d err=%v", retrievals, err)
	}
}

func TestFeedbackServiceCrossesProductionWeightGates(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	svc, err := openFeedbackService(context.Background(), root, dbPath, feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.writer.Exec(`INSERT INTO feedback_retrievals(retrieval_id,query,chunk_keys,created_at) VALUES('seed','q','key',1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < feedbackpkg.DefaultConfig().WarmupSignals-1; i++ {
		if _, err := svc.writer.Exec(`INSERT INTO feedback_signals(retrieval_id,chunk_key,signal_kind,strength,created_at) VALUES('seed','seed-key','file_opened',0.3,1)`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.writer.Exec(`INSERT INTO feedback_aggregates(chunk_key,retrieval_count,weighted_score) VALUES('key',?,0.7)`, feedbackpkg.DefaultConfig().MinRetrievals-1); err != nil {
		t.Fatal(err)
	}
	weights, err := svc.behavioralWeighter().WeightsBatch(context.Background(), []string{"key"})
	if err != nil || weights["key"] != 0 {
		t.Fatalf("pre-crossing weights=%v err=%v", weights, err)
	}
	o := svc.observer("run")
	_ = o.(agent.ToolResultObserver).OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = o.(agent.RetrievalPresentationObserver).OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "key", Source: "a.go"}}}})
	_ = o.(agent.ToolResultObserver).OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("f", "read_file", `{"path":"a.go"}`), Invoked: true})
	deadline := time.Now().Add(time.Second)
	for {
		var count int
		if err := svc.writer.QueryRow(`SELECT COUNT(*) FROM feedback_signals`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == feedbackpkg.DefaultConfig().WarmupSignals {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("signal count = %d", count)
		}
		time.Sleep(time.Millisecond)
	}
	weights, err = svc.behavioralWeighter().WeightsBatch(context.Background(), []string{"key"})
	if err != nil || weights["key"] == 0 {
		t.Fatalf("post-crossing weights=%v err=%v", weights, err)
	}
	_, _ = svc.close()
}

func TestFeedbackObserverFiltersAndIsolatesRuns(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	svc, err := openFeedbackService(context.Background(), root, dbPath, feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	a := svc.observer("a")
	b := svc.observer("b")
	ta := a.(agent.ToolResultObserver)
	pa := a.(agent.RetrievalPresentationObserver)
	tb := b.(agent.ToolResultObserver)

	bad := []agent.ToolResultEvent{
		{Step: 1, Call: feedbackCall("x", "retrieve", `{"query":"q"}`), Invoked: false},
		{Step: 1, Call: feedbackCall("x", "retrieve", `{"query":"q"}`), Invoked: true, Denied: true},
		{Step: 1, Call: feedbackCall("x", "retrieve", `{`), Invoked: true},
		{Step: 1, Call: feedbackCall("x", "retrieve", `{"query":"q"}`), Invoked: true, Result: agent.ToolResult{IsError: true}},
		{Step: 2, Call: feedbackCall("r", "read_file", `{"path":"pkg/a.go"}`), Invoked: true, Result: agent.ToolResult{IsError: true}},
		{Step: 2, Call: feedbackCall("r", "read_file", `{"path":"pkg/a.go"}`), Invoked: false},
		{Step: 2, Call: feedbackCall("r", "read_file", `{"path":"pkg/a.go"}`), Invoked: true, Denied: true},
		{Step: 2, Call: feedbackCall("r", "read_file", `{`), Invoked: true},
		{Step: 2, Call: feedbackCall("r", "read_file", `{"path":""}`), Invoked: true},
	}
	for _, e := range bad {
		if err := ta.OnToolResult(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	_ = ta.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 2, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = pa.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 3, ToolCallID: "missing", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "pkg/a.go"}}}})
	_ = pa.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 3, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "", Source: "pkg/a.go"}}}})
	_ = pa.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 3, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "pkg/a.go"}}}})
	_ = tb.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 4, Call: feedbackCall("read", "read_file", `{"path":"pkg/a.go"}`), Invoked: true})
	_ = ta.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 2, Call: feedbackCall("same-step", "read_file", `{"path":"pkg/a.go"}`), Invoked: true})
	svc.finishRun("a")
	_ = ta.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 5, Call: feedbackCall("after-finish", "read_file", `{"path":"pkg/a.go"}`), Invoked: true})
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if report.attempted != 5 || report.completed != 5 || report.dropped != 0 || report.presentationJoinMisses != 0 {
		t.Fatalf("filtered callbacks or run isolation report = %+v", report)
	}
	assertFeedbackSignalCount(t, dbPath, "file_opened", 0)
}

func TestFeedbackServiceFinishRunClearsPendingRetrieval(t *testing.T) {
	release := make(chan struct{})
	close(release)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: release}
	svc := feedbackServiceForStore(t.TempDir(), store, feedbackTestWarn(t))
	o := svc.observer("run")
	results := o.(agent.ToolResultObserver)
	presentations := o.(agent.RetrievalPresentationObserver)
	_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("pending", "retrieve", `{"query":"q"}`), Invoked: true})
	svc.finishRun("run")
	_ = presentations.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "pending", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("read", "read_file", `{"path":"a.go"}`), Invoked: true})
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if report.attempted != 3 || report.completed != 3 || report.dropped != 0 || report.presentationJoinMisses != 1 {
		t.Fatalf("report=%+v", report)
	}
	if got := feedbackKindCount(store, feedbackpkg.SignalFileOpened); got != 0 {
		t.Fatalf("file opened=%d, want 0", got)
	}
}

func TestFeedbackObserverCreditsEachRetrievalOncePerKey(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	svc, err := openFeedbackService(context.Background(), root, dbPath, feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	o := svc.observer("run")
	results := o.(agent.ToolResultObserver)
	presentations := o.(agent.RetrievalPresentationObserver)
	add := func(call string, step int, keys ...string) {
		_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: step, Call: feedbackCall(call, "retrieve", `{"query":"q"}`), Invoked: true})
		sources := make([]agent.RetrievedSource, len(keys))
		for i, key := range keys {
			sources[i] = agent.RetrievedSource{StableKey: key, Source: "a.go"}
		}
		_ = presentations.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: step + 1, ToolCallID: call, Attribution: agent.RetrievalAttribution{Sources: sources}})
	}
	add("first", 1, "a")
	_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("read-1", "read_file", `{"path":"a.go"}`), Invoked: true})
	add("second", 4, "a", "b")
	_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 6, Call: feedbackCall("read-2", "read_file", `{"path":"a.go"}`), Invoked: true})
	_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 7, Call: feedbackCall("read-3", "read_file", `{"path":"a.go"}`), Invoked: true})
	_, _ = svc.close()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	rows, err := db.Query(`SELECT chunk_key, COUNT(*) FROM feedback_signals WHERE signal_kind='file_opened' GROUP BY chunk_key ORDER BY chunk_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var got []string
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s:%d", key, count))
	}
	if strings.Join(got, ",") != "a:2,b:1" {
		t.Fatalf("signals=%v", got)
	}
}

func TestFeedbackServiceUsesCallbackEventTime(t *testing.T) {
	for _, tt := range []struct {
		name        string
		readAfter   time.Duration
		laterEvent  bool
		completed   int
		wantOpened  int
		wantExpired int
	}{
		{name: "less than boundary", readAfter: 299 * time.Second, completed: 3, wantOpened: 1},
		{name: "exact boundary", readAfter: 300 * time.Second, laterEvent: true, completed: 6, wantOpened: 1, wantExpired: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			releaseStore := make(chan struct{})
			close(releaseStore)
			store := &feedbackBlockingStore{started: make(chan struct{}), release: releaseStore}
			base := time.Unix(10_000, 0)
			var mu sync.Mutex
			now := base
			blocked := make(chan struct{})
			release := make(chan struct{})
			svc := feedbackServiceForStoreConfigured(root, store, feedbackTestWarn(t), func(s *feedbackService) {
				s.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
				s.workerGate = func(point string) {
					if point == "before-handle" {
						select {
						case <-blocked:
						default:
							close(blocked)
							<-release
						}
					}
				}
			})
			o := svc.observer("run")
			results := o.(agent.ToolResultObserver)
			presentations := o.(agent.RetrievalPresentationObserver)
			_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
			<-blocked
			_ = presentations.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
			mu.Lock()
			now = base.Add(tt.readAfter)
			mu.Unlock()
			_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("f", "read_file", `{"path":"a.go"}`), Invoked: true})
			if tt.laterEvent {
				_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 4, Call: feedbackCall("later", "retrieve", `{"query":"later"}`), Invoked: true})
				_ = presentations.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 5, ToolCallID: "later", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "later-key", Source: "later.go"}}}})
				mu.Lock()
				now = base.Add(301 * time.Second)
				mu.Unlock()
				_ = results.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 6, Call: feedbackCall("later-read", "read_file", `{"path":"later.go"}`), Invoked: true})
			}
			mu.Lock()
			now = base.Add(time.Hour)
			mu.Unlock()
			close(release)
			report, err := svc.close()
			if err != nil || report.attempted != tt.completed || report.completed != tt.completed || report.dropped != 0 {
				t.Fatalf("close = %+v, %v", report, err)
			}
			if got := feedbackKindCount(store, feedbackpkg.SignalFileOpened); got != tt.wantOpened {
				t.Fatalf("file opened = %d, want %d", got, tt.wantOpened)
			}
			if got := feedbackKindCount(store, feedbackpkg.SignalWindowExpired); got != tt.wantExpired {
				t.Fatalf("expired = %d, want %d", got, tt.wantExpired)
			}
		})
	}
}

func assertFeedbackSignalCount(t *testing.T, dbPath, kind string, want int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	var got int
	if err := db.QueryRow(`SELECT COUNT(*) FROM feedback_signals WHERE signal_kind = ?`, kind).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s signals = %d, want %d", kind, got, want)
	}
}

func TestFeedbackServiceOverflowDisablesAndAccountsQueuedEvents(t *testing.T) {
	store := &feedbackBlockingStore{started: make(chan struct{}), release: make(chan struct{})}
	svc := feedbackServiceForStore(t.TempDir(), store, feedbackTestWarn(t))
	observer := svc.observer("run")
	obs := observer.(agent.ToolResultObserver)
	present := observer.(agent.RetrievalPresentationObserver)
	emitRetrieve := func(id string) {
		if err := obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall(id, "retrieve", `{"query":"q"}`), Invoked: true}); err != nil {
			t.Fatal(err)
		}
	}
	emitRetrieve("setup")
	if err := present.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "setup", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		svc.admissionMu.Lock()
		completed := svc.report.completed
		svc.admissionMu.Unlock()
		if completed == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("setup did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	svc.admissionMu.Lock()
	svc.report = feedbackReport{reasons: make(map[string]int)}
	svc.admissionMu.Unlock()
	if err := obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("in-flight", "read_file", `{"path":"a.go"}`), Invoked: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("first persistence did not start")
	}
	for i := 0; i < feedbackQueueSize; i++ {
		emitRetrieve(fmt.Sprintf("queued-%d", i))
	}
	returned := make(chan struct{})
	go func() { emitRetrieve("overflow"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("overflow callback blocked")
	}
	close(store.release)
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if report.attempted != 130 || report.completed != 1 || report.dropped != 129 {
		t.Fatalf("report = %+v", report)
	}
	if report.attempted != report.completed+report.dropped {
		t.Fatalf("attempted invariant: %+v", report)
	}
	if report.reasons[dropOverflowNewest] != 1 || report.reasons[dropQueuedAfterDisable] != 128 {
		t.Fatalf("reasons = %v", report.reasons)
	}
}

func TestFeedbackServiceCountedOverflowWarnsOnceWithoutBlockingCallback(t *testing.T) {
	releaseStore := make(chan struct{})
	close(releaseStore)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: releaseStore}
	workerBlocked := make(chan struct{})
	releaseWorker := make(chan struct{})
	warnings := make(chan string, 2)
	var gateOnce sync.Once
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, func(line string) { warnings <- line }, func(s *feedbackService) {
		s.ticks = make(chan time.Time)
		s.workerGate = func(point string) {
			if point == "before-select" {
				gateOnce.Do(func() { close(workerBlocked); <-releaseWorker })
			}
		}
	})
	defer func() { _, _ = svc.close() }()
	<-workerBlocked
	obs := svc.observer("run").(agent.ToolResultObserver)
	emit := func(id string) {
		_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall(id, "retrieve", `{"query":"q"}`), Invoked: true})
	}
	for i := 0; i < feedbackQueueSize; i++ {
		emit(fmt.Sprintf("queued-%d", i))
	}
	returned := make(chan struct{})
	go func() { emit("overflow"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("overflow callback blocked")
	}
	select {
	case warning := <-warnings:
		t.Fatalf("warning ran on callback path: %q", warning)
	default:
	}
	close(releaseWorker)
	select {
	case warning := <-warnings:
		if !strings.Contains(warning, "overflow") {
			t.Fatalf("warning=%q", warning)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not deliver overflow warning")
	}
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if report.attempted != 129 || report.completed != 0 || report.dropped != 129 || report.reasons[dropOverflowNewest] != 1 || report.reasons[dropQueuedAfterDisable] != 128 {
		t.Fatalf("report=%+v", report)
	}
	select {
	case warning := <-warnings:
		t.Fatalf("extra warning=%q", warning)
	default:
	}
}

// Catches removing the disabled-branch overflow warning recheck after the
// worker's initial warning probe.
func TestFeedbackServiceOverflowAfterWarningProbeWarnsOnce(t *testing.T) {
	releaseStore := make(chan struct{})
	close(releaseStore)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: releaseStore}
	afterProbe := make(chan struct{})
	releaseWorker := make(chan struct{})
	warnings := make(chan string, 2)
	var gateOnce sync.Once
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, func(line string) { warnings <- line }, func(s *feedbackService) {
		s.ticks = make(chan time.Time)
		s.workerGate = func(point string) {
			if point == "after-overflow-warning-probe" {
				gateOnce.Do(func() { close(afterProbe); <-releaseWorker })
			}
		}
	})
	defer func() { _, _ = svc.close() }()
	select {
	case <-afterProbe:
	case <-time.After(time.Second):
		t.Fatal("worker did not reach overflow warning/disable boundary")
	}
	obs := svc.observer("run").(agent.ToolResultObserver)
	emit := func(id string) {
		_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall(id, "retrieve", `{"query":"q"}`), Invoked: true})
	}
	for i := 0; i < feedbackQueueSize; i++ {
		emit(fmt.Sprintf("queued-%d", i))
	}
	returned := make(chan struct{})
	go func() { emit("overflow"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("overflow callback blocked")
	}
	select {
	case warning := <-warnings:
		t.Fatalf("warning ran on callback path: %q", warning)
	default:
	}
	close(releaseWorker)
	select {
	case warning := <-warnings:
		if !strings.Contains(warning, "overflow") {
			t.Fatalf("warning=%q", warning)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not deliver overflow warning")
	}
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if report.attempted != 129 || report.completed != 0 || report.dropped != 129 || report.reasons[dropOverflowNewest] != 1 || report.reasons[dropQueuedAfterDisable] != 128 {
		t.Fatalf("report=%+v", report)
	}
	select {
	case warning := <-warnings:
		t.Fatalf("extra warning=%q", warning)
	default:
	}
}

// Catches removing the timeout-branch overflow warning recheck after the
// worker's initial warning probe.
func TestFeedbackServiceOverflowBeforeTimeoutWarnsOnce(t *testing.T) {
	releaseStore := make(chan struct{})
	close(releaseStore)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: releaseStore}
	afterProbe := make(chan struct{})
	releaseWorker := make(chan struct{})
	cancelSeen := make(chan struct{})
	warnings := make(chan string, 2)
	var gateOnce, cancelOnce sync.Once
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, func(line string) { warnings <- line }, func(s *feedbackService) {
		s.ticks = make(chan time.Time)
		s.closeTimeout = 20 * time.Millisecond
		originalCancel := s.cancel
		s.cancel = func() {
			cancelOnce.Do(func() { close(cancelSeen) })
			originalCancel()
		}
		s.workerGate = func(point string) {
			if point == "after-overflow-warning-probe" {
				gateOnce.Do(func() { close(afterProbe); <-releaseWorker })
			}
		}
	})
	released := false
	defer func() {
		if !released {
			close(releaseWorker)
		}
		_, _ = svc.close()
	}()
	<-afterProbe
	obs := svc.observer("run").(agent.ToolResultObserver)
	emit := func(id string) {
		_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall(id, "retrieve", `{"query":"q"}`), Invoked: true})
	}
	for i := 0; i < feedbackQueueSize; i++ {
		emit(fmt.Sprintf("queued-%d", i))
	}
	returned := make(chan struct{})
	go func() { emit("overflow"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("overflow callback blocked")
	}
	select {
	case warning := <-warnings:
		t.Fatalf("warning ran on callback path: %q", warning)
	default:
	}
	type closeResult struct {
		report feedbackReport
		err    error
	}
	closed := make(chan closeResult, 1)
	go func() {
		report, err := svc.close()
		closed <- closeResult{report: report, err: err}
	}()
	<-cancelSeen
	close(releaseWorker)
	released = true
	result := <-closed
	if result.err != nil {
		t.Fatal(result.err)
	}
	select {
	case warning := <-warnings:
		if !strings.Contains(warning, "overflow") {
			t.Fatalf("warning=%q", warning)
		}
	default:
		t.Fatal("worker did not deliver overflow warning")
	}
	if result.report.attempted != 129 || result.report.completed != 0 || result.report.dropped != 129 || result.report.reasons[dropOverflowNewest] != 1 || result.report.reasons[dropQueuedAfterTimeout] != 128 {
		t.Fatalf("report=%+v", result.report)
	}
	select {
	case warning := <-warnings:
		t.Fatalf("extra warning=%q", warning)
	default:
	}
}

func TestFeedbackServiceCleanupOverflowWarnsOnceWithoutBlockingFinishRun(t *testing.T) {
	releaseStore := make(chan struct{})
	close(releaseStore)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: releaseStore}
	workerBlocked := make(chan struct{})
	releaseWorker := make(chan struct{})
	warnings := make(chan string, 2)
	var gateOnce sync.Once
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, func(line string) { warnings <- line }, func(s *feedbackService) {
		s.ticks = make(chan time.Time)
		s.workerGate = func(point string) {
			if point == "before-select" {
				gateOnce.Do(func() { close(workerBlocked); <-releaseWorker })
			}
		}
	})
	defer func() { _, _ = svc.close() }()
	<-workerBlocked
	obs := svc.observer("run").(agent.ToolResultObserver)
	for i := 0; i < feedbackQueueSize; i++ {
		_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall(fmt.Sprintf("queued-%d", i), "retrieve", `{"query":"q"}`), Invoked: true})
	}
	returned := make(chan struct{})
	go func() { svc.finishRun("run"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("finishRun blocked on cleanup overflow")
	}
	select {
	case warning := <-warnings:
		t.Fatalf("warning ran on finishRun path: %q", warning)
	default:
	}
	close(releaseWorker)
	select {
	case warning := <-warnings:
		if !strings.Contains(warning, "overflow") {
			t.Fatalf("warning=%q", warning)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not deliver cleanup overflow warning")
	}
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if report.attempted != 128 || report.completed != 0 || report.dropped != 128 || report.reasons[dropOverflowNewest] != 0 || report.reasons[dropQueuedAfterDisable] != 128 {
		t.Fatalf("report=%+v", report)
	}
	select {
	case warning := <-warnings:
		t.Fatalf("extra warning=%q", warning)
	default:
	}
}

func TestFeedbackServiceOverflowBetweenReceiveAndHandleAbandonsDequeuedEvent(t *testing.T) {
	release := make(chan struct{})
	close(release)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: release}
	dequeued := make(chan struct{})
	continueDequeue := make(chan struct{})
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, feedbackTestWarn(t), func(s *feedbackService) {
		s.workerGate = func(point string) {
			if point == "after-dequeue" {
				select {
				case <-dequeued:
				default:
					close(dequeued)
					<-continueDequeue
				}
			}
		}
	})
	obs := svc.observer("run").(agent.ToolResultObserver)
	emit := func(id string) {
		_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall(id, "retrieve", `{"query":"q"}`), Invoked: true})
	}
	emit("dequeued")
	<-dequeued
	for i := 0; i < feedbackQueueSize; i++ {
		emit(fmt.Sprintf("queued-%d", i))
	}
	emit("overflow")
	close(continueDequeue)
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if report.attempted != 130 || report.completed != 0 || report.dropped != 130 || report.reasons[dropOverflowNewest] != 1 || report.reasons[dropQueuedAfterDisable] != 129 {
		t.Fatalf("report=%+v", report)
	}
}

func TestFeedbackServiceWriteFailureDisablesAndWarnsOnce(t *testing.T) {
	store := &feedbackBlockingStore{started: make(chan struct{}), release: make(chan struct{}), fail: errors.New("write failed")}
	var warnings atomic.Int64
	svc := feedbackServiceForStore(t.TempDir(), store, func(string) { warnings.Add(1) })
	observer := svc.observer("run")
	obs := observer.(agent.ToolResultObserver)
	present := observer.(agent.RetrievalPresentationObserver)
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("setup", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = present.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "setup", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	deadline := time.Now().Add(time.Second)
	for {
		svc.admissionMu.Lock()
		completed := svc.report.completed
		svc.admissionMu.Unlock()
		if completed == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("setup did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	svc.admissionMu.Lock()
	svc.report = feedbackReport{reasons: make(map[string]int)}
	svc.admissionMu.Unlock()
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("first", "read_file", `{"path":"a.go"}`), Invoked: true})
	<-store.started
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("queued", "retrieve", `{"query":"q"}`), Invoked: true})
	close(store.release)
	for !svc.disabledNow.Load() {
		time.Sleep(time.Millisecond)
	}
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("disabled", "retrieve", `{"query":"q"}`), Invoked: true})
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if warnings.Load() != 1 || report.attempted != 3 || report.completed != 0 || report.dropped != 3 {
		t.Fatalf("warnings=%d report=%+v", warnings.Load(), report)
	}
}

func TestFeedbackServiceOperationDeadline(t *testing.T) {
	store := &feedbackBlockingStore{started: make(chan struct{}), release: make(chan struct{})}
	svc := feedbackServiceForStore(t.TempDir(), store, feedbackTestWarn(t))
	o := svc.observer("run")
	obs := o.(agent.ToolResultObserver)
	present := o.(agent.RetrievalPresentationObserver)
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("setup", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = present.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "setup", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	deadline := time.Now().Add(time.Second)
	for {
		svc.admissionMu.Lock()
		completed := svc.report.completed
		svc.admissionMu.Unlock()
		if completed == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("setup did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	svc.admissionMu.Lock()
	svc.report = feedbackReport{reasons: make(map[string]int)}
	svc.admissionMu.Unlock()
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("read", "read_file", `{"path":"a.go"}`), Invoked: true})
	<-store.started
	started := time.Now()
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("deadline elapsed=%v", elapsed)
	}
	if report.attempted != 1 || report.completed != 0 || report.dropped != 1 {
		t.Fatalf("report=%+v", report)
	}
}

func TestFeedbackServiceCloseTimeoutDropsEveryQueuedEvent(t *testing.T) {
	store := &feedbackGateStore{
		feedbackBlockingStore: feedbackBlockingStore{started: make(chan struct{}), release: make(chan struct{})},
		retrievalStarted:      make(chan struct{}), retrievalRelease: make(chan struct{}), retrievalErr: context.Canceled,
	}
	cancelSeen := make(chan struct{})
	var cancelOnce sync.Once
	var readerCloses, writerCloses atomic.Int64
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, feedbackTestWarn(t), func(s *feedbackService) {
		s.closeTimeout = 20 * time.Millisecond
		originalCancel := s.cancel
		s.cancel = func() {
			cancelOnce.Do(func() { close(cancelSeen); close(store.retrievalRelease) })
			originalCancel()
		}
		s.closeReader = func() error {
			select {
			case <-cancelSeen:
			default:
				t.Error("reader closed before cancellation")
			}
			readerCloses.Add(1)
			return nil
		}
		s.closeWriter = func() error {
			select {
			case <-cancelSeen:
			default:
				t.Error("writer closed before cancellation")
			}
			writerCloses.Add(1)
			return nil
		}
	})
	o := svc.observer("run")
	obs := o.(agent.ToolResultObserver)
	present := o.(agent.RetrievalPresentationObserver)
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("blocked", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = present.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "blocked", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	<-store.retrievalStarted
	for i := 0; i < 3; i++ {
		_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall(fmt.Sprintf("queued-%d", i), "retrieve", `{"query":"q"}`), Invoked: true})
	}
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if report.attempted != 5 || report.completed != 1 || report.dropped != 4 || report.reasons[dropOperationFailed] != 1 || report.reasons[dropQueuedAfterTimeout] != 3 {
		t.Fatalf("report=%+v", report)
	}
	_, _ = svc.close()
	if readerCloses.Load() != 1 || writerCloses.Load() != 1 {
		t.Fatalf("reader closes=%d writer closes=%d", readerCloses.Load(), writerCloses.Load())
	}
}

func TestFeedbackServiceCloseTimeoutAfterStopPrecheckDropsDequeuedEvent(t *testing.T) {
	releaseStore := make(chan struct{})
	close(releaseStore)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: releaseStore}
	beforeSelect := make(chan struct{})
	releaseSelect := make(chan struct{})
	afterPrecheck := make(chan struct{})
	releasePrecheck := make(chan struct{})
	cancelSeen := make(chan struct{})
	var selectOnce, precheckOnce, cancelOnce sync.Once
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, feedbackTestWarn(t), func(s *feedbackService) {
		s.ticks = make(chan time.Time)
		s.closeTimeout = 20 * time.Millisecond
		originalCancel := s.cancel
		s.cancel = func() {
			cancelOnce.Do(func() { close(cancelSeen) })
			originalCancel()
		}
		s.workerGate = func(point string) {
			switch point {
			case "before-select":
				selectOnce.Do(func() { close(beforeSelect); <-releaseSelect })
			case "after-stop-precheck":
				precheckOnce.Do(func() { close(afterPrecheck); <-releasePrecheck })
			}
		}
	})
	<-beforeSelect
	obs := svc.observer("run").(agent.ToolResultObserver)
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("queued", "retrieve", `{"query":"q"}`), Invoked: true})
	queued := <-svc.events
	type closeResult struct {
		report feedbackReport
		err    error
	}
	closed := make(chan closeResult, 1)
	go func() {
		report, err := svc.close()
		closed <- closeResult{report: report, err: err}
	}()
	for {
		svc.admissionMu.Lock()
		stopped := svc.stopped
		svc.admissionMu.Unlock()
		if stopped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseSelect)
	<-afterPrecheck
	svc.events <- queued
	<-cancelSeen
	close(releasePrecheck)
	result := <-closed
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.report.attempted != 1 || result.report.completed != 0 || result.report.dropped != 1 || result.report.reasons[dropQueuedAfterTimeout] != 1 {
		t.Fatalf("report=%+v", result.report)
	}
}

func TestFeedbackServiceCloseTimeoutAfterStopPrecheckSkipsFinalSweep(t *testing.T) {
	store := &feedbackBlockingStore{started: make(chan struct{}), release: make(chan struct{})}
	base := time.Unix(50_000, 0)
	var nowUnix atomic.Int64
	nowUnix.Store(base.Unix())
	beforeSweep := make(chan struct{})
	releaseSweep := make(chan struct{})
	cancelSeen := make(chan struct{})
	var sweepOnce, cancelOnce sync.Once
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, feedbackTestWarn(t), func(s *feedbackService) {
		s.ticks = make(chan time.Time)
		s.now = func() time.Time { return time.Unix(nowUnix.Load(), 0) }
		s.closeTimeout = 20 * time.Millisecond
		originalCancel := s.cancel
		s.cancel = func() {
			cancelOnce.Do(func() { close(cancelSeen) })
			originalCancel()
		}
		s.workerGate = func(point string) {
			if point == "before-final-sweep" {
				sweepOnce.Do(func() { close(beforeSweep); <-releaseSweep })
			}
		}
	})
	o := svc.observer("run")
	_ = o.(agent.ToolResultObserver).OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = o.(agent.RetrievalPresentationObserver).OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	waitFeedbackCompleted(t, svc, 2)
	nowUnix.Store(base.Add(300 * time.Second).Unix())
	type closeResult struct {
		report feedbackReport
		err    error
	}
	closed := make(chan closeResult, 1)
	go func() {
		report, err := svc.close()
		closed <- closeResult{report: report, err: err}
	}()
	<-beforeSweep
	<-cancelSeen
	close(releaseSweep)
	result := <-closed
	select {
	case <-store.started:
		t.Fatalf("final sweep started persistence after close timeout: %v", result.err)
	default:
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.report.attempted != 2 || result.report.completed != 2 || result.report.dropped != 0 {
		t.Fatalf("report=%+v", result.report)
	}
}

func TestFeedbackServiceCloseSkipsTickerAfterStopBoundary(t *testing.T) {
	root := t.TempDir()
	releaseStore := make(chan struct{})
	close(releaseStore)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: releaseStore}
	base := time.Unix(30_000, 0)
	ticks := make(chan time.Time, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	var nowMu sync.Mutex
	now := base
	svc := feedbackServiceForStoreConfigured(root, store, feedbackTestWarn(t), func(s *feedbackService) {
		s.ticks = ticks
		s.now = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
		s.workerGate = func(point string) {
			if point == "after-tick" {
				close(entered)
				<-release
			}
		}
	})
	o := svc.observer("run")
	_ = o.(agent.ToolResultObserver).OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = o.(agent.RetrievalPresentationObserver).OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	waitFeedbackCompleted(t, svc, 2)
	ticks <- base.Add(time.Hour)
	<-entered
	nowMu.Lock()
	now = base.Add(299 * time.Second)
	nowMu.Unlock()
	closed := make(chan feedbackReport, 1)
	go func() { report, _ := svc.close(); closed <- report }()
	for {
		svc.admissionMu.Lock()
		stopped := svc.stopped
		svc.admissionMu.Unlock()
		if stopped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	nowMu.Lock()
	now = base.Add(time.Hour)
	nowMu.Unlock()
	close(release)
	<-closed
	if got := feedbackKindCount(store, feedbackpkg.SignalWindowExpired); got != 0 {
		t.Fatalf("expired = %d, want 0", got)
	}
}

func TestFeedbackServiceTickerCapturesSweepTimeBeforeClose(t *testing.T) {
	releaseStore := make(chan struct{})
	close(releaseStore)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: releaseStore}
	base := time.Unix(40_000, 0)
	ticks := make(chan time.Time, 1)
	entered := make(chan struct{})
	release := make(chan struct{})
	var nowMu sync.Mutex
	now := base
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, feedbackTestWarn(t), func(s *feedbackService) {
		s.ticks = ticks
		s.now = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
		s.workerGate = func(point string) {
			if point == "before-tick-sweep" {
				close(entered)
				<-release
			}
		}
	})
	o := svc.observer("run")
	_ = o.(agent.ToolResultObserver).OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = o.(agent.RetrievalPresentationObserver).OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	waitFeedbackCompleted(t, svc, 2)
	nowMu.Lock()
	now = base.Add(299 * time.Second)
	nowMu.Unlock()
	ticks <- base.Add(299 * time.Second)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not capture ticker time")
	}
	closed := make(chan feedbackReport, 1)
	go func() { report, _ := svc.close(); closed <- report }()
	for {
		svc.admissionMu.Lock()
		stopped := svc.stopped
		svc.admissionMu.Unlock()
		if stopped {
			break
		}
		time.Sleep(time.Millisecond)
	}
	nowMu.Lock()
	now = base.Add(time.Hour)
	nowMu.Unlock()
	close(release)
	<-closed
	if got := feedbackKindCount(store, feedbackpkg.SignalWindowExpired); got != 0 {
		t.Fatalf("expired = %d, want 0", got)
	}
}

func TestFeedbackServiceCommittedMaintenanceFailureCompletesThenDisables(t *testing.T) {
	release := make(chan struct{})
	close(release)
	store := &feedbackBlockingStore{started: make(chan struct{}), release: release, recomputeErr: errors.New("recompute failed")}
	var warnings atomic.Int64
	svc := feedbackServiceForStore(t.TempDir(), store, func(string) { warnings.Add(1) })
	o := svc.observer("run")
	obs := o.(agent.ToolResultObserver)
	present := o.(agent.RetrievalPresentationObserver)
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("setup", "retrieve", `{"query":"q"}`), Invoked: true})
	sources := make([]agent.RetrievedSource, 100)
	for i := range sources {
		sources[i] = agent.RetrievedSource{StableKey: fmt.Sprintf("k-%d", i), Source: "a.go"}
	}
	_ = present.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "setup", Attribution: agent.RetrievalAttribution{Sources: sources}})
	deadline := time.Now().Add(time.Second)
	for {
		svc.admissionMu.Lock()
		completed := svc.report.completed
		svc.admissionMu.Unlock()
		if completed == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("setup did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	svc.admissionMu.Lock()
	svc.report = feedbackReport{reasons: make(map[string]int)}
	svc.admissionMu.Unlock()
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("read", "read_file", `{"path":"a.go"}`), Invoked: true})
	for !svc.disabledNow.Load() {
		time.Sleep(time.Millisecond)
	}
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 4, Call: feedbackCall("later", "retrieve", `{"query":"q"}`), Invoked: true})
	report, err := svc.close()
	if err != nil {
		t.Fatal(err)
	}
	if warnings.Load() != 1 || report.attempted != 2 || report.completed != 1 || report.dropped != 1 || store.signals.Load() != 100 {
		t.Fatalf("warnings=%d signals=%d report=%+v", warnings.Load(), store.signals.Load(), report)
	}
}

func TestFeedbackServiceDisabledRecordingKeepsWeightsReadable(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	svc, err := openFeedbackService(context.Background(), root, dbPath, feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.writer.Exec(`INSERT INTO feedback_retrievals(retrieval_id,query,chunk_keys,created_at) VALUES('seed','q','key',1)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < feedbackpkg.DefaultConfig().WarmupSignals; i++ {
		if _, err := svc.writer.Exec(`INSERT INTO feedback_signals(retrieval_id,chunk_key,signal_kind,strength,created_at) VALUES('seed','key','file_opened',0.3,1)`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.writer.Exec(`INSERT INTO feedback_aggregates(chunk_key,retrieval_count,weighted_score) VALUES('key',5,0.7)`); err != nil {
		t.Fatal(err)
	}
	o := svc.observer("run")
	obs := o.(agent.ToolResultObserver)
	present := o.(agent.RetrievalPresentationObserver)
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = present.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "key", Source: "a.go"}}}})
	deadline := time.Now().Add(time.Second)
	for {
		svc.admissionMu.Lock()
		completed := svc.report.completed
		svc.admissionMu.Unlock()
		if completed == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("setup did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	if err := svc.writer.Close(); err != nil {
		t.Fatal(err)
	}
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("read", "read_file", `{"path":"a.go"}`), Invoked: true})
	for !svc.disabledNow.Load() {
		time.Sleep(time.Millisecond)
	}
	weights, err := svc.behavioralWeighter().WeightsBatch(context.Background(), []string{"key"})
	if err != nil || weights["key"] != 0.7 {
		t.Fatalf("weights=%v err=%v", weights, err)
	}
	_, _ = svc.close()
}

func TestFeedbackServiceConcurrentAdmissionAndClose(t *testing.T) {
	svc, err := openFeedbackService(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "feedback.db"), feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	obs := svc.observer("run").(agent.ToolResultObserver)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall(fmt.Sprintf("r-%d", i), "retrieve", `{"query":"q"}`), Invoked: true})
		}(i)
	}
	close(start)
	report, closeErr := svc.close()
	wg.Wait()
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if report.attempted != report.completed+report.dropped {
		t.Fatalf("report=%+v", report)
	}
	frozen, _ := svc.close()
	if frozen.attempted != report.attempted || frozen.completed != report.completed || frozen.dropped != report.dropped {
		t.Fatalf("report changed: %+v -> %+v", report, frozen)
	}
}

func TestFeedbackServiceAdmissionStopBoundary(t *testing.T) {
	svc, err := openFeedbackService(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "feedback.db"), feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	cancelSeen := make(chan struct{})
	var cancelOnce sync.Once
	originalCancel := svc.cancel
	svc.cancel = func() {
		cancelOnce.Do(func() { close(cancelSeen) })
		originalCancel()
	}
	var readerCloses, writerCloses atomic.Int64
	readerClosing := make(chan struct{})
	releaseReader := make(chan struct{})
	originalCloseReader, originalCloseWriter := svc.closeReader, svc.closeWriter
	svc.closeReader = func() error {
		select {
		case <-cancelSeen:
		default:
			t.Error("reader closed before cancellation")
		}
		readerCloses.Add(1)
		close(readerClosing)
		<-releaseReader
		return originalCloseReader()
	}
	svc.closeWriter = func() error {
		select {
		case <-cancelSeen:
		default:
			t.Error("writer closed before cancellation")
		}
		writerCloses.Add(1)
		return originalCloseWriter()
	}
	obs := svc.observer("run").(agent.ToolResultObserver)
	inside := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	svc.admissionGate = func() { once.Do(func() { close(inside); <-release }) }
	firstDone := make(chan struct{})
	go func() {
		_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("before", "retrieve", `{"query":"q"}`), Invoked: true})
		close(firstDone)
	}()
	<-inside
	closeDone := make(chan feedbackReport, 1)
	go func() { report, _ := svc.close(); closeDone <- report }()
	close(release)
	<-firstDone
	<-readerClosing
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("after", "retrieve", `{"query":"q"}`), Invoked: true})
	close(releaseReader)
	report := <-closeDone
	frozen, _ := svc.close()
	if report.attempted != 1 || report.completed != 1 || report.dropped != 0 || frozen.attempted != 1 || frozen.completed != 1 || frozen.dropped != 0 || len(svc.events) != 0 {
		t.Fatalf("report=%+v frozen=%+v queued=%d", report, frozen, len(svc.events))
	}
	if readerCloses.Load() != 1 || writerCloses.Load() != 1 {
		t.Fatalf("reader closes=%d writer closes=%d", readerCloses.Load(), writerCloses.Load())
	}
}

func TestFeedbackServiceCloseDiscardsUnexpiredWindow(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	svc, err := openFeedbackService(context.Background(), root, dbPath, feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(20_000, 0)
	svc.now = func() time.Time { return base }
	o := svc.observer("run")
	_ = o.(agent.ToolResultObserver).OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = o.(agent.RetrievalPresentationObserver).OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	svc.now = func() time.Time { return base.Add(299 * time.Second) }
	_, _ = svc.close()
	assertFeedbackSignalCount(t, dbPath, "window_expired", 0)
}

func TestFeedbackNotifierSwitchIsSynchronized(t *testing.T) {
	store := &feedbackGateStore{
		feedbackBlockingStore: feedbackBlockingStore{started: make(chan struct{}), release: make(chan struct{}), fail: errors.New("write failed")},
		retrievalStarted:      make(chan struct{}), retrievalRelease: make(chan struct{}),
	}
	close(store.retrievalRelease)
	var fallback atomic.Int64
	warningReady := make(chan struct{})
	releaseWarning := make(chan struct{})
	svc := feedbackServiceForStoreConfigured(t.TempDir(), store, func(string) { fallback.Add(1) }, func(s *feedbackService) {
		s.workerGate = func(point string) {
			if point == "before-warning" {
				close(warningReady)
				<-releaseWarning
			}
		}
	})
	var promptOut, promptErr strings.Builder
	ctrl := newReplControl(&promptOut, &promptErr, make(chan struct{}, 1), func() {})
	ctrl.enterTurn()
	observer := svc.observer("run")
	obs := observer.(agent.ToolResultObserver)
	present := observer.(agent.RetrievalPresentationObserver)
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = present.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	waitFeedbackCompleted(t, svc, 2)
	_ = obs.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("read", "read_file", `{"path":"a.go"}`), Invoked: true})
	<-store.started
	close(store.release)
	<-warningReady
	svc.warn.set(ctrl.notice)
	close(releaseWarning)
	for !svc.disabledNow.Load() {
		time.Sleep(time.Millisecond)
	}
	_, _ = svc.close()
	if fallback.Load() != 0 || strings.Count(promptErr.String(), "behavioral feedback recording disabled") != 1 {
		t.Fatalf("fallback=%d prompt stderr=%q", fallback.Load(), promptErr.String())
	}
}

func TestFeedbackConfiguredFalseDoesNothing(t *testing.T) {
	dir := t.TempDir()
	paths, opens := 0, 0
	getenv := func(string) string { paths++; return dir }
	opener := func(context.Context, string, string, func(string)) (*feedbackService, error) {
		opens++
		return nil, nil
	}
	svc, warning := openConfiguredFeedback(context.Background(), false, dir, "", getenv, feedbackTestWarn(t), opener)
	if svc != nil || warning != "" || paths != 0 || opens != 0 {
		t.Fatalf("svc=%v warning=%q path calls=%d open calls=%d", svc, warning, paths, opens)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("false gate touched filesystem: %v, %v", entries, err)
	}
}

func TestFeedbackObserverInterfaces(t *testing.T) {
	var _ agent.ToolResultObserver = (*feedbackObserver)(nil)
	var _ agent.RetrievalPresentationObserver = (*feedbackObserver)(nil)
}

func TestFeedbackDBPathForWorkspace(t *testing.T) {
	base := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return base
		}
		return ""
	}
	root := t.TempDir()
	p, err := feedbackDBPathForWorkspace(getenv, root)
	if err != nil {
		t.Fatalf("feedbackDBPathForWorkspace: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(p), "golem/retrieval-feedback/") || !strings.HasSuffix(p, ".db") {
		t.Errorf("unexpected path %q", p)
	}
}

func TestFeedbackDBPathForWorkspaceRejectsInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return root
		}
		return ""
	}
	if _, err := feedbackDBPathForWorkspace(getenv, root); err == nil {
		t.Fatalf("expected path inside workspace to be rejected")
	}
}
