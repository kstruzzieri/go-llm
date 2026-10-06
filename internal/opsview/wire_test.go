package opsview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsbackend"
	"github.com/kstruzzieri/go-llm/internal/opsfixture"
)

func TestEveryReasonHasALabel(t *testing.T) {
	for _, code := range []string{
		ReasonStale, ReasonUnconfirmed, ReasonRemote, ReasonNoInflight, ReasonOllamaNoInflight,
		ReasonOllamaAbsent, ReasonNoSample, ReasonUnknownProvider, ReasonNotObserved,
		ReasonNoHistory, ReasonTooFewSamples, ReasonNoValues,
		CovEmpty, CovNoncontiguous, CovEvicted, CovRetentionStartUnknown, CovWithinRetained,
	} {
		if Label(code) == code {
			t.Fatalf("reason %q has no human label", code)
		}
	}
	if Label("not-a-code") != "not-a-code" {
		t.Fatal("unknown codes must pass through unchanged")
	}
}

func TestStamp(t *testing.T) {
	if stamp(time.Time{}) != nil {
		t.Fatal("zero time must be null")
	}
	at := time.Date(2026, 10, 5, 6, 7, 8, 9_000_000, time.FixedZone("EDT", -4*3600))
	if got := *stamp(at); got != "2026-10-05T10:07:08.009Z" {
		t.Fatalf("stamp = %q", got)
	}
	// RFC3339 has four-digit years: a UTC year outside 0000-9999 is null.
	for _, c := range []struct {
		at   time.Time
		want string // "" means null
	}{
		{time.Date(9999, 12, 31, 23, 0, 0, 0, time.FixedZone("", -2*3600)), ""}, // 10000-01-01T01:00Z
		{time.Date(0, 1, 1, 0, 0, 0, 0, time.FixedZone("", 14*3600)), ""},       // -0001-12-31T10:00Z
		{time.Date(9999, 12, 31, 23, 59, 59, 999_000_000, time.UTC), "9999-12-31T23:59:59.999Z"},
		{time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC), "0000-01-01T00:00:00.000Z"},
	} {
		if got := deref(stamp(c.at)); got != c.want {
			t.Fatalf("stamp(%v) = %q, want %q (empty means null)", c.at, got, c.want)
		}
	}
	if deref(nil) != "" || strPtr("") != nil {
		t.Fatal("nil/empty helpers")
	}
}

func TestFreshness(t *testing.T) {
	for _, mode := range []Mode{ModeWatch, ModeServe} {
		b := builder{in: Input{Mode: mode, Interval: 2 * time.Second, NowMono: 20 * time.Second}}
		if !b.fresh(14*time.Second) || b.fresh(13*time.Second) {
			t.Fatalf("%s: positive readings: fresh within 3 x interval only", mode)
		}
		// A failure stays fresh until its scheduled retry has had time to
		// answer and be published: retry + interval (2 s) + two ticks
		// (2 x 5 s) + 1 s. NowMono 20 s is exactly that bound for a retry at 7 s.
		if !b.failureFresh(7*time.Second) || b.failureFresh(7*time.Second-time.Millisecond) {
			t.Fatalf("%s: failure readings stay fresh through retry + interval + two ticks + 1 s, and no longer", mode)
		}
	}
	once := builder{in: Input{Mode: ModeOnce, Interval: 2 * time.Second, NowMono: time.Hour}}
	if !once.fresh(0) || !once.failureFresh(0) {
		t.Fatal("one-shot readings come from the run itself")
	}
}

func TestEnvelope(t *testing.T) {
	b := builder{in: Input{Mode: ModeWatch, Interval: 2 * time.Second, NowMono: 20 * time.Second}}
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	if e := b.envelope("src", at, 14*time.Second); e.Stale || *e.AgeMs != 6000 || deref(e.ObservedAt) != "2026-10-05T10:00:00.000Z" || e.Source != "src" {
		t.Fatalf("fresh envelope = %+v age=%d", e, *e.AgeMs)
	}
	if e := b.envelope("src", at, 13*time.Second); !e.Stale || *e.AgeMs != 7000 {
		t.Fatalf("aged-out envelope = %+v age=%d, want stale at 7000 ms", e, *e.AgeMs)
	}
	if e := b.envelope("src", at, 21*time.Second); *e.AgeMs != 0 {
		t.Fatalf("reading after NowMono: age = %d ms, want 0", *e.AgeMs)
	}
}

