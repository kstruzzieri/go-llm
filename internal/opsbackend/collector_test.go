package opsbackend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/internal/opsfixture"
)

// fakeClock lets tests move wall and monotonic time independently.
type fakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{wall: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) clock() Clock {
	return Clock{
		Wall: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.wall },
		Mono: func() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.mono },
	}
}

func (c *fakeClock) advance(wall, mono time.Duration) {
	c.mu.Lock()
	c.wall = c.wall.Add(wall)
	c.mono += mono
	c.mu.Unlock()
}

func lsSpec(url string) BackendSpec {
	return BackendSpec{Provider: "llamacpp", BaseURL: url, APIFormat: "openai-compat"}
}

func countURI(f *opsfixture.Server, uri string) int {
	n := 0
	for _, r := range f.Requests() {
		if r.URI == uri {
			n++
		}
	}
	return n
}

// assertRequests requires exactly want, in order, and no would-be model load.
func assertRequests(t *testing.T, f *opsfixture.Server, want ...opsfixture.Request) {
	t.Helper()
	if got := f.Requests(); !slices.Equal(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if n := f.Loads.Load(); n != 0 {
		t.Fatalf("would-be model loads = %d, want 0", n)
	}
}

var (
	reqVersion = opsfixture.Request{Method: "GET", URI: "/api/version"}
	reqRunning = opsfixture.Request{Method: "GET", URI: "/running"}
	reqMetrics = opsfixture.Request{Method: "GET", URI: "/api/metrics"}
	reqModels  = opsfixture.Request{Method: "GET", URI: "/v1/models"}
	reqPS      = opsfixture.Request{Method: "GET", URI: "/api/ps"}
)

// front serves f through a loopback proxy that lets hook see each request
// first; hook reports whether it answered the request itself.
func front(t *testing.T, f *opsfixture.Server, hook func(w http.ResponseWriter, r *http.Request) bool) string {
	t.Helper()
	u, err := url.Parse(f.URL())
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hook(w, r) {
			rp.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// hangUp closes the connection without a response, so the client reads
// unreachable at once.
func hangUp(w http.ResponseWriter) {
	if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = conn.Close()
	}
}

// waitFor polls cond for up to d.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCollectorOneTickObservesEverySurfaceOnce(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Clock: newFakeClock().clock()})
	obs := c.Tick(context.Background())
	assertRequests(t, f, reqVersion, reqRunning, reqMetrics, reqModels)
	b := obs.Backends[0]
	if b.Kind != KindLlamaSwap || b.Support != SupportSupported || b.Version != "v235" || b.Running == nil || len(b.Running.Value) != 1 ||
		b.Rows == nil || len(b.Rows.Value) != 3 || b.Listed == nil || len(b.Listed.Value) != 3 {
		t.Fatalf("backend = %+v", b)
	}
	assertNoSentinels(t, obs)
}

func TestCollectorRemoteAndInvalidAreNeverContacted(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	c := NewCollector([]BackendSpec{ // out of order: observations sort by provider
		{Provider: "e-key", BaseURL: f.URL(), APIFormat: "openai-compat", APIKey: "k\r\nX-Injected: 1"},
		{Provider: "c-hosted", BaseURL: "https://opencode.ai/zen/go", APIFormat: "openai-compat"},
		{Provider: "a-remote", BaseURL: "https://api.example.com", APIFormat: "openai-compat"},
		{Provider: "d-format", BaseURL: f.URL(), APIFormat: "anthropic"},
		{Provider: "b-prefix", BaseURL: f.URL() + "/upstream/gemma4:31b", APIFormat: "openai-compat"},
	}, Options{Clock: newFakeClock().clock()})
	obs := c.Tick(context.Background())
	assertRequests(t, f)
	if obs.Backends[0].Hosting != HostingRemote || obs.Backends[0].Support != SupportNotObserved {
		t.Fatalf("remote = %+v", obs.Backends[0])
	}
	if obs.Backends[1].Support != SupportInvalidConfig {
		t.Fatalf("prefix = %+v", obs.Backends[1])
	}
	if h := obs.Backends[2]; h.Hosting != HostingRemote || h.Support != SupportNotObserved || h.Endpoint != "https://opencode.ai" {
		t.Fatalf("hosted with a path must read remote with an origin-only endpoint: %+v", h)
	}
	if d := obs.Backends[3]; d.Hosting != HostingLocal || d.Kind != KindNone || d.Support != SupportInvalidConfig {
		t.Fatalf("unknown api_format = %+v", d)
	}
	if e := obs.Backends[4]; e.Hosting != HostingLocal || e.Kind != KindNone || e.Support != SupportInvalidConfig {
		t.Fatalf("api_key that is not a header value = %+v", e)
	}
}

// TestCollectorVersionClassification: a classified runtime is never asked
// again; a 401 is not a classification (v235 puts /api/version behind its
// API-key check), so it stays unidentified and is retried after backoff.
func TestCollectorVersionClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		kind       Kind
		support    string
		version    string
		lastErr    Code
		retried    bool
	}{
		{"other llama-swap", `{"build_date":"x","commit":"y","version":"v240"}`, 0, KindNone, SupportUnsupported, "v240", "", false},
		{"ollama-shaped", `{"version":"0.12.3"}`, 0, KindNone, SupportUnrecognized, "", "", false},
		{"404 runtime", "", http.StatusNotFound, KindNone, SupportUnrecognized, "", CodeHTTPStatus, false},
		{"401 api key", "", http.StatusUnauthorized, KindUnidentified, SupportUnknown, "", CodeUnauthorized, true},
		{"over-cap answer", `{"version":"` + strings.Repeat("a", versionLimit) + `"}`, 0, KindNone, SupportUnrecognized, "", CodeTooLarge, false},
		{"long version", `{"build_date":"x","commit":"y","version":"v` + strings.Repeat("9", 100) + `"}`, 0, KindNone, SupportUnsupported, "v" + strings.Repeat("9", maxVersionLen-1), "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := opsfixture.NewLlamaSwap(t)
			if tc.status != 0 {
				f.SetStatus("/api/version", tc.status)
			} else {
				f.SetBody("/api/version", tc.body)
			}
			clk := newFakeClock()
			c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
			b := c.Tick(context.Background()).Backends[0]
			if b.Kind != tc.kind || b.Support != tc.support || b.Version != tc.version || surface(b, "version").LastError != tc.lastErr {
				t.Fatalf("backend = %+v requests = %v", b, f.Requests())
			}
			clk.advance(2*time.Second, 2*time.Second) // past the first backoff
			c.Tick(context.Background())
			if tc.retried {
				assertRequests(t, f, reqVersion, reqVersion)
			} else {
				assertRequests(t, f, reqVersion)
			}
		})
	}
}

