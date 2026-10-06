package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/internal/opsview"
)

func hostileSnapshot() opsview.Snapshot {
	age := int64(1500)
	retry := int64(4000)
	reason := "remote_endpoint"
	since := "2026-10-05T09:00:00.000Z"
	return opsview.Snapshot{
		Version: 1, Mode: opsview.ModeWatch, GeneratedAt: "2026-10-05T10:00:00.000Z",
		Config: opsview.ConfigView{Source: "user_config", Ready: true, Diagnostics: []opsview.Diagnostic{}},
		Backends: []opsview.Backend{{ID: "llamacpp", Hosting: "local",
			Runtime:      opsview.Runtime{Kind: "llama-swap", Support: "supported"},
			Reachability: opsview.Reachability{State: "unreachable", RetryInMs: &retry, Envelope: opsview.Envelope{Source: "llama-swap", AgeMs: &age}},
			Observation:  opsview.Observation{Since: &since},
			Surfaces:     []opsview.SurfaceView{}}},
		Models: []opsview.Model{
			{ID: "llamacpp/evil\nFAKE ROW loaded", Provider: "llamacpp", Configured: true, UsedBy: []opsview.UsedBy{},
				Residency: opsview.Residency{State: "loaded", Envelope: opsview.Envelope{Source: "llama-swap /running", AgeMs: &age}},
				Activity:  opsview.Activity{State: "unknown", Envelope: opsview.Envelope{Source: "llama-swap /api/metrics"}},
				Loads:     &opsview.Loads{Count: 3, SampleMs: 2000, Transitions: []opsview.TransitionView{}},
				Stats:     []opsview.Stats{}},
			{ID: "llamacpp/osc\x1b]52;c;YQ==\a\u202etab\there\u0085c1", Provider: "llamacpp", Configured: true, UsedBy: []opsview.UsedBy{},
				Residency: opsview.Residency{State: "not_observed", Reason: &reason, Envelope: opsview.Envelope{Source: "none"}},
				Activity:  opsview.Activity{State: "not_observed", Envelope: opsview.Envelope{Source: "none"}}, Stats: []opsview.Stats{}},
			{ID: "llamacpp/\u5bbd\u5bbd\u5bbd\u5bbd\u5bbd\u5bbd\u5bbd\u5bbd\u5bbd\u5bbd\u5bbd\u5bbd", Provider: "llamacpp", Configured: true, UsedBy: []opsview.UsedBy{},
				Residency: opsview.Residency{State: "unloaded", Envelope: opsview.Envelope{Source: "llama-swap /running"}},
				Activity:  opsview.Activity{State: "unknown", Envelope: opsview.Envelope{Source: "llama-swap /api/metrics"}}, Stats: []opsview.Stats{}},
		},
		Attention: []opsview.Attention{{Severity: "warning", Reason: "config_problem", Subject: "x", Text: "line1\nline2\rline3"}},
	}
}

func TestOpsTableEscapesEveryCell(t *testing.T) {
	var out bytes.Buffer
	if err := renderOpsTable(&out, hostileSnapshot(), 0, 0); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, raw := range []string{"\x1b", "\a", "\u202e", "\r", "\u0085"} {
		if strings.Contains(got, raw) {
			t.Fatalf("raw control %q reached the table:\n%q", raw, got)
		}
	}
	for _, quoted := range []string{`\x1b]52;c;YQ==\a`, `\u202e`, `\t`, `\u0085`} {
		if !strings.Contains(got, quoted) {
			t.Fatalf("missing quoted %s in:\n%s", quoted, got)
		}
	}
	modelLines := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "llamacpp/") {
			modelLines++
		}
		if strings.HasPrefix(line, "FAKE ROW") || strings.HasPrefix(line, "line2") || strings.HasPrefix(line, "line3") {
			t.Fatalf("untrusted text forged a line:\n%s", got)
		}
	}
	if modelLines != 3 {
		t.Fatalf("model rows = %d, want exactly 3:\n%s", modelLines, got)
	}
	if !strings.Contains(got, "3 observed loads") || !strings.Contains(got, "next check in 4s") || !strings.Contains(got, "observed since") {
		t.Fatalf("watch-mode facts missing:\n%s", got)
	}
}

func TestOpsTableClipsToTerminalColumns(t *testing.T) {
	var out bytes.Buffer
	if err := renderOpsTable(&out, hostileSnapshot(), 0, 40); err != nil {
		t.Fatal(err)
	}
	// Count columns independently of the renderer's helper: the fixture
	// holds only ASCII, U+00BB and the wide U+5BBD.
	cols := func(line string) int {
		n := 0
		for _, r := range line {
			n++
			if r == 0x5bbd {
				n++
			}
		}
		return n
	}
	clipped := false
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if n := cols(line); n > 40 {
			t.Fatalf("line exceeds 40 columns (%d): %q", n, line)
		}
		clipped = clipped || strings.HasSuffix(line, "»")
	}
	if !clipped {
		t.Fatal("nothing was clipped; the test proves nothing")
	}
}

func TestOpsTableAgesAdvance(t *testing.T) {
	var a, b bytes.Buffer
	_ = renderOpsTable(&a, hostileSnapshot(), 0, 0)
	_ = renderOpsTable(&b, hostileSnapshot(), 90*time.Second, 0)
	if !strings.Contains(a.String(), "(1s)") || !strings.Contains(b.String(), "(1m)") {
		t.Fatalf("ages did not advance:\n%s\n---\n%s", a.String(), b.String())
	}
}

func TestOpsTableOnceModeHasNoLoadsColumn(t *testing.T) {
	s := hostileSnapshot()
	s.Mode = opsview.ModeOnce
	var out bytes.Buffer
	_ = renderOpsTable(&out, s, 0, 0)
	if strings.Contains(out.String(), "LOADS") {
		t.Fatal("once mode rendered a loads column")
	}
}

// TestOpsTableRetryDue pins the retry wording: retry_in_ms sits at 0 for up
// to 13 s past the retry instant, and elapsed time can pass it too; neither
// may read "next check in 0s". A sub-second wait rounds up.
func TestOpsTableRetryDue(t *testing.T) {
	for _, tc := range []struct {
		retry   int64
		elapsed time.Duration
		want    string
	}{
		{0, 0, "retry due"},
		{4000, 5 * time.Second, "retry due"},
		{4000, 3500 * time.Millisecond, "next check in 1s"},
	} {
		s := hostileSnapshot()
		s.Backends[0].Reachability.RetryInMs = &tc.retry
		var out bytes.Buffer
		_ = renderOpsTable(&out, s, tc.elapsed, 0)
		if got := out.String(); !strings.Contains(got, tc.want) || strings.Contains(got, "in 0s") {
			t.Fatalf("retry %dms after %v: want %q:\n%s", tc.retry, tc.elapsed, tc.want, got)
		}
	}
}

// TestOpsTableStatsWithNullCoverageBounds pins a window with retained rows
// but null bounds (a stamp outside years 0..9999 serializes as null): the
// cell still renders its counts.
func TestOpsTableStatsWithNullCoverageBounds(t *testing.T) {
	s := hostileSnapshot()
	calls, errs := 7, 2
	s.Models[0].Stats = []opsview.Stats{{Window: "1h", Calls: &calls, Errors: &errs,
		Coverage: opsview.Coverage{RetainedRows: 7, State: opsview.CoverageIncomplete, Reason: opsview.CovRetentionStartUnknown}}}
	var out bytes.Buffer
	if err := renderOpsTable(&out, s, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "7 calls, 2 errors; retained history incomplete") {
		t.Fatalf("1h stats cell missing:\n%s", got)
	}
}
