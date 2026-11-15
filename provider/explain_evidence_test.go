// provider/explain_evidence_test.go
//
// Tests for the pure capability precedence core (#736). Each fixture holds only
// the sources its name gives; rows are understood by fingerprint.ReadCapabilities
// (chat-stream-tools through openai-compat, ollama-show through ollama).
package provider

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

const eeDigest = "sha256:ee"

// eeNow is when every fixture is read, two hours after logT0.
var eeNow = logT0.Add(2 * time.Hour)

// eeRow is an understood row at the fixture digest with the expiry a writer
// would give it: ollama-show through ollama, any other method through
// openai-compat.
func eeRow(method string, source fingerprint.EvidenceSource, kind fingerprint.EvidenceKind, capability string, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
	adapter := fingerprint.AdapterOpenAICompat
	if method == fingerprint.MethodOllamaShow {
		adapter = fingerprint.AdapterOllama
	}
	r := fingerprint.EvidenceRow{
		KeyVersion: fingerprint.EvidenceKeyVersion, Endpoint: "http://127.0.0.1:8080", Adapter: adapter,
		Model: "m", Digest: eeDigest, Capability: capability, Source: source, Kind: kind, State: state,
		Method: method, MethodVersion: 1, ObservedAt: at,
	}
	r.FreshUntil, r.ExpiresAt = fingerprint.ExpiryFor(r, 0)
	return r
}

func eeProbe(capability string, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
	return eeRow(fingerprint.MethodChatStreamTools, fingerprint.EvidenceSourceProbe, fingerprint.EvidenceKindAnswer, capability, state, at)
}

func eeObserved(capability string, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
	return eeRow(fingerprint.MethodChatStreamTools, fingerprint.EvidenceSourceObserved, fingerprint.EvidenceKindAnswer, capability, state, at)
}

func eeAttempt(capability string, at time.Time) fingerprint.EvidenceRow {
	return eeRow(fingerprint.MethodChatStreamTools, fingerprint.EvidenceSourceProbe, fingerprint.EvidenceKindAttempt, capability, fingerprint.CapProbeInconclusive, at)
}

func eeFlag(capability string, at time.Time) fingerprint.EvidenceRow {
	return eeRow(fingerprint.MethodChatStreamTools, fingerprint.EvidenceSourceObserved, fingerprint.EvidenceKindFlag, capability, fingerprint.CapProbeFlagged, at)
}

// eeShow is what Ollama's /api/show reported, stored as a provider row.
func eeShow(capability string, state fingerprint.CapProbeState, at time.Time) fingerprint.EvidenceRow {
	return eeRow(fingerprint.MethodOllamaShow, fingerprint.EvidenceSourceProvider, fingerprint.EvidenceKindAnswer, capability, state, at)
}

// eeNameOnly is r stored without a content digest, with the expiry that implies.
func eeNameOnly(r fingerprint.EvidenceRow) fingerprint.EvidenceRow {
	r.Digest = ""
	r.FreshUntil, r.ExpiresAt = fingerprint.ExpiryFor(r, 0)
	return r
}

// eeIn reads rows through adapter at the fixture digest at eeNow.
func eeIn(adapter string, rows ...fingerprint.EvidenceRow) EvidenceInputs {
	return EvidenceInputs{Adapter: adapter, Digest: eeDigest, Now: eeNow, Rows: rows}
}

func eeNone(capability string) CapabilityFact {
	return CapabilityFact{Capability: capability, State: "unknown", Source: "none"}
}

func eeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// eeWant checks one fact per canonical capability in canonical order: those
// named in want equal it, every other one is unanswered.
func eeWant(t *testing.T, facts []CapabilityFact, want ...CapabilityFact) {
	t.Helper()
	if len(facts) != len(CanonicalCapabilityNames) {
		t.Fatalf("%d facts %s; want %d", len(facts), eeJSON(facts), len(CanonicalCapabilityNames))
	}
	byName := make(map[string]CapabilityFact, len(want))
	for _, w := range want {
		byName[w.Capability] = w
	}
	for i, name := range CanonicalCapabilityNames {
		w, ok := byName[name]
		if !ok {
			w = eeNone(name)
		}
		if !reflect.DeepEqual(facts[i], w) {
			t.Errorf("fact %d (%s):\n got %s\nwant %s", i, name, eeJSON(facts[i]), eeJSON(w))
		}
	}
}

// eeFact is the fact for capability, failing when there is none.
func eeFact(t *testing.T, facts []CapabilityFact, capability string) CapabilityFact {
	t.Helper()
	for _, f := range facts {
		if f.Capability == capability {
			return f
		}
	}
	t.Fatalf("no fact for %q in %s", capability, eeJSON(facts))
	return CapabilityFact{}
}

// --- one per source ---

func TestExplainEvidence_Explicit(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat)
	in.Explicit = []string{"tool_call", "chat"}
	var want []CapabilityFact
	for _, name := range CanonicalCapabilityNames {
		state := "no"
		if name == "chat" || name == "tool_call" {
			state = "yes"
		}
		want = append(want, CapabilityFact{Capability: name, State: state, Source: "explicit"})
	}
	eeWant(t, ExplainEvidence(in), want...)
}