func TestCollectorRejectedSampleKeepsPreviousObservation(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	first := c.Tick(context.Background()).Backends[0]
	firstAt := first.Running.Mono

	f.SetBody("/running", `{"running":[{"model":"`+opsfixture.SentinelCmd+`","state":"bogus"}]}`)
	clk.advance(2*time.Second, 2*time.Second)
	second := c.Tick(context.Background()).Backends[0]
	if second.Running == nil || second.Running.Mono != firstAt || second.Running.Value[0].Model != "gemma4:31b" {
		t.Fatalf("rejected sample replaced the previous one: %+v", second.Running)
	}
	if s := surface(second, "running"); s.LastError != CodeMalformed {
		t.Fatalf("running surface = %+v", s)
	}

	f.SetBody("/running", `{"running":[]}`)
	clk.advance(30*time.Second, 30*time.Second) // past backoff
	third := c.Tick(context.Background()).Backends[0]
	if third.Running == nil || len(third.Running.Value) != 0 || third.Running.Mono == firstAt {
		t.Fatalf("trusted empty sample not applied: %+v", third.Running)
	}
}

func TestCollectorLoadEpisodesAndGaps(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	step := func(running string) BackendObservation {
		f.SetBody("/running", running)
		clk.advance(2*time.Second, 2*time.Second)
		return c.Tick(context.Background()).Backends[0]
	}
	const none = `{"running":[]}`
	starting := `{"running":[{"model":"m","state":"starting","ttl":600}]}`
	ready := `{"running":[{"model":"m","state":"ready","ttl":600}]}`

	base := step(none)      // baseline, nothing tracked
	first := step(starting) // episode 1 begins
	if !base.Since.Equal(base.Running.At) || !memory(t, first, "m").Since.Equal(first.Running.At) {
		t.Fatalf("Since: period %v (want %v), model %v (want %v)", base.Since, base.Running.At, memory(t, first, "m").Since, first.Running.At)
	}
	b := step(ready) // same episode
	if got := memory(t, b, "m"); got.Loads != 1 || got.FirstObserved.IsZero() {
		t.Fatalf("after starting->ready: %+v", got)
	}
	step(none)      // unloaded
	b = step(ready) // missed starting: episode 2
	if got := memory(t, b, "m"); got.Loads != 2 {
		t.Fatalf("missed-starting load not counted: %+v", got)
	}

	clk.advance(20*time.Second, 20*time.Second) // late tick: gap
	// The first sample after a gap is a baseline: a model still loaded
	// across the gap is not a new load episode.
	b = step(ready)
	if got := memory(t, b, "m"); got.Loads != 2 || got.Gaps != 1 || b.Gaps != 1 || b.Periods != 2 || !b.Since.Equal(b.Running.At) || !got.Since.Equal(first.Running.At) {
		t.Fatalf("gap handling: mem=%+v gaps=%d periods=%d since=%v", got, b.Gaps, b.Periods, b.Since)
	}
	step(none)
	b = step(ready)
	if got := memory(t, b, "m"); got.Loads != 3 {
		t.Fatalf("load after gap baseline: %+v", got)
	}
}

// TestCollectorClockJumpDropsSamples: macOS monotonic time stops during
// sleep, so a pre-sleep reading would look seconds old after wake. Every
// reading is dropped when wall and monotonic time diverge by more than 1 s
// either way; drift within that is not a jump.
func TestCollectorClockJumpDropsSamples(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wall, mono time.Duration
		jump       bool
	}{
		{"slept 2 h", 2 * time.Hour, 2 * time.Second, true},
		{"wall 2 s ahead", 4 * time.Second, 2 * time.Second, true},
		{"wall stepped back", -time.Hour, 2 * time.Second, true},
		{"0.5 s drift", 2500 * time.Millisecond, 2 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := opsfixture.NewLlamaSwap(t)
			clk := newFakeClock()
			c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
			c.Tick(context.Background())
			f.SetStatus("/running", http.StatusInternalServerError) // the post-wake re-read fails
			clk.advance(tc.wall, tc.mono)
			obs := c.Tick(context.Background())
			b := obs.Backends[0]
			if (b.Running == nil) != tc.jump || b.Gaps != 1 || obs.TimingUncertain != tc.jump {
				t.Fatalf("clock jump: running=%v gaps=%d uncertain=%v, want jump=%v", b.Running, b.Gaps, obs.TimingUncertain, tc.jump)
			}
			clk.advance(2*time.Second, 2*time.Second)
			if c.Tick(context.Background()).TimingUncertain {
				t.Fatal("timing uncertainty must clear on the next clean tick")
			}
		})
	}
}

