package opsview

import (
	"math"
	"slices"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsbackend"
)

type window struct {
	name string
	d    time.Duration
}

var windows = []window{{"15m", 15 * time.Minute}, {"1h", time.Hour}}

// ring summarizes the whole activity ring once per backend, before any model
// or window filtering, and indexes its rows by model so each model's windows
// read only that model's rows: a ring at the 8 MiB cap holds ~150k rows, and
// rescanning it per model per window cost ~0.7 s per Build at the model caps.
type ring struct {
	rows       []opsbackend.ActivityRow // the sample's rows, shared, not copied
	byModel    map[string][]int32       // indexes into rows, in ring order
	oldestAt   time.Time                // earliest timestamp, for display
	newestAt   time.Time
	firstAt    time.Time // timestamp of the lowest-ID row: eviction follows ID order
	evicted    bool
	contiguous bool
}

func newRing(rows []opsbackend.ActivityRow) ring {
	r := ring{rows: rows, byModel: map[string][]int32{}, contiguous: true}
	if len(rows) == 0 {
		return r
	}
	// Seed the bounds from a row: the zero time cannot mean "none yet",
	// because an offset can date a row before year 1.
	r.oldestAt, r.newestAt, r.firstAt = rows[0].Timestamp, rows[0].Timestamp, rows[0].Timestamp
	minID := rows[0].ID
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
		// ponytail: int32 halves the index; the 8 MiB body bounds rows far below MaxInt32.
		r.byModel[row.Model] = append(r.byModel[row.Model], int32(i))
		if row.ID < minID {
			minID, r.firstAt = row.ID, row.Timestamp
		}
		if row.Timestamp.Before(r.oldestAt) {
			r.oldestAt = row.Timestamp
		}
		if row.Timestamp.After(r.newestAt) {
			r.newestAt = row.Timestamp
		}
	}
	slices.Sort(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1]+1 {
			r.contiguous = false
		}
	}
	r.evicted = ids[0] > 0
	return r
}

// coverage never claims completeness: IDs are assigned after timestamps and
// eviction follows insertion, so timestamps cannot bound what was evicted.
// The evicted rows preceded the lowest-ID row, so its timestamp, not the
// earliest one, decides whether they could fall inside the window.
func (r ring) coverage(windowStart time.Time) Coverage {
	c := Coverage{RetainedRows: len(r.rows), OldestRetainedAt: stamp(r.oldestAt), NewestRetainedAt: stamp(r.newestAt),
		Evicted: r.evicted, Contiguous: r.contiguous, State: CoverageUnknown}
	switch {
	case len(r.rows) == 0:
		c.Reason = CovEmpty
	case !r.contiguous:
		c.Reason = CovNoncontiguous
	case r.evicted && r.firstAt.After(windowStart):
		c.State, c.Reason = CoverageIncomplete, CovEvicted
	case r.evicted:
		c.Reason = CovWithinRetained
	default:
		c.Reason = CovRetentionStartUnknown
	}
	return c
}

type tokenAcc struct {
	sum         int64
	n           int
	unavailable int
}

// add counts only positive values: a zero may be a defaulted field. The sum
// saturates at MaxInt64 rather than wrapping on hostile values.
func (a *tokenAcc) add(v int64) {
	if v > 0 {
		a.sum += min(v, math.MaxInt64-a.sum)
		a.n++
		return
	}
	a.unavailable++
}

func (a tokenAcc) result() TokenSum {
	t := TokenSum{N: a.n, Unavailable: a.unavailable}
	if a.n == 0 {
		t.Reason = strPtr(ReasonNoValues)
		return t
	}
	s := a.sum
	t.Sum = &s
	return t
}

// rowSkew is how far past Now a row may be dated and still count. llama-swap
// runs on this host and shares its clock, so a row dated later comes from a
// wall-clock step or a hostile peer (year 9999 would otherwise sit in every
// window forever). Such a row enters no window; coverage still reports it,
// because the ring does retain it.
const rowSkew = time.Minute

