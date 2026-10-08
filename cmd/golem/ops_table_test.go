package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/internal/opsbackend"
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
	clipped, exact := false, false
	for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		if n := cols(line); n > 40 {
			t.Fatalf("line exceeds 40 columns (%d): %q", n, line)
		}
		clipped = clipped || strings.HasSuffix(line, "»")
		exact = exact || cols(line) == 40 && strings.HasSuffix(line, "»")
	}
	if !clipped {
		t.Fatal("nothing was clipped; the test proves nothing")
	}
	if !exact {
		t.Fatalf("no clipped line fills all 40 columns; the clip cuts too early:\n%s", out.String())
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

// TestOpsTableModelsHeader pins the MODELS labels in order, and that each
// labels its own cells: activity last, so a terminal clip cuts it first.
func TestOpsTableModelsHeader(t *testing.T) {
	for _, tc := range []struct {
		mode opsview.Mode
		want string
	}{
		{opsview.ModeOnce, "MODELS RESIDENCY LAST 1H USED BY ACTIVITY"},
		{opsview.ModeWatch, "MODELS RESIDENCY LOADS LAST 1H USED BY ACTIVITY"},
	} {
		s := hostileSnapshot()
		s.Mode = tc.mode
		var out bytes.Buffer
		_ = renderOpsTable(&out, s, 0, 0)
		var header, row string
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(line, "MODELS") {
				header = line
			}
			if strings.HasPrefix(line, "llamacpp/evil") {
				row = line
			}
		}
		if got := strings.Join(strings.Fields(header), " "); got != tc.want {
			t.Fatalf("%s header = %q, want %q", tc.mode, got, tc.want)
		}
		if got := strings.Index(header, "ACTIVITY"); got != strings.LastIndex(row, "unknown") {
			t.Fatalf("%s: ACTIVITY at column %d, its cell at %d:\n%s\n%s", tc.mode, got, strings.LastIndex(row, "unknown"), header, row)
		}
		if tc.mode == opsview.ModeWatch && strings.Index(header, "LOADS") != strings.Index(row, "3 observed loads") {
			t.Fatalf("LOADS does not label the loads cell:\n%s\n%s", header, row)
		}
	}
}