// TestCollectorLateTickOpensGap: a tick more than 3 intervals after the last
// opens a gap (spec §4.5), which also clears FirstObserved; exactly 3 does
// not.
func TestCollectorLateTickOpensGap(t *testing.T) {
	for _, tc := range []struct {
		late time.Duration
		gaps int
	}{{6 * time.Second, 0}, {7 * time.Second, 1}} {
		t.Run(tc.late.String(), func(t *testing.T) {
			f := opsfixture.NewLlamaSwap(t)
			clk := newFakeClock()
			c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
			f.SetBody("/running", `{"running":[{"model":"m","state":"ready","ttl":600}]}`)
			c.Tick(context.Background()) // baseline: m loaded
			f.SetBody("/running", `{"running":[]}`)
			clk.advance(tc.late, tc.late)
			b := c.Tick(context.Background()).Backends[0]
			if m := memory(t, b, "m"); b.Gaps != tc.gaps || m.Gaps != tc.gaps || !m.FirstObserved.IsZero() {
				t.Fatalf("late by %v: gaps=%d mem=%+v, want %d gaps and no FirstObserved", tc.late, b.Gaps, m, tc.gaps)
			}
		})
	}
}

func TestObservationsDroppedOnSkewSinceCollection(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	obs := c.Tick(context.Background())
	clk.advance(time.Hour, time.Second) // slept between collection and render
	if obs.SkewSince(clk.clock()) < time.Minute {
		t.Fatal("skew not detected")
	}
	d := obs.Dropped()
	if b := d.Backends[0]; b.Running != nil || b.Reachable != nil || !d.TimingUncertain || b.Kind != KindLlamaSwap {
		t.Fatalf("dropped = %+v", b)
	}
	if obs.Backends[0].Running == nil {
		t.Fatal("Dropped mutated its receiver")
	}
	full := Observations{Backends: []BackendObservation{{
		Reachable: &Sample[bool]{}, ReachCode: CodeTimeout, ReachRetry: time.Second, Running: &Sample[[]RunningModel]{},
		Rows: &Sample[[]ActivityRow]{}, Listed: &Sample[[]ListedModel]{}, PS: &Sample[[]PSModel]{},
	}}}
	if b := full.Dropped().Backends[0]; b.Reachable != nil || b.ReachCode != "" || b.ReachRetry != 0 || b.Running != nil || b.Rows != nil || b.Listed != nil || b.PS != nil {
		t.Fatalf("Dropped kept a reading: %+v", b)
	}
	ahead := Observations{CollectedAt: clk.clock().Wall(), CollectedMono: clk.clock().Mono()}
	clk.advance(time.Second, time.Hour)
	if ahead.SkewSince(clk.clock()) < time.Minute {
		t.Fatal("monotonic time running ahead of wall time not detected")
	}
}

func TestCollectorBackoffSequenceAndRetryTime(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	c.Tick(context.Background())
	f.SetStatus("/running", http.StatusInternalServerError)
	before := countURI(f, "/running")
	// Failures at +1 s (retry +3), +3 s (retry +7), +7 s.
	var last BackendObservation
	for _, wait := range []time.Duration{time.Second, time.Second, time.Second, 2 * time.Second, 2 * time.Second} {
		clk.advance(wait, wait)
		last = c.Tick(context.Background()).Backends[0]
	}
	if got := countURI(f, "/running") - before; got != 3 {
		t.Fatalf("running attempts = %d, want 3 under 2s,4s backoff", got)
	}
	if s := surface(last, "running"); s.NextAttempt != 7*time.Second+8*time.Second {
		t.Fatalf("NextAttempt = %v, want 15s (third failure at 7s, backoff 8s)", s.NextAttempt)
	}
	f.SetStatus("/running", 0)
	clk.advance(8*time.Second, 8*time.Second) // 15 s: the retry succeeds
	c.Tick(context.Background())
	f.SetStatus("/running", http.StatusInternalServerError)
	clk.advance(2*time.Second, 2*time.Second) // 17 s: fails again
	if s := surface(c.Tick(context.Background()).Backends[0], "running"); s.NextAttempt != 19*time.Second {
		t.Fatalf("NextAttempt = %v, want 19s: a success restarts backoff at 2 s", s.NextAttempt)
	}
}

func TestBackoffDoublesToThe30sCap(t *testing.T) {
	for n, want := range map[int]time.Duration{
		1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 4: 16 * time.Second,
		5: 30 * time.Second, 6: 30 * time.Second, 64: 30 * time.Second, 1 << 20: 30 * time.Second,
	} {
		if got := backoff(n); got != want {
			t.Fatalf("backoff(%d) = %v, want %v", n, got, want)
		}
	}
}

// TestCollectorCallerDeadlineIsUnclassified: a deadline the caller set is
// the caller's, like a cancellation: a request it cuts short is not the
// backend's timeout and starts no backoff.
func TestCollectorCallerDeadlineIsUnclassified(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	f.HoldFor(t, "/running")
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: newFakeClock().clock()})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	b := c.Tick(ctx).Backends[0]
	if s := surface(b, "running"); s.LastError != "" || s.NextAttempt != 0 || b.ReachCode != "" || b.Kind != KindLlamaSwap {
		t.Fatalf("caller deadline classified: running=%+v reach=%q kind=%s", s, b.ReachCode, b.Kind)
	}
}