func TestExplainEvidence_Probe(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat, eeProbe("tool_call", fingerprint.CapProbeYes, logT0))
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "yes", Source: "probe", Method: "chat-stream-tools",
		Identity: "content-digest", CheckedAt: logT0,
	})
}

func TestExplainEvidence_Observed(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat, eeObserved("tool_call", fingerprint.CapProbeNo, logT0))
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "no", Source: "observed", Method: "chat-stream-tools",
		Identity: "content-digest", CheckedAt: logT0,
	})
}

func TestExplainEvidence_Provider(t *testing.T) {
	in := eeIn(fingerprint.AdapterOllama, eeShow("thinking", fingerprint.CapProbeYes, logT0))
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "thinking", State: "yes", Source: "provider", Method: "ollama-show",
		Identity: "content-digest", CheckedAt: logT0,
	})
}

func TestExplainEvidence_ProviderLive(t *testing.T) {
	tests := []struct {
		name   string
		digest string
		live   fingerprint.CapProbeState
		want   CapabilityFact
	}{
		{"yes at a digest", eeDigest, fingerprint.CapProbeYes,
			CapabilityFact{Capability: "thinking", State: "yes", Source: "provider", Method: "ollama-show", Identity: "content-digest"}},
		{"no, name-only", "", fingerprint.CapProbeNo,
			CapabilityFact{Capability: "thinking", State: "no", Source: "provider", Method: "ollama-show", Identity: "name-only"}},
		{"inconclusive answers nothing", eeDigest, fingerprint.CapProbeInconclusive, eeNone("thinking")},
		{"empty answers nothing", eeDigest, "", eeNone("thinking")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := eeIn(fingerprint.AdapterOllama)
			in.Digest = tt.digest
			in.Runtime = map[string]fingerprint.CapProbeState{"thinking": tt.live}
			eeWant(t, ExplainEvidence(in), tt.want)
		})
	}
}

func TestExplainEvidence_Catalog(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat)
	in.Catalog = &ModelProfile{Family: "fam", Caps: CapChat | CapToolCall}
	eeWant(t, ExplainEvidence(in),
		CapabilityFact{Capability: "chat", State: "yes", Source: "catalog", Method: "catalog:fam"},
		CapabilityFact{Capability: "tool_call", State: "yes", Source: "catalog", Method: "catalog:fam"},
	)
}

func TestExplainEvidence_Type(t *testing.T) {
	tests := []struct {
		name       string
		names      []string
		wantMethod string
	}{
		{"named type", []string{"embedding"}, "type:embedding"},
		{"no type names", nil, "type"},
		{"empty type names", []string{}, "type"},
		{"unsorted and duplicated names", []string{"embedding", "dense", "embedding"}, "type:dense+embedding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := eeIn(fingerprint.AdapterOpenAICompat)
			in.TypeCaps, in.TypeNames = CapEmbed, tt.names
			before := slices.Clone(tt.names)
			eeWant(t, ExplainEvidence(in),
				CapabilityFact{Capability: "embed", State: "yes", Source: "type", Method: tt.wantMethod})
			if !slices.Equal(tt.names, before) {
				t.Errorf("TypeNames changed to %q; want the caller's %q untouched", tt.names, before)
			}
		})
	}
}

// TestExplainEvidence_Attempt: an attempt answers inconclusive while it holds
// its cooldown, its expiry inclusive; one with no ExpiresAt holds indefinitely.
func TestExplainEvidence_Attempt(t *testing.T) {
	expiry := logT0.Add(24 * time.Hour)
	tests := []struct {
		name      string
		expiresAt time.Time
		now       time.Time
	}{
		{"within its cooldown", expiry, eeNow},
		{"read at exactly its expiry", expiry, expiry},
		{"no expiry holds indefinitely", time.Time{}, logT0.Add(365 * 24 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := eeAttempt("tool_call", logT0)
			attempt.Detail, attempt.ExpiresAt = fingerprint.DetailToolIgnored, tt.expiresAt
			in := eeIn(fingerprint.AdapterOpenAICompat, attempt)
			in.Now = tt.now
			eeWant(t, ExplainEvidence(in), CapabilityFact{
				Capability: "tool_call", State: "inconclusive", Source: "probe", Method: "chat-stream-tools",
				Detail: "tool_ignored", Identity: "content-digest", CheckedAt: logT0, ExpiresAt: tt.expiresAt,
			})
		})
	}
}

func TestExplainEvidence_History(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat)
	in.Legacy = &fingerprint.CapProbe{BackendID: "local", Capability: "tool_call", State: fingerprint.CapProbeNo, TestedAt: logT0}
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "unknown", Source: "history",
		History: &HistoryAnswer{Provider: "local", State: "no", At: logT0},
	})
}

func TestExplainEvidence_None(t *testing.T) {
	eeWant(t, ExplainEvidence(eeIn(fingerprint.AdapterOpenAICompat)))
	eeWant(t, ExplainEvidence(EvidenceInputs{}))
}

