package opsview

import (
	"testing"
	"time"
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
	watch := builder{in: Input{Mode: ModeWatch, Interval: 2 * time.Second, NowMono: 20 * time.Second}}
	if !watch.fresh(14*time.Second) || watch.fresh(13*time.Second) {
		t.Fatal("positive readings: fresh within 3 x interval only")
	}
	// A failure stays fresh until its scheduled retry has had time to answer
	// and be published: retry + interval (2 s) + two ticks (2 x 5 s) + 1 s.
	// NowMono 20 s is exactly that bound for a retry at 7 s.
	if !watch.failureFresh(7*time.Second) || watch.failureFresh(7*time.Second-time.Millisecond) {
		t.Fatal("failure readings stay fresh through retry + interval + two ticks + 1 s, and no longer")
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
