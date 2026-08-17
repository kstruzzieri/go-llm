package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestFetchSlotCapacity(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    int
		wantErr bool
	}{
		{
			name: "total_slots captured",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"total_slots": 4}`))
			},
			want: 4,
		},
		{
			name: "non-200 is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantErr: true,
		},
		{
			name: "malformed JSON is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"total_slots"`))
			},
			wantErr: true,
		},
		{
			name: "missing total_slots is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"model_path": "/x.gguf"}`))
			},
			wantErr: true,
		},
		{
			name: "zero total_slots is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"total_slots": 0}`))
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			got, err := fetchSlotCapacity(context.Background(), srv.Client(), SlotBackend{BaseURL: srv.URL}, "m")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got capacity %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("capacity = %d, want %d", got, tt.want)
			}
		})
	}
}

// The model-qualified query is load-bearing for llama-swap. The model name
// contains characters that break an unescaped query ("&", "="): if
// QueryEscape is dropped, the decoded param truncates at "&" and this test
// fails — a ":"-or-"/"-only name would decode identically and let the
// mutation survive.
func TestFetchSlotCapacityModelQualifiedQuery(t *testing.T) {
	const model = "team/qwen3:8b&rev=q4"
	var gotPath, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotModel = r.URL.Query().Get("model")
		_, _ = w.Write([]byte(`{"total_slots": 2}`))
	}))
	defer srv.Close()

	if _, err := fetchSlotCapacity(context.Background(), srv.Client(), SlotBackend{BaseURL: srv.URL}, model); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/props" {
		t.Fatalf("path = %q, want /props", gotPath)
	}
	if gotModel != model {
		t.Fatalf("model param = %q, want %q", gotModel, model)
	}
}

// Probes must authenticate exactly like the provider's own client: llama-swap
// v235 places /props behind its authenticated model-dispatch chain, so a
// keyless probe against a keyed backend 401s and permanently fail-safes a
// working backend.
func TestFetchSlotCapacityAuth(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"total_slots": 2}`))
	}))
	defer srv.Close()

	if _, err := fetchSlotCapacity(context.Background(), srv.Client(), SlotBackend{BaseURL: srv.URL, APIKey: "sk-test"}, "m"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer sk-test")
	}

	if _, err := fetchSlotCapacity(context.Background(), srv.Client(), SlotBackend{BaseURL: srv.URL}, "m"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("keyless probe sent Authorization %q, want no header", gotAuth)
	}
}

// capturingLauncher records probe thunks instead of running them, making
// spawn decisions deterministic (immediate counters against a real goroutine
// race the goroutine; capturing removes the race entirely). drain() runs
// captured thunks synchronously, so probe completion is also deterministic —
// no sleeps.
type capturingLauncher struct {
	mu      sync.Mutex
	pending []func()
}

func (cl *capturingLauncher) launch(f func()) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.pending = append(cl.pending, f)
}

// drain runs and clears all captured thunks, returning how many ran.
func (cl *capturingLauncher) drain() int {
	cl.mu.Lock()
	fs := cl.pending
	cl.pending = nil
	cl.mu.Unlock()
	for _, f := range fs {
		f()
	}
	return len(fs)
}

func (cl *capturingLauncher) count() int {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return len(cl.pending)
}

// newTestSlotSource builds a source over one governed provider "lc" with a
// deterministic clock and a capturing launcher. Cleanup drains before Close
// so pending wg.Add slots cannot deadlock Close.
func newTestSlotSource(t *testing.T, be SlotBackend, opts ...SlotSourceOption) (*OpenAICompatSlotSource, *capturingLauncher, *time.Time) {
	t.Helper()
	ss := NewOpenAICompatSlotSource(map[string]SlotBackend{"lc": be}, opts...)
	cl := &capturingLauncher{}
	ss.launch = cl.launch
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	ss.nowFn = func() time.Time { return now }
	t.Cleanup(func() {
		cl.drain()
		_ = ss.Close()
	})
	return ss, cl, &now
}

// countingPropsServer serves /props from *slots and counts requests under mu.
func countingPropsServer(t *testing.T, mu *sync.Mutex, slots *int, count *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*count++
		n := *slots
		mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"total_slots": %d}`, n)
	}))
}

func TestSlotSourceCapacitySemantics(t *testing.T) {
	var mu sync.Mutex
	slots, count := 4, 0
	srv := countingPropsServer(t, &mu, &slots, &count)
	defer srv.Close()
	ss, cl, _ := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL})

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}

	// Ungoverned provider: never (1, true) — the serialization guard.
	if n, ok := ss.Capacity(ModelKey{Provider: "ollama", Model: "x"}); ok || n != 0 {
		t.Fatalf("ungoverned Capacity = (%d, %v), want (0, false)", n, ok)
	}
	// Governed but unprobed: fail-safe serial.
	if n, ok := ss.Capacity(key); !ok || n != 1 {
		t.Fatalf("unprobed Capacity = (%d, %v), want (1, true)", n, ok)
	}
	// Capacity and RecordUse are non-blocking; nothing has probed yet.
	ss.RecordUse(key)
	mu.Lock()
	if count != 0 {
		mu.Unlock()
		t.Fatalf("server saw %d requests before drain, want 0 (probe must be async)", count)
	}
	mu.Unlock()

	// One captured probe; running it populates the cache from the server.
	if ran := cl.drain(); ran != 1 {
		t.Fatalf("captured probes = %d, want 1", ran)
	}
	if n, ok := ss.Capacity(key); !ok || n != 4 {
		t.Fatalf("probed Capacity = (%d, %v), want (4, true)", n, ok)
	}
	// Capacity is a pure cache read: repeated reads issue no requests.
	for range 10 {
		_, _ = ss.Capacity(key)
	}
	mu.Lock()
	if count != 1 {
		mu.Unlock()
		t.Fatalf("request count = %d after Capacity reads, want 1", count)
	}
	mu.Unlock()

	// Ungoverned RecordUse captures nothing.
	ss.RecordUse(ModelKey{Provider: "ollama", Model: "x"})
	if cl.count() != 0 {
		t.Fatalf("ungoverned RecordUse captured a probe")
	}
}

func TestSlotSourceSingleFlight(t *testing.T) {
	var mu sync.Mutex
	slots, count := 4, 0
	srv := countingPropsServer(t, &mu, &slots, &count)
	defer srv.Close()
	ss, cl, _ := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL})

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	ss.RecordUse(key) // in-flight: must not capture a second probe
	if cl.count() != 1 {
		t.Fatalf("captured probes = %d, want 1 (single-flight)", cl.count())
	}
	if ran := cl.drain(); ran != 1 {
		t.Fatalf("drained probes = %d, want 1", ran)
	}
	// In-flight cleared after completion — checked directly. Whether a
	// LATER use re-probes is TTL policy, owned by the TTL tests; asserting
	// a re-probe here would go red when TTL freshness lands.
	ss.mu.RLock()
	inflight := len(ss.inflight)
	ss.mu.RUnlock()
	if inflight != 0 {
		t.Fatalf("inflight = %d after drain, want 0", inflight)
	}
}
