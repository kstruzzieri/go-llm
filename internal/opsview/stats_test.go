package opsview

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsbackend"
)

var now = time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)

func row(id int64, ago time.Duration, model string, status int, in, out int64, pps, tps float64, dur int64) opsbackend.ActivityRow {
	return opsbackend.ActivityRow{ID: id, Timestamp: now.Add(-ago), Model: model, Status: status, InputTokens: in, OutputTokens: out, PromptPerSecond: pps, TokensPerSecond: tps, DurationMs: dur}
}

func statsFor(model string, rows []opsbackend.ActivityRow) Stats {
	b := builder{in: Input{Now: now, Mode: ModeOnce}}
	return b.windowStats(model, &opsbackend.Sample[[]opsbackend.ActivityRow]{Value: rows, At: now}, newRing(rows), windows[0])
}

func TestNearestRankBoundaries(t *testing.T) {
	vals := func(n int) []float64 {
		v := make([]float64, n)
		for i := range v {
			v[i] = float64(i + 1)
		}
		return v
	}
	if d := dist(vals(4), ""); d.P50 != nil || d.N != 4 || deref(d.Reason) != ReasonTooFewSamples {
		t.Fatalf("n=4: %+v", d)
	}
	if d := dist(vals(5), ""); d.P50 == nil || *d.P50 != 3 || d.P95 != nil || deref(d.Reason) != ReasonTooFewSamples {
		t.Fatalf("n=5: %+v", d)
	}
	if d := dist(vals(19), ""); d.P95 != nil || *d.P50 != 10 {
		t.Fatalf("n=19: %+v", d)
	}
	if d := dist(vals(20), ""); d.P95 == nil || *d.P95 != 19 || !d.Provisional || *d.P50 != 10 || d.Reason != nil {
		t.Fatalf("n=20: %+v", d)
	}
	// Unsorted input: sorted it is 10..50, and rank ceil(0.5*5) = 3 is 30.
	if d := dist([]float64{50, 10, 40, 20, 30}, ""); d.P50 == nil || *d.P50 != 30 {
		t.Fatalf("unsorted n=5: %+v", d)
	}
}

func TestWindowStatsTokenBasesAndZeros(t *testing.T) {
	s := statsFor("m", []opsbackend.ActivityRow{
		row(0, 2*time.Minute, "m", 200, 900, 300, 910, 41, 8000), // timings basis
		row(1, 3*time.Minute, "m", 200, 1200, 80, -1, -1, 2000),  // usage basis
		row(2, 4*time.Minute, "m", 200, 0, 0, 0, 0, 10),          // ambiguous zeros
		row(3, 5*time.Minute, "m", 500, 0, 0, 0, 0, 40),          // error
		row(4, 6*time.Minute, "other", 200, 5, 5, 5, 5, 5),       // other model
		row(5, 20*time.Minute, "m", 200, 7, 7, 7, 7, 7),          // outside 15m
	})
	if *s.Calls != 4 || *s.Errors != 1 {
		t.Fatalf("calls/errors = %d/%d, want 4/1", *s.Calls, *s.Errors)
	}
	if s.PromptTokensProcessed.Sum == nil || *s.PromptTokensProcessed.Sum != 900 || s.PromptTokensProcessed.N != 1 || s.PromptTokensProcessed.Unavailable != 1 {
		t.Fatalf("processed = %+v", s.PromptTokensProcessed)
	}
	if *s.PromptTokensUsage.Sum != 1200 || s.PromptTokensUsage.N != 1 {
		t.Fatalf("usage = %+v", s.PromptTokensUsage)
	}
	if *s.OutputTokens.Sum != 380 || s.OutputTokens.Unavailable != 1 {
		t.Fatalf("output = %+v", s.OutputTokens)
	}
	if s.PrefillTPS.N != 1 || s.DecodeTPS.N != 1 || s.DurationMs.N != 3 || s.DurationMs.Label != LabelDuration {
		t.Fatalf("prefill/decode/duration = %+v / %+v / %+v", s.PrefillTPS, s.DecodeTPS, s.DurationMs)
	}
}

func TestAllUnavailableSumIsNullWithReason(t *testing.T) {
	s := statsFor("m", []opsbackend.ActivityRow{row(0, time.Minute, "m", 200, 0, 0, 0, 0, 5)})
	if s.PromptTokensProcessed.Sum != nil || deref(s.PromptTokensProcessed.Reason) != ReasonNoValues {
		t.Fatalf("all-unavailable sum = %+v", s.PromptTokensProcessed)
	}
}

