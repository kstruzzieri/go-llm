package opsview

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/internal/opsbackend"
	"github.com/kstruzzieri/go-llm/internal/opsfixture"
)

func reasons(s Snapshot) map[string]Attention {
	out := map[string]Attention{}
	for _, a := range s.Attention {
		out[a.Reason+":"+a.Subject] = a
	}
	return out
}

func TestUnreachableIsSteadyBetweenRetries(t *testing.T) {
	down := healthyLlamaSwap()
	down.Kind = opsbackend.KindUnidentified
	down.Reachable = sample(false, 2*time.Second)
	down.ReachCode, down.ReachRetry = opsbackend.CodeUnreachable, 18*time.Second
	down.Surfaces = []opsbackend.Surface{{Name: "models", LastError: opsbackend.CodeMalformed}} // from before the outage
	for _, nowMono := range []time.Duration{3 * time.Second, 12 * time.Second, 20 * time.Second} {
		in := input(ModeWatch, down, remote())
		in.NowMono = nowMono
		r := reasons(Build(in))
		if a, ok := r[AttnBackendUnreachable+":llamacpp"]; !ok || a.Severity != SevCritical || a.Text != "llamacpp is unreachable; its models read unknown" {
			t.Fatalf("t=%v: unreachable attention missing: %+v", nowMono, r)
		}
		if _, ok := r[AttnTelemetryStale+":llamacpp"]; ok {
			t.Fatalf("t=%v: a known-down backend must not also read stale", nowMono)
		}
		if a, ok := r[AttnTelemetryUnavailable+":llamacpp"]; ok {
			t.Fatalf("t=%v: a surface error from before the outage still alerts: %+v", nowMono, a)
		}
	}
	down.ReachCode = opsbackend.CodeTimeout
	if a := reasons(Build(input(ModeWatch, down, remote())))[AttnBackendUnreachable+":llamacpp"]; a.Text != "llamacpp did not answer in time; its models read unknown" {
		t.Fatalf("timeout text = %q", a.Text)
	}
}

// TestFailurePastItsRetryReadsStale pins both edges of the failure window
// (retry 18 s + interval 2 s + 2 x TickTimeout + 1 s = 31 s) for a backend
// down since start, with no cached positive reading to go stale instead.
func TestFailurePastItsRetryReadsStale(t *testing.T) {
	down := opsbackend.BackendObservation{
		Provider: "llamacpp", Endpoint: "http://127.0.0.1:8090", Hosting: opsbackend.HostingLocal,
		Kind: opsbackend.KindUnidentified, Support: opsbackend.SupportUnknown,
		Reachable: sample(false, 2*time.Second), ReachCode: opsbackend.CodeUnreachable, ReachRetry: 18 * time.Second,
	}
	in := input(ModeWatch, down, remote())
	in.NowMono = 31 * time.Second // last fresh instant: critical, not stale
	if got := Build(in).Attention; len(got) != 1 || got[0].Reason != AttnBackendUnreachable {
		t.Fatalf("a fresh failure must stay critical only: %+v", got)
	}
	in.NowMono = 32 * time.Second
	got := Build(in).Attention
	if len(got) != 1 || got[0].Reason != AttnTelemetryStale || got[0].Severity != SevWarning || got[0].Subject != "llamacpp" || got[0].Since != nil ||
		got[0].Text != "llamacpp last check failed (no response); next check overdue, so its facts read unknown" {
		t.Fatalf("a failure past its retry window must read stale: %+v", got)
	}
}

