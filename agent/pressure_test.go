package agent

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

func TestAssemblePressureBuckets(t *testing.T) {
	t.Parallel()
	attributedPinned := pinned("tool", "PP")
	attributedPinned.Attrib = &RetrievalAttribution{}
	retrieval := elastic("tool", "RRRRRRR")
	retrieval.Attrib = &RetrievalAttribution{}
	all := State{System: "SYS", Messages: []Message{attributedPinned, elastic("assistant", "HHHH"), elastic("tool", "TTTTTT"), retrieval}}
	for _, tc := range []struct {
		name           string
		state          State
		schema, budget int
		compactor      Compactor
		want           Pressure
		exhausted      bool
	}{
		{name: "all buckets and precedence", state: all, schema: 3, budget: 100, want: Pressure{UsedPct: .25, InputTokens: 25, InputBudget: 100, Cause: CauseRetrieval, Buckets: PressureBuckets{Available: true, Pinned: 5, ToolSchema: 3, History: 4, ToolOutput: 6, Retrieval: 7}}},
		{name: "available zero", budget: 100, want: Pressure{InputBudget: 100, Buckets: PressureBuckets{Available: true}}},
		{name: "evicted history", state: State{System: "s", Messages: []Message{pinned("user", "g"), elastic("assistant", "oldold"), elastic("user", "new")}}, budget: 5, want: Pressure{UsedPct: 1, Evicted: 1, Compactions: 1, InputTokens: 5, InputBudget: 5, Level: LevelCritical, Cause: CauseHistory, Mitigation: MitigationEvict, Buckets: PressureBuckets{Available: true, Pinned: 2, History: 3}}},
		{name: "final exhaustion", state: all, schema: 3, budget: 20, compactor: passthroughCompactor{}, exhausted: true, want: Pressure{UsedPct: 1.25, InputTokens: 25, InputBudget: 20, Level: LevelCritical, Cause: CauseRetrieval, Mitigation: MitigationHalt, Buckets: PressureBuckets{Available: true, Pinned: 5, ToolSchema: 3, History: 4, ToolOutput: 6, Retrieval: 7}}},
		{name: "early pinned overflow", state: all, schema: 3, budget: 7, exhausted: true, want: Pressure{UsedPct: float64(8) / 7, InputTokens: 8, InputBudget: 7, Level: LevelCritical, Cause: CausePinned, Mitigation: MitigationHalt}},
		{name: "custom compactor exhaustion", state: all, schema: 3, budget: 20, compactor: pressureExhaustingCompactor{}, exhausted: true, want: Pressure{UsedPct: .6, InputTokens: 12, InputBudget: 20, Level: LevelCritical, Cause: CauseRetrieval, Mitigation: MitigationHalt}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := ContextManager{Estimate: runeEstimator, Compactor: tc.compactor}
			_, got, err := m.Assemble(context.Background(), tc.state, tc.schema, TokenBudget{Input: tc.budget})
			if errors.Is(err, ErrContextExhausted) != tc.exhausted || (err != nil && !tc.exhausted) {
				t.Fatalf("Assemble(%s) error = %v, want exhaustion %v", tc.name, err, tc.exhausted)
			}
			if got != tc.want {
				t.Errorf("Assemble(%s) pressure = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

type pressureExhaustingCompactor struct{}

func (pressureExhaustingCompactor) Compact(_ context.Context, st State, _ TokenBudget) (State, CompactionReport, error) {
	return st, CompactionReport{TokensAfter: 9}, ErrContextExhausted
}

func TestPressureBucketsTieOrderAndSaturation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		buckets PressureBuckets
		want    PressureCause
	}{
		{"zero", PressureBuckets{Available: true}, CauseUnknown},
		{"pinned", PressureBuckets{Pinned: 8, ToolSchema: 8, History: 8, ToolOutput: 8, Retrieval: 8}, CausePinned},
		{"schema", PressureBuckets{ToolSchema: 8, History: 8, ToolOutput: 8, Retrieval: 8}, CauseToolSchema},
		{"history", PressureBuckets{History: 8, ToolOutput: 8, Retrieval: 8}, CauseHistory},
		{"tool output", PressureBuckets{ToolOutput: 8, Retrieval: 8}, CauseToolOutput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.buckets.dominantCause(); got != tc.want {
				t.Errorf("dominantCause(%+v) = %v, want %v", tc.buckets, got, tc.want)
			}
		})
	}
	m := ContextManager{Estimate: func(s string) int {
		if s != "" {
			return math.MaxInt - 1
		}
		return 0
	}}
	got := m.pressureBuckets(State{System: "s", Messages: []Message{pinned("user", "p"), elastic("user", "a"), elastic("user", "b")}}, 3)
	want := PressureBuckets{Available: true, Pinned: math.MaxInt, ToolSchema: 3, History: math.MaxInt}
	if got != want {
		t.Errorf("pressureBuckets(overflow) = %+v, want %+v", got, want)
	}
}

func TestAssembleMixedPressureBuckets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		mixed, stripAttrib bool
		want               PressureBuckets
		tokens             int
		cause              PressureCause
	}{
		{"mixed projection", true, false, PressureBuckets{Available: true, Pinned: 7, ToolSchema: 5, History: 28, Retrieval: 31}, 71, CauseRetrieval},
		{"mixed drops fallback attribution", true, true, PressureBuckets{Available: true, Pinned: 7, ToolSchema: 5, History: 28, ToolOutput: 31}, 71, CauseToolOutput},
		{"legacy fallback", false, false, PressureBuckets{Available: true, Pinned: 7, ToolSchema: 5, History: 28, Retrieval: 23}, 63, CauseHistory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := mixedTraceState()
			if tc.stripAttrib {
				st.Messages[6].Context.Groups[1].Alternatives[0].Attrib = nil
			}
			_, got, err := (ContextManager{Mixed: tc.mixed, Estimate: runeEstimator}).Assemble(context.Background(), st, 5, TokenBudget{Input: 100})
			if err != nil {
				t.Fatalf("Assemble(%s) error = %v, want nil", tc.name, err)
			}
			want := Pressure{UsedPct: float64(tc.tokens) / 100, InputTokens: tc.tokens, InputBudget: 100, Level: LevelWatch, Cause: tc.cause, Buckets: tc.want}
			if got != want {
				t.Errorf("Assemble(%s) pressure = %+v, want %+v", tc.name, got, want)
			}
		})
	}
}