// future reports whether a backend-reported time is too far past Now to place.
func (b builder) future(t time.Time) bool { return t.After(b.in.Now.Add(rowSkew)) }

// windowStats computes one window for model from llama-swap's retained ring.
// r must be newRing(s.Value).
func (b builder) windowStats(model string, s *opsbackend.Sample[[]opsbackend.ActivityRow], r ring, w window) Stats {
	st := unavailableStats(w, ReasonNoSample)
	st.Envelope = Envelope{Source: "llama-swap /api/metrics"}
	if s == nil {
		st.Coverage = Coverage{State: CoverageUnknown, Reason: ReasonNoSample}
		return st
	}
	if len(s.Value) == 0 {
		st = unavailableStats(w, ReasonNoValues)
	}
	st.Envelope = b.envelope("llama-swap /api/metrics", s.At, s.Mono)
	start := b.in.Now.Add(-w.d)
	st.Coverage = r.coverage(start)
	if len(s.Value) == 0 {
		return st
	}
	calls, errs := 0, 0
	var processed, usage, output tokenAcc
	var prefill, decode, dur []float64
	for _, i := range r.byModel[model] {
		row := r.rows[i]
		if row.Timestamp.Before(start) || b.future(row.Timestamp) {
			continue
		}
		calls++
		if row.Status < 200 || row.Status > 299 {
			errs++
			continue
		}
		if row.PromptPerSecond == -1 && row.TokensPerSecond == -1 {
			usage.add(row.InputTokens)
		} else {
			processed.add(row.InputTokens)
		}
		output.add(row.OutputTokens)
		if row.PromptPerSecond > 0 {
			prefill = append(prefill, row.PromptPerSecond)
		}
		if row.TokensPerSecond > 0 {
			decode = append(decode, row.TokensPerSecond)
		}
		if row.DurationMs >= 0 {
			dur = append(dur, float64(row.DurationMs))
		}
	}
	st.Calls, st.Errors = &calls, &errs
	st.PromptTokensProcessed, st.PromptTokensUsage, st.OutputTokens = processed.result(), usage.result(), output.result()
	st.PrefillTPS, st.DecodeTPS, st.DurationMs = dist(prefill, ""), dist(decode, ""), dist(dur, LabelDuration)
	return st
}

// unavailableStats is one window whose every value is null with reason.
func unavailableStats(w window, reason string) Stats {
	return Stats{
		Window: w.name, Scope: ScopeBackendAllClients,
		PromptTokensProcessed: TokenSum{Reason: strPtr(reason)},
		PromptTokensUsage:     TokenSum{Reason: strPtr(reason)},
		OutputTokens:          TokenSum{Reason: strPtr(reason)},
		PrefillTPS:            Dist{Reason: strPtr(reason)},
		DecodeTPS:             Dist{Reason: strPtr(reason)},
		DurationMs:            Dist{Label: LabelDuration, Reason: strPtr(reason)},
	}
}

// noHistoryStats is one window for a backend with no per-request history
// (Ollama).
func noHistoryStats(w window) Stats {
	s := unavailableStats(w, ReasonNoHistory)
	s.Envelope = Envelope{Source: "ollama"}
	s.Coverage = Coverage{State: CoverageUnknown, Reason: ReasonNoHistory}
	return s
}

// dist applies the sample rules: p50 needs 5 samples, p95 needs 20 and is
// provisional. A null percentile carries too_few_samples. It sorts v in
// place; callers pass slices they own.
func dist(v []float64, label string) Dist {
	d := Dist{N: len(v), Label: label}
	if len(v) < 5 {
		d.Reason = strPtr(ReasonTooFewSamples)
		return d
	}
	slices.Sort(v)
	p50 := nearestRank(v, 50)
	d.P50 = &p50
	if len(v) < 20 {
		d.Reason = strPtr(ReasonTooFewSamples)
		return d
	}
	p95 := nearestRank(v, 95)
	d.P95, d.Provisional = &p95, true
	return d
}

func nearestRank(sorted []float64, p float64) float64 {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}
