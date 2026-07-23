package provider

import (
	"strings"
	"testing"
	"time"
)

func TestCapability_Has(t *testing.T) {
	tests := []struct {
		name string
		cap  Capability
		flag Capability
		want bool
	}{
		{name: "single bit set and tested", cap: CapChat, flag: CapChat, want: true},
		{name: "single bit not set", cap: CapChat, flag: CapEmbed, want: false},
		{name: "multi-bit contains single", cap: CapChat | CapStream, flag: CapStream, want: true},
		{name: "multi-bit missing single", cap: CapChat | CapStream, flag: CapEmbed, want: false},
		{name: "multi-bit flag subset match", cap: CapChat | CapStream | CapEmbed, flag: CapChat | CapEmbed, want: true},
		{name: "multi-bit flag partial miss", cap: CapChat | CapStream, flag: CapChat | CapEmbed, want: false},
		{name: "zero has zero", cap: 0, flag: 0, want: true},
		{name: "nonzero has zero", cap: CapChat, flag: 0, want: true},
		{name: "zero lacks any bit", cap: 0, flag: CapChat, want: false},
		{name: "all caps has thinking", cap: CapChat | CapGenerate | CapInsert | CapEmbed | CapStream | CapToolCall | CapThinking, flag: CapThinking, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cap.Has(tt.flag)
			if got != tt.want {
				t.Errorf("Capability(%d).Has(%d) = %v, want %v", tt.cap, tt.flag, got, tt.want)
			}
		})
	}
}

func TestCapability_String(t *testing.T) {
	tests := []struct {
		name string
		cap  Capability
		want string
	}{
		{name: "none", cap: 0, want: "none"},
		{name: "single chat", cap: CapChat, want: "chat"},
		{name: "single embed", cap: CapEmbed, want: "embed"},
		{name: "chat and stream", cap: CapChat | CapStream, want: "chat|stream"},
		{name: "generate and embed", cap: CapGenerate | CapEmbed, want: "generate|embed"},
		{name: "all capabilities", cap: CapChat | CapGenerate | CapInsert | CapEmbed | CapStream | CapToolCall | CapThinking, want: "chat|generate|insert|embed|stream|tool_call|thinking"},
		{name: "insert only", cap: CapInsert, want: "insert"},
		{name: "thinking only", cap: CapThinking, want: "thinking"},
		{name: "tool_call and thinking", cap: CapToolCall | CapThinking, want: "tool_call|thinking"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cap.String()
			if got != tt.want {
				t.Errorf("Capability(%d).String() = %q, want %q", tt.cap, got, tt.want)
			}
		})
	}
}

func TestModelKey_String(t *testing.T) {
	tests := []struct {
		name string
		key  ModelKey
		want string
	}{
		{name: "ollama model", key: ModelKey{Provider: "ollama", Model: "qwen3:8b"}, want: "ollama/qwen3:8b"},
		{name: "openai model", key: ModelKey{Provider: "openai", Model: "gpt-4o"}, want: "openai/gpt-4o"},
		{name: "empty provider", key: ModelKey{Provider: "", Model: "test"}, want: "/test"},
		{name: "empty model", key: ModelKey{Provider: "ollama", Model: ""}, want: "ollama/"},
		{name: "both empty", key: ModelKey{Provider: "", Model: ""}, want: "/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.key.String()
			if got != tt.want {
				t.Errorf("ModelKey{%q, %q}.String() = %q, want %q", tt.key.Provider, tt.key.Model, got, tt.want)
			}
		})
	}
}

func TestThinkMode_String(t *testing.T) {
	tests := []struct {
		name string
		mode ThinkMode
		want string
	}{
		{name: "none", mode: ThinkNone, want: "none"},
		{name: "always", mode: ThinkAlways, want: "always"},
		{name: "toggle", mode: ThinkToggle, want: "toggle"},
		{name: "auto", mode: ThinkAuto, want: "auto"},
		{name: "unknown value", mode: ThinkMode(99), want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.mode.String()
			if got != tt.want {
				t.Errorf("ThinkMode(%d).String() = %q, want %q", tt.mode, got, tt.want)
			}
		})
	}
}

