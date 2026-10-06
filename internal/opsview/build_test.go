package opsview

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/internal/opsbackend"
)

var update = flag.Bool("update", false, "rewrite golden files")

func sample[T any](v T, mono time.Duration) *opsbackend.Sample[T] {
	return &opsbackend.Sample[T]{Value: v, At: now, Mono: mono}
}

func healthyLlamaSwap() opsbackend.BackendObservation {
	return opsbackend.BackendObservation{
		Provider: "llamacpp", Endpoint: "http://127.0.0.1:8090", Hosting: opsbackend.HostingLocal,
		Kind: opsbackend.KindLlamaSwap, Version: "v235", Support: opsbackend.SupportSupported,
		Reachable: sample(true, 10*time.Second),
		Running:   sample([]opsbackend.RunningModel{{Model: "gemma4:31b", State: "ready", TTL: 600}}, 10*time.Second),
		Rows: sample([]opsbackend.ActivityRow{
			row(0, 2*time.Minute, "gemma4:31b", 200, 900, 300, 910, 41, 8000),
			row(1, time.Minute, "qwen3-embedding:8b", 200, 50, 0, 2000, 0, 30),
		}, 10*time.Second),
		Listed: sample([]opsbackend.ListedModel{{ID: "gemma4:31b"}, {ID: "qwen3-embedding:8b"}, {ID: "peer-model", Peer: true}}, 10*time.Second),
		Since:  now.Add(-time.Hour), Periods: 1,
		Models: []opsbackend.ModelMemory{{Model: "gemma4:31b", FirstObserved: now.Add(-14 * time.Minute), Loads: 1, Since: now.Add(-time.Hour)}},
	}
}

func input(mode Mode, backends ...opsbackend.BackendObservation) Input {
	return Input{
		Config: configview.Snapshot{
			Ready:  true,
			Origin: configview.OriginProjection{Source: "user_config"},
			Bindings: []configview.RoleBinding{
				{UseCase: "agent", Role: "agent", Chain: []string{"llamacpp/gemma4:31b", "opencode/kimi-k3"}},
				{UseCase: "embedding", Role: "embed", Chain: []string{"llamacpp/qwen3-embedding:8b"}},
			},
		},
		Configured:   []string{"llamacpp/alias-name", "llamacpp/gemma4:31b", "llamacpp/qwen3-embedding:8b", "opencode/kimi-k3"},
		Revision:     "3f9c1a",
		Observations: opsbackend.Observations{Interval: 2 * time.Second, Backends: backends},
		Now:          now, NowMono: 11 * time.Second, Mode: mode, Interval: 2 * time.Second,
	}
}

func remote() opsbackend.BackendObservation {
	return opsbackend.BackendObservation{Provider: "opencode", Endpoint: "https://opencode.ai", Hosting: opsbackend.HostingRemote, Kind: opsbackend.KindNone, Support: opsbackend.SupportNotObserved}
}