func TestCoverageNeverComplete(t *testing.T) {
	m := func(id int64, ago time.Duration) opsbackend.ActivityRow { return row(id, ago, "m", 200, 1, 1, 1, 1, 1) }
	at := func(hms string) string { return "2026-10-05T" + hms + ".000Z" }
	type rows = []opsbackend.ActivityRow
	for _, tc := range []struct {
		name                string
		rows                rows
		state, reason       string
		retained            int
		evicted, contiguous bool
		oldest, newest      string
	}{
		{"empty", rows{}, CoverageUnknown, CovEmpty, 0, false, true, "", ""},
		{"unevicted", rows{m(0, time.Minute)}, CoverageUnknown, CovRetentionStartUnknown, 1, false, true, at("09:59:00"), at("09:59:00")},
		{"evicted inside window", rows{m(7, time.Minute), m(8, 30*time.Second)}, CoverageIncomplete, CovEvicted, 2, true, true, at("09:59:00"), at("09:59:30")},
		{"evicted older than window", rows{m(7, 20*time.Minute), m(8, time.Minute)}, CoverageUnknown, CovWithinRetained, 2, true, true, at("09:40:00"), at("09:59:00")},
		{"id gap", rows{m(0, 2*time.Minute), m(2, time.Minute)}, CoverageUnknown, CovNoncontiguous, 2, false, false, at("09:58:00"), at("09:59:00")},
		// The gap row belongs to another model: contiguity is judged on the
		// whole ring, so filtering by model first would wrongly report a gap.
		{"other model fills gap", rows{m(0, 2*time.Minute), row(1, 90*time.Second, "x", 200, 1, 1, 1, 1, 1), m(2, time.Minute)}, CoverageUnknown, CovRetentionStartUnknown, 3, false, true, at("09:58:00"), at("09:59:00")},
		// Evicted with its oldest row inside the window, but a gap outranks it.
		{"gap outranks eviction", rows{m(7, time.Minute), m(9, 30*time.Second)}, CoverageUnknown, CovNoncontiguous, 2, true, false, at("09:59:00"), at("09:59:30")},
		// A repeated ID is not one run.
		{"duplicate ids", rows{m(0, 2*time.Minute), m(0, time.Minute)}, CoverageUnknown, CovNoncontiguous, 2, false, false, at("09:58:00"), at("09:59:00")},
		// Listed newest first. The lowest ID (100) predates the window, so
		// the evicted rows that preceded it do too.
		{"newest listed first", rows{m(101, time.Minute), m(100, 20*time.Minute)}, CoverageUnknown, CovWithinRetained, 2, true, true, at("09:40:00"), at("09:59:00")},
		// Eviction follows ID order: the rows evicted before ID 100 (5 min
		// ago) may sit inside the window although ID 101 is dated 40 min ago.
		{"lowest id inside window", rows{m(100, 5*time.Minute), m(101, 40*time.Minute), m(102, time.Minute)}, CoverageIncomplete, CovEvicted, 3, true, true, at("09:20:00"), at("09:59:00")},
	} {
		s := statsFor("m", tc.rows)
		c := s.Coverage
		if c.State != tc.state || c.Reason != tc.reason || c.RetainedRows != tc.retained || c.Evicted != tc.evicted || c.Contiguous != tc.contiguous ||
			deref(c.OldestRetainedAt) != tc.oldest || deref(c.NewestRetainedAt) != tc.newest {
			t.Fatalf("%s: coverage = %+v (oldest %q, newest %q)", tc.name, c, deref(c.OldestRetainedAt), deref(c.NewestRetainedAt))
		}
		if tc.name == "empty" && s.Calls != nil {
			t.Fatalf("empty ring must report calls null, got %d", *s.Calls)
		}
	}
}

// windowStats reads only the model's indexed rows. Relabeling an indexed
// row of another model after indexing makes a full rescan count it.
func TestWindowStatsReadsOnlyItsModelsRows(t *testing.T) {
	rs := []opsbackend.ActivityRow{row(0, time.Minute, "m", 200, 1, 1, 1, 1, 1), row(1, time.Minute, "x", 200, 1, 1, 1, 1, 1), row(2, time.Minute, "m", 200, 1, 1, 1, 1, 1)}
	r := newRing(rs)
	rs[1].Model = "m"
	b := builder{in: Input{Now: now, Mode: ModeOnce}}
	if s := b.windowStats("m", &opsbackend.Sample[[]opsbackend.ActivityRow]{Value: rs, At: now}, r, windows[0]); *s.Calls != 2 {
		t.Fatalf("calls = %d, want 2: windowStats rescanned rows outside the model's index", *s.Calls)
	}
}