func TestCollectorUnreachableRecordsRetry(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	release := f.HoldFor(t, "/running")
	defer release()
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	b := c.Tick(context.Background()).Backends[0] // /running times out at the 2 s client deadline
	if b.Reachable == nil || b.Reachable.Value || b.ReachCode != CodeTimeout || b.ReachRetry != 2*time.Second {
		t.Fatalf("timeout: reachable=%+v code=%q retry=%v", b.Reachable, b.ReachCode, b.ReachRetry)
	}
	// Demoted to unidentified, the backend is read through version alone:
	// /running's error and backoff schedule are not published.
	if len(b.Surfaces) != 1 || b.Surfaces[0].Name != "version" {
		t.Fatalf("demoted backend published surfaces it no longer polls: %+v", b.Surfaces)
	}
	// Recovery: the backend is identified again before it is observed, with
	// the clients built for it the first time.
	observe := c.backends[0].observe
	release()
	clk.advance(2*time.Second, 2*time.Second)
	b = c.Tick(context.Background()).Backends[0]
	if b.Kind != KindLlamaSwap || b.Reachable == nil || !b.Reachable.Value || c.backends[0].observe != observe {
		t.Fatalf("recovery: kind=%s reachable=%+v same observe client=%v", b.Kind, b.Reachable, c.backends[0].observe == observe)
	}
	assertRequests(t, f, reqVersion, reqRunning, reqVersion, reqRunning, reqMetrics, reqModels)
}

func TestCollectorCallerCancellationIsUnclassifiedWithoutBackoff(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	release := f.HoldFor(t, "/running")
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: newFakeClock().clock()})
	ctx, cancel := context.WithCancel(context.Background())
	canceled := make(chan struct{})
	go func() {
		defer close(canceled)
		defer cancel() // bounded: cancels after 2 s even if the request never arrives
		deadline := time.Now().Add(2 * time.Second)
		for f.InflightOn("/running") == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}()
	b := c.Tick(ctx).Backends[0]
	<-canceled
	if s := surface(b, "running"); s.LastError != "" || s.NextAttempt != 0 || b.ReachCode != "" {
		t.Fatalf("cancellation was classified: running=%+v reach=%q", s, b.ReachCode)
	}
	release()
	before := countURI(f, "/running")
	c.Tick(context.Background()) // same instant: no backoff may apply
	if countURI(f, "/running") != before+1 {
		t.Fatal("caller cancellation started a backoff")
	}
}

func TestCollectorTickIsSerialized(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	release := f.HoldFor(t, "/running")
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: newFakeClock().clock()})
	done := make(chan struct{}, 2)
	start := make(chan struct{})
	for range 2 {
		go func() { <-start; c.Tick(context.Background()); done <- struct{}{} }()
	}
	close(start) // both contenders race for the collector together
	waitFor(t, time.Second, func() bool { return f.InflightOn("/running") >= 1 })
	// An unserialized second tick reaches the same held route within this
	// window; a serialized one is blocked on the collector's lock.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := f.InflightOn("/running"); n > 1 {
			t.Fatalf("concurrent /running requests = %d, want 1", n)
		}
		time.Sleep(time.Millisecond)
	}
	release()
	for range 2 {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("ticks did not finish")
		}
	}
}

// TestCollectorSnapshotsAreDetached: writing through every slice and pointer
// of a returned snapshot, or of Dropped's result, leaves the collector (and
// the receiver) unchanged.
func TestCollectorSnapshotsAreDetached(t *testing.T) {
	asJSON := func(o Observations) string {
		t.Helper()
		b, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	f.SetBody("/running", `{"running":[]}`)
	c.Tick(context.Background())
	f.SetBody("/running", `{"running":[{"model":"m","state":"ready","ttl":600}]}`)
	clk.advance(2*time.Second, 2*time.Second)
	obs := c.Tick(context.Background())
	want := asJSON(c.snapshot())
	b := &obs.Backends[0]
	b.Reachable.Value, b.Reachable.Mono = false, 999
	b.Running.Value[0].Model, b.Running.Mono = "x", 999
	b.Rows.Value[0].Model, b.Rows.Mono = "x", 999
	b.Listed.Value[0].ID, b.Listed.Mono = "x", 999
	b.Surfaces[0].Name = "x"
	b.Models[0].Model = "x"
	b.Models[0].Transitions[0].To = "x"
	b.Models[0].Transitions = append(b.Models[0].Transitions[:1], Transition{To: "y"})
	if got := asJSON(c.snapshot()); got != want {
		t.Fatalf("collector state changed through a snapshot:\n got %s\nwant %s", got, want)
	}

	recv := c.snapshot()
	keep := asJSON(recv)
	d := recv.Dropped()
	d.Backends[0].Surfaces[0].Name = "x"
	d.Backends[0].Models[0].Transitions[0].To = "x"
	if asJSON(recv) != keep {
		t.Fatal("Dropped result aliases its receiver")
	}

	o := opsfixture.NewOllama(t, `{"models":[{"name":"llama3:latest","model":"llama3:latest","size_vram":1,"expires_at":"2026-10-05T10:05:00Z"}]}`)
	oc := NewCollector([]BackendSpec{{Provider: "ollama", BaseURL: o.URL(), APIFormat: "ollama"}}, Options{Clock: newFakeClock().clock()})
	oobs := oc.Tick(context.Background())
	owant := asJSON(oc.snapshot())
	oobs.Backends[0].PS.Value[0].Name, oobs.Backends[0].PS.Mono = "x", 999
	if asJSON(oc.snapshot()) != owant {
		t.Fatal("Ollama ps sample shares storage with collector state")
	}
}

func TestCollectorPeerListingFailsClosed(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	if c.Tick(context.Background()).Backends[0].Listed == nil {
		t.Fatal("valid listing not recorded")
	}
	f.SetBody("/v1/models", `{"data":null}`)
	clk.advance(2*time.Second, 2*time.Second)
	if b := c.Tick(context.Background()).Backends[0]; b.Listed != nil {
		t.Fatalf("a failed listing must clear the previous one (rows then confirm nothing): %+v", b.Listed)
	}
}

// TestCollectorReclassificationClearsSamples covers the three answers spec
// §4.5 names: another version, another runtime, a 404.
func TestCollectorReclassificationClearsSamples(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		support    string
		version    string
	}{
		{"other version", `{"build_date":"x","commit":"y","version":"v240"}`, 0, SupportUnsupported, "v240"},
		{"other runtime", `{"version":"0.12.3"}`, 0, SupportUnrecognized, ""},
		{"404", "", http.StatusNotFound, SupportUnrecognized, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel() // each waits out the 2 s client deadline
			f := opsfixture.NewLlamaSwap(t)
			clk := newFakeClock()
			c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
			c.Tick(context.Background())
			release := f.HoldFor(t, "/running")
			clk.advance(2*time.Second, 2*time.Second)
			c.Tick(context.Background()) // /running times out: unidentified
			release()
			if tc.status != 0 {
				f.SetStatus("/api/version", tc.status)
			} else {
				f.SetBody("/api/version", tc.body)
			}
			clk.advance(30*time.Second, 30*time.Second)
			b := c.Tick(context.Background()).Backends[0]
			if b.Support != tc.support || b.Version != tc.version || b.Running != nil || b.Rows != nil || b.Listed != nil {
				t.Fatalf("v235 samples or version survived reclassification: %+v", b)
			}
		})
	}
}