func modelByID(t *testing.T, s Snapshot, id string) Model {
	t.Helper()
	for _, m := range s.Models {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("model %q missing from %+v", id, s.Models)
	return Model{}
}

func TestResidencyRules(t *testing.T) {
	s := Build(input(ModeWatch, healthyLlamaSwap(), remote()))
	if m := modelByID(t, s, "llamacpp/gemma4:31b"); m.Residency.State != StateLoaded || m.Residency.FirstObserved == nil {
		t.Fatalf("gemma = %+v", m.Residency)
	}
	// confirmed by a 2xx row, absent from /running
	if m := modelByID(t, s, "llamacpp/qwen3-embedding:8b"); m.Residency.State != StateUnloaded {
		t.Fatalf("embedding = %+v", m.Residency)
	}
	// alias: never in /running or rows
	m := modelByID(t, s, "llamacpp/alias-name")
	if m.Residency.State != StateUnknown || deref(m.Residency.Reason) != ReasonUnconfirmed {
		t.Fatalf("alias = %+v", m.Residency)
	}
	if m := modelByID(t, s, "opencode/kimi-k3"); m.Residency.State != StateNotObserved || deref(m.Residency.Reason) != ReasonRemote {
		t.Fatalf("remote = %+v", m.Residency)
	}
}

func TestErrorRowsConfirmNothing(t *testing.T) {
	b := healthyLlamaSwap()
	b.Rows.Value = append(b.Rows.Value, row(2, 30*time.Second, "alias-name", 500, 0, 0, 0, 0, 1))
	if m := modelByID(t, Build(input(ModeWatch, b, remote())), "llamacpp/alias-name"); m.Residency.State != StateUnknown {
		t.Fatalf("a non-2xx row confirmed identity: %+v", m.Residency)
	}
}

func TestPeerConfirmationFailsClosed(t *testing.T) {
	b := healthyLlamaSwap()
	b.Listed = nil
	if m := modelByID(t, Build(input(ModeWatch, b, remote())), "llamacpp/qwen3-embedding:8b"); m.Residency.State != StateUnknown {
		t.Fatalf("without a listing, ring rows must confirm nothing: %+v", m.Residency)
	}
	b = healthyLlamaSwap()
	b.Rows.Value = append(b.Rows.Value, row(2, 30*time.Second, "peer-model", 200, 1, 1, 1, 1, 1))
	for _, m := range Build(input(ModeWatch, b, remote())).Models {
		if m.BackendModel == "peer-model" {
			t.Fatalf("peer model surfaced as a backend model: %+v", m)
		}
	}
}

func TestStaleDegradesToUnknown(t *testing.T) {
	in := input(ModeWatch, healthyLlamaSwap(), remote())
	in.NowMono = 17 * time.Second // > 3 x 2s since the 10s samples
	m := modelByID(t, Build(in), "llamacpp/gemma4:31b")
	if m.Residency.State != StateUnknown || deref(m.Residency.Reason) != ReasonStale || deref(m.Residency.LastState) != StateLoaded || !m.Residency.Stale {
		t.Fatalf("stale = %+v", m.Residency)
	}
	// Staleness is the envelope's to report: identity proven by an aged
	// sample still holds, so the counts stay, marked stale.
	if st := m.Stats[0]; st.Calls == nil || *st.Calls != 1 || !st.Stale {
		t.Fatalf("stale stats lost their values: %+v", st)
	}
}

func TestUnsupportedBackendIgnoresCachedSamples(t *testing.T) {
	b := healthyLlamaSwap()
	b.Kind, b.Support = opsbackend.KindNone, opsbackend.SupportUnsupported // samples still attached
	m := modelByID(t, Build(input(ModeWatch, b, remote())), "llamacpp/gemma4:31b")
	if m.Residency.State != StateNotObserved || len(m.Stats) != 0 || m.Loads != nil {
		t.Fatalf("unsupported backend still projected cached facts: %+v", m)
	}
}

func TestFailureReachabilityFollowsRetry(t *testing.T) {
	b := healthyLlamaSwap()
	b.Kind = opsbackend.KindUnidentified
	b.Reachable = sample(false, 2*time.Second)
	b.ReachCode, b.ReachRetry = opsbackend.CodeUnreachable, 18*time.Second // 16 s backoff
	in := input(ModeWatch, b, remote())
	in.NowMono = 12 * time.Second // 10 s after the failure: past 6 s, before the retry
	r := Build(in).Backends[0].Reachability
	if r.State != ReachUnreachable || r.Stale || r.RetryInMs == nil || *r.RetryInMs != 6000 {
		t.Fatalf("between retries = %+v", r)
	}
	// The retry's answer may publish up to retry + interval + 2 ticks + 1 s
	// (31 s) later; past that, the console missed its own schedule.
	in.NowMono = 32 * time.Second
	if r := Build(in).Backends[0].Reachability; r.State != StateUnknown || !r.Stale {
		t.Fatalf("missed retry = %+v", r)
	}
}

func TestActivityNeverIdleAndUsedBy(t *testing.T) {
	s := Build(input(ModeOnce, healthyLlamaSwap(), remote()))
	for _, m := range s.Models {
		if m.Activity.State != StateUnknown && m.Activity.State != StateNotObserved {
			t.Fatalf("%s activity = %q", m.ID, m.Activity.State)
		}
		if m.Loads != nil {
			t.Fatalf("once mode must omit loads: %s", m.ID)
		}
	}
	m := modelByID(t, s, "opencode/kimi-k3")
	if !reflect.DeepEqual(m.UsedBy, []UsedBy{{UseCase: "agent", Role: "agent", IsFallback: true}}) {
		t.Fatalf("used_by = %+v", m.UsedBy)
	}
}

func TestLastCompletedAtSkipsFutureRows(t *testing.T) {
	b := healthyLlamaSwap()
	b.Rows.Value = append(b.Rows.Value,
		row(2, 30*time.Second, "gemma4:31b", 500, 0, 0, 0, 0, 1), // newest placeable row; any status counts
		opsbackend.ActivityRow{ID: 3, Timestamp: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), Model: "gemma4:31b", Status: 200},
	)
	m := modelByID(t, Build(input(ModeWatch, b, remote())), "llamacpp/gemma4:31b")
	if got, want := deref(m.Activity.LastCompletedAt), deref(stamp(now.Add(-30*time.Second))); got != want {
		t.Fatalf("last_completed_at = %q, want %q (newest non-future row of any status)", got, want)
	}
}