func TestParseThinkModeStrict(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    ThinkMode
		wantErr bool
	}{
		{name: "none", input: "none", want: ThinkNone},
		{name: "always", input: "always", want: ThinkAlways},
		{name: "toggle", input: "toggle", want: ThinkToggle},
		{name: "auto", input: "auto", want: ThinkAuto},
		{name: "case insensitive", input: "ALWAYS", want: ThinkAlways},
		{name: "invalid", input: "maybe", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseThinkModeStrict(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.input) {
					t.Errorf("error %q does not name input %q", err.Error(), tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ParseThinkModeStrict(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestDefaultThinkTags(t *testing.T) {
	tags := DefaultThinkTags()
	if tags.Open != "<think>" {
		t.Errorf("DefaultThinkTags().Open = %q, want %q", tags.Open, "<think>")
	}
	if tags.Close != "</think>" {
		t.Errorf("DefaultThinkTags().Close = %q, want %q", tags.Close, "</think>")
	}
}

func TestPtr(t *testing.T) {
	t.Run("float64", func(t *testing.T) {
		p := Ptr(0.7)
		if p == nil {
			t.Fatal("Ptr(0.7) returned nil")
		}
		if *p != 0.7 {
			t.Errorf("*Ptr(0.7) = %v, want 0.7", *p)
		}
	})

	t.Run("int", func(t *testing.T) {
		p := Ptr(42)
		if p == nil {
			t.Fatal("Ptr(42) returned nil")
		}
		if *p != 42 {
			t.Errorf("*Ptr(42) = %v, want 42", *p)
		}
	})

	t.Run("string", func(t *testing.T) {
		p := Ptr("hello")
		if p == nil {
			t.Fatal("Ptr returned nil")
		}
		if *p != "hello" {
			t.Errorf("*Ptr(\"hello\") = %q, want \"hello\"", *p)
		}
	})
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int
	}{
		{name: "empty string", input: "", want: 0},
		{name: "short string", input: "hi", want: 1},
		{name: "single char", input: "a", want: 1},
		{name: "three chars", input: "abc", want: 1},
		{name: "four chars", input: "abcd", want: 1},
		{name: "eight chars", input: "abcdefgh", want: 2},
		{name: "typical sentence", input: "The quick brown fox jumps over the lazy dog.", want: 11},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateTokens(tt.input)
			if got != tt.want {
				t.Errorf("estimateTokens(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestModelOptions_PointerFields(t *testing.T) {
	// Verify that nil pointer fields represent "use default" semantics.
	opts := ModelOptions{}
	if opts.Temperature != nil {
		t.Error("zero-value ModelOptions should have nil Temperature")
	}
	if opts.TopP != nil {
		t.Error("zero-value ModelOptions should have nil TopP")
	}
	if opts.TopK != nil {
		t.Error("zero-value ModelOptions should have nil TopK")
	}
	if opts.RepeatPenalty != nil {
		t.Error("zero-value ModelOptions should have nil RepeatPenalty")
	}

	// Verify that Ptr sets values correctly.
	opts = ModelOptions{
		Temperature:   Ptr(0.8),
		TopP:          Ptr(0.9),
		TopK:          Ptr(40),
		NumPredict:    256,
		NumCtx:        4096,
		Stop:          []string{"\n"},
		RepeatPenalty: Ptr(1.1),
	}
	if *opts.Temperature != 0.8 {
		t.Errorf("Temperature = %v, want 0.8", *opts.Temperature)
	}
	if *opts.TopP != 0.9 {
		t.Errorf("TopP = %v, want 0.9", *opts.TopP)
	}
	if *opts.TopK != 40 {
		t.Errorf("TopK = %v, want 40", *opts.TopK)
	}
	if *opts.RepeatPenalty != 1.1 {
		t.Errorf("RepeatPenalty = %v, want 1.1", *opts.RepeatPenalty)
	}
}

func TestThinkBudget(t *testing.T) {
	budget := ThinkBudget{
		MaxTokens:  1000,
		MaxTime:    5 * time.Second,
		OnExceeded: ThinkBudgetCancel,
	}
	if budget.MaxTokens != 1000 {
		t.Errorf("MaxTokens = %d, want 1000", budget.MaxTokens)
	}
	if budget.MaxTime != 5*time.Second {
		t.Errorf("MaxTime = %v, want 5s", budget.MaxTime)
	}
	if budget.OnExceeded != ThinkBudgetCancel {
		t.Errorf("OnExceeded = %d, want ThinkBudgetCancel(%d)", budget.OnExceeded, ThinkBudgetCancel)
	}
}

func TestThinkMetrics(t *testing.T) {
	m := ThinkMetrics{
		ThinkingTokens: 200,
		ContentTokens:  800,
		ThinkRatio:     0.2,
		ThinkDuration:  3 * time.Second,
	}
	if m.ThinkingTokens+m.ContentTokens != 1000 {
		t.Errorf("total tokens = %d, want 1000", m.ThinkingTokens+m.ContentTokens)
	}
	if m.ThinkRatio != 0.2 {
		t.Errorf("ThinkRatio = %v, want 0.2", m.ThinkRatio)
	}
}

func TestCapability_BitwiseOps(t *testing.T) {
	// Verify that capabilities compose correctly with bitwise OR.
	combined := CapChat | CapStream | CapToolCall
	if combined&CapChat == 0 {
		t.Error("combined should include CapChat")
	}
	if combined&CapStream == 0 {
		t.Error("combined should include CapStream")
	}
	if combined&CapToolCall == 0 {
		t.Error("combined should include CapToolCall")
	}
	if combined&CapInsert != 0 {
		t.Error("combined should not include CapInsert")
	}
	if combined&CapEmbed != 0 {
		t.Error("combined should not include CapEmbed")
	}
	if combined&CapThinking != 0 {
		t.Error("combined should not include CapThinking")
	}
}

func TestFIMConfig(t *testing.T) {
	cfg := FIMConfig{
		Prefix:          "<|fim_prefix|>",
		Suffix:          "<|fim_suffix|>",
		Middle:          "<|fim_middle|>",
		PrefixBudgetPct: 75,
	}
	if cfg.PrefixBudgetPct != 75 {
		t.Errorf("PrefixBudgetPct = %d, want 75", cfg.PrefixBudgetPct)
	}
	if cfg.StopTokens != nil {
		t.Errorf("StopTokens = %v, want nil", cfg.StopTokens)
	}
}

func TestFIMConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     FIMConfig
		wantErr bool
	}{
		{
			name: "valid config",
			cfg: FIMConfig{
				Prefix:          "<|fim_prefix|>",
				Suffix:          "<|fim_suffix|>",
				Middle:          "<|fim_middle|>",
				StopTokens:      []string{"<|endoftext|>"},
				PrefixBudgetPct: 75,
			},
			wantErr: false,
		},
		{
			name: "valid without stop tokens",
			cfg: FIMConfig{
				Prefix: "<|fim_prefix|>",
				Suffix: "<|fim_suffix|>",
				Middle: "<|fim_middle|>",
			},
			wantErr: false,
		},
		{
			name: "valid with zero budget uses default",
			cfg: FIMConfig{
				Prefix:          "<|fim_prefix|>",
				Suffix:          "<|fim_suffix|>",
				Middle:          "<|fim_middle|>",
				PrefixBudgetPct: 0,
			},
			wantErr: false,
		},
		{
			name:    "legacy token fields ignored",
			cfg:     FIMConfig{Prefix: "", Suffix: "", Middle: ""},
			wantErr: false,
		},
		{
			name: "empty stop token entry",
			cfg: FIMConfig{
				Prefix:     "<|fim_prefix|>",
				Suffix:     "<|fim_suffix|>",
				Middle:     "<|fim_middle|>",
				StopTokens: []string{"<|endoftext|>", ""},
			},
			wantErr: true,
		},
		{
			name: "budget pct too low",
			cfg: FIMConfig{
				Prefix:          "<|fim_prefix|>",
				Suffix:          "<|fim_suffix|>",
				Middle:          "<|fim_middle|>",
				PrefixBudgetPct: -1,
			},
			wantErr: true,
		},
		{
			name: "budget pct too high",
			cfg: FIMConfig{
				Prefix:          "<|fim_prefix|>",
				Suffix:          "<|fim_suffix|>",
				Middle:          "<|fim_middle|>",
				PrefixBudgetPct: 100,
			},
			wantErr: true,
		},
		{
			name: "stop tokens preserve whitespace",
			cfg: FIMConfig{
				Prefix:     "<PRE>",
				Suffix:     " <SUF>",
				Middle:     " <MID>",
				StopTokens: []string{" <EOT>"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCompletionConfidence(t *testing.T) {
	cc := CompletionConfidence{
		Score:      0.95,
		AvgLogProb: -0.5,
		MinLogProb: -2.1,
		DropOffIdx: 42,
	}
	if cc.Score != 0.95 {
		t.Errorf("Score = %v, want 0.95", cc.Score)
	}
	if cc.DropOffIdx != 42 {
		t.Errorf("DropOffIdx = %d, want 42", cc.DropOffIdx)
	}
}

func TestRouteOutcomeOnChatResponse(t *testing.T) {
	outcome := &RouteOutcome{
		PlannedModel:  ModelKey{Provider: "ollama", Model: "qwen3:32b"},
		ActualModel:   ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		FallbacksUsed: 1,
		WasSticky:     false,
		Score:         0.85,
		Reason:        "primary model overloaded, fell back to smaller variant",
	}
	resp := ChatResponse{
		Model:        "qwen3:8b",
		Provider:     "ollama",
		Content:      "Hello!",
		Done:         true,
		RouteOutcome: outcome,
	}

	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome should not be nil")
	}
	if resp.RouteOutcome.PlannedModel.String() != "ollama/qwen3:32b" {
		t.Errorf("PlannedModel = %q, want %q", resp.RouteOutcome.PlannedModel.String(), "ollama/qwen3:32b")
	}
	if resp.RouteOutcome.ActualModel.String() != "ollama/qwen3:8b" {
		t.Errorf("ActualModel = %q, want %q", resp.RouteOutcome.ActualModel.String(), "ollama/qwen3:8b")
	}
	if resp.RouteOutcome.FallbacksUsed != 1 {
		t.Errorf("FallbacksUsed = %d, want 1", resp.RouteOutcome.FallbacksUsed)
	}
	if resp.RouteOutcome.WasSticky {
		t.Error("WasSticky should be false")
	}
	if resp.RouteOutcome.Score != 0.85 {
		t.Errorf("Score = %v, want 0.85", resp.RouteOutcome.Score)
	}
	if resp.RouteOutcome.Reason != "primary model overloaded, fell back to smaller variant" {
		t.Errorf("Reason = %q, want %q", resp.RouteOutcome.Reason, "primary model overloaded, fell back to smaller variant")
	}
}

func TestRouteOutcomeNilWhenDirectCall(t *testing.T) {
	// When a response comes from a direct provider call (no Router),
	// RouteOutcome should be nil on all response types.
	chat := ChatResponse{
		Model:    "qwen3:8b",
		Provider: "ollama",
		Content:  "Hello!",
		Done:     true,
	}
	if chat.RouteOutcome != nil {
		t.Error("ChatResponse.RouteOutcome should be nil for direct calls")
	}

	gen := GenerateResponse{
		Model:    "qwen3:8b",
		Provider: "ollama",
		Response: "generated text",
		Done:     true,
	}
	if gen.RouteOutcome != nil {
		t.Error("GenerateResponse.RouteOutcome should be nil for direct calls")
	}

	embed := EmbedResponse{
		Model:      "nomic-embed-text",
		Provider:   "ollama",
		Embeddings: [][]float64{{0.1, 0.2, 0.3}},
	}
	if embed.RouteOutcome != nil {
		t.Error("EmbedResponse.RouteOutcome should be nil for direct calls")
	}
}

func TestRouteOutcomeOnGenerateResponse(t *testing.T) {
	outcome := &RouteOutcome{
		PlannedModel:  ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		ActualModel:   ModelKey{Provider: "ollama", Model: "qwen3:8b"},
		FallbacksUsed: 0,
		WasSticky:     true,
		Score:         0.92,
		Reason:        "model was warm, sticky routing applied",
	}
	resp := GenerateResponse{
		Model:        "qwen3:8b",
		Provider:     "ollama",
		Response:     "func main() {}",
		Done:         true,
		RouteOutcome: outcome,
	}

	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome should not be nil")
	}
	if resp.RouteOutcome.WasSticky != true {
		t.Error("WasSticky should be true")
	}
	if resp.RouteOutcome.FallbacksUsed != 0 {
		t.Errorf("FallbacksUsed = %d, want 0", resp.RouteOutcome.FallbacksUsed)
	}
}

func TestRouteOutcomeOnEmbedResponse(t *testing.T) {
	outcome := &RouteOutcome{
		PlannedModel:  ModelKey{Provider: "ollama", Model: "nomic-embed-text"},
		ActualModel:   ModelKey{Provider: "ollama", Model: "nomic-embed-text"},
		FallbacksUsed: 0,
		WasSticky:     false,
		Score:         1.0,
		Reason:        "only embedding model available",
	}
	resp := EmbedResponse{
		Model:        "nomic-embed-text",
		Provider:     "ollama",
		Embeddings:   [][]float64{{0.1, 0.2}},
		RouteOutcome: outcome,
	}

	if resp.RouteOutcome == nil {
		t.Fatal("RouteOutcome should not be nil")
	}
	if resp.RouteOutcome.Score != 1.0 {
		t.Errorf("Score = %v, want 1.0", resp.RouteOutcome.Score)
	}
}