// BenchmarkWindowStats is Build's statistics cost at the caps: 276 models
// (20 configured plus the 256 backend-only cap), both windows, over a
// 150k-row ring (about what fits in the 8 MiB metrics body).
func BenchmarkWindowStats(b *testing.B) {
	const models, n = 276, 150_000
	names := make([]string, models)
	for i := range names {
		names[i] = fmt.Sprintf("m%d", i)
	}
	rs := make([]opsbackend.ActivityRow, n)
	for i := range rs {
		rs[i] = row(int64(i), time.Duration(n-i)*20*time.Millisecond, names[i%models], 200, 1, 1, 1, 1, 1)
	}
	s := &opsbackend.Sample[[]opsbackend.ActivityRow]{Value: rs, At: now}
	bd := builder{in: Input{Now: now, Mode: ModeOnce}}
	b.ReportAllocs()
	for b.Loop() {
		r := newRing(rs)
		for _, name := range names {
			for _, w := range windows {
				bd.windowStats(name, s, r, w)
			}
		}
	}
}

func TestAbsentAndEmptyMetricsCarryReasons(t *testing.T) {
	b := builder{in: Input{Now: now, Mode: ModeOnce}}
	absent := b.windowStats("m", nil, ring{}, windows[0])
	empty := statsFor("m", []opsbackend.ActivityRow{})
	for name, s := range map[string]Stats{"absent": absent, "empty": empty} {
		if s.Calls != nil || s.PromptTokensProcessed.Reason == nil || s.DecodeTPS.Reason == nil || s.DurationMs.Reason == nil || s.DurationMs.Label != LabelDuration {
			t.Fatalf("%s metrics left null values without reasons or the duration label: %+v", name, s)
		}
	}
	if deref(absent.DecodeTPS.Reason) != ReasonNoSample || deref(empty.DecodeTPS.Reason) != ReasonNoValues {
		t.Fatalf("reasons = %q / %q", deref(absent.DecodeTPS.Reason), deref(empty.DecodeTPS.Reason))
	}
}

func TestNoHistoryStats(t *testing.T) {
	s := noHistoryStats(windows[1])
	if s.Calls != nil || s.Coverage.Reason != ReasonNoHistory || s.Window != "1h" {
		t.Fatalf("no-history stats = %+v", s)
	}
}

// A row dated more than one minute past Now (a clock step or a hostile peer)
// enters no window: at year 9999 it would otherwise sit in every window
// forever. The skew is a literal here so the test pins its value.
func TestFutureRowsEnterNoWindow(t *testing.T) {
	far := opsbackend.ActivityRow{ID: 1, Timestamp: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), Model: "m", Status: 500, DurationMs: 9}
	s := statsFor("m", []opsbackend.ActivityRow{
		row(0, time.Minute, "m", 200, 3, 3, 3, 3, 3),
		far,
		row(2, -time.Minute, "m", 200, 4, 4, 4, 4, 4),                  // exactly at the bound: counts
		row(3, -time.Minute-time.Millisecond, "m", 500, 5, 5, 5, 5, 5), // just past it: does not
	})
	if *s.Calls != 2 || *s.Errors != 0 || *s.PromptTokensProcessed.Sum != 7 || s.DurationMs.N != 2 {
		t.Fatalf("future-row window: calls=%d errors=%d processed=%+v duration=%+v", *s.Calls, *s.Errors, s.PromptTokensProcessed, s.DurationMs)
	}
}

// Token sums saturate at MaxInt64 instead of wrapping negative, whichever
// operand comes first.
func TestTokenSumsSaturate(t *testing.T) {
	s := statsFor("m", []opsbackend.ActivityRow{
		row(0, time.Minute, "m", 200, math.MaxInt64, 7, 1, 1, 1),
		row(1, 2*time.Minute, "m", 200, 5, math.MaxInt64, 1, 1, 1),
	})
	if *s.PromptTokensProcessed.Sum != math.MaxInt64 || s.PromptTokensProcessed.N != 2 {
		t.Fatalf("processed sum did not saturate: %+v (sum %d)", s.PromptTokensProcessed, *s.PromptTokensProcessed.Sum)
	}
	if *s.OutputTokens.Sum != math.MaxInt64 || s.OutputTokens.N != 2 {
		t.Fatalf("output sum did not saturate: %+v (sum %d)", s.OutputTokens, *s.OutputTokens.Sum)
	}
}

// The ring's bounds come from its rows, not from a zero-time sentinel: a row
// dated before year 1 is still the newest when it is the only one.
func TestRingBoundsBeforeYearOne(t *testing.T) {
	early := time.Date(0, 12, 31, 23, 0, 0, 0, time.UTC)
	r := newRing([]opsbackend.ActivityRow{{ID: 0, Timestamp: early, Model: "m", Status: 200}})
	if !r.oldestAt.Equal(early) || !r.newestAt.Equal(early) {
		t.Fatalf("ring bounds = %v .. %v, want %v for both", r.oldestAt, r.newestAt, early)
	}
}