func TestUnconfirmedNameClaimsNoStats(t *testing.T) {
	b := healthyLlamaSwap()
	b.Rows.Value = append(b.Rows.Value, row(2, 30*time.Second, "alias-name", 500, 0, 0, 0, 0, 1))
	m := modelByID(t, Build(input(ModeWatch, b, remote())), "llamacpp/alias-name")
	if len(m.Stats) != len(windows) {
		t.Fatalf("alias stats = %+v", m.Stats)
	}
	for _, st := range m.Stats {
		if st.Calls != nil || st.Errors != nil || deref(st.OutputTokens.Reason) != ReasonUnconfirmed || st.Coverage.Reason != ReasonUnconfirmed {
			t.Fatalf("an alias's requests are recorded under its canonical ID, so counting its name claims a false measured value: %+v", st)
		}
	}
}

func TestLoadsOnlyForObservedBackends(t *testing.T) {
	s := Build(input(ModeWatch, healthyLlamaSwap(), remote()))
	if modelByID(t, s, "opencode/kimi-k3").Loads != nil {
		t.Fatal("remote model got a fabricated loads count")
	}
	if l := modelByID(t, s, "llamacpp/gemma4:31b").Loads; l == nil || l.Count != 1 {
		t.Fatalf("observed loads = %+v", l)
	}
	// A backend re-identified as unsupported keeps its memory counters; a
	// remote one has none, so only this case shows the gate itself.
	u := healthyLlamaSwap()
	u.Kind, u.Support = opsbackend.KindNone, opsbackend.SupportUnsupported
	if l := modelByID(t, Build(input(ModeWatch, u, remote())), "llamacpp/gemma4:31b").Loads; l != nil {
		t.Fatalf("unobserved backend got loads from its memory: %+v", l)
	}
}

func TestUnmeasuredLoadsAreOmitted(t *testing.T) {
	never := healthyLlamaSwap()
	never.Periods, never.Models = 0, nil // no residency sample has succeeded yet
	if l := modelByID(t, Build(input(ModeWatch, never, remote())), "llamacpp/gemma4:31b").Loads; l != nil {
		t.Fatalf("loads before any residency sample = %+v", l)
	}
	capped := healthyLlamaSwap()
	capped.ModelsOverflow = 3 // an untracked model may have been dropped by the cap
	if l := modelByID(t, Build(input(ModeWatch, capped, remote())), "llamacpp/qwen3-embedding:8b").Loads; l != nil {
		t.Fatalf("loads for a possibly capped model = %+v", l)
	}
	s := Build(input(ModeWatch, healthyLlamaSwap(), remote()))
	if l := modelByID(t, s, "llamacpp/qwen3-embedding:8b").Loads; l == nil || l.Count != 0 {
		t.Fatalf("measured zero missing: %+v", l)
	}
	// An alias loads under its canonical ID, so its zero would be invented.
	if l := modelByID(t, s, "llamacpp/alias-name").Loads; l != nil {
		t.Fatalf("loads for an unconfirmed name = %+v", l)
	}
}

func TestBackendOnlyModelsBoundedAndRowOnly(t *testing.T) {
	b := healthyLlamaSwap()
	b.Running = nil // /running failing; rows and a fresh listing still confirm identity
	for i := range 300 {
		name := fmt.Sprintf("extra-%03d", i)
		b.Rows.Value = append(b.Rows.Value, row(int64(2+i), time.Minute, name, 200, 1, 1, 1, 1, 1))
	}
	s := Build(input(ModeWatch, b, remote()))
	only := 0
	for _, m := range s.Models {
		if !m.Configured {
			only++
		}
	}
	if only != maxBackendOnly || s.Backends[0].Observation.BackendOnlyOverflow != 300-maxBackendOnly {
		t.Fatalf("backend-only = %d overflow = %d", only, s.Backends[0].Observation.BackendOnlyOverflow)
	}
	modelByID(t, s, "backend:llamacpp/extra-000") // sorted: the first names are kept
}