func TestCollectorAdmissionIsDeterministicAndCapped(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	var entries []string
	for i := range 300 {
		entries = append(entries, fmt.Sprintf(`{"model":"m%03d","state":"ready","ttl":600}`, i))
	}
	f.SetBody("/running", `{"running":[`+strings.Join(entries, ",")+`]}`)
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Clock: newFakeClock().clock()})
	b := c.Tick(context.Background()).Backends[0]
	if len(b.Models) != 256 || b.ModelsOverflow != 44 || b.Models[0].Model != "m000" || b.Models[255].Model != "m255" {
		t.Fatalf("tracked=%d overflow=%d first=%s last=%s", len(b.Models), b.ModelsOverflow, b.Models[0].Model, b.Models[len(b.Models)-1].Model)
	}
	if b = c.Tick(context.Background()).Backends[0]; b.ModelsOverflow != 44 {
		t.Fatalf("overflow after the same sample again = %d, want 44: the same models count once", b.ModelsOverflow)
	}
	f.SetBody("/running", `{"running":[]}`)
	if b = c.Tick(context.Background()).Backends[0]; b.ModelsOverflow != 44 {
		t.Fatalf("overflow after an empty sample = %d, want 44: a dropped model stays untracked", b.ModelsOverflow)
	}
}

// TestCollectorDoesNotTrackOverlongNames bounds residency memory by name
// size as well as count: a name over MaxNameLen bytes is never admitted and
// reads as overflow, so its loads read as not measured.
func TestCollectorDoesNotTrackOverlongNames(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	longest, over := strings.Repeat("a", MaxNameLen), strings.Repeat("b", MaxNameLen+1)
	f.SetBody("/running", `{"running":[{"model":"`+longest+`","state":"ready"},{"model":"`+over+`","state":"ready"},{"model":"m","state":"ready"}]}`)
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Clock: newFakeClock().clock()})
	b := c.Tick(context.Background()).Backends[0]
	if len(b.Models) != 2 || b.Models[0].Model != longest || b.Models[1].Model != "m" || b.ModelsOverflow != 1 {
		t.Fatalf("tracked=%d overflow=%d", len(b.Models), b.ModelsOverflow)
	}
}

func TestCollectorOllama(t *testing.T) {
	// Memory is keyed by NormalizeOllama("Llama3") = "llama3:latest".
	f := opsfixture.NewOllama(t, `{"models":[{"name":"Llama3","model":"Llama3","size_vram":1,"expires_at":"2026-10-05T10:05:00Z"}]}`)
	c := NewCollector([]BackendSpec{{Provider: "ollama", BaseURL: f.URL(), APIFormat: "ollama"}}, Options{Clock: newFakeClock().clock()})
	b := c.Tick(context.Background()).Backends[0]
	if b.Kind != KindOllama || b.PS == nil || len(b.PS.Value) != 1 {
		t.Fatalf("ollama = %+v", b)
	}
	// Spec §4.4: absence from /api/ps is not proof of unload.
	f.SetBody("/api/ps", `{"models":[]}`)
	b = c.Tick(context.Background()).Backends[0]
	if tr := memory(t, b, "llama3:latest").Transitions; len(tr) != 1 || tr[0].To != "unknown" {
		t.Fatalf("an Ollama model leaving /api/ps must read unknown, never unloaded: %+v", tr)
	}
	assertRequests(t, f, reqPS, reqPS)
}

// TestCollectorOllamaOutageAndAnswer: an unreachable Ollama stays an Ollama
// (it has no identification to redo), and any HTTP answer reads reachable.
func TestCollectorOllamaOutageAndAnswer(t *testing.T) {
	f := opsfixture.NewOllama(t, `{"models":[]}`)
	var down atomic.Bool
	down.Store(true)
	base := front(t, f, func(w http.ResponseWriter, _ *http.Request) bool {
		if down.Load() {
			hangUp(w)
			return true
		}
		return false
	})
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{{Provider: "ollama", BaseURL: base, APIFormat: "ollama"}}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	b := c.Tick(context.Background()).Backends[0]
	if b.Kind != KindOllama || b.Reachable == nil || b.Reachable.Value || b.ReachCode != CodeUnreachable {
		t.Fatalf("outage: kind=%s reachable=%+v code=%q", b.Kind, b.Reachable, b.ReachCode)
	}
	down.Store(false)
	f.SetStatus("/api/ps", http.StatusInternalServerError)
	clk.advance(2*time.Second, 2*time.Second)
	b = c.Tick(context.Background()).Backends[0]
	if b.Kind != KindOllama || b.Reachable == nil || !b.Reachable.Value || b.ReachCode != "" || surface(b, "ps").LastError != CodeHTTPStatus {
		t.Fatalf("status answer: kind=%s reachable=%+v code=%q ps=%+v", b.Kind, b.Reachable, b.ReachCode, surface(b, "ps"))
	}
}