// --- conflicts (#736 acceptance) ---

func TestExplainEvidence_CatalogYesVsCheckedNo(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat, eeProbe("tool_call", fingerprint.CapProbeNo, logT0))
	in.Catalog = &ModelProfile{Family: "fam", Caps: CapToolCall}
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "no", Source: "probe", Method: "chat-stream-tools",
		Identity: "content-digest", CheckedAt: logT0,
	})
}

func TestExplainEvidence_TypeYesVsCheckedNo(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat, eeProbe("chat", fingerprint.CapProbeNo, logT0))
	in.TypeCaps, in.TypeNames = CapChat, []string{"dense"}
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "chat", State: "no", Source: "probe", Method: "chat-stream-tools",
		Identity: "content-digest", CheckedAt: logT0,
	})
}

func TestExplainEvidence_InconclusiveBesideTypeYes(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat, eeAttempt("chat", logT0))
	in.TypeCaps, in.TypeNames = CapChat, []string{"dense"}
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "chat", State: "yes", Source: "type", Method: "type:dense",
		LastAttempt: &CheckedAnswer{State: "inconclusive", Method: "chat-stream-tools", At: logT0},
	})
}

// TestExplainEvidence_HistoryOtherCapabilityIgnored: a legacy row filed for
// another capability is not shown as tool_call's history.
func TestExplainEvidence_HistoryOtherCapabilityIgnored(t *testing.T) {
	for _, capability := range []string{"chat", ""} {
		in := eeIn(fingerprint.AdapterOpenAICompat)
		in.Legacy = &fingerprint.CapProbe{BackendID: "local", Capability: capability, State: fingerprint.CapProbeYes, TestedAt: logT0}
		eeWant(t, ExplainEvidence(in))
	}
}

func TestExplainEvidence_HistoryNeverCounted(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat)
	in.Legacy = &fingerprint.CapProbe{BackendID: "local", Capability: "tool_call", State: fingerprint.CapProbeYes, TestedAt: logT0}
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "unknown", Source: "history",
		History: &HistoryAnswer{Provider: "local", State: "yes", At: logT0},
	})
}

func TestExplainEvidence_HistoryBesideCatalog(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat)
	in.Catalog = &ModelProfile{Family: "fam", Caps: CapToolCall}
	in.Legacy = &fingerprint.CapProbe{BackendID: "local", Capability: "tool_call", State: fingerprint.CapProbeNo, TestedAt: logT0}
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "yes", Source: "catalog", Method: "catalog:fam",
		History: &HistoryAnswer{Provider: "local", State: "no", At: logT0},
	})
}

func TestExplainEvidence_ExplicitWithDifferingLastCheck(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat, eeProbe("tool_call", fingerprint.CapProbeNo, logT0))
	in.Explicit = []string{"tool_call"}
	got := eeFact(t, ExplainEvidence(in), "tool_call")
	want := CapabilityFact{
		Capability: "tool_call", State: "yes", Source: "explicit",
		LastCheck: &CheckedAnswer{State: "no", Method: "chat-stream-tools", At: logT0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tool_call:\n got %s\nwant %s", eeJSON(got), eeJSON(want))
	}
}

func TestExplainEvidence_OllamaShowBesideOpenAICompat(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat, eeShow("tool_call", fingerprint.CapProbeYes, logT0))
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "unknown", Source: "none",
		OtherAdapter: &OtherAdapterAnswer{Adapter: "ollama", State: "yes", Method: "ollama-show", At: logT0},
	})
}

func TestExplainEvidence_StoredProviderBeatsLive(t *testing.T) {
	in := eeIn(fingerprint.AdapterOllama, eeShow("thinking", fingerprint.CapProbeYes, logT0))
	in.Runtime = map[string]fingerprint.CapProbeState{"thinking": fingerprint.CapProbeNo}
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "thinking", State: "yes", Source: "provider", Method: "ollama-show",
		Identity: "content-digest", CheckedAt: logT0,
	})
}

func TestExplainEvidence_LiveAnswersWhenNoStoredRow(t *testing.T) {
	in := eeIn(fingerprint.AdapterOllama)
	in.Runtime = map[string]fingerprint.CapProbeState{"thinking": fingerprint.CapProbeNo}
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "thinking", State: "no", Source: "provider", Method: "ollama-show", Identity: "content-digest",
	})
}

// TestExplainEvidence_NameOnlyProviderRowAcrossRepull documents a limit of
// name-only reads: an old name-only stored yes outlives a live no and a newer
// digest-keyed no, because a stored provider row outranks a live fact and a
// name-only read does not count digest-keyed rows.
func TestExplainEvidence_NameOnlyProviderRowAcrossRepull(t *testing.T) {
	newer := eeShow("thinking", fingerprint.CapProbeNo, logT0.Add(time.Hour))
	newer.Digest = "sha256:repulled"
	in := eeIn(fingerprint.AdapterOllama, eeNameOnly(eeShow("thinking", fingerprint.CapProbeYes, logT0)), newer)
	in.Digest = ""
	in.Runtime = map[string]fingerprint.CapProbeState{"thinking": fingerprint.CapProbeNo}
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "thinking", State: "yes", Source: "provider", Method: "ollama-show",
		Identity: "name-only", CheckedAt: logT0, FreshUntil: logT0.Add(30 * 24 * time.Hour),
	})
}