func TestCoverageUsesWholeRing(t *testing.T) {
	b := healthyLlamaSwap()
	b.Rows.Value = []opsbackend.ActivityRow{
		row(0, 3*time.Minute, "gemma4:31b", 200, 1, 1, 1, 1, 1),
		row(1, 2*time.Minute, "qwen3-embedding:8b", 200, 1, 1, 1, 1, 1),
		row(2, time.Minute, "gemma4:31b", 200, 1, 1, 1, 1, 1),
	}
	m := modelByID(t, Build(input(ModeWatch, b, remote())), "llamacpp/gemma4:31b")
	if !m.Stats[0].Coverage.Contiguous {
		t.Fatal("coverage judged on model-filtered rows")
	}
}

func TestOllamaStatsCarryNoHistoryReason(t *testing.T) {
	o := opsbackend.BackendObservation{
		Provider: "ollama", Endpoint: "http://127.0.0.1:11434", Hosting: opsbackend.HostingLocal, Kind: opsbackend.KindOllama,
		Support: opsbackend.SupportSupported, Reachable: sample(true, 10*time.Second),
		PS: sample([]opsbackend.PSModel{{Name: "llama3:latest", Model: "llama3:latest", ExpiresAt: now.Add(5 * time.Minute)}}, 10*time.Second),
	}
	in := input(ModeWatch, o)
	in.Configured = []string{"ollama/llama3", "ollama/qwen3:8b"}
	s := Build(in)
	if m := modelByID(t, s, "ollama/llama3"); m.Residency.State != StateLoaded || m.Residency.ExpiresAt == nil || len(m.Stats) != 2 || m.Stats[0].Coverage.Reason != ReasonNoHistory {
		t.Fatalf("ollama present = %+v", m)
	}
	if m := modelByID(t, s, "ollama/qwen3:8b"); m.Residency.State != StateUnknown || deref(m.Residency.Reason) != ReasonOllamaAbsent {
		t.Fatalf("ollama absent = %+v", m.Residency)
	}
}

func TestBlankOllamaNamesNeverMatch(t *testing.T) {
	o := opsbackend.BackendObservation{
		Provider: "ollama", Endpoint: "http://127.0.0.1:11434", Hosting: opsbackend.HostingLocal, Kind: opsbackend.KindOllama,
		Support: opsbackend.SupportSupported, Reachable: sample(true, 10*time.Second), Since: now.Add(-time.Hour), Periods: 1,
		// decodePS admits a blank model and a whitespace name, which both
		// normalize to "", and memory keys the whitespace name as "".
		PS:     sample([]opsbackend.PSModel{{Name: " ", Model: ""}}, 10*time.Second),
		Models: []opsbackend.ModelMemory{{Model: "", Loads: 3, Since: now.Add(-time.Hour)}},
	}
	in := input(ModeWatch, o)
	in.Configured = []string{"ollama/"}
	s := Build(in)
	if m := modelByID(t, s, "ollama/"); m.Residency.State != StateUnknown || (m.Loads != nil && m.Loads.Count != 0) {
		t.Fatalf("a blank configured name matched a blank ps entry: %+v", m)
	}
	modelByID(t, s, "backend:ollama/ ") // and that entry was not taken for the configured name
}

func TestEnumsNeverIdleOrComplete(t *testing.T) {
	s := Build(input(ModeWatch, healthyLlamaSwap(), remote()))
	for _, m := range s.Models {
		for _, v := range []string{m.Residency.State, m.Activity.State} {
			if v == "idle" || v == "complete" {
				t.Fatalf("%s carries forbidden enum %q", m.ID, v)
			}
		}
		for _, st := range m.Stats {
			if st.Coverage.State == "complete" {
				t.Fatalf("%s coverage complete", m.ID)
			}
		}
	}
}

func TestHealthyGolden(t *testing.T) {
	s := Build(input(ModeWatch, healthyLlamaSwap(), remote()))
	var got bytes.Buffer
	enc := json.NewEncoder(&got)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "healthy.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("golden mismatch; run go test ./internal/opsview -run TestHealthyGolden -update and review the diff\n%s", got.String())
	}
}