// TestCollectorSendsTheAPIKeyOnEveryRequest: the identify, observe (which
// also re-checks identity) and Ollama clients all carry the api_key, and it
// never reaches a snapshot.
func TestCollectorSendsTheAPIKeyOnEveryRequest(t *testing.T) {
	ls := opsfixture.NewLlamaSwap(t)
	ol := opsfixture.NewOllama(t, `{"models":[]}`)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{
		{Provider: "llamacpp", BaseURL: ls.URL(), APIFormat: "openai-compat", APIKey: opsfixture.SentinelAPIKey},
		{Provider: "ollama", BaseURL: ol.URL(), APIFormat: "ollama", APIKey: opsfixture.SentinelAPIKey},
	}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	c.Tick(context.Background())
	clk.advance(2*time.Second, 2*time.Second)
	assertNoSentinels(t, c.Tick(context.Background()))
	auth := append(ls.Auth(), ol.Auth()...)
	if len(auth) != 10 || slices.ContainsFunc(auth, func(a string) bool { return a != "Bearer "+opsfixture.SentinelAPIKey }) {
		t.Fatalf("Authorization = %q, want 10 bearer headers (8 llama-swap, 2 Ollama)", auth)
	}
}

// TestCollectorKeepsTheLast20Transitions: the display list is capped at 20,
// newest last; the load counter is not capped.
func TestCollectorKeepsTheLast20Transitions(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	var b BackendObservation
	for i := range 26 { // baseline, then 25 transitions: loaded, unloaded, ...
		body := `{"running":[]}`
		if i%2 == 1 {
			body = `{"running":[{"model":"m","state":"ready","ttl":600}]}`
		}
		f.SetBody("/running", body)
		clk.advance(2*time.Second, 2*time.Second)
		b = c.Tick(context.Background()).Backends[0]
	}
	m := memory(t, b, "m")
	if tr := m.Transitions; len(tr) != 20 || m.Loads != 13 || tr[0].To != "unloaded" || tr[19].To != "loaded" || !tr[19].At.Equal(b.Running.At) {
		t.Fatalf("transitions=%d loads=%d first=%+v last=%+v", len(tr), m.Loads, tr[0], tr[len(tr)-1])
	}
}

// TestCollectorMissedResidencyReadOpensGap: spec §4.5 opens a gap when a
// residency read fails. A read the tick deadline skipped, or one the caller
// cancelled in flight, is no sample either, so no transition is inferred
// across it; cancellation still sets no backoff and no error.
func TestCollectorMissedResidencyReadOpensGap(t *testing.T) {
	for _, tc := range []struct {
		name string
		miss func(t *testing.T, f *opsfixture.Server, c *Collector)
	}{
		{"failed", func(t *testing.T, f *opsfixture.Server, c *Collector) {
			f.SetStatus("/running", http.StatusInternalServerError)
			c.Tick(context.Background())
			f.SetStatus("/running", 0)
		}},
		{"skipped", func(t *testing.T, f *opsfixture.Server, c *Collector) {
			spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			c.Tick(spent) // on schedule, but no budget left: /running is never read
		}},
		{"cancelled in flight", func(t *testing.T, f *opsfixture.Server, c *Collector) {
			release := f.HoldFor(t, "/running")
			defer release()
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				defer cancel() // bounded: cancels after 2 s even if the request never arrives
				deadline := time.Now().Add(2 * time.Second)
				for f.InflightOn("/running") == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}()
			b := c.Tick(ctx).Backends[0]
			if s := surface(b, "running"); s.LastError != "" || s.NextAttempt != 0 {
				t.Fatalf("cancellation was classified: %+v", s)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := opsfixture.NewLlamaSwap(t)
			clk := newFakeClock()
			c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
			f.SetBody("/running", `{"running":[]}`)
			c.Tick(context.Background()) // baseline, nothing loaded
			clk.advance(2*time.Second, 2*time.Second)
			tc.miss(t, f, c)
			f.SetBody("/running", `{"running":[{"model":"m","state":"ready","ttl":600}]}`)
			clk.advance(2*time.Second, 2*time.Second)
			b := c.Tick(context.Background()).Backends[0]
			if got := memory(t, b, "m"); got.Loads != 0 || b.Gaps != 1 || b.Periods != 2 {
				t.Fatalf("load inferred across a missed read: mem=%+v gaps=%d periods=%d", got, b.Gaps, b.Periods)
			}
		})
	}
}

