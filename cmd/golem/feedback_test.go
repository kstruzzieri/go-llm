package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
}

func (s *feedbackBlockingStore) InsertRetrievalWithCounts(ctx context.Context, _ string, _ string, _ []string, _ time.Time) error {
	return nil
}
func (*feedbackBlockingStore) InsertRetrieval(context.Context, string, string, []string) error {
	return nil
}
func (s *feedbackBlockingStore) InsertSignals(ctx context.Context, _ string, keys []string, _ feedbackpkg.SignalKind, _ float64, _ time.Time) error {
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
	ctx, cancel := context.WithCancel(context.Background())
	s := &feedbackService{
		root: root, collector: feedbackpkg.NewManualCollector(store, feedbackpkg.DefaultConfig()),
		events: make(chan feedbackEvent, feedbackQueueSize), stop: make(chan struct{}), done: make(chan struct{}),
		cancel: cancel, now: time.Now, warn: newFeedbackNotifier(warn), report: feedbackReport{reasons: make(map[string]int)},
	}
	go s.work(ctx)
	return s
}

func TestFeedbackPathNormalization(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "Pkg", "file.go")
	for _, tt := range []struct {
		name, input, want string
		ok                bool
	}{
		{"relative", "Pkg/file.go", "Pkg/file.go", true},
		{"absolute equivalent", abs, "Pkg/file.go", true},
		{"clean", "Pkg/./sub/../file.go", "Pkg/file.go", true},
		{"slash conversion", `Pkg\\file.go`, "Pkg/file.go", true},
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
	defer db.Close()
	rows, err := db.Query(`SELECT chunk_key, signal_kind FROM feedback_signals ORDER BY chunk_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
	if report.attempted != 6 || report.completed != 6 || report.dropped != 0 || report.presentationJoinMisses != 1 {
		t.Fatalf("filtered callbacks or run isolation report = %+v", report)
	}
	assertFeedbackSignalCount(t, dbPath, "file_opened", 0)
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
	defer db.Close()
	rows, err := db.Query(`SELECT chunk_key, COUNT(*) FROM feedback_signals WHERE signal_kind='file_opened' GROUP BY chunk_key ORDER BY chunk_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
	root := t.TempDir()
	firstPath := filepath.Join(t.TempDir(), "feedback.db")
	svc, err := openFeedbackService(context.Background(), root, firstPath, feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(10_000, 0)
	var mu sync.Mutex
	now := base
	svc.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	obs := svc.observer("run")
	tro := obs.(agent.ToolResultObserver)
	rpo := obs.(agent.RetrievalPresentationObserver)
	_ = tro.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = rpo.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	mu.Lock()
	now = base.Add(299 * time.Second)
	mu.Unlock()
	_ = tro.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("f", "read_file", `{"path":"a.go"}`), Invoked: true})
	mu.Lock()
	now = base.Add(time.Hour)
	mu.Unlock() // worker/close time is irrelevant to the positive.
	report, err := svc.close()
	if err != nil || report.dropped != 0 {
		t.Fatalf("close = %+v, %v", report, err)
	}
	assertFeedbackSignalCount(t, firstPath, "file_opened", 1)

	secondPath := filepath.Join(t.TempDir(), "feedback.db")
	second, err := openFeedbackService(context.Background(), root, secondPath, feedbackTestWarn(t))
	if err != nil {
		t.Fatal(err)
	}
	now = base
	second.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	o := second.observer("run")
	tro2 := o.(agent.ToolResultObserver)
	p := o.(agent.RetrievalPresentationObserver)
	_ = tro2.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 1, Call: feedbackCall("r", "retrieve", `{"query":"q"}`), Invoked: true})
	_ = p.OnRetrievalPresentation(context.Background(), agent.RetrievalPresentationEvent{Step: 2, ToolCallID: "r", Attribution: agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{StableKey: "k", Source: "a.go"}}}})
	mu.Lock()
	now = base.Add(300 * time.Second)
	mu.Unlock()
	_ = tro2.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 3, Call: feedbackCall("f", "read_file", `{"path":"a.go"}`), Invoked: true})
	_, _ = second.close()
	assertFeedbackSignalCount(t, secondPath, "file_opened", 0)
	assertFeedbackSignalCount(t, secondPath, "window_expired", 1)
}

