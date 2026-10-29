package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// slowProvider delays every call so the attempt has a nonzero latency, which
// RoutingFeedback records as a Latency signal for any attempt it attributes.
type slowProvider struct{ *rpMockProvider }

func (s slowProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	time.Sleep(2 * time.Millisecond)
	return s.rpMockProvider.Chat(ctx, req)
}

func (s slowProvider) ChatStream(ctx context.Context, req ChatRequest, fn func(ChatResponse) error) error {
	time.Sleep(2 * time.Millisecond)
	return s.rpMockProvider.ChatStream(ctx, req, fn)
}

func (s slowProvider) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	time.Sleep(2 * time.Millisecond)
	return s.rpMockProvider.Generate(ctx, req)
}

func (s slowProvider) GenerateStream(ctx context.Context, req GenerateRequest, fn func(GenerateResponse) error) error {
	time.Sleep(2 * time.Millisecond)
	return s.rpMockProvider.GenerateStream(ctx, req, fn)
}

func (s slowProvider) Embed(ctx context.Context, req EmbedRequest) (*EmbedResponse, error) {
	time.Sleep(2 * time.Millisecond)
	return s.rpMockProvider.Embed(ctx, req)
}

// TestContentDenialNoFallback (#424): a primary that refuses a request body for
// carrying a configured credential ends the walk. The fallback is never called
// (a fallback would be sent the same body) and no breaker failure is recorded.
// The model never received the request, so the attempt is Unknown: routing
// feedback records no failure, latency or success signal against it. Every
// execute path walks its own fallback loop, so each is a row.
func TestContentDenialNoFallback(t *testing.T) {
	denied := fmt.Errorf("openaicompat: /v1/chat/completions: %w", ErrContentDenied)
	if got := makeAttempt(ModelKey{Provider: "p", Model: "m"}, denied, time.Second); got.Status != AttemptStatusUnknown || got.ErrorClass != "" {
		t.Errorf("denial attempt = %+v, want AttemptStatusUnknown with no class", got)
	}

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
			plan.Provider = slowProvider{primary}
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
			if got := store.Signals(primaryKey); len(got) != 0 {
				t.Errorf("primary signals = %+v, want none (an Unknown attempt)", got)
			}
			fallbackKey := FeedbackKey{Provider: "ollama-b", Model: "qwen3:8b", UseCase: "chat"}
			if got := store.Signals(fallbackKey); len(got) != 0 {
				t.Errorf("fallback signals = %+v, want none", got)
			}
		})
	}
}