func TestExplainEvidence_ProviderNoBeatsCatalogYes(t *testing.T) {
	tests := []struct {
		name    string
		rows    []fingerprint.EvidenceRow
		runtime map[string]fingerprint.CapProbeState
		want    CapabilityFact
	}{
		{"stored provider row", []fingerprint.EvidenceRow{eeShow("tool_call", fingerprint.CapProbeNo, logT0)}, nil,
			CapabilityFact{Capability: "tool_call", State: "no", Source: "provider", Method: "ollama-show", Identity: "content-digest", CheckedAt: logT0}},
		{"live provider fact", nil, map[string]fingerprint.CapProbeState{"tool_call": fingerprint.CapProbeNo},
			CapabilityFact{Capability: "tool_call", State: "no", Source: "provider", Method: "ollama-show", Identity: "content-digest"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := eeIn(fingerprint.AdapterOllama, tt.rows...)
			in.Runtime = tt.runtime
			in.Catalog = &ModelProfile{Family: "fam", Caps: CapToolCall}
			eeWant(t, ExplainEvidence(in), tt.want)
		})
	}
}

func TestExplainEvidence_ObservedNewerThanProbeWins(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat,
		eeProbe("tool_call", fingerprint.CapProbeYes, logT0),
		eeObserved("tool_call", fingerprint.CapProbeNo, logT0.Add(time.Minute)))
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "no", Source: "observed", Method: "chat-stream-tools",
		Identity: "content-digest", CheckedAt: logT0.Add(time.Minute),
		LastCheck: &CheckedAnswer{State: "yes", Method: "chat-stream-tools", At: logT0},
	})
}

func TestExplainEvidence_EqualTimeProbeWins(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat,
		eeObserved("tool_call", fingerprint.CapProbeNo, logT0),
		eeProbe("tool_call", fingerprint.CapProbeYes, logT0))
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "yes", Source: "probe", Method: "chat-stream-tools",
		Identity: "content-digest", CheckedAt: logT0,
	})
	// The resolver reads the same rows the same way.
	reading := fingerprint.ReadCapabilities(in.Rows, in.Adapter, in.Digest, in.Now)["tool_call"]
	if a := readingAnswer(reading); a == nil || a.State != fingerprint.CapProbeYes || a.Source != fingerprint.EvidenceSourceProbe {
		t.Errorf("readingAnswer = %+v; want the probe yes", a)
	}
}

func TestExplainEvidence_InconclusiveNeverReplacesDefinitive(t *testing.T) {
	later := logT0.Add(time.Hour)
	showAttempt := eeRow(fingerprint.MethodOllamaShow, fingerprint.EvidenceSourceProvider, fingerprint.EvidenceKindAttempt, "thinking", fingerprint.CapProbeInconclusive, later)
	tests := []struct {
		name    string
		adapter string
		rows    []fingerprint.EvidenceRow
		want    CapabilityFact
	}{
		{"probe", fingerprint.AdapterOpenAICompat,
			[]fingerprint.EvidenceRow{eeProbe("tool_call", fingerprint.CapProbeNo, logT0), eeAttempt("tool_call", later)},
			CapabilityFact{Capability: "tool_call", State: "no", Source: "probe", Method: "chat-stream-tools", Identity: "content-digest", CheckedAt: logT0,
				LastAttempt: &CheckedAnswer{State: "inconclusive", Method: "chat-stream-tools", At: later}}},
		{"observed", fingerprint.AdapterOpenAICompat,
			[]fingerprint.EvidenceRow{eeObserved("tool_call", fingerprint.CapProbeNo, logT0), eeAttempt("tool_call", later)},
			CapabilityFact{Capability: "tool_call", State: "no", Source: "observed", Method: "chat-stream-tools", Identity: "content-digest", CheckedAt: logT0,
				LastAttempt: &CheckedAnswer{State: "inconclusive", Method: "chat-stream-tools", At: later}}},
		{"provider", fingerprint.AdapterOllama,
			[]fingerprint.EvidenceRow{eeShow("thinking", fingerprint.CapProbeNo, logT0), showAttempt},
			CapabilityFact{Capability: "thinking", State: "no", Source: "provider", Method: "ollama-show", Identity: "content-digest", CheckedAt: logT0,
				LastAttempt: &CheckedAnswer{State: "inconclusive", Method: "ollama-show", At: later}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eeWant(t, ExplainEvidence(eeIn(tt.adapter, tt.rows...)), tt.want)
		})
	}
}

func TestExplainEvidence_ExpiredAttemptIsUnknown(t *testing.T) {
	at := eeNow.Add(-48 * time.Hour)
	in := eeIn(fingerprint.AdapterOpenAICompat, eeAttempt("tool_call", at))
	eeWant(t, ExplainEvidence(in), CapabilityFact{
		Capability: "tool_call", State: "unknown", Source: "none",
		LastAttempt: &CheckedAnswer{State: "inconclusive", Method: "chat-stream-tools", At: at},
	})
}