// TestCollectorTransitions: transitions use the console vocabulary and the
// tick's time, unloading back to loaded is a new episode (spec §5.4), and
// FirstObserved is set exactly while the model reads loaded.
func TestCollectorTransitions(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	var ats []time.Time
	var b BackendObservation
	for i, st := range []string{"", "starting", "ready", "stopping", "ready", "starting", "ready", ""} {
		body := `{"running":[]}`
		if st != "" {
			body = `{"running":[{"model":"m","state":"` + st + `","ttl":600}]}`
		}
		f.SetBody("/running", body)
		clk.advance(2*time.Second, 2*time.Second)
		b = c.Tick(context.Background()).Backends[0]
		if i == 0 {
			continue // baseline
		}
		ats = append(ats, b.Running.At)
		m := memory(t, b, "m")
		if loaded := st == "ready"; loaded == m.FirstObserved.IsZero() || (loaded && !m.FirstObserved.Equal(b.Running.At)) {
			t.Fatalf("step %d (%q): FirstObserved = %v", i, st, m.FirstObserved)
		}
	}
	m := memory(t, b, "m")
	var got []string
	for i, tr := range m.Transitions {
		got = append(got, tr.To)
		if !tr.At.Equal(ats[i]) {
			t.Fatalf("transition %d at %v, want %v", i, tr.At, ats[i])
		}
	}
	if want := "loading,loaded,unloading,loaded,loading,loaded,unloaded"; strings.Join(got, ",") != want || m.Loads != 2 {
		t.Fatalf("transitions = %s loads=%d, want %s loads=2", strings.Join(got, ","), m.Loads, want)
	}
}

// TestCollectorListingCountsOnlyOnItsTick: peer confirmation fails closed
// (spec §5.4), so a listing from an earlier tick never survives a tick that
// did not read /v1/models, here because the identity re-check was
// unreachable and the llama-swap surfaces were never polled.
func TestCollectorListingCountsOnlyOnItsTick(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	var down atomic.Bool
	base := front(t, f, func(w http.ResponseWriter, r *http.Request) bool {
		if down.Load() && r.URL.Path == "/api/version" {
			hangUp(w)
			return true
		}
		return false
	})
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(base)}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	if c.Tick(context.Background()).Backends[0].Listed == nil {
		t.Fatal("valid listing not recorded")
	}
	down.Store(true)
	clk.advance(2*time.Second, 2*time.Second)
	if b := c.Tick(context.Background()).Backends[0]; b.ReachCode != CodeUnreachable || b.Listed != nil {
		t.Fatalf("a listing not read on this tick survived: reach=%q listed=%+v", b.ReachCode, b.Listed)
	}
}

// TestBoundKeepsWholeRunes: a backend-sourced version is cut to
// maxVersionLen bytes without splitting a UTF-8 sequence.
func TestBoundKeepsWholeRunes(t *testing.T) {
	for in, want := range map[string]string{
		"v235":                                "v235",
		strings.Repeat("a", 70):               strings.Repeat("a", maxVersionLen),
		strings.Repeat("a", 63) + "\u00e9xyz": strings.Repeat("a", 63),
		strings.Repeat("a", 62) + "\u20acxyz": strings.Repeat("a", 62),
	} {
		if got := bound(in); got != want || !utf8.ValidString(got) {
			t.Fatalf("bound(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCollectorUnstartedRequestIsNotAnObservation: a request is not started
// once the tick deadline has passed, or when less than one request timeout
// of the tick is left (it could be cut short by a budget its neighbors
// spent), and nothing is recorded against the backend: a timeout would raise
// a false backend_unreachable for a backend that was never contacted.
func TestCollectorUnstartedRequestIsNotAnObservation(t *testing.T) {
	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		budget time.Duration // tick time left
	}{
		{"tick deadline passed", spent, TickTimeout},
		{"under one request timeout left", context.Background(), RequestTimeout - 100*time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := opsfixture.NewLlamaSwap(t)
			c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: newFakeClock().clock()})
			c.tickEnd = time.Now().Add(tc.budget)
			c.lastWall, c.lastMono = c.now() // as Tick leaves them: no jump
			b := c.backends[0]
			if c.fetch(context.Background(), tc.ctx, b, b.identify, "version", "/api/version", versionLimit, func([]byte) error { return nil }) {
				t.Fatal("fetch without the budget for a request applied a sample")
			}
			o := c.snapshot().Backends[0]
			if s := surface(o, "version"); s.LastError != "" || s.NextAttempt != 0 || o.Reachable != nil || o.ReachCode != "" {
				t.Fatalf("an unstarted request was recorded: version=%+v reachable=%+v code=%q", s, o.Reachable, o.ReachCode)
			}
			assertRequests(t, f)
		})
	}
}

// TestCollectorTickBudgetIsNotBlamedOnTheLastBackend: two hung neighbors
// spend 4 s of the 5 s tick. A healthy backend that answers /running in
// 1.2 s is not started, rather than cut short by the tick deadline and read
// as down.
func TestCollectorTickBudgetIsNotBlamedOnTheLastBackend(t *testing.T) {
	hung := func() string {
		f := opsfixture.NewLlamaSwap(t)
		f.HoldFor(t, "/api/version")
		return f.URL()
	}
	ok := opsfixture.NewLlamaSwap(t)
	slow := front(t, ok, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/running" {
			select {
			case <-time.After(1200 * time.Millisecond):
			case <-r.Context().Done():
			}
		}
		return false
	})
	c := NewCollector([]BackendSpec{
		{Provider: "a1-hung", BaseURL: hung(), APIFormat: "openai-compat"},
		{Provider: "a2-hung", BaseURL: hung(), APIFormat: "openai-compat"},
		{Provider: "c-slow", BaseURL: slow, APIFormat: "openai-compat"},
	}, Options{Interval: 2 * time.Second, Clock: newFakeClock().clock()})
	b := c.Tick(context.Background()).Backends[2]
	if b.Reachable != nil || b.ReachCode != "" || slices.ContainsFunc(b.Surfaces, func(s Surface) bool { return s.LastError != "" }) {
		t.Fatalf("a backend the tick budget never reached was read as down: reachable=%+v code=%q surfaces=%+v", b.Reachable, b.ReachCode, b.Surfaces)
	}
	assertRequests(t, ok)
}

