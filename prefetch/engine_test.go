package prefetch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/rag"
)

// mockRetriever implements Retriever for testing.
type mockRetriever struct {
	mu          sync.Mutex
	results     *RetrieveResult
	err         error
	callCount   int
	lastQuery   string
	lastK       int
}

func (m *mockRetriever) Retrieve(_ context.Context, query string, k int,
	_ rag.QueryContext, _ RetrieveOptions) (*RetrieveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callCount++
	m.lastQuery = query
	m.lastK = k
	if m.err != nil {
		return nil, m.err
	}
	return m.results, nil
}

func (m *mockRetriever) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
}

// mockStateProvider implements StateProvider for testing.
type mockStateProvider struct {
	mu            sync.Mutex
	activeFile    string
	openFiles     []string
	recentEdits   []EditEvent
	cursorContext CursorContext
}

func (m *mockStateProvider) ActiveFile() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeFile
}

func (m *mockStateProvider) OpenFiles() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.openFiles
}

func (m *mockStateProvider) RecentEdits() []EditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recentEdits
}

func (m *mockStateProvider) CursorContext() CursorContext {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursorContext
}

func (m *mockStateProvider) setActiveFile(f string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeFile = f
}

func (m *mockStateProvider) setOpenFiles(files []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.openFiles = files
}

func defaultTestResults() *RetrieveResult {
	return &RetrieveResult{
		Chunks: []rag.ScoredResult{
			{
				SearchResult: rag.SearchResult{
					Chunk: rag.Chunk{
						ID:      "chunk-1",
						Content: "test content",
						Source:  "main.go",
					},
					Score:    0.95,
					Distance: 0.05,
				},
				Signals: map[string]float64{"semantic": 0.95},
			},
		},
		CacheHit: false,
	}
}

func TestEngine_CacheMiss(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{activeFile: "main.go"}

	engine := NewEngine(retriever, store, state,
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 65536}),
	)

	result, err := engine.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CacheHit {
		t.Error("expected cache miss on first call")
	}
	if len(result.Chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(result.Chunks))
	}
	if retriever.calls() != 1 {
		t.Errorf("expected 1 retriever call, got %d", retriever.calls())
	}
}

func TestEngine_CacheHit(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{activeFile: "main.go"}

	engine := NewEngine(retriever, store, state,
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 65536}),
	)

	// First call warms the cache.
	_, err := engine.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Second call should be a cache hit.
	result, err := engine.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.CacheHit {
		t.Error("expected cache hit on second call")
	}
	if retriever.calls() != 1 {
		t.Errorf("expected 1 retriever call (cached), got %d", retriever.calls())
	}
}

func TestEngine_SkipCache(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{activeFile: "main.go"}

	engine := NewEngine(retriever, store, state,
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 65536}),
	)

	// Warm the cache.
	_, err := engine.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Skip cache should force a cold retrieval.
	result, err := engine.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{}, RetrieveOptions{SkipCache: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CacheHit {
		t.Error("expected cache miss with SkipCache=true")
	}
	if retriever.calls() != 2 {
		t.Errorf("expected 2 retriever calls, got %d", retriever.calls())
	}
}

func TestEngine_ResourceAdaptation(t *testing.T) {
	tests := []struct {
		name          string
		memory        int64
		wantCapacity  int
		wantPolicy    string
		wantCacheNil  bool
	}{
		{
			name:         "high resources (64GB)",
			memory:       65536,
			wantCapacity: 500,
			wantPolicy:   "every_open",
		},
		{
			name:         "medium resources (16GB)",
			memory:       16384,
			wantCapacity: 100,
			wantPolicy:   "debounced",
		},
		{
			name:         "low resources (8GB)",
			memory:       8192,
			wantCapacity: 0,
			wantPolicy:   "disabled",
			wantCacheNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retriever := &mockRetriever{results: defaultTestResults()}
			store := &mockVectorStore{}
			state := &mockStateProvider{activeFile: "main.go"}

			engine := NewEngine(retriever, store, state,
				WithResources(fingerprint.ResourceProfile{TotalMemoryMB: tt.memory}),
			)

			if engine.config.cacheCapacity != tt.wantCapacity {
				t.Errorf("expected cache capacity %d, got %d",
					tt.wantCapacity, engine.config.cacheCapacity)
			}
			if engine.config.triggerPolicy != tt.wantPolicy {
				t.Errorf("expected trigger policy %q, got %q",
					tt.wantPolicy, engine.config.triggerPolicy)
			}
			if tt.wantCacheNil && engine.cache != nil {
				t.Error("expected nil cache for low resources")
			}
			if !tt.wantCacheNil && engine.cache == nil {
				t.Error("expected non-nil cache")
			}
		})
	}
}