func assertFeedbackSignalCount(t *testing.T, dbPath, kind string, want int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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
	obs := svc.observer("run").(agent.ToolResultObserver)
	present := svc.observer("run").(agent.RetrievalPresentationObserver)
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

func TestFeedbackServiceWriteFailureDisablesAndWarnsOnce(t *testing.T) {
	store := &feedbackBlockingStore{started: make(chan struct{}), release: make(chan struct{}), fail: errors.New("write failed")}
	var warnings atomic.Int64
	svc := feedbackServiceForStore(t.TempDir(), store, func(string) { warnings.Add(1) })
	obs := svc.observer("run").(agent.ToolResultObserver)
	present := svc.observer("run").(agent.RetrievalPresentationObserver)
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
	var first, second atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	n := newFeedbackNotifier(func(string) {
		first.Add(1)
		close(entered)
		<-release
	})
	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(start)
		n.notify("warning")
		n.notify("warning")
		close(done)
	}()
	<-start
	<-entered
	n.set(func(string) { second.Add(1) })
	close(release)
	<-done
	if first.Load() != 1 || second.Load() != 1 {
		t.Fatalf("fallback=%d active=%d", first.Load(), second.Load())
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

func TestOpenBehavioralWeighterValid(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sub", "fb.db") // parent dir does not exist yet
	h, warn := openBehavioralWeighter(context.Background(), dbPath)
	if h == nil || h.weighter == nil || h.db == nil {
		t.Fatalf("want non-nil handle, warn=%q", warn)
	}
	defer func() { _ = h.db.Close() }()
	if warn != "" {
		t.Errorf("unexpected warn: %q", warn)
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Errorf("feedback DB not created: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("feedback DB mode = %o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Errorf("feedback DB dir not created: %v", err)
	} else if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("feedback DB dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestOpenBehavioralWeighterFailsOpen(t *testing.T) {
	dir := t.TempDir() // a directory path is not a valid SQLite file target
	h, warn := openBehavioralWeighter(context.Background(), dir)
	if h != nil {
		t.Errorf("want nil handle for bad path, got non-nil")
	}
	if warn == "" {
		t.Errorf("want a warning for bad path")
	}
}

func TestEnableRetrieveFeedbackValid(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	seedIndex(t, dbPath, "workspace:k", "ollama/nomic")
	removeSQLiteSidecars(t, dbPath)

	feedbackDB := filepath.Join(dataDir, "feedback", "fb.db")
	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath:  dbPath,
		workspaceID: "workspace:k",
		feedbackDB:  feedbackDB,
	})
	if got.tool == nil {
		t.Fatalf("retrieve should register; warns=%v", got.warns)
	}
	if got.reader == nil || got.reader.feedback == nil || got.reader.feedback.db == nil || got.reader.feedback.weighter == nil {
		t.Fatalf("feedback handle not retained by reader: %#v", got.reader)
	}
	defer func() {
		if err := got.reader.closeAfterDrain(); err != nil {
			t.Error(err)
		}
	}()
}

func TestEnableRetrieveFeedbackFailsOpen(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "indexes", "k.db")
	seedIndex(t, dbPath, "workspace:k", "ollama/nomic")
	removeSQLiteSidecars(t, dbPath)

	got := enableRetrieve(context.Background(), embedCfg(), &provider.Router{}, retrieveOpts{
		autoDBPath:  dbPath,
		workspaceID: "workspace:k",
		feedbackDB:  t.TempDir(), // directory path: invalid SQLite file target
	})
	if got.tool == nil {
		t.Fatalf("retrieve should remain registered when feedback fails open; warns=%v", got.warns)
	}
	if got.reader != nil && got.reader.feedback != nil {
		t.Fatalf("bad feedback DB should not return a handle")
	}
	joined := strings.Join(got.warns, "\n")
	if !strings.Contains(joined, "behavioral feedback disabled") {
		t.Fatalf("missing feedback warning in %v", got.warns)
	}
}