// TestVocabularyLiterals pins every wire enum to its spelling in spec §5. The
// constants are the v1 contract, so a typo is a contract break.
func TestVocabularyLiterals(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{string(ModeOnce), "once"}, {string(ModeWatch), "watch"}, {string(ModeServe), "serve"},
		{StateLoaded, "loaded"}, {StateLoading, "loading"}, {StateUnloading, "unloading"},
		{StateUnloaded, "unloaded"}, {StateUnknown, "unknown"}, {StateNotObserved, "not_observed"},
		{ReachOK, "ok"}, {ReachUnreachable, "unreachable"},
		{ReasonStale, "stale"}, {ReasonUnconfirmed, "unconfirmed_model_id"}, {ReasonRemote, "remote_endpoint"},
		{ReasonNoInflight, "no_inflight_source"}, {ReasonOllamaNoInflight, "ollama_no_inflight"},
		{ReasonOllamaAbsent, "absent_not_proof"}, {ReasonNoSample, "no_sample"},
		{ReasonUnknownProvider, "unknown_provider"}, {ReasonNotObserved, "backend_not_observed"},
		{ReasonNoHistory, "backend_no_history"}, {ReasonTooFewSamples, "too_few_samples"},
		{ReasonNoValues, "no_available_values"},
		{CoverageIncomplete, "incomplete"}, {CoverageUnknown, "unknown"}, {CovEmpty, "empty_ring"},
		{CovNoncontiguous, "noncontiguous_ids"}, {CovEvicted, "older_rows_evicted"},
		{CovRetentionStartUnknown, "retention_start_unknown"}, {CovWithinRetained, "window_within_retained"},
		{ScopeBackendAllClients, "backend_all_clients"}, {LabelDuration, "backend-reported request duration"},
		{SevCritical, "critical"}, {SevSerious, "serious"}, {SevWarning, "warning"}, {SevInfo, "info"},
		{AttnBackendUnreachable, "backend_unreachable"}, {AttnRefused, "refused_requests"},
		{AttnModelErrors, "model_errors"}, {AttnTelemetryUnavailable, "telemetry_unavailable"},
		{AttnTelemetryStale, "telemetry_stale"}, {AttnBackendUnsupported, "backend_unsupported"},
		{AttnConfigProblem, "config_problem"}, {AttnCoverageNote, "coverage_note"},
	} {
		if c.got != c.want {
			t.Errorf("wire literal %q, want %q", c.got, c.want)
		}
	}
}

// cadenceClock is a fake opsbackend clock whose wall and monotonic readings
// advance together, so the collector never sees a jump.
type cadenceClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (c *cadenceClock) clock() opsbackend.Clock {
	return opsbackend.Clock{
		Wall: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.wall },
		Mono: func() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.mono },
	}
}

func (c *cadenceClock) advance(d time.Duration) {
	c.mu.Lock()
	c.wall, c.mono = c.wall.Add(d), c.mono+d
	c.mu.Unlock()
}

// fixtureFront serves a fixture llama-swap through hook, which handles the
// request itself when it returns true.
func fixtureFront(t *testing.T, hook func(w http.ResponseWriter, r *http.Request) bool) string {
	t.Helper()
	u, err := url.Parse(opsfixture.NewLlamaSwap(t).URL())
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

// TestFailureStaysFreshThroughWorstCaseServeCadence drives a real collector
// through serve's worst case: the 4 s backoff retry falls 1 ms after a gated
// tick starts, that tick spends its full 5 s, and the retry tick does too, so
// serve keeps showing the old failure until retry + interval + 2 x
// TickTimeout - 1 ms. It must still read fresh there (spec §5.4).
func TestFailureStaysFreshThroughWorstCaseServeCadence(t *testing.T) {
	const interval = 2 * time.Second
	ms := time.Millisecond
	clk := &cadenceClock{wall: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)}
	var slow atomic.Int64 // fake time b-slow spends on this tick
	// a-down hangs up every request: a dial-level failure, observed first.
	down := fixtureFront(t, func(w http.ResponseWriter, _ *http.Request) bool {
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return true
	})
	// b-slow is healthy; its /api/version spends the tick's chosen fake time.
	slowURL := fixtureFront(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/version" {
			clk.advance(time.Duration(slow.Load()))
		}
		return false
	})
	c := opsbackend.NewCollector([]opsbackend.BackendSpec{
		{Provider: "a-down", BaseURL: down, APIFormat: "openai-compat"},
		{Provider: "b-slow", BaseURL: slowURL, APIFormat: "openai-compat"},
	}, opsbackend.Options{Interval: interval, Clock: clk.clock()})

	// t0 at 0: fail, retry 2 s. t1 at 2 s: fail, retry 6 s; b-slow 2 s - 1 ms.
	// t2 at 6 s - 1 ms: gated; b-slow 5 s. t3 at 13 s - 1 ms: retry; b-slow 5 s.
	var prev *opsbackend.BackendObservation
	var worst time.Duration
	for i, spend := range []time.Duration{0, 2*time.Second - ms, opsbackend.TickTimeout, opsbackend.TickTimeout} {
		slow.Store(int64(spend))
		start := clk.clock().Mono()
		obs := c.Tick(context.Background())
		end := clk.clock().Mono()
		if end-start > opsbackend.TickTimeout {
			t.Fatalf("tick %d took %v of fake time, over the tick budget", i, end-start)
		}
		// Serve shows the previous snapshot until this tick stores its own.
		shown := end - time.Nanosecond
		if prev != nil && prev.Reachable != nil && !prev.Reachable.Value {
			worst = max(worst, shown-prev.ReachRetry)
			b := builder{in: Input{Mode: ModeServe, Interval: interval, NowMono: shown}}
			if !b.failureFresh(prev.ReachRetry) {
				t.Fatalf("tick %d: failure with retry %v read stale at %v (overshoot %v) before its retry was published at %v",
					i, prev.ReachRetry, shown, shown-prev.ReachRetry, end)
			}
		}
		d := obs.Backends[0]
		prev = &d
		clk.advance(interval)
	}
	if want := interval + 2*opsbackend.TickTimeout - ms - time.Nanosecond; worst != want {
		t.Fatalf("schedule overshoot = %v, want the two-tick worst case %v", worst, want)
	}
}
