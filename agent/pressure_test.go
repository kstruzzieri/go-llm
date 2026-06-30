package agent

import (
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestPressureThresholdsClassify(t *testing.T) {
	def := PressureThresholds{}.normalize() // Watch .60 Warn .75 Critical .90
	cases := []struct {
		name      string
		used      float64
		exhausted bool
		evicted   bool
		wantLevel PressureLevel
		wantMit   PressureMitigation
	}{
		{"ok", 0.10, false, false, LevelOK, MitigationNone},
		{"watch", 0.65, false, false, LevelWatch, MitigationNone},
		{"warn", 0.80, false, false, LevelWarn, MitigationWarn},
		{"critical", 0.95, false, false, LevelCritical, MitigationWarn},
		{"evict_wins_over_warn", 0.80, false, true, LevelWarn, MitigationEvict},
		{"exhausted_overrides_all", 0.50, true, true, LevelCritical, MitigationHalt},
		{"watch_with_evict", 0.65, false, true, LevelWatch, MitigationEvict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotLevel, gotMit := def.Classify(c.used, c.exhausted, c.evicted)
			if gotLevel != c.wantLevel || gotMit != c.wantMit {
				t.Fatalf("Classify(%.2f,%v,%v)=%v/%v want %v/%v",
					c.used, c.exhausted, c.evicted, gotLevel, gotMit, c.wantLevel, c.wantMit)
			}
		})
	}
}

func TestPressureThresholdsNormalize(t *testing.T) {
	def := PressureThresholds{Watch: 0.60, Warn: 0.75, Critical: 0.90}
	if got := (PressureThresholds{}).normalize(); got != def {
		t.Fatalf("zero => %+v, want %+v", got, def)
	}
	if got := (PressureThresholds{Warn: 0.80}).normalize(); got != (PressureThresholds{Watch: 0.60, Warn: 0.80, Critical: 0.90}) {
		t.Fatalf("partial fill => %+v", got)
	}
	if got := (PressureThresholds{Warn: 0.99}).normalize(); got != def {
		t.Fatalf("non-monotonic => %+v, want default", got)
	}
	if got := (PressureThresholds{Watch: -0.1, Warn: 0.5, Critical: 0.6}).normalize(); got != def {
		t.Fatalf("out-of-range => %+v, want default", got)
	}
}

func TestPressureThresholdsForWarn(t *testing.T) {
	// mid-range warn keeps the default watch/critical bands.
	if got := PressureThresholdsForWarn(0.80); got != (PressureThresholds{Watch: 0.60, Warn: 0.80, Critical: 0.90}) {
		t.Fatalf("warn 0.80 => %+v", got)
	}
	// high warn widens critical so normalize keeps the warn instead of discarding it.
	if got := PressureThresholdsForWarn(0.95); got != (PressureThresholds{Watch: 0.60, Warn: 0.95, Critical: 0.95}) {
		t.Fatalf("warn 0.95 => %+v", got)
	}
	// low warn lowers watch so normalize keeps the warn.
	if got := PressureThresholdsForWarn(0.50); got != (PressureThresholds{Watch: 0.50, Warn: 0.50, Critical: 0.90}) {
		t.Fatalf("warn 0.50 => %+v", got)
	}
}

func TestDominantCause(t *testing.T) {
	m := ContextManager{Estimate: runeEstimator}
	mkPinned := func(role, content string) Message {
		return Message{ChatMessage: provider.ChatMessage{Role: role, Content: content}, Segment: Pinned}
	}
	mkElastic := func(role, content string) Message {
		return Message{ChatMessage: provider.ChatMessage{Role: role, Content: content}, Segment: Elastic}
	}
	mkRetrieval := func(content string) Message {
		return Message{
			ChatMessage: provider.ChatMessage{Role: "tool", Content: content},
			Segment:     Elastic,
			Attrib:      &RetrievalAttribution{Sources: []RetrievedSource{{StableKey: "k"}}},
		}
	}
	cases := []struct {
		name             string
		st               State
		toolSchemaTokens int
		want             PressureCause
	}{
		{"empty", State{}, 0, CauseUnknown},
		{"tool_schema_dominates", State{Messages: []Message{mkElastic("user", "hi")}}, 1000, CauseToolSchema},
		{"pinned_dominates", State{System: "", Messages: []Message{mkPinned("system", "PPPPPPPPPP")}}, 1, CausePinned},
		{"history_dominates", State{Messages: []Message{mkElastic("assistant", "HHHHHHHHHH")}}, 1, CauseHistory},
		{"tool_output_dominates", State{Messages: []Message{mkElastic("tool", "TTTTTTTTTT")}}, 1, CauseToolOutput},
		{"retrieval_beats_tool_output", State{Messages: []Message{mkRetrieval("RRRRRRRRRR")}}, 1, CauseRetrieval},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := m.dominantCause(c.st, c.toolSchemaTokens); got != c.want {
				t.Fatalf("dominantCause = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPressureLabelStrings(t *testing.T) {
	if LevelOK.String() != "ok" || LevelWatch.String() != "watch" ||
		LevelWarn.String() != "warn" || LevelCritical.String() != "critical" {
		t.Fatal("level labels wrong")
	}
	if MitigationNone.String() != "none" || MitigationWarn.String() != "warn" ||
		MitigationEvict.String() != "evict" || MitigationHalt.String() != "halt" {
		t.Fatal("mitigation labels wrong")
	}
	if CauseUnknown.String() != "unknown" || CausePinned.String() != "pinned" ||
		CauseToolSchema.String() != "tool_schema" || CauseHistory.String() != "history" ||
		CauseToolOutput.String() != "tool_output" || CauseRetrieval.String() != "retrieval" {
		t.Fatal("cause labels wrong")
	}
}