// TestCollectorSleepDuringTickDropsReadings: the lid closes after /running
// is read and opens before /api/metrics answers, so wall time moves 3 h
// inside one tick while monotonic time does not.
func TestCollectorSleepDuringTickDropsReadings(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	var once sync.Once
	base := front(t, f, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/metrics" {
			once.Do(func() { clk.advance(3*time.Hour, 0) })
		}
		return false
	})
	c := NewCollector([]BackendSpec{lsSpec(base)}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	obs := c.Tick(context.Background())
	if b := obs.Backends[0]; !obs.TimingUncertain || b.Running != nil {
		t.Fatalf("sleep inside a tick: uncertain=%v running=%+v", obs.TimingUncertain, b.Running)
	}
	clk.advance(2*time.Second, 2*time.Second)
	if c.Tick(context.Background()).TimingUncertain {
		t.Fatal("a jump inside one tick was counted again on the next")
	}
}

// TestCollectorNoTransitionAcrossAnInTickJump: the lid closes during the
// identity re-check and /running is read after wake. Comparing that sample
// with the pre-sleep one would infer a load across the gap (spec §4.5), so
// a sample that arrives after an in-tick jump is not applied.
func TestCollectorNoTransitionAcrossAnInTickJump(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	var armed atomic.Bool
	base := front(t, f, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/version" && armed.CompareAndSwap(true, false) {
			clk.advance(3*time.Hour, 0)
		}
		return false
	})
	c := NewCollector([]BackendSpec{lsSpec(base)}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	f.SetBody("/running", `{"running":[]}`)
	c.Tick(context.Background()) // baseline, nothing loaded
	armed.Store(true)
	f.SetBody("/running", `{"running":[{"model":"m","state":"ready","ttl":600}]}`)
	clk.advance(2*time.Second, 2*time.Second)
	obs := c.Tick(context.Background())
	if !obs.TimingUncertain {
		t.Fatal("jump inside the tick not detected")
	}
	for _, m := range obs.Backends[0].Models {
		if m.Loads != 0 || len(m.Transitions) != 0 {
			t.Fatalf("transition inferred across an in-tick jump: %+v", m)
		}
	}
}

// TestCollectorReachabilityIsSteadyBetweenRetries: llama-swap answers
// /api/version but /running fails at the dial level. Once the backend reads
// unreachable nothing is sent to it before its retry time, so reachability
// reads unreachable on every tick between retries (spec §5.4) instead of
// flipping to ok on surfaces /running's backoff does not cover.
func TestCollectorReachabilityIsSteadyBetweenRetries(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	var sent atomic.Int64
	base := front(t, f, func(w http.ResponseWriter, r *http.Request) bool {
		sent.Add(1)
		if r.URL.Path == "/running" {
			hangUp(w)
			return true
		}
		return false
	})
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(base)}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	// Failures at 0 s (retry 2 s) and 2 s (retry 6 s); retried again at 6 s.
	retryTicks := map[time.Duration]bool{0: true, 2 * time.Second: true, 6 * time.Second: true}
	for range 8 {
		now := clk.clock().Mono()
		before := sent.Load()
		b := c.Tick(context.Background()).Backends[0]
		if b.Reachable == nil || b.Reachable.Value || b.ReachCode != CodeUnreachable || b.ReachRetry <= now {
			t.Fatalf("tick at %v: reachable=%+v code=%q retry=%v, want unreachable with a later retry", now, b.Reachable, b.ReachCode, b.ReachRetry)
		}
		if n := sent.Load() - before; (n > 0) != retryTicks[now] {
			t.Fatalf("tick at %v sent %d requests; want traffic only at retry times 0s, 2s, 6s", now, n)
		}
		clk.advance(time.Second, time.Second)
	}
}

// TestCollectorRechecksIdentityEveryTick: a llama-swap upgraded or replaced
// between two ticks is never seen down, so its version is read on every
// tick after the one that identified it.
func TestCollectorRechecksIdentityEveryTick(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		support    string
		version    string
	}{
		{"upgrade restart", `{"build_date":"x","commit":"y","version":"v240"}`, 0, SupportUnsupported, "v240"},
		{"llama-server took the port", "", http.StatusNotFound, SupportUnrecognized, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := opsfixture.NewLlamaSwap(t)
			clk := newFakeClock()
			c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
			c.Tick(context.Background())
			if tc.status != 0 {
				f.SetStatus("/api/version", tc.status)
			} else {
				f.SetBody("/api/version", tc.body)
			}
			clk.advance(2*time.Second, 2*time.Second)
			b := c.Tick(context.Background()).Backends[0]
			if b.Kind != KindNone || b.Support != tc.support || b.Version != tc.version || b.Running != nil || b.Rows != nil || b.Listed != nil {
				t.Fatalf("v235 readings survived a new identity: %+v", b)
			}
			// Never read again, it publishes only the answer that classified it.
			if len(b.Surfaces) != 1 || b.Surfaces[0].Name != "version" {
				t.Fatalf("a backend no kind reads published %+v, want version alone", b.Surfaces)
			}
			assertRequests(t, f, reqVersion, reqRunning, reqMetrics, reqModels, reqVersion)
		})
	}
}

func surface(b BackendObservation, name string) Surface {
	for _, s := range b.Surfaces {
		if s.Name == name {
			return s
		}
	}
	return Surface{Name: "missing", LastError: "missing"}
}

// memory requires the row to exist before its state is asserted.
func memory(t *testing.T, b BackendObservation, model string) ModelMemory {
	t.Helper()
	for _, m := range b.Models {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("no memory for %q in %+v", model, b.Models)
	return ModelMemory{}
}
