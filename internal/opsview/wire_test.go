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
	if deref(nil) != "" || strPtr("") != nil {
		t.Fatal("nil/empty helpers")
	}
}

func TestFreshness(t *testing.T) {
	watch := builder{in: Input{Mode: ModeWatch, Interval: 2 * time.Second, NowMono: 20 * time.Second}}
	if !watch.fresh(14*time.Second) || watch.fresh(13*time.Second) {
		t.Fatal("positive readings: fresh within 3 x interval only")
	}
	// A failure stays fresh until its scheduled retry has had time to answer.
	if !watch.failureFresh(18*time.Second) || !watch.failureFresh(17*time.Second) || watch.failureFresh(16*time.Second) {
		t.Fatal("failure readings follow the retry schedule plus the request timeout")
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