func TestEngine_LowResourcesNoCaching(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{activeFile: "main.go"}

	engine := NewEngine(retriever, store, state,
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 4096}),
	)

	// Even with repeated calls, retriever should always be called.
	for i := 0; i < 3; i++ {
		result, err := engine.Retrieve(context.Background(), "query", 5,
			rag.QueryContext{}, RetrieveOptions{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.CacheHit {
			t.Errorf("call %d: expected cache miss with low resources", i)
		}
	}
	if retriever.calls() != 3 {
		t.Errorf("expected 3 retriever calls, got %d", retriever.calls())
	}
}

func TestEngine_WatchCancellation(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{activeFile: "main.go"}

	engine := NewEngine(retriever, store, state,
		WithDebounce(10*time.Millisecond),
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 65536}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- engine.Watch(ctx)
	}()

	// Let it run a couple of ticks.
	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return after context cancellation")
	}
}

func TestEngine_WatchDisabledForLowResources(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{activeFile: "main.go"}

	engine := NewEngine(retriever, store, state,
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 4096}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- engine.Watch(ctx)
	}()

	cancel()

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return for disabled engine")
	}
}

func TestEngine_WatchPrefetches(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{
		activeFile: "pkg/handler.go",
		openFiles:  []string{"pkg/handler.go", "pkg/handler_test.go"},
	}

	engine := NewEngine(retriever, store, state,
		WithDebounce(10*time.Millisecond),
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 65536}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = engine.Watch(ctx) }()

	// Give Watch time to detect state and prefetch.
	time.Sleep(100 * time.Millisecond)
	cancel()

	// The retriever should have been called at least once for prefetching.
	if retriever.calls() < 1 {
		t.Error("expected at least 1 prefetch retrieval call")
	}
}

func TestEngine_WatchStateChange(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{
		activeFile: "file_a.go",
		openFiles:  []string{"file_a.go"},
	}

	engine := NewEngine(retriever, store, state,
		WithDebounce(10*time.Millisecond),
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 65536}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = engine.Watch(ctx) }()

	// Let initial state be processed.
	time.Sleep(50 * time.Millisecond)
	initialCalls := retriever.calls()

	// Change state.
	state.setActiveFile("file_b.go")
	state.setOpenFiles([]string{"file_b.go"})

	// Let new state be detected and processed.
	time.Sleep(50 * time.Millisecond)
	cancel()

	if retriever.calls() <= initialCalls {
		t.Error("expected additional retriever calls after state change")
	}
}

func TestEngine_DifferentKSeparateCacheEntries(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{activeFile: "main.go"}

	engine := NewEngine(retriever, store, state,
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 65536}),
	)

	// Warm cache with k=5.
	_, err := engine.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// k=2 is a different cache key -- should miss and call retriever again.
	result, err := engine.Retrieve(context.Background(), "query", 2,
		rag.QueryContext{}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CacheHit {
		t.Error("expected cache miss for different k")
	}
	if retriever.calls() != 2 {
		t.Errorf("expected 2 retriever calls, got %d", retriever.calls())
	}
}