// Usage basis needs both rates exactly -1; any other pair is the timings
// basis. Rates count only when positive, and prefill and decode are counted
// separately. Inputs are powers of two so each sum names its rows.
func TestBasisAndRateRules(t *testing.T) {
	s := statsFor("m", []opsbackend.ActivityRow{
		row(0, time.Minute, "m", 200, 100, 1, 500, 0, 0),   // timings; prefill only; duration 0 counts
		row(1, time.Minute, "m", 200, 200, 1, -1, 41, 10),  // one rate -1: timings; decode only
		row(2, time.Minute, "m", 200, 400, 1, -2, -2, 10),  // both negative, not -1: timings
		row(3, time.Minute, "m", 200, 800, 1, -1, -1, 10),  // both exactly -1: usage
		row(4, time.Minute, "m", 200, 1600, 1, 700, 0, 10), // timings; prefill only
		row(5, time.Minute, "m", 200, 3200, 1, 0, 0, 10),   // zero rates: neither
	})
	if p := s.PromptTokensProcessed; p.Sum == nil || *p.Sum != 100+200+400+1600+3200 || p.N != 5 {
		t.Fatalf("processed = %+v", p)
	}
	if u := s.PromptTokensUsage; u.Sum == nil || *u.Sum != 800 || u.N != 1 {
		t.Fatalf("usage = %+v", u)
	}
	if s.PrefillTPS.N != 2 || s.DecodeTPS.N != 1 || s.DurationMs.N != 6 {
		t.Fatalf("prefill/decode/duration n = %d/%d/%d, want 2/1/6", s.PrefillTPS.N, s.DecodeTPS.N, s.DurationMs.N)
	}
}

// Errors are the non-2xx rows (spec 5.4), so 199 and 300 are errors and 200
// and 299 are not.
func TestErrorsAreNon2xx(t *testing.T) {
	s := statsFor("m", []opsbackend.ActivityRow{
		row(0, time.Minute, "m", 199, 1, 1, 1, 1, 1),
		row(1, time.Minute, "m", 200, 1, 1, 1, 1, 1),
		row(2, time.Minute, "m", 299, 1, 1, 1, 1, 1),
		row(3, time.Minute, "m", 300, 1, 1, 1, 1, 1),
	})
	if *s.Calls != 4 || *s.Errors != 2 {
		t.Fatalf("calls/errors = %d/%d, want 4/2", *s.Calls, *s.Errors)
	}
}

// A window is [Now-d, Now]: a row exactly at its start is inside.
func TestWindowBounds(t *testing.T) {
	rows := []opsbackend.ActivityRow{
		row(0, 14*time.Minute+59*time.Second, "m", 200, 1, 1, 1, 1, 1),
		row(1, 15*time.Minute, "m", 200, 1, 1, 1, 1, 1),
		row(2, 15*time.Minute+time.Second, "m", 200, 1, 1, 1, 1, 1),
		row(3, 59*time.Minute+59*time.Second, "m", 200, 1, 1, 1, 1, 1),
		row(4, time.Hour, "m", 200, 1, 1, 1, 1, 1),
		row(5, time.Hour+time.Second, "m", 200, 1, 1, 1, 1, 1),
	}
	b := builder{in: Input{Now: now, Mode: ModeOnce}}
	for i, want := range []struct {
		name  string
		calls int
	}{{"15m", 2}, {"1h", 5}} {
		s := b.windowStats("m", &opsbackend.Sample[[]opsbackend.ActivityRow]{Value: rows, At: now}, newRing(rows), windows[i])
		if s.Window != want.name || *s.Calls != want.calls {
			t.Fatalf("window %d = %q with %d calls, want %q with %d", i, s.Window, *s.Calls, want.name, want.calls)
		}
	}
}

// Statistics carry the metrics sample's envelope: in watch mode a sample
// older than 3 x interval is stale.
func TestStatsEnvelopeDecays(t *testing.T) {
	b := builder{in: Input{Now: now, NowMono: 10 * time.Second, Mode: ModeWatch, Interval: 2 * time.Second}}
	rows := []opsbackend.ActivityRow{row(0, time.Minute, "m", 200, 1, 1, 1, 1, 1)}
	s := b.windowStats("m", &opsbackend.Sample[[]opsbackend.ActivityRow]{Value: rows, At: now.Add(-7 * time.Second), Mono: 3 * time.Second}, newRing(rows), windows[0])
	if !s.Stale || deref(s.ObservedAt) != "2026-10-05T09:59:53.000Z" || s.AgeMs == nil || *s.AgeMs != 7000 || s.Source != "llama-swap /api/metrics" {
		t.Fatalf("stats envelope = %+v (observed_at %q)", s.Envelope, deref(s.ObservedAt))
	}
}