// TestOpsTableOnceModeHasNoLoadsOrRetry pins once mode: it exits after one
// render, so it has no loads history and no next check.
func TestOpsTableOnceModeHasNoLoadsOrRetry(t *testing.T) {
	s := hostileSnapshot()
	s.Mode = opsview.ModeOnce
	var out bytes.Buffer
	_ = renderOpsTable(&out, s, 0, 0)
	if strings.Contains(out.String(), "LOADS") {
		t.Fatal("once mode rendered a loads column")
	}
	if got := out.String(); strings.Contains(got, "next check") || strings.Contains(got, "retry due") {
		t.Fatalf("once mode promised a retry:\n%s", got)
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

// TestOpsTableLabelsCodes pins that codes reach the operator as label text,
// and that a null-valued window names the values' own reason: an alias row's
// ring has a retention reason, but its values are null because the ID is
// unconfirmed.
func TestOpsTableLabelsCodes(t *testing.T) {
	s := hostileSnapshot()
	code, unconfirmed, noValues := "timeout", opsview.ReasonUnconfirmed, opsview.ReasonNoValues
	s.Backends[0].Runtime.Support = "unsupported_version"
	s.Backends[0].Reachability.Code = &code
	s.Models[0].Stats = []opsview.Stats{{Window: "1h", DurationMs: opsview.Dist{Reason: &unconfirmed},
		Coverage: opsview.Coverage{RetainedRows: 4, State: opsview.CoverageUnknown, Reason: opsview.CovRetentionStartUnknown}}}
	s.Models[2].Stats = []opsview.Stats{{Window: "1h", DurationMs: opsview.Dist{Reason: &noValues},
		Coverage: opsview.Coverage{State: opsview.CoverageUnknown, Reason: opsview.CovEmpty}}}
	var out bytes.Buffer
	_ = renderOpsTable(&out, s, 0, 0)
	got := out.String()
	for _, want := range []string{
		"llama-swap version other than v235;",          // support
		"unreachable (no answer in time)",              // reachability code
		"not_observed: remote endpoint, not observed;", // residency reason
		"n/a (not confirmed as a llama-swap model ID)", // alias: the values' reason
		"n/a (no retained rows)",                       // no values: the coverage reason
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "retention start unknown") {
		t.Fatalf("alias row shows the ring's coverage reason:\n%s", got)
	}
}

// opsHostilePayload holds every class a cell must neutralize: C0/C1
// controls, raw and encoded CSI, bidi overrides and isolates, zero-width and
// other format runes, line and paragraph separators, tags, private use,
// invalid and truncated UTF-8, and the clip marker itself.
const opsHostilePayload = "\x1b[31m\x9b\u009b\x07\x08\r\x7f\x00\t\v\f\u0085\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069\u200e\u200f\u061c\u2028\u2029\u200b\u200c\u200d\u2060\ufeff\ufe0f\U000e0041\U000e0001\u3164\u115f\u3000\u00ad\u180e\ue000\xff\xfe\u00bb\xe2\x80\n"

// everyFieldSnapshot puts p in every string field of a snapshot.
func everyFieldSnapshot(p string) opsview.Snapshot {
	sp := func(s string) *string { return &s }
	age, retry := int64(1500), int64(4000)
	calls, errs := 1, 0
	p50 := 1.5
	return opsview.Snapshot{
		Version: 1, Mode: opsview.Mode("watch" + p), GeneratedAt: "2026-10-05T10:00:00Z",
		Config: opsview.ConfigView{Source: "src" + p, Revision: sp(p + "abcdefgh"), Diagnostics: []opsview.Diagnostic{{Code: "c" + p, Subject: "s" + p}}},
		Backends: []opsview.Backend{{ID: "b" + p, Hosting: "h" + p,
			Runtime:      opsview.Runtime{Kind: "k" + p, Version: sp("v" + p), Support: "su" + p},
			Reachability: opsview.Reachability{State: "st" + p, Code: sp("co" + p), RetryInMs: &retry, Envelope: opsview.Envelope{Source: "src" + p, AgeMs: &age}},
			Observation:  opsview.Observation{Since: sp("since" + p)},
		}},
		Models: []opsview.Model{{ID: "m" + p, Configured: true, UsedBy: []opsview.UsedBy{{UseCase: "uc" + p, IsFallback: true}},
			Residency: opsview.Residency{State: "rs" + p, Reason: sp("rr" + p), LastState: sp("ls" + p), FirstObserved: sp("2026-10-05T09:00:00Z"), ExpiresAt: sp("ex" + p), Envelope: opsview.Envelope{Source: "rsrc" + p, AgeMs: &age}},
			Activity:  opsview.Activity{State: "as" + p, Reason: sp("ar" + p), LastCompletedAt: sp("lc" + p), Envelope: opsview.Envelope{Source: "asrc" + p}},
			Loads:     &opsview.Loads{Count: 1, SampleMs: 2000, Transitions: []opsview.TransitionView{{To: "to" + p, At: "at" + p}}},
			Stats: []opsview.Stats{{Window: "1h", Calls: &calls, Errors: &errs, DecodeTPS: opsview.Dist{P50: &p50, N: 1},
				Coverage: opsview.Coverage{State: "cs" + p, Reason: "cr" + p}}},
		}, {ID: "m2" + p, Configured: false, Residency: opsview.Residency{State: "x"},
			Stats: []opsview.Stats{{Window: "1h", Coverage: opsview.Coverage{State: "cs" + p, Reason: "cr" + p}, DecodeTPS: opsview.Dist{N: 2, Reason: sp("dr" + p)}}}}},
		Attention: []opsview.Attention{{Severity: "sev" + p, Subject: "sub" + p, Text: "t" + p}},
	}
}

// TestOpsTableEscapesEveryField pins escaping per call site: with the
// payload in every field, the table must be valid UTF-8 of graphic runes
// and newlines only, with exactly the benign render's line count.
func TestOpsTableEscapesEveryField(t *testing.T) {
	for _, width := range []int{0, 80} {
		var benign, hostile bytes.Buffer
		if err := renderOpsTable(&benign, everyFieldSnapshot(""), 0, width); err != nil {
			t.Fatal(err)
		}
		if err := renderOpsTable(&hostile, everyFieldSnapshot(opsHostilePayload), 0, width); err != nil {
			t.Fatal(err)
		}
		got := hostile.String()
		if !utf8.ValidString(got) {
			t.Fatalf("width %d: output is not valid UTF-8:\n%q", width, got)
		}
		for _, r := range got {
			if r != '\n' && !unicode.IsGraphic(r) {
				t.Fatalf("width %d: non-graphic %U reached the table:\n%q", width, r, got)
			}
		}
		if b, h := strings.Count(benign.String(), "\n"), strings.Count(got, "\n"); b != h {
			t.Fatalf("width %d: hostile render has %d lines, benign %d:\n%s", width, h, b, got)
		}
	}
}

// TestOpsTableBoundsHugeNames pins the cell bound: tabwriter pads every row
// to the longest cell, so an unbounded name would be copied into each row.
func TestOpsTableBoundsHugeNames(t *testing.T) {
	huge := "llamacpp/" + strings.Repeat("a", 1<<20)
	s := opsview.Snapshot{Version: 1, Mode: opsview.ModeWatch, GeneratedAt: "2026-10-05T10:00:00Z",
		Attention: []opsview.Attention{{Severity: "serious", Text: huge}}}
	for i := range 20 {
		s.Models = append(s.Models, opsview.Model{ID: fmt.Sprintf("llamacpp/m%02d", i), Residency: opsview.Residency{State: "loaded"}})
	}
	s.Models = append(s.Models, opsview.Model{ID: huge, Residency: opsview.Residency{State: "loaded"}})
	var out bytes.Buffer
	if err := renderOpsTable(&out, s, 0, 0); err != nil {
		t.Fatal(err)
	}
	if out.Len() > 64<<10 {
		t.Fatalf("a 1 MiB name produced a %d-byte table", out.Len())
	}
}

// TestOpsTableLongIDKeepsFactsVisible pins the ID column bound on a
// terminal: one long ID must not push every row's residency past the clip.
func TestOpsTableLongIDKeepsFactsVisible(t *testing.T) {
	s := opsview.Snapshot{Version: 1, Mode: opsview.ModeWatch, GeneratedAt: "2026-10-05T10:00:00Z",
		Models: []opsview.Model{
			{ID: "llamacpp/" + strings.Repeat("a", 200), Residency: opsview.Residency{State: "loaded"}},
			{ID: "llamacpp/short", Residency: opsview.Residency{State: "loaded"}},
		}}
	var out bytes.Buffer
	_ = renderOpsTable(&out, s, 0, 120)
	rows := 0
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "llamacpp/") {
			rows++
			if !strings.Contains(line, "loaded") {
				t.Fatalf("row lost its residency to a long ID:\n%s", out.String())
			}
		}
	}
	if rows != 2 {
		t.Fatalf("model rows = %d, want 2:\n%s", rows, out.String())
	}
}

