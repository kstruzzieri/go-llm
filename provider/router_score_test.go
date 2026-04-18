package provider

import (
	"fmt"
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// TestTierToFloat
// ---------------------------------------------------------------------------

func TestTierToFloat(t *testing.T) {
	tests := []struct {
		tier Tier
		want float64
	}{
		{TierBasic, 0.25},
		{TierGood, 0.50},
		{TierGreat, 0.75},
		{TierBest, 1.00},
		{Tier(99), 0.25}, // unknown defaults to Basic
	}

	for _, tt := range tests {
		t.Run(tt.tier.String(), func(t *testing.T) {
			got := tierToFloat(tt.tier)
			if got != tt.want {
				t.Errorf("tierToFloat(%v) = %v, want %v", tt.tier, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// TestDefaultWeightProfile
// ---------------------------------------------------------------------------

func TestDefaultWeightProfile(t *testing.T) {
	useCases := []string{"fim", "chat", "embedding", "reasoning", "code-review", "agent", "tool-use"}

	for _, uc := range useCases {
		t.Run(uc, func(t *testing.T) {
			wp := defaultWeightProfile(uc)
			if wp == nil {
				t.Fatalf("defaultWeightProfile(%q) returned nil", uc)
			}
			// All weights must be non-negative.
			weights := []struct {
				name string
				val  int
			}{
				{"Warmth", wp.Warmth},
				{"Headroom", wp.Headroom},
				{"Feedback", wp.Feedback},
				{"Quality", wp.Quality},
				{"Speed", wp.Speed},
				{"KVCache", wp.KVCache},
				{"Cost", wp.Cost},
			}
			for _, w := range weights {
				if w.val < 0 {
					t.Errorf("%s weight = %d, want >= 0", w.name, w.val)
				}
			}
		})
	}

	t.Run("unknown falls back to chat", func(t *testing.T) {
		unknown := defaultWeightProfile("some-unknown-use-case")
		chat := defaultWeightProfile("chat")
		if unknown == nil {
			t.Fatal("defaultWeightProfile for unknown returned nil")
		}
		if *unknown != *chat {
			t.Errorf("unknown profile = %+v, want chat profile %+v", unknown, chat)
		}
	})

	t.Run("agent and tool-use are aliased", func(t *testing.T) {
		agent := defaultWeightProfile("agent")
		toolUse := defaultWeightProfile("tool-use")
		if *agent != *toolUse {
			t.Errorf("agent (%+v) and tool-use (%+v) should share a profile", agent, toolUse)
		}
		// Agent workloads must weight Speed and Feedback higher than chat.
		chat := defaultWeightProfile("chat")
		if agent.Speed <= chat.Speed {
			t.Errorf("agent Speed (%d) should exceed chat Speed (%d)", agent.Speed, chat.Speed)
		}
		if agent.Feedback <= chat.Feedback {
			t.Errorf("agent Feedback (%d) should exceed chat Feedback (%d)", agent.Feedback, chat.Feedback)
		}
	})
}

// ---------------------------------------------------------------------------
// TestScoreCandidateBasic
// ---------------------------------------------------------------------------

func TestScoreCandidateBasic(t *testing.T) {
	profile := &ModelProfile{
		Key:           ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		Quality:       TierGood,
		Speed:         TierGreat,
		Caps:          CapChat | CapStream,
		ContextWindow: 32768,
	}

	req := RoutingRequest{
		UseCase:      "chat",
		RequiredCaps: CapChat,
	}

	budget := BudgetResult{
		Decision:      BudgetOK,
		HeadroomScore: 0.8,
	}

	breaker := NewCircuitBreaker()

	bd := scoreCandidate(profile, req, budget, nil, nil, breaker)

	if !bd.capabilityGate {
		t.Error("capabilityGate should be true")
	}
	if bd.breakerPenalty != 0 {
		t.Errorf("breakerPenalty = %v, want 0", bd.breakerPenalty)
	}
	if bd.qualityTier != 0.5 {
		t.Errorf("qualityTier = %v, want 0.5", bd.qualityTier)
	}
	if bd.speedTier != 0.75 {
		t.Errorf("speedTier = %v, want 0.75", bd.speedTier)
	}
	if bd.headroomScore != 0.8 {
		t.Errorf("headroomScore = %v, want 0.8", bd.headroomScore)
	}
	if bd.feedbackScore != 0.5 {
		t.Errorf("feedbackScore = %v, want 0.5", bd.feedbackScore)
	}
	if bd.warmthBonus != 0.0 {
		t.Errorf("warmthBonus = %v, want 0.0 (no warmth source)", bd.warmthBonus)
	}
}

// ---------------------------------------------------------------------------
// TestScoreCandidateCapabilityGate
// ---------------------------------------------------------------------------

func TestScoreCandidateCapabilityGate(t *testing.T) {
	profile := &ModelProfile{
		Key:           ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		Quality:       TierGood,
		Speed:         TierGood,
		Caps:          CapChat, // only Chat, no ToolCall
		ContextWindow: 32768,
	}

	req := RoutingRequest{
		UseCase:      "chat",
		RequiredCaps: CapChat | CapToolCall, // requires both
	}

	budget := BudgetResult{
		Decision:      BudgetOK,
		HeadroomScore: 0.8,
	}

	breaker := NewCircuitBreaker()

	bd := scoreCandidate(profile, req, budget, nil, nil, breaker)

	if bd.capabilityGate {
		t.Error("capabilityGate should be false when model lacks required capabilities")
	}
}

// ---------------------------------------------------------------------------
// TestScoreCandidateBreakerOpen
// ---------------------------------------------------------------------------

func TestScoreCandidateBreakerOpen(t *testing.T) {
	profile := &ModelProfile{
		Key:           ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		Quality:       TierGood,
		Speed:         TierGood,
		Caps:          CapChat,
		ContextWindow: 32768,
	}

	req := RoutingRequest{
		UseCase:      "chat",
		RequiredCaps: CapChat,
	}

	budget := BudgetResult{
		Decision:      BudgetOK,
		HeadroomScore: 0.8,
	}

	// Threshold=1: a single failure opens the breaker.
	breaker := NewCircuitBreaker(WithFailureThreshold(1))
	breaker.RecordFailure(fmt.Errorf("test failure"))

	if breaker.State() != BreakerOpen {
		t.Fatalf("breaker should be open after 1 failure with threshold=1, got %v", breaker.State())
	}

	bd := scoreCandidate(profile, req, budget, nil, nil, breaker)

	if !bd.capabilityGate {
		t.Error("capabilityGate should be true (caps matched)")
	}
	if !math.IsInf(bd.breakerPenalty, -1) {
		t.Errorf("breakerPenalty = %v, want -Inf", bd.breakerPenalty)
	}
}

// ---------------------------------------------------------------------------
// TestScoreCandidateWarmthBonus
// ---------------------------------------------------------------------------

func TestScoreCandidateWarmthBonus(t *testing.T) {
	warmKey := ModelKey{Provider: "ollama", Model: "warm-model"}
	coldKey := ModelKey{Provider: "ollama", Model: "cold-model"}

	warmProfile := &ModelProfile{
		Key:           warmKey,
		Quality:       TierGood,
		Speed:         TierGood,
		Caps:          CapChat,
		ContextWindow: 32768,
	}
	coldProfile := &ModelProfile{
		Key:           coldKey,
		Quality:       TierGood,
		Speed:         TierGood,
		Caps:          CapChat,
		ContextWindow: 32768,
	}

	req := RoutingRequest{
		UseCase:      "chat",
		RequiredCaps: CapChat,
	}

	budget := BudgetResult{
		Decision:      BudgetOK,
		HeadroomScore: 0.8,
	}

	ws := newMockWarmthSource()
	ws.SetWarm(warmKey, 4.0)
	// coldKey is not in the warmth source → IsWarm returns false.

	breakerWarm := NewCircuitBreaker()
	breakerCold := NewCircuitBreaker()

	bdWarm := scoreCandidate(warmProfile, req, budget, ws, nil, breakerWarm)
	bdCold := scoreCandidate(coldProfile, req, budget, ws, nil, breakerCold)

	if bdWarm.warmthBonus != warmthBonusMax {
		t.Errorf("warm model warmthBonus = %v, want %v", bdWarm.warmthBonus, warmthBonusMax)
	}
	if bdCold.warmthBonus != 0.0 {
		t.Errorf("cold model warmthBonus = %v, want 0.0", bdCold.warmthBonus)
	}
	if bdWarm.warmthBonus <= bdCold.warmthBonus {
		t.Error("warm model should have higher warmthBonus than cold model")
	}
}

// ---------------------------------------------------------------------------
// TestComputeWeightedScoreNormalization
// ---------------------------------------------------------------------------

func TestComputeWeightedScoreNormalization(t *testing.T) {
	bd := scoreBreakdown{
		warmthBonus:    0.3,
		headroomScore:  0.8,
		feedbackScore:  0.5,
		qualityTier:    0.75,
		speedTier:      0.75,
		kvCacheBonus:   0,
		costPenalty:    0,
		breakerPenalty: 0,
		capabilityGate: true,
	}

	activeSignals := map[string]bool{
		"warmth":   true,
		"headroom": true,
		"quality":  true,
		"speed":    true,
	}

	score := computeWeightedScore(bd, "chat", activeSignals, nil)

	if score <= 0 || score > 1.0 {
		t.Errorf("score = %v, want in (0, 1.0]", score)
	}
}

// ---------------------------------------------------------------------------
// TestComputeWeightedScoreCustomWeights
// ---------------------------------------------------------------------------

func TestComputeWeightedScoreCustomWeights(t *testing.T) {
	bd := scoreBreakdown{
		warmthBonus:    0,
		headroomScore:  0.5,
		feedbackScore:  0.5,
		qualityTier:    1.0, // Best quality
		speedTier:      0.25,
		kvCacheBonus:   0,
		costPenalty:    0,
		breakerPenalty: 0,
		capabilityGate: true,
	}

	activeSignals := map[string]bool{
		"headroom": true,
		"quality":  true,
		"speed":    true,
	}

	customWeights := &WeightProfile{
		Warmth:   0,
		Headroom: 1,
		Feedback: 0,
		Quality:  10, // Heavily weighted toward quality
		Speed:    1,
		KVCache:  0,
		Cost:     0,
	}

	score := computeWeightedScore(bd, "chat", activeSignals, customWeights)

	if score <= 0.7 {
		t.Errorf("score = %v, want > 0.7 when quality=1.0 and Quality weight=10", score)
	}
}

// ---------------------------------------------------------------------------
// TestComputeWeightedScoreNoActiveSignals
// ---------------------------------------------------------------------------

func TestComputeWeightedScoreNoActiveSignals(t *testing.T) {
	bd := scoreBreakdown{
		warmthBonus:    0.3,
		headroomScore:  0.8,
		qualityTier:    1.0,
		speedTier:      1.0,
		capabilityGate: true,
	}

	activeSignals := map[string]bool{} // empty

	score := computeWeightedScore(bd, "chat", activeSignals, nil)

	if score != 0 {
		t.Errorf("score = %v, want 0 with no active signals", score)
	}
}

// ---------------------------------------------------------------------------
// TestTiebreakOrder
// ---------------------------------------------------------------------------

func TestTiebreakOrder(t *testing.T) {
	candidates := []scoredCandidate{
		{
			profile: &ModelProfile{
				Key:           ModelKey{Provider: "ollama", Model: "model-b"},
				ContextWindow: 8192,
			},
			score: 0.8,
		},
		{
			profile: &ModelProfile{
				Key:           ModelKey{Provider: "ollama", Model: "model-a"},
				ContextWindow: 32768,
			},
			score: 0.8,
		},
		{
			profile: &ModelProfile{
				Key:           ModelKey{Provider: "ollama", Model: "model-c"},
				ContextWindow: 32768,
			},
			score: 0.8,
		},
	}

	sortScoredCandidates(candidates, nil)

	// Same score → higher context first.
	if candidates[0].profile.Key.Model != "model-a" {
		t.Errorf("first candidate = %v, want model-a (higher context, alphabetical)", candidates[0].profile.Key.Model)
	}
	if candidates[1].profile.Key.Model != "model-c" {
		t.Errorf("second candidate = %v, want model-c (higher context, alphabetical after a)", candidates[1].profile.Key.Model)
	}
	if candidates[2].profile.Key.Model != "model-b" {
		t.Errorf("third candidate = %v, want model-b (lower context)", candidates[2].profile.Key.Model)
	}
}

// ---------------------------------------------------------------------------
// TestTiebreakPreferWarm
// ---------------------------------------------------------------------------

func TestTiebreakPreferWarm(t *testing.T) {
	warmKey := ModelKey{Provider: "ollama", Model: "warm-model"}
	coldKey := ModelKey{Provider: "ollama", Model: "cold-model"}

	ws := newMockWarmthSource()
	ws.SetWarm(warmKey, 4.0)
	// cold-model is not warm.

	candidates := []scoredCandidate{
		{
			profile: &ModelProfile{
				Key:           coldKey,
				ContextWindow: 32768,
			},
			score: 0.8,
		},
		{
			profile: &ModelProfile{
				Key:           warmKey,
				ContextWindow: 32768,
			},
			score: 0.8,
		},
	}

	sortScoredCandidates(candidates, ws)

	// Same score, same context → prefer warm.
	if candidates[0].profile.Key.Model != "warm-model" {
		t.Errorf("first candidate = %v, want warm-model (prefer warm tiebreak)",
			candidates[0].profile.Key.Model)
	}
	if candidates[1].profile.Key.Model != "cold-model" {
		t.Errorf("second candidate = %v, want cold-model",
			candidates[1].profile.Key.Model)
	}
}