// TestBackoffKeepsReachabilitySteady drives a real collector against a
// backend whose only positive reading is reachability, re-stamped only at the
// backoff retries (2, 6, 14, 30 and 60 s): it must read ok through several
// cycles instead of flapping to stale between them.
func TestBackoffKeepsReachabilitySteady(t *testing.T) {
	const interval = 2 * time.Second
	for _, tc := range []struct {
		name, format, path string
		status             int
		fixture            func(testing.TB) *opsfixture.Server
	}{
		{"llama-swap 401 from start", "openai-compat", "/api/version", 401, opsfixture.NewLlamaSwap},
		{"ollama 500 from start", "ollama", "/api/ps", 500, func(t testing.TB) *opsfixture.Server { return opsfixture.NewOllama(t, `{"models":[]}`) }},
	} {
		fx := tc.fixture(t)
		fx.SetStatus(tc.path, tc.status)
		clk := &cadenceClock{wall: now}
		c := opsbackend.NewCollector([]opsbackend.BackendSpec{{Provider: "llamacpp", BaseURL: fx.URL(), APIFormat: tc.format}},
			opsbackend.Options{Interval: interval, Clock: clk.clock()})
		for range 40 { // 80 s
			obs := c.Tick(context.Background())
			for _, d := range []time.Duration{0, interval - time.Millisecond} { // as published, and just before the next tick
				in := input(ModeServe, obs.Backends...)
				in.NowMono = clk.clock().Mono() + d
				s := Build(in)
				if r := s.Backends[0].Reachability; r.State != ReachOK || r.Stale {
					t.Fatalf("%s: t=%v reachability between backoff retries = %+v", tc.name, in.NowMono, r)
				}
				if a, ok := reasons(s)[AttnTelemetryStale+":llamacpp"]; ok {
					t.Fatalf("%s: t=%v telemetry_stale between backoff retries: %+v", tc.name, in.NowMono, a)
				}
			}
			clk.advance(interval)
		}
		if n := len(fx.Requests()); n != 6 {
			t.Fatalf("%s: %d requests in 80 s, want 6 (backoff retries at 0, 2, 6, 14, 30, 60 s)", tc.name, n)
		}
	}
}

// TestPositiveReachabilityHeldOnlyByPendingAttempts: the hold lasts until an
// attempt that would re-stamp reachability can have been published, and no
// longer; a backend with nothing pending, or whose pending attempts never
// re-stamp (denied, refused), goes stale on the positive rule.
func TestPositiveReachabilityHeldOnlyByPendingAttempts(t *testing.T) {
	pending := func(code opsbackend.Code) opsbackend.Surface {
		return opsbackend.Surface{Name: "version", LastError: code, NextAttempt: 18 * time.Second}
	}
	for _, tc := range []struct {
		name    string
		surface opsbackend.Surface
		mono    time.Duration
		ok      bool
	}{
		{"nothing pending", opsbackend.Surface{Name: "version"}, 17 * time.Second, false},
		{"401 pending at 18 s, last instant (18 + 2 + 10 + 1 s)", pending(opsbackend.CodeUnauthorized), 31 * time.Second, true},
		{"401 pending at 18 s, missed", pending(opsbackend.CodeUnauthorized), 32 * time.Second, false},
		{"denied attempts never re-stamp", pending(opsbackend.CodeDenied), 17 * time.Second, false},
		{"refused attempts never re-stamp", pending(opsbackend.CodeRefused), 17 * time.Second, false},
	} {
		b := healthyLlamaSwap() // reachability ok at 10 s
		b.Kind, b.Running, b.Rows, b.Listed = opsbackend.KindUnidentified, nil, nil, nil
		b.Surfaces = []opsbackend.Surface{tc.surface}
		in := input(ModeWatch, b, remote())
		in.NowMono = tc.mono
		s := Build(in)
		r := s.Backends[0].Reachability
		a, stale := reasons(s)[AttnTelemetryStale+":llamacpp"]
		if (r.State == ReachOK && !r.Stale) != tc.ok || stale == tc.ok || (stale && a.Text != "llamacpp has no fresh reading of reachability") {
			t.Fatalf("%s: reachability = %+v, telemetry_stale = %v %q, want ok = %v", tc.name, r, stale, a.Text, tc.ok)
		}
	}
}