// TestOpsTableAmbiguousWidthCannotWrap pins the conservative count against
// terminals that draw East Asian Ambiguous runes (here Cyrillic) two columns
// wide: under that count no line may exceed the width, or it wraps and the
// next visual line starts with untrusted text.
func TestOpsTableAmbiguousWidthCannotWrap(t *testing.T) {
	forged := strings.Repeat("\u0430", 35) + "FAKEROW loaded: forged by peer (1s)"
	s := opsview.Snapshot{Version: 1, Mode: opsview.ModeWatch, GeneratedAt: "2026-10-05T10:00:00Z",
		Models: []opsview.Model{
			{ID: "llamacpp/" + forged, Residency: opsview.Residency{State: "unloaded"}},
			{ID: "llamacpp/real", Residency: opsview.Residency{State: "unloaded"}},
		},
		Attention: []opsview.Attention{{Severity: "warning", Text: forged}}}
	var out bytes.Buffer
	_ = renderOpsTable(&out, s, 0, 80)
	for _, line := range strings.Split(out.String(), "\n") {
		n := 0
		for _, r := range line {
			n++
			if r >= 0x80 && r != 0xbb {
				n++
			}
		}
		if n > 80 {
			t.Fatalf("line is %d columns where ambiguous runes are wide: %q", n, line)
		}
	}
}

