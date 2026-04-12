package provider

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// BudgetDecision.String
// ---------------------------------------------------------------------------

func TestBudgetDecisionString(t *testing.T) {
	tests := []struct {
		d    BudgetDecision
		want string
	}{
		{BudgetOK, "ok"},
		{BudgetTruncate, "truncate"},
		{BudgetReject, "reject"},
		{BudgetDecision(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.d.String(); got != tt.want {
			t.Errorf("BudgetDecision(%d).String() = %q, want %q", tt.d, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// TokenBudgetValidator tests
// ---------------------------------------------------------------------------

func TestTokenBudgetValidateOK(t *testing.T) {
	v := NewTokenBudgetValidator()
	req := RoutingRequest{
		UseCase: "chat",
		Prompt:  "Hello, how are you?", // ~5 tokens
	}
	profile := &ModelProfile{
		ContextWindow: 32768,
	}

	result := v.Validate(req, profile)

	if result.Decision != BudgetOK {
		t.Errorf("expected BudgetOK, got %v (reason: %s)", result.Decision, result.Reason)
	}
	if result.HeadroomScore < 0.9 {
		t.Errorf("expected HeadroomScore > 0.9 for short prompt in 32K context, got %f", result.HeadroomScore)
	}
	if result.BudgetTokens != 32768-2048 {
		t.Errorf("expected BudgetTokens = %d, got %d", 32768-2048, result.BudgetTokens)
	}
}

func TestTokenBudgetValidateReject(t *testing.T) {
	v := NewTokenBudgetValidator()
	// 8000 chars ≈ 2000 tokens (len/4 estimation)
	largePrompt := strings.Repeat("x", 8000)
	req := RoutingRequest{
		UseCase: "chat",
		Prompt:  largePrompt,
	}
	profile := &ModelProfile{
		ContextWindow: 1024,
	}

	result := v.Validate(req, profile)

	if result.Decision != BudgetReject {
		t.Errorf("expected BudgetReject, got %v (reason: %s)", result.Decision, result.Reason)
	}
}

func TestTokenBudgetValidateTruncate(t *testing.T) {
	v := NewTokenBudgetValidator()
	// 9000 chars ≈ 2250 tokens (len/4)
	// chat default output = 2048, so budget = 4096 - 2048 = 2048
	// 2250 > 2048 (budget) but 2250 < 2048*1.5=3072 → BudgetTruncate
	mediumPrompt := strings.Repeat("x", 9000)
	req := RoutingRequest{
		UseCase: "chat",
		Prompt:  mediumPrompt,
	}
	profile := &ModelProfile{
		ContextWindow: 4096,
	}

	result := v.Validate(req, profile)

	if result.Decision != BudgetTruncate {
		t.Errorf("expected BudgetTruncate, got %v (reason: %s)", result.Decision, result.Reason)
	}
}

func TestTokenBudgetPerUseCaseOutput(t *testing.T) {
	v := NewTokenBudgetValidator()
	contextWindow := 32768

	tests := []struct {
		useCase       string
		expectedBudget int
	}{
		{"fim", contextWindow - 200},
		{"chat", contextWindow - 2048},
		{"embedding", contextWindow - 0},
		{"code-review", contextWindow - 4096},
		{"reasoning", contextWindow - 4096},
	}

	for _, tt := range tests {
		t.Run(tt.useCase, func(t *testing.T) {
			req := RoutingRequest{
				UseCase: tt.useCase,
				Prompt:  "test",
			}
			profile := &ModelProfile{
				ContextWindow: contextWindow,
			}
			result := v.Validate(req, profile)
			if result.BudgetTokens != tt.expectedBudget {
				t.Errorf("useCase=%q: BudgetTokens = %d, want %d", tt.useCase, result.BudgetTokens, tt.expectedBudget)
			}
		})
	}
}

func TestTokenBudgetExplicitExpectedOutput(t *testing.T) {
	v := NewTokenBudgetValidator()
	req := RoutingRequest{
		UseCase:        "chat",
		Prompt:         "hello",
		ExpectedOutput: 8192,
	}
	profile := &ModelProfile{
		ContextWindow: 32768,
	}

	result := v.Validate(req, profile)

	expectedBudget := 32768 - 8192
	if result.BudgetTokens != expectedBudget {
		t.Errorf("expected BudgetTokens = %d, got %d", expectedBudget, result.BudgetTokens)
	}
	if result.Decision != BudgetOK {
		t.Errorf("expected BudgetOK, got %v", result.Decision)
	}
}

func TestTokenBudgetQualityCtxCeiling(t *testing.T) {
	v := NewTokenBudgetValidator()
	profile := &ModelProfile{
		ContextWindow:     131072,
		QualityCtxCeiling: 32768,
	}

	// Chat is quality-sensitive → uses ceiling (32768)
	chatReq := RoutingRequest{
		UseCase: "chat",
		Prompt:  "hello",
	}
	chatResult := v.Validate(chatReq, profile)
	expectedChatBudget := 32768 - 2048
	if chatResult.BudgetTokens != expectedChatBudget {
		t.Errorf("chat: BudgetTokens = %d, want %d", chatResult.BudgetTokens, expectedChatBudget)
	}

	// FIM is not quality-sensitive → uses full window (131072)
	fimReq := RoutingRequest{
		UseCase: "fim",
		Prompt:  "hello",
	}
	fimResult := v.Validate(fimReq, profile)
	expectedFIMBudget := 131072 - 200
	if fimResult.BudgetTokens != expectedFIMBudget {
		t.Errorf("fim: BudgetTokens = %d, want %d", fimResult.BudgetTokens, expectedFIMBudget)
	}
}

func TestTokenBudgetHeadroomScore(t *testing.T) {
	tests := []struct {
		name    string
		util    float64 // target utilization
		wantMin float64
		wantMax float64
	}{
		{"10pct", 0.10, 0.99, 1.0},
		{"40pct", 0.40, 0.99, 1.0},
		{"75pct", 0.75, 0.40, 0.60},
		{"95pct", 0.95, 0.0, 0.15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Build a scenario that produces the desired utilization.
			// budget = contextWindow - expectedOutput
			// We use embedding (expectedOutput=0) so budget = contextWindow.
			// utilization = inputTokens / budget
			// inputTokens = estimateTokens(prompt) = len(prompt)/4
			// We want: len(prompt)/4 / contextWindow = tt.util
			// So: len(prompt) = tt.util * contextWindow * 4
			contextWindow := 10000
			promptLen := int(tt.util * float64(contextWindow) * 4)
			if promptLen < 1 {
				promptLen = 1
			}
			prompt := strings.Repeat("x", promptLen)

			v := NewTokenBudgetValidator()
			req := RoutingRequest{
				UseCase: "embedding",
				Prompt:  prompt,
			}
			profile := &ModelProfile{
				ContextWindow: contextWindow,
			}

			result := v.Validate(req, profile)

			if result.HeadroomScore < tt.wantMin || result.HeadroomScore > tt.wantMax {
				t.Errorf("util=%.2f: HeadroomScore = %f, want [%f, %f]",
					tt.util, result.HeadroomScore, tt.wantMin, tt.wantMax)
			}
		})
	}
}

func TestTokenBudgetOutputExceedsContext(t *testing.T) {
	v := NewTokenBudgetValidator()
	req := RoutingRequest{
		UseCase:        "chat",
		Prompt:         "hello",
		ExpectedOutput: 50000, // bigger than context window
	}
	profile := &ModelProfile{
		ContextWindow: 4096,
	}

	result := v.Validate(req, profile)

	if result.Decision != BudgetReject {
		t.Errorf("expected BudgetReject when output exceeds context, got %v", result.Decision)
	}
	if result.BudgetTokens != 0 {
		t.Errorf("expected BudgetTokens = 0, got %d", result.BudgetTokens)
	}
}

func TestTokenBudgetWithOutputDefault(t *testing.T) {
	v := NewTokenBudgetValidator(WithOutputDefault("custom-task", 512))
	req := RoutingRequest{
		UseCase: "custom-task",
		Prompt:  "hello",
	}
	profile := &ModelProfile{
		ContextWindow: 32768,
	}

	result := v.Validate(req, profile)

	expectedBudget := 32768 - 512
	if result.BudgetTokens != expectedBudget {
		t.Errorf("expected BudgetTokens = %d, got %d", expectedBudget, result.BudgetTokens)
	}
}

func TestTokenBudgetZeroContextWindow(t *testing.T) {
	v := NewTokenBudgetValidator()
	profile := &ModelProfile{ContextWindow: 0}
	req := RoutingRequest{UseCase: "chat", Prompt: "hello"}

	result := v.Validate(req, profile)
	if result.Decision != BudgetReject {
		t.Errorf("Decision = %v, want BudgetReject for zero context window", result.Decision)
	}
	if result.Reason != "model has no context window configured" {
		t.Errorf("Reason = %q, want 'model has no context window configured'", result.Reason)
	}
}