// TestRetryDoesNotReorderAttention: a failed retry re-stamps the reading, so
// a since taken from it would swap two down backends every cycle.
func TestRetryDoesNotReorderAttention(t *testing.T) {
	down := func(provider string, ago, mono, retry time.Duration) opsbackend.BackendObservation {
		o := healthyLlamaSwap()
		o.Provider, o.Kind = provider, opsbackend.KindUnidentified
		o.Reachable = &opsbackend.Sample[bool]{Value: false, At: now.Add(-ago), Mono: mono}
		o.ReachCode, o.ReachRetry = opsbackend.CodeUnreachable, retry
		return o
	}
	other := down("opencode", 3*time.Second, 8*time.Second, 12*time.Second) // used by the agent chain
	before := Build(input(ModeWatch, down("llamacpp", 5*time.Second, 6*time.Second, 10*time.Second), other)).Attention
	after := Build(input(ModeWatch, down("llamacpp", time.Second, 10*time.Second, 14*time.Second), other)).Attention
	if len(before) != 2 || !reflect.DeepEqual(before, after) {
		t.Fatalf("a retry reordered attention:\nbefore %+v\n after %+v", before, after)
	}
}

func TestBackendUnsupportedOnlyWhenUsed(t *testing.T) {
	for _, support := range []string{opsbackend.SupportUnsupported, opsbackend.SupportUnrecognized, opsbackend.SupportInvalidConfig} {
		for _, provider := range []string{"llamacpp", "spare"} {
			b := healthyLlamaSwap() // cached samples stay attached and age out
			b.Provider, b.Kind, b.Support = provider, opsbackend.KindNone, support
			if support == opsbackend.SupportInvalidConfig {
				b.Hosting = opsbackend.HostingUnknown
			}
			in := input(ModeWatch, b, remote())
			in.NowMono = 17 * time.Second
			got := Build(in).Attention
			if provider == "spare" {
				if len(got) != 0 {
					t.Fatalf("%s on an unused provider raised %+v", support, got)
				}
				continue
			}
			if len(got) != 1 || got[0].Reason != AttnBackendUnsupported || got[0].Severity != SevWarning || got[0].Subject != provider || got[0].Text != provider+" is not observed: "+Label(support) {
				t.Fatalf("%s on a used provider must raise only backend_unsupported: %+v", support, got)
			}
		}
	}
}

func TestUnusedProviderDoesNotAlertWhenDown(t *testing.T) {
	down := healthyLlamaSwap()
	down.Provider = "spare"
	down.Reachable = sample(false, 10*time.Second)
	down.ReachCode, down.ReachRetry = opsbackend.CodeUnreachable, 12*time.Second
	// Known down, so not stale or overdue either: nothing at all.
	if got := Build(input(ModeWatch, down, remote())).Attention; len(got) != 0 {
		t.Fatalf("a provider no role uses raised %+v", got)
	}
}

func TestModelErrorsThresholdAndWindow(t *testing.T) {
	b := healthyLlamaSwap()
	future := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	b.Rows.Value = append(b.Rows.Value,
		row(5, 3*time.Minute, "gemma4:31b", 500, 0, 0, 0, 0, 1),
		row(6, 2*time.Minute, "gemma4:31b", 503, 0, 0, 0, 0, 1),
		row(7, 4*time.Minute, "qwen3-embedding:8b", 500, 0, 0, 0, 0, 1), // one recent error: below threshold
		row(8, 30*time.Minute, "alias-name", 500, 0, 0, 0, 0, 1),        // two old errors: outside 10 min
		row(9, 40*time.Minute, "alias-name", 500, 0, 0, 0, 0, 1),
		// Two future-dated errors (clock step or hostile peer) enter no window.
		opsbackend.ActivityRow{ID: 10, Timestamp: future, Model: "future-model", Status: 500},
		opsbackend.ActivityRow{ID: 11, Timestamp: future, Model: "future-model", Status: 500},
		row(12, time.Minute, "cached-model", 304, 0, 0, 0, 0, 1), // non-2xx, so not a success
		row(13, time.Minute, "cached-model", 304, 0, 0, 0, 0, 1),
	)
	r := reasons(Build(input(ModeWatch, b, remote())))
	a, ok := r[AttnModelErrors+":llamacpp/gemma4:31b"]
	if !ok || a.Severity != SevSerious || a.Since != nil ||
		a.Text != "at least 2 retained requests to gemma4:31b on llamacpp returned non-2xx in the last 10 min (newest at 2026-10-05T09:58:00.000Z)" {
		t.Fatalf("model_errors missing, dated (since must stay null), or not naming its newest error: %+v", r)
	}
	if _, ok := r[AttnModelErrors+":llamacpp/qwen3-embedding:8b"]; ok {
		t.Fatal("one error raised model_errors")
	}
	if _, ok := r[AttnModelErrors+":llamacpp/alias-name"]; ok {
		t.Fatal("model_errors counted rows older than 10 minutes")
	}
	if _, ok := r[AttnModelErrors+":llamacpp/future-model"]; ok {
		t.Fatal("model_errors counted future-dated rows, which would stay in the window forever")
	}
	if _, ok := r[AttnModelErrors+":llamacpp/cached-model"]; !ok {
		t.Fatal("3xx rows are non-2xx and must count as errors")
	}
	// Stale rows speak through telemetry_stale, never as current errors.
	b.Rows.Mono = time.Second
	if _, ok := reasons(Build(input(ModeWatch, b, remote())))[AttnModelErrors+":llamacpp/gemma4:31b"]; ok {
		t.Fatal("model_errors raised from a stale metrics reading")
	}
}

