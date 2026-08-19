package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
		{
			// ok=true implies n >= 1: a "< 1" check weakened to "== 0"
			// would admit negative capacity.
			name: "negative total_slots is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"total_slots": -3}`))
			},
			wantErr: true,
		},
		{
			// Pins the LOWER bound of the read cap: llama-server /props
			// legitimately carries large chat templates, so a response
			// just under 1MB must still decode (a cap accidentally
			// shrunk to a few KB fails here, while the oversized case
			// below pins the upper bound).
			name: "large response under the cap succeeds",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"padding": "`))
				pad := make([]byte, 64*1024)
				for i := range pad {
					pad[i] = 'x'
				}
				for range 14 { // ~896KB of padding, under the 1MB cap
					_, _ = w.Write(pad)
				}
				_, _ = w.Write([]byte(`", "total_slots": 4}`))
			},
			want: 4,
		},
		{
			// The read is bounded (a misconfigured backend must not make a
			// probe slurp an arbitrarily large body): a response whose JSON
			// object does not complete within the cap is an error, which
			// callers translate to fail-safe 1.
			name: "oversized response is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"padding": "`))
				pad := make([]byte, 64*1024)
				for i := range pad {
					pad[i] = 'x'
				}
				for range 20 { // ~1.25MB of padding, over the 1MB cap
					_, _ = w.Write(pad)
				}
				_, _ = w.Write([]byte(`", "total_slots": 4}`))
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
	// Capacity is the hot-path read: it must not QUEUE a probe either — a
	// Capacity that called RecordUse would single-flight into the later
	// explicit use and every other assertion would still pass.
	if cl.count() != 0 {
		t.Fatalf("unprobed Capacity queued %d probes, want 0", cl.count())
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

// countingTransport wraps the default RoundTripper and counts trips —
// proof that probes go through the CUSTOM client, which pointer equality
// on the field cannot give.
type countingTransport struct {
	mu    sync.Mutex
	trips int
}

func (ct *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ct.mu.Lock()
	ct.trips++
	ct.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

func (ct *countingTransport) count() int {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.trips
}

// Options must take effect and their invalid-value guards must hold: the
// custom client actually carries the probes (counted at its transport),
// the custom TTL moves the refresh boundary on BOTH sides (a shortened or
// ignored TTL fails one of them), and non-positive/nil values leave
// defaults intact.
func TestSlotSourceOptions(t *testing.T) {
	var mu sync.Mutex
	slots, count := 4, 0
	srv := countingPropsServer(t, &mu, &slots, &count)
	defer srv.Close()

	ct := &countingTransport{}
	custom := &http.Client{Timeout: time.Second, Transport: ct}
	ss, cl, now := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL},
		WithSlotTTL(time.Minute), WithSlotHTTPClient(custom))
	start := *now

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	if ran := cl.drain(); ran != 1 {
		t.Fatalf("probes = %d, want 1", ran)
	}
	if ct.count() != 1 {
		t.Fatalf("custom transport trips = %d, want 1 (probe bypassed the custom client)", ct.count())
	}

	// Just inside the custom TTL: still fresh, no probe.
	*now = start.Add(time.Minute - time.Second)
	ss.RecordUse(key)
	if cl.count() != 0 {
		t.Fatalf("entry fresh under custom TTL captured a probe (TTL shortened?)")
	}
	// Exactly at the custom TTL: expired, a use re-probes. Under the
	// default 5m TTL this entry would still be fresh.
	*now = start.Add(time.Minute)
	ss.RecordUse(key)
	if ran := cl.drain(); ran != 1 {
		t.Fatalf("custom-TTL expiry probes = %d, want 1 (TTL option ignored?)", ran)
	}

	// Invalid values are ignored, defaults retained.
	guarded := NewOpenAICompatSlotSource(map[string]SlotBackend{"lc": {BaseURL: srv.URL}},
		WithSlotTTL(0), WithSlotTTL(-time.Second), WithSlotHTTPClient(nil))
	defer func() { _ = guarded.Close() }()
	if guarded.ttl != defaultSlotTTL {
		t.Fatalf("ttl = %v after invalid options, want default %v", guarded.ttl, defaultSlotTTL)
	}
	if guarded.client == nil {
		t.Fatalf("client = nil after WithSlotHTTPClient(nil), want default")
	}
}

// Acceptance: TTL boundary observable without wall-clock sleeps; stale value
// replaced after expiry. Expired entries READ as fail-safe (1, true) — after
// an 8-slot backend restarts with 1, serving the stale 8 would let #400
// over-admit; unknown-by-age = unknown. Boundary: fresh iff
// now < fetchedAt+TTL, so exactly-at-expiry is expired.
func TestSlotSourceTTLBoundary(t *testing.T) {
	var mu sync.Mutex
	slots, count := 4, 0
	srv := countingPropsServer(t, &mu, &slots, &count)
	defer srv.Close()
	ss, cl, now := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL})
	fetchedAt := *now // probes stamp entries with the injected clock

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	if ran := cl.drain(); ran != 1 {
		t.Fatalf("initial probes = %d, want 1", ran)
	}
	if n, _ := ss.Capacity(key); n != 4 {
		t.Fatalf("capacity = %d, want 4", n)
	}

	// Backend restarted with a different slot count; entry still fresh:
	// RecordUse must not probe and the cached value must be served.
	mu.Lock()
	slots = 7
	mu.Unlock()
	*now = fetchedAt.Add(defaultSlotTTL - time.Second) // just inside
	ss.RecordUse(key)
	if cl.count() != 0 {
		t.Fatalf("fresh entry captured a probe")
	}
	if n, ok := ss.Capacity(key); !ok || n != 4 {
		t.Fatalf("fresh Capacity = (%d, %v), want (4, true)", n, ok)
	}

	// Exactly at expiry: the read degrades to fail-safe, and use re-probes.
	*now = fetchedAt.Add(defaultSlotTTL)
	if n, ok := ss.Capacity(key); !ok || n != 1 {
		t.Fatalf("expired Capacity = (%d, %v), want (1, true)", n, ok)
	}
	// Even an EXPIRED read stays pure: refresh is RecordUse's job.
	if cl.count() != 0 {
		t.Fatalf("expired Capacity queued %d probes, want 0", cl.count())
	}
	ss.RecordUse(key)
	if ran := cl.drain(); ran != 1 {
		t.Fatalf("expired-entry probes = %d, want 1", ran)
	}
	if n, ok := ss.Capacity(key); !ok || n != 7 {
		t.Fatalf("refreshed Capacity = (%d, %v), want (7, true)", n, ok)
	}
	mu.Lock()
	if count != 2 {
		mu.Unlock()
		t.Fatalf("request count = %d, want 2", count)
	}
	mu.Unlock()
}

