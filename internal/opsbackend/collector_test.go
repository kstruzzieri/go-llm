package opsbackend

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
	if b.Kind != KindLlamaSwap || b.Support != SupportSupported || b.Version != "v235" || b.Running == nil || len(b.Running.Value) != 1 {
		t.Fatalf("backend = %+v", b)
	}
	assertNoSentinels(t, obs)
}

func TestCollectorRemoteAndInvalidAreNeverContacted(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	c := NewCollector([]BackendSpec{
		{Provider: "a-remote", BaseURL: "https://api.example.com", APIFormat: "openai-compat"},
		{Provider: "b-prefix", BaseURL: f.URL() + "/upstream/gemma4:31b", APIFormat: "openai-compat"},
		{Provider: "c-hosted", BaseURL: "https://opencode.ai/zen/go", APIFormat: "openai-compat"},
		{Provider: "d-format", BaseURL: f.URL(), APIFormat: "anthropic"},
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
}

func TestCollectorVersionClassification(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		support    string
		version    string
	}{
		{"other llama-swap", `{"build_date":"x","commit":"y","version":"v240"}`, 0, SupportUnsupported, "v240"},
		{"ollama-shaped", `{"version":"0.12.3"}`, 0, SupportUnrecognized, ""},
		{"404 runtime", "", http.StatusNotFound, SupportUnrecognized, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := opsfixture.NewLlamaSwap(t)
			if tc.status != 0 {
				f.SetStatus("/api/version", tc.status)
			} else {
				f.SetBody("/api/version", tc.body)
			}
			c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Clock: newFakeClock().clock()})
			b := c.Tick(context.Background()).Backends[0]
			if b.Kind != KindNone || b.Support != tc.support || b.Version != tc.version {
				t.Fatalf("backend = %+v requests = %v", b, f.Requests())
			}
			assertRequests(t, f, reqVersion)
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

	step(none)       // baseline, nothing tracked
	step(starting)   // episode 1 begins
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
	if got := memory(t, b, "m"); got.Loads != 2 || got.Gaps != 1 || b.Gaps != 1 || b.Periods != 2 {
		t.Fatalf("gap handling: mem=%+v gaps=%d periods=%d", got, b.Gaps, b.Periods)
	}
	step(none)
	b = step(ready)
	if got := memory(t, b, "m"); got.Loads != 3 {
		t.Fatalf("load after gap baseline: %+v", got)
	}
}

// TestCollectorClockJumpDropsSamples: macOS monotonic time stops during
// sleep, so a pre-sleep reading would look seconds old after wake. Every
// reading is dropped when wall and monotonic time diverge.
func TestCollectorClockJumpDropsSamples(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	c.Tick(context.Background())
	f.SetStatus("/running", http.StatusInternalServerError) // the post-wake re-read fails
	clk.advance(2*time.Hour, 2*time.Second)
	obs := c.Tick(context.Background())
	b := obs.Backends[0]
	if b.Running != nil || b.Gaps != 1 || !obs.TimingUncertain {
		t.Fatalf("clock jump: running=%v gaps=%d uncertain=%v", b.Running, b.Gaps, obs.TimingUncertain)
	}
	clk.advance(2*time.Second, 2*time.Second)
	if c.Tick(context.Background()).TimingUncertain {
		t.Fatal("timing uncertainty must clear on the next clean tick")
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

func TestCollectorSnapshotsAreDetached(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	clk := newFakeClock()
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: clk.clock()})
	f.SetBody("/running", `{"running":[]}`)
	c.Tick(context.Background())
	f.SetBody("/running", `{"running":[{"model":"m","state":"ready","ttl":600}]}`)
	clk.advance(2*time.Second, 2*time.Second)
	obs := c.Tick(context.Background())
	obs.Backends[0].Running.Value[0].Model = "mutated"
	obs.Backends[0].Models[0].Transitions[0].To = "mutated"
	// Keep the cached running sample: the next read fails, so the collector
	// must still hold its own unmutated copy.
	f.SetStatus("/running", http.StatusInternalServerError)
	clk.advance(2*time.Second, 2*time.Second)
	next := c.Tick(context.Background()).Backends[0]
	if next.Running.Value[0].Model != "m" || memory(t, next, "m").Transitions[0].To == "mutated" {
		t.Fatalf("snapshot shares storage with collector state: %+v", next)
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
}

// TestCollectorDoesNotTrackOverlongNames bounds residency memory by name
// size as well as count: a name over maxNameLen bytes is never admitted and
// reads as overflow, so its loads read as not measured.
func TestCollectorDoesNotTrackOverlongNames(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	longest, over := strings.Repeat("a", maxNameLen), strings.Repeat("b", maxNameLen+1)
	f.SetBody("/running", `{"running":[{"model":"`+longest+`","state":"ready"},{"model":"`+over+`","state":"ready"},{"model":"m","state":"ready"}]}`)
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Clock: newFakeClock().clock()})
	b := c.Tick(context.Background()).Backends[0]
	if len(b.Models) != 2 || b.Models[0].Model != longest || b.Models[1].Model != "m" || b.ModelsOverflow != 1 {
		t.Fatalf("tracked=%d overflow=%d", len(b.Models), b.ModelsOverflow)
	}
}

func TestCollectorOllama(t *testing.T) {
	f := opsfixture.NewOllama(t, `{"models":[{"name":"llama3:latest","model":"llama3:latest","size_vram":1,"expires_at":"2026-10-05T10:05:00Z"}]}`)
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

// TestCollectorUnstartedRequestIsNotAnObservation: once the tick deadline
// has passed, a backend's request never starts, and nothing is recorded
// against it; a timeout would raise a false backend_unreachable for a
// backend that was never contacted.
func TestCollectorUnstartedRequestIsNotAnObservation(t *testing.T) {
	f := opsfixture.NewLlamaSwap(t)
	c := NewCollector([]BackendSpec{lsSpec(f.URL())}, Options{Interval: 2 * time.Second, Clock: newFakeClock().clock()})
	spent, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	b := c.backends[0]
	if c.fetch(context.Background(), spent, b, b.identify, "version", "/api/version", versionLimit, func([]byte) error { return nil }) {
		t.Fatal("fetch past the tick deadline applied a sample")
	}
	o := c.snapshot().Backends[0]
	if s := surface(o, "version"); s.LastError != "" || s.NextAttempt != 0 || o.Reachable != nil || o.ReachCode != "" {
		t.Fatalf("an unstarted request was recorded: version=%+v reachable=%+v code=%q", s, o.Reachable, o.ReachCode)
	}
	assertRequests(t, f)
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