func TestModelErrorsAreCapped(t *testing.T) {
	b := healthyLlamaSwap()
	for i := range 300 {
		name := fmt.Sprintf("m%03d", i)
		b.Rows.Value = append(b.Rows.Value, row(int64(2+2*i), time.Minute, name, 500, 0, 0, 0, 0, 1), row(int64(3+2*i), time.Minute, name, 500, 0, 0, 0, 0, 1))
	}
	var items []Attention
	for _, a := range Build(input(ModeWatch, b, remote())).Attention {
		if a.Reason == AttnModelErrors {
			items = append(items, a)
		}
	}
	if len(items) != maxModelErrors+1 || items[0].Subject != "" || items[0].Text != "44 more models returned non-2xx in the last 10 min; not listed" ||
		items[1].Subject != "llamacpp/m000" || items[maxModelErrors].Subject != "llamacpp/m255" {
		t.Fatalf("model_errors not capped at %d plus one overflow item: %d items, first %+v", maxModelErrors, len(items), items[:min(2, len(items))])
	}
}

func TestTelemetryUnavailableGroupsByCode(t *testing.T) {
	b := healthyLlamaSwap()
	b.Surfaces = []opsbackend.Surface{ // a wrong api_key fails every surface at once
		{Name: "version", LastError: opsbackend.CodeUnauthorized}, {Name: "running", LastError: opsbackend.CodeUnauthorized},
		{Name: "metrics", LastError: opsbackend.CodeUnauthorized}, {Name: "models", LastError: opsbackend.CodeMalformed},
	}
	var got []string
	for _, a := range Build(input(ModeWatch, b, remote())).Attention {
		if a.Reason == AttnTelemetryUnavailable {
			got = append(got, a.Text)
		}
	}
	want := []string{
		"llamacpp telemetry unavailable on metrics, running, version: not authorized (check api_key)",
		"llamacpp telemetry unavailable on models: malformed response",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("telemetry_unavailable = %q, want one item per code naming its surfaces in order: %q", got, want)
	}
}