// Acceptance: error /props degrades to capacity 1 without failing the call
// path. The fail-safe result is CACHED at TTL cadence — this is what
// distinguishes "error caches 1" from "error writes nothing": with no write,
// the immediate second RecordUse would capture another probe (the
// constructed distinguishing input for that mutation).
func TestSlotSourceFailSafeAndRecovery(t *testing.T) {
	var mu sync.Mutex
	healthy := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := healthy
		mu.Unlock()
		if !ok {
			http.Error(w, "no props here", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"total_slots": 6}`))
	}))
	defer srv.Close()
	ss, cl, now := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL})
	start := *now

	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key) // probe fails -> fail-safe 1, still governed
	if ran := cl.drain(); ran != 1 {
		t.Fatalf("probes = %d, want 1", ran)
	}
	if n, ok := ss.Capacity(key); !ok || n != 1 {
		t.Fatalf("failed-probe Capacity = (%d, %v), want (1, true)", n, ok)
	}
	ss.RecordUse(key) // fail-safe entry is fresh: no re-probe
	if cl.count() != 0 {
		t.Fatalf("cached fail-safe captured a probe")
	}

	// Backend recovers; after the TTL the next use re-discovers reality.
	mu.Lock()
	healthy = true
	mu.Unlock()
	*now = start.Add(defaultSlotTTL + time.Second)
	ss.RecordUse(key)
	if ran := cl.drain(); ran != 1 {
		t.Fatalf("recovery probes = %d, want 1", ran)
	}
	if n, ok := ss.Capacity(key); !ok || n != 6 {
		t.Fatalf("recovered Capacity = (%d, %v), want (6, true)", n, ok)
	}
}

// Close must (a) cancel in-flight probes, (b) wait for them, (c) never let
// a cancelled probe write, (d) make later RecordUse a no-op, (e) be
// idempotent. The handler blocks until the probe's own context is
// cancelled by Close — no gates to release, fully deterministic.
func TestSlotSourceClose(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ss := NewOpenAICompatSlotSource(map[string]SlotBackend{"lc": {BaseURL: srv.URL}})
	// Default (real) launcher: this test IS about goroutine lifecycle.
	key := ModelKey{Provider: "lc", Model: "qwen3:8b"}
	ss.RecordUse(key)
	<-started // probe goroutine is live and blocked in the backend

	if err := ss.Close(); err != nil { // must cancel the probe and wait it out
		t.Fatalf("Close: %v", err)
	}
	// Close returned => wg drained => probe finished => reading state is
	// race-free. A cancelled probe must not have written fail-safe 1.
	if len(ss.entries) != 0 {
		t.Fatalf("entries after Close-aborted probe = %d, want 0", len(ss.entries))
	}
	if err := ss.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Post-Close RecordUse must not spawn. Deterministic via the capturing
// launcher: after Close, nothing may be captured (a select/default check
// on an async hook could miss a mutant's late goroutine; a captured-thunk
// count cannot).
func TestSlotSourceRecordUseAfterClose(t *testing.T) {
	ss, cl, _ := newTestSlotSource(t, SlotBackend{BaseURL: "http://127.0.0.1:0"})
	if err := ss.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ss.RecordUse(ModelKey{Provider: "lc", Model: "qwen3:8b"})
	if cl.count() != 0 {
		t.Fatalf("RecordUse after Close captured %d probes, want 0", cl.count())
	}
}

// Close overlap, established rather than hoped for: overlap is PROVEN on
// both sides of the Add/Wait handoff —
//
//	(a) a resident probe is live in the backend (barrier) before Close;
//	(b) one WORKER launch is trapped between its wg.Add (already done
//	    inside RecordUse, under the mutex) and the real go-launch, and is
//	    released only after Close has observably cancelled — so Close's
//	    wg.Wait provably overlaps a launch in flight, immune to scheduler
//	    luck.
//
// An atomic launch counter then proves the count stops changing once
// Close returns.
func TestSlotSourceCloseOverlapsLiveTraffic(t *testing.T) {
	started := make(chan struct{}, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()

	ss := NewOpenAICompatSlotSource(map[string]SlotBackend{"lc": {BaseURL: srv.URL}})
	inner := ss.launch
	var launches atomic.Int64

	// Phase 1: one probe live and blocked in the backend before Close.
	ss.launch = func(f func()) {
		launches.Add(1)
		inner(f)
	}
	ss.RecordUse(ModelKey{Provider: "lc", Model: "resident"})
	<-started

	// Phase 2: the NEXT launch (a worker's) is held at a gate. Swapping
	// ss.launch here is race-free: only this goroutine has used it so far,
	// and the resident probe goroutine never reads it.
	launchGate := make(chan struct{})
	gateArmed := make(chan struct{})
	var gateOnce sync.Once
	ss.launch = func(f func()) {
		launches.Add(1)
		trapped := false
		gateOnce.Do(func() { trapped = true })
		if trapped {
			close(gateArmed)
			go func() {
				<-launchGate
				inner(f)
			}()
			return
		}
		inner(f)
	}

	stop := make(chan struct{})
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			for j := 0; ; j++ {
				select {
				case <-stop:
					return
				default:
				}
				k := ModelKey{Provider: "lc", Model: fmt.Sprintf("m%d", (i+j)%4)}
				ss.RecordUse(k)
				_, _ = ss.Capacity(k)
			}
		}(i)
	}
	<-gateArmed // a worker launch is now trapped mid-flight, wg.Add done

	closeDone := make(chan error, 1)
	go func() { closeDone <- ss.Close() }()
	<-ss.ctx.Done()   // Close has marked closed and cancelled; it is now in wg.Wait
	close(launchGate) // release the trapped launch INTO the closing source

	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := launches.Load()

	// Workers are STILL hammering here; none of their post-Close
	// RecordUse calls may launch.
	ss.RecordUse(ModelKey{Provider: "lc", Model: "late"})
	close(stop)
	workers.Wait()
	if got := launches.Load(); got != after {
		t.Fatalf("launches grew from %d to %d after Close returned", after, got)
	}
	if err := ss.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Close must WAIT for in-flight probes — the drain itself, not just the
// no-new-launches property. The overlap test above cannot kill a mutant
// that deletes wg.Wait() (the trapped launch was already counted before
// Close returned), so this test proves the block directly: with a probe
// pending, synctest.Wait() parks every bubble goroutine, and Close must
// NOT have returned; draining the probe is the only thing that releases
// it. Sleep-free and deterministic. No real I/O runs in the bubble: Close
// has already cancelled the source ctx, so the drained probe's fetch
// fails before dialing (the BaseURL is never reachable anyway).
func TestSlotSourceCloseWaitsForInflightProbes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ss := NewOpenAICompatSlotSource(map[string]SlotBackend{"lc": {BaseURL: "http://127.0.0.1:0"}})
		cl := &capturingLauncher{}
		ss.launch = cl.launch

		ss.RecordUse(ModelKey{Provider: "lc", Model: "qwen3:8b"}) // wg.Add done, thunk pending

		closeDone := make(chan error, 1)
		go func() { closeDone <- ss.Close() }()
		synctest.Wait() // all bubble goroutines durably blocked

		select {
		case err := <-closeDone:
			t.Fatalf("Close returned (%v) with a probe still pending — wg.Wait is missing", err)
		default:
		}

		if ran := cl.drain(); ran != 1 {
			t.Fatalf("drained probes = %d, want 1", ran)
		}
		if err := <-closeDone; err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// Acceptance: llama-swap fronts multiple upstreams and REQUIRES the
// model-qualified /props form. The fake proxy 400s on a missing model param
// (as the proxy cannot dispatch without it), so an implementation that drops
// the param collapses every capacity to fail-safe 1 and the per-model
// assertions below fail. The evicted model exercises the swap-out window:
// a probe for a model the proxy can no longer serve must degrade to
// fail-safe 1, not error into the call path.
func TestSlotSourceLlamaSwapModelQualified(t *testing.T) {
	totals := map[string]int{
		"qwen3-coder-next": 4,
		"gemma4:31b":       2,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		m := r.URL.Query().Get("model")
		if m == "" {
			http.Error(w, "model query parameter required", http.StatusBadRequest)
			return
		}
		n, ok := totals[m]
		if !ok {
			http.Error(w, "unknown model", http.StatusNotFound) // evicted/unknown upstream
			return
		}
		_, _ = fmt.Fprintf(w, `{"total_slots": %d}`, n)
	}))
	defer srv.Close()
	ss, cl, _ := newTestSlotSource(t, SlotBackend{BaseURL: srv.URL})

	k1 := ModelKey{Provider: "lc", Model: "qwen3-coder-next"}
	k2 := ModelKey{Provider: "lc", Model: "gemma4:31b"}
	kEvicted := ModelKey{Provider: "lc", Model: "swapped-out"}
	ss.RecordUse(k1)
	ss.RecordUse(k2)
	ss.RecordUse(kEvicted)
	if ran := cl.drain(); ran != 3 {
		t.Fatalf("probes = %d, want 3", ran)
	}

	// Per-model capacities must differ per key — a fake that ignored the
	// forwarded key could not catch a swapped-key mutation.
	if n, ok := ss.Capacity(k1); !ok || n != 4 {
		t.Fatalf("qwen3-coder-next capacity = (%d, %v), want (4, true)", n, ok)
	}
	if n, ok := ss.Capacity(k2); !ok || n != 2 {
		t.Fatalf("gemma4:31b capacity = (%d, %v), want (2, true)", n, ok)
	}
	if n, ok := ss.Capacity(kEvicted); !ok || n != 1 {
		t.Fatalf("evicted capacity = (%d, %v), want fail-safe (1, true)", n, ok)
	}
}

// ---------------------------------------------------------------------------
// Capacity overrides (#400 config override)
// ---------------------------------------------------------------------------

func TestSlotOverridePinsCapacityAndSurvivesTTL(t *testing.T) {
	key := ModelKey{Provider: "lc", Model: "m1"}
	ss, cl, now := newTestSlotSource(t, SlotBackend{BaseURL: "http://127.0.0.1:1"},
		WithSlotCapacityOverrides(map[ModelKey]int{key: 5}))
	if n, ok := ss.Capacity(key); !ok || n != 5 {
		t.Fatalf("Capacity = (%d, %v), want pinned (5, true)", n, ok)
	}
	*now = now.Add(24 * time.Hour) // far past any TTL: pinned keys never expire
	if n, ok := ss.Capacity(key); !ok || n != 5 {
		t.Fatalf("Capacity after TTL = (%d, %v), want still (5, true)", n, ok)
	}
	ss.RecordUse(key)
	if got := cl.count(); got != 0 {
		t.Fatalf("RecordUse on pinned key launched %d probes, want 0", got)
	}
	// Non-overridden keys on the same source still probe as before.
	other := ModelKey{Provider: "lc", Model: "m2"}
	ss.RecordUse(other)
	if got := cl.count(); got != 1 {
		t.Fatalf("RecordUse on unpinned key launched %d probes, want 1", got)
	}
}

func TestSlotOverrideDoesNotGovernUnknownProvider(t *testing.T) {
	// Governed check precedes the override lookup: stray override entries
	// for providers outside the backends map stay ungoverned.
	stray := ModelKey{Provider: "not-governed", Model: "m1"}
	ss, _, _ := newTestSlotSource(t, SlotBackend{BaseURL: "http://127.0.0.1:1"},
		WithSlotCapacityOverrides(map[ModelKey]int{stray: 7}))
	if n, ok := ss.Capacity(stray); ok || n != 0 {
		t.Fatalf("Capacity = (%d, %v), want ungoverned (0, false)", n, ok)
	}
}

func TestSlotOverrideClampsContractViolatingValues(t *testing.T) {
	// Public API entry point: the SlotSource contract (ok=true => n >= 1)
	// must hold no matter who built the map.
	zero := ModelKey{Provider: "lc", Model: "z"}
	neg := ModelKey{Provider: "lc", Model: "n"}
	ss, _, _ := newTestSlotSource(t, SlotBackend{BaseURL: "http://127.0.0.1:1"},
		WithSlotCapacityOverrides(map[ModelKey]int{zero: 0, neg: -2}))
	if n, ok := ss.Capacity(zero); !ok || n != 1 {
		t.Fatalf("Capacity(zero) = (%d, %v), want clamped (1, true)", n, ok)
	}
	if n, ok := ss.Capacity(neg); !ok || n != 1 {
		t.Fatalf("Capacity(neg) = (%d, %v), want clamped (1, true)", n, ok)
	}
}