func TestAssembleMixedFinalPressureBuckets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		finalCost int
		schema    int
		want      Pressure
	}{
		{"valid full state", 100, 0, Pressure{UsedPct: 1.3, InputTokens: 130, InputBudget: 100, Level: LevelCritical, Cause: CauseToolOutput, Mitigation: MitigationHalt, Buckets: PressureBuckets{Available: true, History: 20, ToolOutput: 110}}},
		{"arithmetic failure", math.MaxInt - 5, 0, Pressure{UsedPct: float64(math.MaxInt) / 100, InputTokens: math.MaxInt, InputBudget: 100, Level: LevelCritical, Cause: CauseToolOutput, Mitigation: MitigationHalt}},
		{"schema arithmetic failure", math.MaxInt - 32, 5, Pressure{UsedPct: float64(math.MaxInt) / 100, InputTokens: math.MaxInt, InputBudget: 100, Level: LevelCritical, Cause: CauseToolOutput, Mitigation: MitigationHalt}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Deliberately impure: admission sees 5, the final full-state check
			// sees the new cost. Pure estimators cannot reach this mixed branch.
			calls := 0
			estimate := func(s string) int {
				if s == "FINAL" {
					calls++
					if calls > 1 {
						return tc.finalCost
					}
				}
				return len(s)
			}
			set := validSet()
			set.Groups[0].Alternatives[0].Content = "FINAL"
			st := State{Messages: []Message{mixedAsstCall("c1"), mixedToolResult("c1", traceFallback, set, 4096)}}
			_, got, err := (ContextManager{Mixed: true, Estimate: estimate}).Assemble(context.Background(), st, tc.schema, TokenBudget{Input: 100})
			if !errors.Is(err, ErrContextExhausted) {
				t.Fatalf("Assemble(%s) error = %v, want ErrContextExhausted", tc.name, err)
			}
			if got != tc.want {
				t.Errorf("Assemble(%s) pressure = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

func TestAssembleMixedPressureBucketsAfterEviction(t *testing.T) {
	t.Parallel()
	_, got, err := (ContextManager{Mixed: true, Estimate: runeEstimator}).Assemble(context.Background(), mixedTraceState(), 5, TokenBudget{Input: 67})
	if err != nil {
		t.Fatalf("Assemble(mixed eviction) error = %v, want nil", err)
	}
	// The chain's admission floor does not fit; its call and anchor are both
	// evicted, leaving system + goal (7) and the two plain exchanges (8).
	want := Pressure{UsedPct: float64(20) / 67, Evicted: 1, Compactions: 1, InputTokens: 20, InputBudget: 67, Level: LevelOK, Cause: CauseHistory, Mitigation: MitigationEvict, Buckets: PressureBuckets{Available: true, Pinned: 7, ToolSchema: 5, History: 8}}
	if got != want {
		t.Errorf("Assemble(mixed eviction) pressure = %+v, want %+v", got, want)
	}
}

func TestAssemblePressureBucketsGuardedAndFramed(t *testing.T) {
	t.Parallel()
	set := validSet()
	set.Groups[0].Alternatives[0].Content = "ABCD"
	st := State{Messages: []Message{mixedAsstCall("c1"), mixedToolResult("c1", traceFallback, set, 4096)}}
	// Negative estimates for nonempty text must fall back to ceil(bytes/4)
	// on the mixed path, including the tool frame and tool-call metadata.
	m := ContextManager{Mixed: true, frameToolResults: true, Estimate: func(string) int { return -1 }}
	_, got, err := m.Assemble(context.Background(), st, 3, TokenBudget{Input: 100})
	if err != nil {
		t.Fatalf("Assemble(guarded framed anchor) error = %v, want nil", err)
	}
	want := Pressure{UsedPct: .37, InputTokens: 37, InputBudget: 100, Cause: CauseToolOutput, Buckets: PressureBuckets{Available: true, ToolSchema: 3, History: 6, ToolOutput: 28}}
	if got != want {
		t.Errorf("Assemble(guarded framed anchor) pressure = %+v, want %+v", got, want)
	}
}

func TestRunPressureBucketsExhaustionSkipsModel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		manager ContextManager
		goal    string
		history []provider.ChatMessage
		want    Pressure
	}{
		{name: "pinned", manager: ContextManager{Estimate: runeEstimator}, goal: "123456", want: Pressure{UsedPct: 1.2, InputTokens: 6, InputBudget: 5, Level: LevelCritical, Cause: CausePinned, Mitigation: MitigationHalt}},
		{name: "custom compactor", manager: ContextManager{Estimate: runeEstimator, Compactor: pressureExhaustingCompactor{}}, goal: "g", want: Pressure{UsedPct: 1.8, InputTokens: 9, InputBudget: 5, Level: LevelCritical, Cause: CausePinned, Mitigation: MitigationHalt}},
		{name: "final full state", manager: ContextManager{Estimate: runeEstimator, Compactor: passthroughCompactor{}}, goal: "g", history: []provider.ChatMessage{{Role: "user", Content: "123456"}}, want: Pressure{UsedPct: 1.4, InputTokens: 7, InputBudget: 5, Level: LevelCritical, Cause: CauseHistory, Mitigation: MitigationHalt, Buckets: PressureBuckets{Available: true, Pinned: 1, History: 6}}},
		{name: "final arithmetic", manager: ContextManager{Estimate: func(s string) int {
			if s == "huge" {
				return math.MaxInt
			}
			return len(s)
		}, Compactor: passthroughCompactor{}}, goal: "g", history: []provider.ChatMessage{{Role: "user", Content: "huge"}}, want: Pressure{UsedPct: float64(math.MaxInt) / 5, InputTokens: math.MaxInt, InputBudget: 5, Level: LevelCritical, Cause: CauseHistory, Mitigation: MitigationHalt}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &scriptedCaller{}
			rec := &pressureRec{}
			estimate := tc.manager.Estimate
			tc.manager.Estimate = func(s string) int {
				if s == withToolTrustContract("") {
					return 0
				}
				return estimate(s)
			}
			_, err := New(model, tc.manager).Run(context.Background(), Request{Goal: tc.goal, History: tc.history, Budget: Budget{InputCeiling: 5}}, rec)
			if !errors.Is(err, ErrContextExhausted) {
				t.Fatalf("Run(%s) error = %v, want ErrContextExhausted", tc.name, err)
			}
			if model.calls != 0 {
				t.Errorf("Run(%s) model calls = %d, want 0", tc.name, model.calls)
			}
			if len(rec.pressures) != 1 {
				t.Fatalf("Run(%s) pressure events = %d, want 1", tc.name, len(rec.pressures))
			}
			if got := rec.pressures[0].Pressure; got != tc.want {
				t.Errorf("Run(%s) pressure = %+v, want %+v", tc.name, got, tc.want)
			}
		})
	}
}

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

func TestPressureThresholdsClassifyNormalizesZeroValue(t *testing.T) {
	level, mitigation := (PressureThresholds{}).Classify(0.10, false, false)
	if level != LevelOK || mitigation != MitigationNone {
		t.Fatalf("zero-value Classify = %v/%v, want ok/none", level, mitigation)
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
