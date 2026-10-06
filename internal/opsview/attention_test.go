package opsview

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/internal/opsbackend"
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
	for _, nowMono := range []time.Duration{3 * time.Second, 12 * time.Second, 20 * time.Second} {
		in := input(ModeWatch, down, remote())
		in.NowMono = nowMono
		r := reasons(Build(in))
		if a, ok := r[AttnBackendUnreachable+":llamacpp"]; !ok || a.Severity != SevCritical {
			t.Fatalf("t=%v: unreachable attention missing: %+v", nowMono, r)
		}
		if _, ok := r[AttnTelemetryStale+":llamacpp"]; ok {
			t.Fatalf("t=%v: a known-down backend must not also read stale", nowMono)
		}
	}
}

func TestUnusedProviderDoesNotAlertWhenDown(t *testing.T) {
	down := healthyLlamaSwap()
	down.Provider = "spare"
	down.Reachable = sample(false, 10*time.Second)
	down.ReachCode, down.ReachRetry = opsbackend.CodeUnreachable, 12*time.Second
	if _, ok := reasons(Build(input(ModeWatch, down, remote())))[AttnBackendUnreachable+":spare"]; ok {
		t.Fatal("a provider no role uses raised a critical alert")
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
	)
	r := reasons(Build(input(ModeWatch, b, remote())))
	if a, ok := r[AttnModelErrors+":gemma4:31b"]; !ok || a.Severity != SevSerious || deref(a.Since) != deref(stamp(now.Add(-2*time.Minute))) {
		t.Fatalf("model_errors missing or not dated by its newest error: %+v", r)
	}
	if _, ok := r[AttnModelErrors+":qwen3-embedding:8b"]; ok {
		t.Fatal("one error raised model_errors")
	}
	if _, ok := r[AttnModelErrors+":alias-name"]; ok {
		t.Fatal("model_errors counted rows older than 10 minutes")
	}
	if _, ok := r[AttnModelErrors+":future-model"]; ok {
		t.Fatal("model_errors counted future-dated rows, which would stay in the window forever")
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

	// Metrics fresh, residency stale: the stale residency must still alert.
	stale := healthyLlamaSwap()
	stale.Running = sample(stale.Running.Value, 1*time.Second)
	stale.Reachable = sample(true, 10*time.Second)
	if _, ok := reasons(Build(input(ModeWatch, stale, remote())))[AttnTelemetryStale+":llamacpp"]; !ok {
		t.Fatal("stale residency masked by fresh metrics")
	}
}

func TestRefusedConfigAndCoverageAttention(t *testing.T) {
	refused := healthyLlamaSwap()
	refused.Refused = 1
	if _, ok := reasons(Build(input(ModeWatch, refused, remote())))[AttnRefused+":llamacpp"]; !ok {
		t.Fatal("refused_requests missing")
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
	if got := Build(missing).Attention; len(got) != 1 || got[0].Reason != AttnConfigProblem {
		t.Fatalf("a missing models.json must raise exactly one config_problem: %+v", got)
	}

	if _, ok := reasons(Build(input(ModeWatch, remote())))[AttnCoverageNote+":"]; !ok {
		t.Fatal("coverage_note missing for an all-remote configuration")
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

	// Within a severity, the older since comes first, ahead of subject.
	e := healthyLlamaSwap()
	e.Rows.Value = append(e.Rows.Value,
		row(5, time.Minute, "alpha", 500, 0, 0, 0, 0, 1), row(6, time.Minute, "alpha", 500, 0, 0, 0, 0, 1),
		row(7, 5*time.Minute, "zeta", 500, 0, 0, 0, 0, 1), row(8, 5*time.Minute, "zeta", 500, 0, 0, 0, 0, 1),
	)
	if got := Build(input(ModeWatch, e, remote())).Attention; len(got) != 2 || got[0].Subject != "zeta" || got[1].Subject != "alpha" {
		t.Fatalf("within a severity, attention orders by since then subject: %+v", got)
	}
}

// TestAttentionTiesOrderDeterministically pins a total order: two providers
// serving the same model name raise model_errors with equal severity, since
// and subject, so a renderer comparing the top item would see a change on
// every reordering unless the text, which names the provider, breaks the tie.
func TestAttentionTiesOrderDeterministically(t *testing.T) {
	a, c := healthyLlamaSwap(), healthyLlamaSwap()
	c.Provider = "spare"
	for _, o := range []*opsbackend.BackendObservation{&a, &c} {
		o.Rows.Value = append(o.Rows.Value, row(5, time.Minute, "gemma4:31b", 500, 0, 0, 0, 0, 1), row(6, time.Minute, "gemma4:31b", 500, 0, 0, 0, 0, 1))
	}
	want := Build(input(ModeWatch, a, c)).Attention
	if len(want) != 2 || want[0].Text == want[1].Text {
		t.Fatalf("same-name model errors on two providers must say which provider: %+v", want)
	}
	if got := Build(input(ModeWatch, c, a)).Attention; !reflect.DeepEqual(got, want) {
		t.Fatalf("attention order followed backend order:\n got %+v\nwant %+v", got, want)
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