func TestExplainEvidence_LastAttemptAlwaysShown(t *testing.T) {
	tests := []struct {
		name string
		at   time.Time
	}{
		{"older than the winner", logT0.Add(-time.Hour)},
		{"equal time", logT0},
		{"newer than the winner", logT0.Add(time.Hour)},
		{"expired", eeNow.Add(-48 * time.Hour)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := eeIn(fingerprint.AdapterOpenAICompat, eeProbe("tool_call", fingerprint.CapProbeNo, logT0), eeAttempt("tool_call", tt.at))
			eeWant(t, ExplainEvidence(in), CapabilityFact{
				Capability: "tool_call", State: "no", Source: "probe", Method: "chat-stream-tools",
				Identity: "content-digest", CheckedAt: logT0,
				LastAttempt: &CheckedAnswer{State: "inconclusive", Method: "chat-stream-tools", At: tt.at},
			})
		})
	}
}

func TestExplainEvidence_FlagBesideExplicitList(t *testing.T) {
	flag := eeFlag("tool_call", logT0)
	flag.Detail, flag.HTTPStatus = "run_refused", 422
	in := eeIn(fingerprint.AdapterOpenAICompat, flag)
	in.Explicit = []string{"tool_call"}
	got := eeFact(t, ExplainEvidence(in), "tool_call")
	want := CapabilityFact{
		Capability: "tool_call", State: "yes", Source: "explicit",
		Flag: &CapabilityFlag{Cause: "run_refused", At: logT0, Status: 422},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tool_call:\n got %s\nwant %s", eeJSON(got), eeJSON(want))
	}
}

func TestExplainEvidence_StaleYesCountsAndIsStale(t *testing.T) {
	freshUntil := logT0.Add(30 * 24 * time.Hour)
	tests := []struct {
		name      string
		now       time.Time
		wantStale bool
	}{
		{"past its freshness", freshUntil.Add(time.Nanosecond), true},
		{"at its freshness end", freshUntil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := eeIn(fingerprint.AdapterOpenAICompat, eeNameOnly(eeProbe("tool_call", fingerprint.CapProbeYes, logT0)))
			in.Digest, in.Now = "", tt.now
			eeWant(t, ExplainEvidence(in), CapabilityFact{
				Capability: "tool_call", State: "yes", Source: "probe", Method: "chat-stream-tools",
				Identity: "name-only", CheckedAt: logT0, FreshUntil: freshUntil, Stale: tt.wantStale,
			})
		})
	}
}

// TestExplainEvidence_SkipsNonCanonicalName: a name in CanonicalCapabilityNames
// with no canonical bit gets no fact, even beside an explicit list (whose Has(0)
// would say yes). Not parallel: it swaps the package-level slice for a new one,
// never writing the shared backing array, and restores it. Parallel tests in
// this package resume only after every sequential test has returned, so none
// can read the swapped slice.
func TestExplainEvidence_SkipsNonCanonicalName(t *testing.T) {
	orig := CanonicalCapabilityNames
	t.Cleanup(func() { CanonicalCapabilityNames = orig })
	CanonicalCapabilityNames = append(slices.Clone(orig), "bogus")

	in := eeIn(fingerprint.AdapterOpenAICompat)
	in.Explicit = []string{"chat"}
	facts := ExplainEvidence(in)
	if len(facts) != len(orig) {
		t.Fatalf("%d facts %s; want %d, the unknown name skipped", len(facts), eeJSON(facts), len(orig))
	}
	for _, f := range facts {
		if f.Capability == "bogus" {
			t.Errorf("fact for a non-canonical name: %s", eeJSON(f))
		}
	}
}

func TestExplainEvidence_RowOrderIrrelevant(t *testing.T) {
	probeYes := eeProbe("tool_call", fingerprint.CapProbeYes, logT0)
	probeYes.ID = 1
	observed := eeObserved("tool_call", fingerprint.CapProbeYes, logT0)
	observed.ID = 2
	probeNo := eeProbe("tool_call", fingerprint.CapProbeNo, logT0) // wins the exact tie by ID
	probeNo.ID = 3
	attempt := eeAttempt("tool_call", logT0.Add(time.Minute))
	attempt.ID = 4
	flag := eeFlag("tool_call", logT0.Add(2*time.Minute))
	flag.ID = 5
	rows := []fingerprint.EvidenceRow{probeYes, observed, probeNo, attempt, flag}

	first := ExplainEvidence(eeIn(fingerprint.AdapterOpenAICompat, rows...))
	if f := eeFact(t, first, "tool_call"); f.State != "no" || f.Source != "probe" || f.LastAttempt == nil || f.Flag == nil {
		t.Fatalf("fixture's tool_call = %s; want the ID 3 probe no with an attempt and a flag beside it", eeJSON(f))
	}
	n := 0
	eePermute(rows, 0, func(perm []fingerprint.EvidenceRow) {
		n++
		if got := ExplainEvidence(eeIn(fingerprint.AdapterOpenAICompat, perm...)); !reflect.DeepEqual(got, first) {
			t.Errorf("permutation %d:\n got %s\nwant %s", n, eeJSON(got), eeJSON(first))
		}
	})
	if n != 120 {
		t.Fatalf("checked %d permutations; want 120", n)
	}
}

