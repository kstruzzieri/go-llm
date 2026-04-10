package provider

import (
	"testing"
	"time"
)

func TestTierString(t *testing.T) {
	tests := []struct {
		tier Tier
		want string
	}{
		{TierBasic, "basic"},
		{TierGood, "good"},
		{TierGreat, "great"},
		{TierBest, "best"},
		{Tier(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.tier.String(); got != tt.want {
			t.Errorf("Tier(%d).String() = %q, want %q", tt.tier, got, tt.want)
		}
	}
}

func TestProfileSourceString(t *testing.T) {
	tests := []struct {
		src  ProfileSource
		want string
	}{
		{SourceStatic, "static"},
		{SourceFingerprint, "fingerprint"},
		{SourceRuntime, "runtime"},
		{SourceMerged, "merged"},
		{ProfileSource(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.src.String(); got != tt.want {
			t.Errorf("ProfileSource(%d).String() = %q, want %q", tt.src, got, tt.want)
		}
	}
}

func TestRAMAtContext(t *testing.T) {
	tests := []struct {
		name    string
		rp      ResourceProfile
		ctxSize int
		want    float64
	}{
		{
			name: "8B model at 4096 context",
			rp: ResourceProfile{
				ParameterSize:  "8B",
				QuantLevel:     "Q4_K_M",
				RAMRequired:    5.0,
				RAMRecommended: 8.0,
			},
			ctxSize: 4096,
			want:    5.0 + (4096.0/1024.0)*0.5, // 5.0 + 2.0 = 7.0
		},
		{
			name: "70B model at 32768 context",
			rp: ResourceProfile{
				ParameterSize:  "70B",
				QuantLevel:     "Q4_K_M",
				RAMRequired:    40.0,
				RAMRecommended: 48.0,
			},
			ctxSize: 32768,
			want:    40.0 + (32768.0/1024.0)*0.5, // 40.0 + 16.0 = 56.0
		},
		{
			name:    "zero context returns base RAM",
			rp:      ResourceProfile{RAMRequired: 5.0},
			ctxSize: 0,
			want:    5.0,
		},
		{
			name: "1024 context adds exactly kvScaleFactor",
			rp: ResourceProfile{
				ParameterSize: "3B",
				RAMRequired:   2.0,
			},
			ctxSize: 1024,
			want:    2.0 + 0.5, // 2.5
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.rp.RAMAtContext(tt.ctxSize)
			if got != tt.want {
				t.Errorf("RAMAtContext(%d) = %f, want %f", tt.ctxSize, got, tt.want)
			}
		})
	}
}

func TestModelProfileConstruction(t *testing.T) {
	now := time.Now()
	profile := ModelProfile{
		Key:      ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		Name:     "Qwen 3 8B",
		Family:   "qwen3",
		Provider: "ollama",
		Resources: ResourceProfile{
			ParameterSize:   "8B",
			QuantLevel:      "Q4_K_M",
			RAMRequired:     5.0,
			RAMRecommended:  8.0,
			VRAMRecommended: 6.0,
		},
		Caps: CapChat | CapStream | CapThinking,
		FIM: &FIMConfig{
			Prefix:          "<|fim_prefix|>",
			Suffix:          "<|fim_suffix|>",
			Middle:          "<|fim_middle|>",
			PrefixBudgetPct: 75,
		},
		ThinkMode:     ThinkToggle,
		ThinkTags:     &ThinkTags{Open: "<think>", Close: "</think>"},
		Quality:       TierGood,
		Speed:         TierGood,
		ContextWindow: 32768,
		Dimensions:    0,
		Source:        SourceMerged,
		Digest:        "sha256:abc123",
		UpdatedAt:     now,
	}

	// Verify key fields.
	if got := profile.Key.String(); got != "ollama/qwen3:8b" {
		t.Errorf("Key.String() = %q, want %q", got, "ollama/qwen3:8b")
	}
	if profile.Family != "qwen3" {
		t.Errorf("Family = %q, want %q", profile.Family, "qwen3")
	}
	if !profile.Caps.Has(CapChat | CapStream) {
		t.Error("Caps should have CapChat|CapStream")
	}
	if !profile.Caps.Has(CapThinking) {
		t.Error("Caps should have CapThinking")
	}
	if profile.Caps.Has(CapEmbed) {
		t.Error("Caps should not have CapEmbed")
	}
	if profile.FIM == nil {
		t.Fatal("FIM should not be nil")
	}
	if profile.FIM.Prefix != "<|fim_prefix|>" {
		t.Errorf("FIM.Prefix = %q, want %q", profile.FIM.Prefix, "<|fim_prefix|>")
	}
	if profile.ThinkMode != ThinkToggle {
		t.Errorf("ThinkMode = %v, want ThinkToggle", profile.ThinkMode)
	}
	if profile.Quality.String() != "good" {
		t.Errorf("Quality.String() = %q, want %q", profile.Quality.String(), "good")
	}
	if profile.Speed.String() != "good" {
		t.Errorf("Speed.String() = %q, want %q", profile.Speed.String(), "good")
	}
	if profile.ContextWindow != 32768 {
		t.Errorf("ContextWindow = %d, want %d", profile.ContextWindow, 32768)
	}
	if profile.Dimensions != 0 {
		t.Errorf("Dimensions = %d, want 0 for non-embedding model", profile.Dimensions)
	}
	if profile.Source != SourceMerged {
		t.Errorf("Source = %v, want SourceMerged", profile.Source)
	}
	if profile.Digest != "sha256:abc123" {
		t.Errorf("Digest = %q, want %q", profile.Digest, "sha256:abc123")
	}
	if profile.UpdatedAt != now {
		t.Errorf("UpdatedAt = %v, want %v", profile.UpdatedAt, now)
	}

	// Verify RAMAtContext through the profile's Resources.
	ram := profile.Resources.RAMAtContext(32768)
	expectedRAM := 5.0 + (32768.0/1024.0)*0.5 // 5.0 + 16.0 = 21.0
	if ram != expectedRAM {
		t.Errorf("Resources.RAMAtContext(32768) = %f, want %f", ram, expectedRAM)
	}
}

func TestModelProfileEmbeddingModel(t *testing.T) {
	profile := ModelProfile{
		Key:        ModelKey{Provider: "ollama", Model: "nomic-embed-text"},
		Name:       "Nomic Embed Text",
		Family:     "nomic-embed",
		Provider:   "ollama",
		Caps:       CapEmbed,
		Quality:    TierGood,
		Speed:      TierGreat,
		Dimensions: 768,
		Source:     SourceStatic,
	}

	if !profile.Caps.Has(CapEmbed) {
		t.Error("embedding model should have CapEmbed")
	}
	if profile.Caps.Has(CapChat) {
		t.Error("embedding model should not have CapChat")
	}
	if profile.Dimensions != 768 {
		t.Errorf("Dimensions = %d, want 768", profile.Dimensions)
	}
	if profile.FIM != nil {
		t.Error("embedding model should have nil FIM")
	}
}

func TestModelProfileNilThinkTags(t *testing.T) {
	profile := ModelProfile{
		Key:       ModelKey{Provider: "ollama", Model: "codellama:7b"},
		ThinkMode: ThinkNone,
		ThinkTags: nil,
	}

	if profile.ThinkTags != nil {
		t.Error("ThinkTags should be nil for ThinkNone models")
	}
	if profile.ThinkMode != ThinkNone {
		t.Errorf("ThinkMode = %v, want ThinkNone", profile.ThinkMode)
	}
}

func TestRecommendOptsConstruction(t *testing.T) {
	opts := RecommendOpts{
		RequestedModel:     "qwen3:8b",
		UseCase:            "chat",
		AvailableRAM:       16.0,
		ContextSize:        8192,
		PreferWarm:         true,
		PreferredProviders: []string{"ollama"},
		RequiredCaps:       CapChat | CapStream,
	}

	if opts.RequestedModel != "qwen3:8b" {
		t.Errorf("RequestedModel = %q, want %q", opts.RequestedModel, "qwen3:8b")
	}
	if opts.UseCase != "chat" {
		t.Errorf("UseCase = %q, want %q", opts.UseCase, "chat")
	}
	if opts.AvailableRAM != 16.0 {
		t.Errorf("AvailableRAM = %f, want 16.0", opts.AvailableRAM)
	}
	if opts.ContextSize != 8192 {
		t.Errorf("ContextSize = %d, want 8192", opts.ContextSize)
	}
	if !opts.PreferWarm {
		t.Error("PreferWarm should be true")
	}
	if len(opts.PreferredProviders) != 1 || opts.PreferredProviders[0] != "ollama" {
		t.Errorf("PreferredProviders = %v, want [ollama]", opts.PreferredProviders)
	}
	if !opts.RequiredCaps.Has(CapChat | CapStream) {
		t.Error("RequiredCaps should have CapChat|CapStream")
	}
}
