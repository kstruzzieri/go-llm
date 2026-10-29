package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestContentDenialNoFallback (#424): a primary that refuses a request body for
// carrying a configured credential ends the walk. The fallback is never called
// (a fallback would be sent the same body), no breaker failure is recorded, and
// the attempt is attributed with the bounded class "unknown" rather than a
// transport class. Every execute path walks its own fallback loop, so each is a
// row.
func TestContentDenialNoFallback(t *testing.T) {
	denied := fmt.Errorf("openaicompat: /v1/chat/completions: %w", ErrContentDenied)

	tests := []struct {
		name string
		// arm makes the primary refuse and returns the call under test.
		arm func(primary *rpMockProvider) func(*RoutePlan) error
		// fallbackCalls reads the call counter of the path under test.
		fallbackCalls func(*rpMockProvider) int
	}{
		{
			name: "chat",
			arm: func(p *rpMockProvider) func(*RoutePlan) error {
				p.chatErr = denied
				return func(rp *RoutePlan) error { _, err := rp.ExecuteChat(context.Background()); return err }
			},
			fallbackCalls: (*rpMockProvider).getChatCalls,
		},
		{
			name: "chat stream",
			arm: func(p *rpMockProvider) func(*RoutePlan) error {
				p.chatStreamErr = denied
				return func(rp *RoutePlan) error {
					return rp.ExecuteChatStream(context.Background(), func(ChatResponse) error { return nil })
				}
			},
			fallbackCalls: (*rpMockProvider).getChatCalls,
		},
		{
			name: "generate",
			arm: func(p *rpMockProvider) func(*RoutePlan) error {
				p.genErr = denied
				return func(rp *RoutePlan) error { _, err := rp.ExecuteGenerate(context.Background()); return err }
			},
			fallbackCalls: (*rpMockProvider).getGenCalls,
		},
		{
			name: "generate stream",
			arm: func(p *rpMockProvider) func(*RoutePlan) error {
				p.genStreamErr = denied
				return func(rp *RoutePlan) error {
					return rp.ExecuteGenerateStream(context.Background(), func(GenerateResponse) error { return nil })
				}
			},
			fallbackCalls: (*rpMockProvider).getGenCalls,
		},
		{
			name: "embed",
			arm: func(p *rpMockProvider) func(*RoutePlan) error {
				p.embedErr = denied
				return func(rp *RoutePlan) error { _, err := rp.ExecuteEmbed(context.Background()); return err }
			},
			fallbackCalls: (*rpMockProvider).getEmbedCalls,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, err := NewMemoryStore(MemoryStoreConfig{})
			if err != nil {
				t.Fatalf("NewMemoryStore: %v", err)
			}
			primary := &rpMockProvider{name: "ollama-a", caps: CapChat}
			// The fallback would succeed on every path, so a walk that reached it
			// would show as a call, not as an error.
			fallback := &rpMockProvider{
				name:             "ollama-b",
				caps:             CapChat,
				chatResp:         &ChatResponse{Content: "ok", Done: true},
				chatStreamChunks: []ChatResponse{{Content: "ok", Done: true}},
				genResp:          &GenerateResponse{Response: "ok", Done: true},
				genStreamChunks:  []GenerateResponse{{Response: "ok", Done: true}},
				embedResp:        &EmbedResponse{Embeddings: [][]float64{{1}}},
			}
			rec := &rpMockRecorder{}
			plan := newTestPlan(primary, rec)
			plan.Request.Prompt = "p"
			plan.Request.Input = []string{"i"}
			plan.SetFeedback(NewRoutingFeedback(store))
			plan.Fallbacks = []RoutePlan{{
				Profile:  &ModelProfile{Key: ModelKey{Provider: "ollama-b", Model: "qwen3:8b"}},
				Provider: fallback,
				Model:    "qwen3:8b",
			}}
			run := tc.arm(primary)

			err = run(plan)

			if !errors.Is(err, ErrContentDenied) {
				t.Fatalf("err = %v, want errors.Is ErrContentDenied", err)
			}
			if n := tc.fallbackCalls(fallback); n != 0 {
				t.Errorf("fallback calls = %d, want 0", n)
			}
			if got := rec.getFailures(); len(got) != 0 {
				t.Errorf("breaker failures = %v, want none", got)
			}
			primaryKey := FeedbackKey{Provider: "ollama-a", Model: "test-model", UseCase: "chat"}
			sigs := store.Signals(primaryKey)
			var failures int
			for _, sig := range sigs {
				if sig.Kind != RoutingSignalFailure {
					continue
				}
				failures++
				if sig.ErrorClass != "unknown" {
					t.Errorf("attempt class = %q, want %q", sig.ErrorClass, "unknown")
				}
			}
			if failures != 1 {
				t.Errorf("failure attributions on the primary = %d, want 1 (signals %+v)", failures, sigs)
			}
			fallbackKey := FeedbackKey{Provider: "ollama-b", Model: "qwen3:8b", UseCase: "chat"}
			if got := store.Signals(fallbackKey); len(got) != 0 {
				t.Errorf("fallback signals = %+v, want none", got)
			}
		})
	}
}