// eePermute calls visit with every ordering of rows[k:] after rows[:k].
func eePermute(rows []fingerprint.EvidenceRow, k int, visit func([]fingerprint.EvidenceRow)) {
	if k == len(rows) {
		visit(append([]fingerprint.EvidenceRow(nil), rows...))
		return
	}
	for i := k; i < len(rows); i++ {
		rows[k], rows[i] = rows[i], rows[k]
		eePermute(rows, k+1, visit)
		rows[k], rows[i] = rows[i], rows[k]
	}
}

func TestExplainEvidence_UnparseableExplicitIgnored(t *testing.T) {
	tests := []struct {
		name     string
		explicit []string
	}{
		{"alias", []string{"tools"}},
		{"one bad token", []string{"chat", "bogus"}},
		{"empty list", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := eeIn(fingerprint.AdapterOpenAICompat)
			in.Explicit = tt.explicit
			in.Catalog = &ModelProfile{Family: "fam", Caps: CapToolCall}
			eeWant(t, ExplainEvidence(in),
				CapabilityFact{Capability: "tool_call", State: "yes", Source: "catalog", Method: "catalog:fam"})
		})
	}
}

func TestExplainEvidence_MultipleTypesKeepEveryContributedCapability(t *testing.T) {
	in := eeIn(fingerprint.AdapterOpenAICompat)
	in.TypeCaps = CapChat | CapGenerate | CapStream | CapEmbed
	in.TypeNames = []string{"dense", "embedding"}
	var want []CapabilityFact
	for _, name := range []string{"chat", "generate", "stream", "embed"} {
		want = append(want, CapabilityFact{Capability: name, State: "yes", Source: "type", Method: "type:dense+embedding"})
	}
	eeWant(t, ExplainEvidence(in), want...)
}

// --- savedness: every kept row is marked unsaved ---

func TestExplainEvidence_UnsavedOnEveryRecord(t *testing.T) {
	unsaved := func(r fingerprint.EvidenceRow, id int64) fingerprint.EvidenceRow { r.ID = id; return r }
	t.Run("observed winner with every record beside it", func(t *testing.T) {
		in := eeIn(fingerprint.AdapterOpenAICompat,
			unsaved(eeProbe("tool_call", fingerprint.CapProbeYes, logT0), -10),
			unsaved(eeObserved("tool_call", fingerprint.CapProbeNo, logT0.Add(time.Minute)), -9),
			unsaved(eeFlag("tool_call", logT0.Add(2*time.Minute)), -8),
			unsaved(eeAttempt("tool_call", logT0.Add(3*time.Minute)), -7),
			unsaved(eeShow("tool_call", fingerprint.CapProbeYes, logT0), -6))
		f := eeFact(t, ExplainEvidence(in), "tool_call")
		if f.Source != "observed" || f.LastCheck == nil || f.LastAttempt == nil || f.Flag == nil || f.OtherAdapter == nil {
			t.Fatalf("tool_call = %s; want an observed winner with every record beside it", eeJSON(f))
		}
		if !f.Unsaved || !f.LastCheck.Unsaved || !f.LastAttempt.Unsaved || !f.Flag.Unsaved || !f.OtherAdapter.Unsaved {
			t.Errorf("tool_call = %s; want Unsaved on the winner and every record", eeJSON(f))
		}
	})
	t.Run("attempt winner", func(t *testing.T) {
		f := eeFact(t, ExplainEvidence(eeIn(fingerprint.AdapterOpenAICompat, unsaved(eeAttempt("tool_call", logT0), -1))), "tool_call")
		if f.State != "inconclusive" || !f.Unsaved {
			t.Errorf("tool_call = %s; want an unsaved inconclusive", eeJSON(f))
		}
	})
	t.Run("provider winner", func(t *testing.T) {
		f := eeFact(t, ExplainEvidence(eeIn(fingerprint.AdapterOllama, unsaved(eeShow("thinking", fingerprint.CapProbeYes, logT0), -1))), "thinking")
		if f.Source != "provider" || !f.Unsaved {
			t.Errorf("thinking = %s; want an unsaved provider answer", eeJSON(f))
		}
	})
}

func TestExplainEvidence_SavedWinnerAboveUnsavedCheck(t *testing.T) {
	probe := eeProbe("tool_call", fingerprint.CapProbeNo, logT0)
	probe.ID = -1
	observed := eeObserved("tool_call", fingerprint.CapProbeYes, logT0.Add(time.Minute))
	observed.ID = 5
	eeWant(t, ExplainEvidence(eeIn(fingerprint.AdapterOpenAICompat, probe, observed)), CapabilityFact{
		Capability: "tool_call", State: "yes", Source: "observed", Method: "chat-stream-tools",
		Identity: "content-digest", CheckedAt: logT0.Add(time.Minute),
		LastCheck: &CheckedAnswer{State: "no", Method: "chat-stream-tools", At: logT0, Unsaved: true},
	})
}