func TestTelemetryAttention(t *testing.T) {
	failing := healthyLlamaSwap()
	failing.Surfaces = []opsbackend.Surface{{Name: "running", LastError: opsbackend.CodeHTTPStatus}}
	r := reasons(Build(input(ModeWatch, failing, remote())))
	if _, ok := r[AttnTelemetryUnavailable+":llamacpp"]; !ok {
		t.Fatalf("http_status on /running raised nothing: %+v", r)
	}
	if _, ok := r[AttnBackendUnreachable+":llamacpp"]; ok {
		t.Fatal("an HTTP error must not read as unreachable")
	}
	for code, want := range map[opsbackend.Code]bool{
		opsbackend.CodeUnauthorized: true, opsbackend.CodeDenied: true, opsbackend.CodeMalformed: true,
		opsbackend.CodeTooLarge: true, opsbackend.CodeHTTPStatus: true,
		opsbackend.CodeTimeout: false, opsbackend.CodeUnreachable: false, // reachability's to report
	} {
		f := healthyLlamaSwap()
		f.Surfaces = []opsbackend.Surface{{Name: "running", LastError: code}}
		a, ok := reasons(Build(input(ModeWatch, f, remote())))[AttnTelemetryUnavailable+":llamacpp"]
		if ok != want || strings.Contains(a.Text, "unreachable") {
			t.Fatalf("%s on a surface: telemetry_unavailable = %v (%q), want %v and never worded as unreachable", code, ok, a.Text, want)
		}
	}

	// Each fact is judged on its own reading: one stale fact alerts even
	// while every other reading is fresh.
	for _, tc := range []struct {
		name string
		edit func(*opsbackend.BackendObservation)
		text string
	}{
		{"residency", func(o *opsbackend.BackendObservation) { o.Running.Mono = time.Second }, "llamacpp has no fresh reading of residency (/running)"},
		{"statistics", func(o *opsbackend.BackendObservation) { o.Rows.Mono = time.Second }, "llamacpp has no fresh reading of statistics (/api/metrics)"},
		{"reachability", func(o *opsbackend.BackendObservation) { o.Reachable.Mono = time.Second }, "llamacpp has no fresh reading of reachability"},
		{"ollama residency", func(o *opsbackend.BackendObservation) {
			*o = localOllama()
			o.Provider, o.PS.Mono = "llamacpp", time.Second
		}, "llamacpp has no fresh reading of residency (/api/ps)"},
		{"every fact", func(o *opsbackend.BackendObservation) {
			o.Running.Mono, o.Rows.Mono, o.Reachable.Mono = time.Second, time.Second, time.Second
		}, "llamacpp has no fresh reading of residency (/running), statistics (/api/metrics), reachability"},
	} {
		stale := healthyLlamaSwap()
		tc.edit(&stale)
		if a, ok := reasons(Build(input(ModeWatch, stale, remote())))[AttnTelemetryStale+":llamacpp"]; !ok || a.Severity != SevWarning || a.Text != tc.text {
			t.Fatalf("stale %s masked by fresh readings or misnamed: %+v, want %q", tc.name, a, tc.text)
		}
	}
	if r := reasons(Build(input(ModeWatch, healthyLlamaSwap(), remote()))); len(r) != 0 {
		t.Fatalf("a fresh healthy backend raised %+v", r)
	}
}

func TestRefusedConfigAndCoverageAttention(t *testing.T) {
	refused := healthyLlamaSwap()
	refused.Refused = 1
	if a, ok := reasons(Build(input(ModeWatch, refused, remote())))[AttnRefused+":llamacpp"]; !ok || a.Severity != SevSerious {
		t.Fatalf("refused_requests missing or not serious: %+v", a)
	}

	in := input(ModeWatch, healthyLlamaSwap(), remote())
	in.Config.Diagnostics = []configview.Diagnostic{{Code: "chain_invalid", Subject: "agent"}}
	if _, ok := reasons(Build(in))[AttnConfigProblem+":agent"]; !ok {
		t.Fatal("config_problem missing while Ready stays true")
	}

	// configview reports a missing models.json as not ready plus a
	// config_missing diagnostic: one problem, one item.
	missing := input(ModeOnce)
	missing.Config = configview.Build(configview.BuildInput{})
	if got := Build(missing).Attention; len(got) != 1 || got[0].Reason != AttnConfigProblem || got[0].Text != "models.json problem: config_missing" {
		t.Fatalf("a missing models.json must raise exactly one config_problem: %+v", got)
	}

	if a, ok := reasons(Build(input(ModeWatch, remote())))[AttnCoverageNote+":"]; !ok || a.Severity != SevInfo {
		t.Fatalf("coverage_note missing for an all-remote configuration, or not info: %+v", a)
	}
	invalid := opsbackend.BackendObservation{Provider: "broken", Hosting: opsbackend.HostingUnknown, Kind: opsbackend.KindNone, Support: opsbackend.SupportInvalidConfig}
	if _, ok := reasons(Build(input(ModeWatch, remote(), invalid)))[AttnCoverageNote+":"]; ok {
		t.Fatal("an invalid local provider is not remote; coverage_note must not claim all-remote")
	}
}