// TestOpsCellQuotesMarkFloods pins the combining-mark bound: two marks on
// one base stay (Vietnamese needs them), the rest are quoted.
func TestOpsCellQuotesMarkFloods(t *testing.T) {
	for in, want := range map[string]string{
		"a\u0301\u0301\u0301\u20dd":  "a\u0301\u0301" + `\u0301\u20dd`,
		"a\u0301\u0301b\u0301\u0301": "a\u0301\u0301b\u0301\u0301",
	} {
		if got := opsCell(in); got != want {
			t.Fatalf("opsCell(%+q) = %+q, want %+q", in, got, want)
		}
	}
}

// TestOpsTableNotObservedBackends pins the backend cell for every backend
// golem ops does not read, built by opsview.Build from the observation the
// collector leaves: the reason as label text, then "not observed". No raw
// kind ("none"), state or code reaches the operator, and no observation
// facts follow, since nothing is observed.
func TestOpsTableNotObservedBackends(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    opsbackend.BackendObservation
		want string
	}{
		{"remote", opsbackend.BackendObservation{Provider: "opencode", Endpoint: "https://opencode.invalid", Hosting: opsbackend.HostingRemote,
			Kind: opsbackend.KindNone, Support: opsbackend.SupportNotObserved}, "remote endpoint; not observed"},
		{"invalid configuration", opsbackend.BackendObservation{Provider: "broken", Hosting: opsbackend.HostingUnknown,
			Kind: opsbackend.KindNone, Support: opsbackend.SupportInvalidConfig}, "unusable base_url or api_key in models.json; not observed"},
		{"unsupported", opsbackend.BackendObservation{Provider: "llamacpp", Endpoint: "http://127.0.0.1:8090", Hosting: opsbackend.HostingLocal,
			Kind: opsbackend.KindNone, Support: opsbackend.SupportUnsupported, Version: "v236"}, "llama-swap version other than v235 (v236); not observed"},
		{"unrecognized", opsbackend.BackendObservation{Provider: "vllm", Endpoint: "http://127.0.0.1:8000", Hosting: opsbackend.HostingLocal,
			Kind: opsbackend.KindNone, Support: opsbackend.SupportUnrecognized}, "not llama-swap (runtime not recognized); not observed"},
	} {
		s := opsview.Build(opsview.Input{Observations: opsbackend.Observations{Backends: []opsbackend.BackendObservation{tc.o}},
			Mode: opsview.ModeWatch, Interval: 2 * time.Second, NowMono: 11 * time.Second})
		var out bytes.Buffer
		if err := renderOpsTable(&out, s, 0, 0); err != nil {
			t.Fatal(err)
		}
		var line string
		for _, l := range strings.Split(out.String(), "\n") {
			if strings.HasPrefix(l, tc.o.Provider+" ") {
				line = l
			}
		}
		if !strings.HasSuffix(line, "  "+tc.want) {
			t.Errorf("%s: backend line = %q, want cell %q", tc.name, line, tc.want)
		}
	}
}