// --- projection: details and flag statuses map onto bounded codes ---

func TestExplainEvidence_DetailProjected(t *testing.T) {
	tests := []struct{ detail, want string }{
		{"", ""},
		{fingerprint.DetailToolCalled, "tool_called"},
		{fingerprint.DetailToolIgnored, "tool_ignored"},
		{fingerprint.DetailToolNotCalled, "tool_not_called"},
		{fingerprint.DetailRequestRejected, "request_rejected"},
		{fingerprint.DetailCutOff, "cut_off"},
		{fingerprint.DetailToolCallInvalid, "tool_call_invalid"},
		{"run_refused", "run_refused"},
		{"HTTP 500 /v1/x", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.detail, func(t *testing.T) {
			with := func(r fingerprint.EvidenceRow) fingerprint.EvidenceRow { r.Detail = tt.detail; return r }
			in := eeIn(fingerprint.AdapterOpenAICompat,
				with(eeProbe("tool_call", fingerprint.CapProbeYes, logT0)),
				with(eeObserved("tool_call", fingerprint.CapProbeNo, logT0.Add(time.Minute))),
				with(eeFlag("tool_call", logT0.Add(2*time.Minute))),
				with(eeAttempt("tool_call", logT0.Add(3*time.Minute))))
			f := eeFact(t, ExplainEvidence(in), "tool_call")
			if f.LastCheck == nil || f.LastAttempt == nil || f.Flag == nil {
				t.Fatalf("tool_call = %s; want LastCheck, LastAttempt and Flag", eeJSON(f))
			}
			for field, got := range map[string]string{
				"Detail": f.Detail, "LastCheck.Detail": f.LastCheck.Detail,
				"LastAttempt.Detail": f.LastAttempt.Detail, "Flag.Cause": f.Flag.Cause,
			} {
				if got != tt.want {
					t.Errorf("%s = %q; want %q", field, got, tt.want)
				}
			}
		})
	}
}

func TestExplainEvidence_FlagStatusBounded(t *testing.T) {
	for _, tt := range []struct{ status, want int }{{400, 400}, {422, 422}, {500, 0}, {401, 0}, {0, 0}} {
		flag := eeFlag("tool_call", logT0)
		flag.Detail, flag.HTTPStatus = "run_refused", tt.status
		eeWant(t, ExplainEvidence(eeIn(fingerprint.AdapterOpenAICompat, flag)), CapabilityFact{
			Capability: "tool_call", State: "unknown", Source: "none",
			Flag: &CapabilityFlag{Cause: "run_refused", At: logT0, Status: tt.want},
		})
	}
}

func TestExplainEvidence_HistoryStateBounded(t *testing.T) {
	for _, state := range []fingerprint.CapProbeState{"<script>", "", "flagged", "yes", "no", "inconclusive"} {
		t.Run(string(state), func(t *testing.T) {
			in := eeIn(fingerprint.AdapterOpenAICompat)
			in.Legacy = &fingerprint.CapProbe{BackendID: "local", Capability: "tool_call", State: state, TestedAt: logT0}
			want := eeNone("tool_call")
			if state == "yes" || state == "no" || state == "inconclusive" {
				want.Source = "history"
				want.History = &HistoryAnswer{Provider: "local", State: string(state), At: logT0}
			}
			eeWant(t, ExplainEvidence(in), want)
		})
	}
}

func TestCapabilityMasks(t *testing.T) {
	all := CapChat | CapGenerate | CapInsert | CapEmbed | CapStream | CapToolCall | CapThinking
	explicit := eeIn(fingerprint.AdapterOpenAICompat)
	explicit.Explicit = []string{"chat", "embed"}
	tests := []struct {
		name                 string
		in                   EvidenceInputs
		yes, no, wantUnknown Capability
	}{
		{"mixed evidence; inconclusive is unknown", eeIn(fingerprint.AdapterOpenAICompat,
			eeProbe("chat", fingerprint.CapProbeYes, logT0),
			eeProbe("stream", fingerprint.CapProbeNo, logT0),
			eeAttempt("tool_call", logT0)),
			CapChat, CapStream, CapGenerate | CapInsert | CapEmbed | CapToolCall | CapThinking},
		{"explicit list answers everything", explicit, CapChat | CapEmbed, all &^ (CapChat | CapEmbed), 0},
		{"nothing", eeIn(fingerprint.AdapterOpenAICompat), 0, 0, all},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yes, no, unknown := CapabilityMasks(ExplainEvidence(tt.in))
			if yes != tt.yes || no != tt.no || unknown != tt.wantUnknown {
				t.Errorf("CapabilityMasks = yes %v no %v unknown %v; want %v %v %v", yes, no, unknown, tt.yes, tt.no, tt.wantUnknown)
			}
			if yes|no|unknown != all || yes&no != 0 || yes&unknown != 0 || no&unknown != 0 {
				t.Errorf("yes %v no %v unknown %v do not partition the canonical set", yes, no, unknown)
			}
		})
	}
	t.Run("non-canonical names skipped, missing capabilities in no mask", func(t *testing.T) {
		yes, no, unknown := CapabilityMasks([]CapabilityFact{
			{Capability: "bogus", State: "yes"},
			{Capability: "tools", State: "no"},
			{Capability: "chat", State: "yes"},
			{Capability: "stream", State: "inconclusive"},
		})
		if yes != CapChat || no != 0 || unknown != CapStream {
			t.Errorf("CapabilityMasks = yes %v no %v unknown %v; want chat, none, stream", yes, no, unknown)
		}
	})
	t.Run("a canonical capability absent from the facts is in no mask", func(t *testing.T) {
		yes, no, unknown := CapabilityMasks([]CapabilityFact{{Capability: "chat", State: "yes"}})
		if yes != CapChat || no != 0 || unknown != 0 {
			t.Errorf("CapabilityMasks = yes %v no %v unknown %v; want chat, none, none", yes, no, unknown)
		}
		for _, absent := range []Capability{CapGenerate, CapInsert, CapEmbed, CapStream, CapToolCall, CapThinking} {
			if (yes|no|unknown)&absent != 0 {
				t.Errorf("absent %v landed in a mask: yes %v no %v unknown %v", absent, yes, no, unknown)
			}
		}
	})
}