func TestAttentionOrdering(t *testing.T) {
	b := healthyLlamaSwap()
	b.Kind = opsbackend.KindUnidentified
	b.Reachable = sample(false, 10*time.Second)
	b.ReachCode, b.ReachRetry = opsbackend.CodeTimeout, 12*time.Second
	b.Refused = 2
	s := Build(input(ModeWatch, b, remote()))
	if len(s.Attention) < 2 {
		t.Fatalf("expected several items: %+v", s.Attention)
	}
	for i := 1; i < len(s.Attention); i++ {
		if severityRank[s.Attention[i-1].Severity] > severityRank[s.Attention[i].Severity] {
			t.Fatalf("attention not severity-ordered: %+v", s.Attention)
		}
	}

	// Within a severity, subject decides before text: alpha's text
	// ("at least 3") sorts after zeta's ("at least 2").
	e := healthyLlamaSwap()
	e.Rows.Value = append(e.Rows.Value,
		row(5, time.Minute, "alpha", 500, 0, 0, 0, 0, 1), row(6, time.Minute, "alpha", 500, 0, 0, 0, 0, 1), row(7, time.Minute, "alpha", 500, 0, 0, 0, 0, 1),
		row(8, time.Minute, "zeta", 500, 0, 0, 0, 0, 1), row(9, time.Minute, "zeta", 500, 0, 0, 0, 0, 1),
	)
	if got := Build(input(ModeWatch, e, remote())).Attention; len(got) != 2 || got[0].Subject != "llamacpp/alpha" || got[1].Subject != "llamacpp/zeta" {
		t.Fatalf("within a severity, attention orders by subject before text: %+v", got)
	}

	// Same severity and subject: reason decides before text, whose order
	// ("agent has..." before "models.json...") is the reverse.
	a := healthyLlamaSwap()
	a.Provider, a.Running.Mono = "agent", time.Second
	in := input(ModeWatch, a, remote())
	in.Config.Diagnostics = []configview.Diagnostic{{Code: "chain_invalid", Subject: "agent"}}
	if got := Build(in).Attention; len(got) != 2 || got[0].Reason != AttnConfigProblem || got[1].Reason != AttnTelemetryStale {
		t.Fatalf("within a severity and subject, attention orders by reason before text: %+v", got)
	}
}

// TestAttentionTiesOrderDeterministically pins a total order: two
// diagnostics on one use case raise items with equal severity, subject and
// reason, so a renderer comparing the top item would see a change on every
// reordering unless the text breaks the tie.
func TestAttentionTiesOrderDeterministically(t *testing.T) {
	diags := []configview.Diagnostic{{Code: "selector_type_conflict", Subject: "agent"}, {Code: "chain_invalid", Subject: "agent"}}
	in := input(ModeWatch, healthyLlamaSwap(), remote())
	in.Config.Diagnostics = diags
	want := Build(in).Attention
	in.Config.Diagnostics = []configview.Diagnostic{diags[1], diags[0]}
	if got := Build(in).Attention; len(want) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("attention order followed diagnostics order:\n got %+v\nwant %+v", got, want)
	}
}

// TestAttentionJSONKeys pins every Attention field's wire name: a null since
// stays null and an empty subject stays present.
func TestAttentionJSONKeys(t *testing.T) {
	got, err := json.Marshal([]Attention{
		{Severity: SevCritical, Reason: AttnBackendUnreachable, Subject: "llamacpp", Since: stamp(now), Text: "t"},
		{Severity: SevInfo, Reason: AttnCoverageNote, Text: "u"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"severity":"critical","reason":"backend_unreachable","subject":"llamacpp","since":"2026-10-05T10:00:00.000Z","text":"t"},` +
		`{"severity":"info","reason":"coverage_note","subject":"","since":null,"text":"u"}]`
	if string(got) != want {
		t.Fatalf("attention wire keys:\n got %s\nwant %s", got, want)
	}
}