func TestEngine_DifferentContextSeparateCacheEntries(t *testing.T) {
	retriever := &mockRetriever{results: defaultTestResults()}
	store := &mockVectorStore{}
	state := &mockStateProvider{activeFile: "main.go"}

	engine := NewEngine(retriever, store, state,
		WithResources(fingerprint.ResourceProfile{TotalMemoryMB: 65536}),
	)

	// Warm cache from file A context.
	_, err := engine.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{CurrentFile: "a.go"}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Same query from file B context should miss cache.
	result, err := engine.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{CurrentFile: "b.go"}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CacheHit {
		t.Error("expected cache miss for different CurrentFile")
	}
	if retriever.calls() != 2 {
		t.Errorf("expected 2 retriever calls, got %d", retriever.calls())
	}

	// Same query + same context should hit cache.
	result, err = engine.Retrieve(context.Background(), "query", 5,
		rag.QueryContext{CurrentFile: "b.go"}, RetrieveOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.CacheHit {
		t.Error("expected cache hit for same context")
	}
}

func TestEngine_RetrieverInterface(t *testing.T) {
	// Verify that Engine implements Retriever at compile time.
	var _ Retriever = (*Engine)(nil)
}

// Heuristic helper tests.

func TestTestFilePair(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"main.go", "main_test.go"},
		{"main_test.go", "main.go"},
		{"pkg/handler.go", "pkg/handler_test.go"},
		{"pkg/handler_test.go", "pkg/handler.go"},
		{"script.py", "script_test.py"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := testFilePair(tt.input)
			if got != tt.want {
				t.Errorf("testFilePair(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestCoEditedFiles(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name    string
		edits   []EditEvent
		window  time.Duration
		wantLen int
	}{
		{
			name:    "no edits",
			edits:   nil,
			window:  30 * time.Second,
			wantLen: 0,
		},
		{
			name: "single edit",
			edits: []EditEvent{
				{Source: "a.go", Timestamp: now},
			},
			window:  30 * time.Second,
			wantLen: 0,
		},
		{
			name: "two co-edited files",
			edits: []EditEvent{
				{Source: "a.go", Timestamp: now},
				{Source: "b.go", Timestamp: now.Add(10 * time.Second)},
			},
			window:  30 * time.Second,
			wantLen: 2,
		},
		{
			name: "edits outside window",
			edits: []EditEvent{
				{Source: "a.go", Timestamp: now},
				{Source: "b.go", Timestamp: now.Add(60 * time.Second)},
			},
			window:  30 * time.Second,
			wantLen: 0,
		},
		{
			name: "same file edited twice",
			edits: []EditEvent{
				{Source: "a.go", Timestamp: now},
				{Source: "a.go", Timestamp: now.Add(5 * time.Second)},
			},
			window:  30 * time.Second,
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := coEditedFiles(tt.edits, tt.window)
			if len(got) != tt.wantLen {
				t.Errorf("expected %d co-edited files, got %d: %v",
					tt.wantLen, len(got), got)
			}
		})
	}
}

func TestCommonPathPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"pkg/handler/handler.go", "pkg/"},
		{"a/b/c.go", "a/"},
		{"top.go", ""},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := commonPathPrefix(tt.input)
			if got != tt.want {
				t.Errorf("commonPathPrefix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestStateChanged(t *testing.T) {
	tests := []struct {
		name       string
		active     string
		open       []string
		lastActive string
		lastOpen   []string
		want       bool
	}{
		{
			name:       "no change",
			active:     "a.go",
			open:       []string{"a.go", "b.go"},
			lastActive: "a.go",
			lastOpen:   []string{"a.go", "b.go"},
			want:       false,
		},
		{
			name:       "active file changed",
			active:     "b.go",
			open:       []string{"a.go", "b.go"},
			lastActive: "a.go",
			lastOpen:   []string{"a.go", "b.go"},
			want:       true,
		},
		{
			name:       "open files changed",
			active:     "a.go",
			open:       []string{"a.go", "c.go"},
			lastActive: "a.go",
			lastOpen:   []string{"a.go", "b.go"},
			want:       true,
		},
		{
			name:       "open files added",
			active:     "a.go",
			open:       []string{"a.go", "b.go", "c.go"},
			lastActive: "a.go",
			lastOpen:   []string{"a.go", "b.go"},
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stateChanged(tt.active, tt.open, tt.lastActive, tt.lastOpen)
			if got != tt.want {
				t.Errorf("stateChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}