// TestReadingAnswer_Table pins readingAnswer, the stored log tiers the resolver
// and the core share: the newer direct answer, an equal time to the probe,
// the provider row last, nil when the reading holds no answer.
func TestReadingAnswer_Table(t *testing.T) {
	t0 := time.UnixMilli(1_800_000_000_000)
	probe := &fingerprint.EvidenceRow{ID: 1, Source: fingerprint.EvidenceSourceProbe, State: fingerprint.CapProbeYes, ObservedAt: t0}
	observedNewer := &fingerprint.EvidenceRow{ID: 2, Source: fingerprint.EvidenceSourceObserved, State: fingerprint.CapProbeNo, ObservedAt: t0.Add(time.Minute)}
	observedOlder := &fingerprint.EvidenceRow{ID: 3, Source: fingerprint.EvidenceSourceObserved, State: fingerprint.CapProbeNo, ObservedAt: t0.Add(-time.Minute)}
	observedEqual := &fingerprint.EvidenceRow{ID: 4, Source: fingerprint.EvidenceSourceObserved, State: fingerprint.CapProbeNo, ObservedAt: t0}
	providerNewest := &fingerprint.EvidenceRow{ID: 5, Source: fingerprint.EvidenceSourceProvider, State: fingerprint.CapProbeNo, ObservedAt: t0.Add(time.Hour)}
	attempt := &fingerprint.EvidenceRow{ID: 6, Source: fingerprint.EvidenceSourceProbe, Kind: fingerprint.EvidenceKindAttempt, State: fingerprint.CapProbeInconclusive, ObservedAt: t0.Add(time.Hour)}
	flag := &fingerprint.EvidenceRow{ID: 7, Source: fingerprint.EvidenceSourceObserved, Kind: fingerprint.EvidenceKindFlag, State: fingerprint.CapProbeFlagged, ObservedAt: t0.Add(time.Hour)}
	other := &fingerprint.EvidenceRow{ID: 8, Adapter: fingerprint.AdapterOllama, Source: fingerprint.EvidenceSourceProvider, State: fingerprint.CapProbeYes, ObservedAt: t0.Add(time.Hour)}

	tests := []struct {
		name    string
		reading fingerprint.CapabilityReading
		want    *fingerprint.EvidenceRow
	}{
		{"empty reading", fingerprint.CapabilityReading{}, nil},
		{"probe alone", fingerprint.CapabilityReading{Probe: probe}, probe},
		{"observed alone", fingerprint.CapabilityReading{Observed: observedOlder}, observedOlder},
		{"newer observed beats probe", fingerprint.CapabilityReading{Probe: probe, Observed: observedNewer}, observedNewer},
		{"newer probe beats observed", fingerprint.CapabilityReading{Probe: probe, Observed: observedOlder}, probe},
		{"equal time goes to the probe", fingerprint.CapabilityReading{Probe: probe, Observed: observedEqual}, probe},
		{"provider alone", fingerprint.CapabilityReading{Provider: providerNewest}, providerNewest},
		{"newer provider loses to a probe", fingerprint.CapabilityReading{Probe: probe, Provider: providerNewest}, probe},
		{"newer provider loses to an observed", fingerprint.CapabilityReading{Observed: observedOlder, Provider: providerNewest}, observedOlder},
		{"attempt, flag and other adapter answer nothing", fingerprint.CapabilityReading{Attempt: attempt, Flag: flag, OtherAdapter: other}, nil},
		{"attempt beside a provider row", fingerprint.CapabilityReading{Provider: providerNewest, Attempt: attempt}, providerNewest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := readingAnswer(tt.reading); got != tt.want {
				t.Errorf("readingAnswer = %+v; want %+v", got, tt.want)
			}
		})
	}
}
