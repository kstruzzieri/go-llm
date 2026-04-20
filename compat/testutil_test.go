package compat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// mockProvider is a scriptable provider.Provider for handler tests.
type mockProvider struct {
	name string
	caps provider.Capability

	models []provider.ModelInfo
	health error

	chatFn       func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error)
	chatStreamFn func(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) error
	genFn        func(context.Context, provider.GenerateRequest) (*provider.GenerateResponse, error)
	genStreamFn  func(context.Context, provider.GenerateRequest, func(provider.GenerateResponse) error) error
	embedFn      func(context.Context, provider.EmbedRequest) (*provider.EmbedResponse, error)
}

func (m *mockProvider) Name() string                      { return m.name }
func (m *mockProvider) Capabilities() provider.Capability { return m.caps }
func (m *mockProvider) Health(ctx context.Context) error  { return m.health }
func (m *mockProvider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	return m.models, nil
}
func (m *mockProvider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	if m.chatFn == nil {
		return nil, errors.New("mock: chat not scripted")
	}
	return m.chatFn(ctx, req)
}
func (m *mockProvider) ChatStream(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
	if m.chatStreamFn == nil {
		return errors.New("mock: chat-stream not scripted")
	}
	return m.chatStreamFn(ctx, req, fn)
}
func (m *mockProvider) Generate(ctx context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
	if m.genFn == nil {
		return nil, errors.New("mock: generate not scripted")
	}
	return m.genFn(ctx, req)
}
func (m *mockProvider) GenerateStream(ctx context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
	if m.genStreamFn == nil {
		return errors.New("mock: generate-stream not scripted")
	}
	return m.genStreamFn(ctx, req, fn)
}
func (m *mockProvider) Embed(ctx context.Context, req provider.EmbedRequest) (*provider.EmbedResponse, error) {
	if m.embedFn == nil {
		return nil, errors.New("mock: embed not scripted")
	}
	return m.embedFn(ctx, req)
}

// newTestServer builds a Server wired to a real Router backed by the given
// scripted mock provider. The returned teardown closes the Router.
func newTestServer(t *testing.T, mp *mockProvider, opts ...Option) (*Server, func()) {
	t.Helper()
	provReg := provider.NewRegistry()
	if err := provReg.Register(mp); err != nil {
		t.Fatalf("provider registry register: %v", err)
	}
	if err := provReg.RefreshModels(context.Background(), mp.Name()); err != nil {
		t.Fatalf("provider registry refresh models: %v", err)
	}
	modelReg, err := provider.NewModelRegistry(provReg, nil)
	if err != nil {
		t.Fatalf("model registry: %v", err)
	}
	router := provider.NewRouter(modelReg, provReg,
		provider.WithStickyTTL(time.Second),
		provider.WithAvailableRAM(256),
	)
	srv := New(router, modelReg, provReg, opts...)
	return srv, func() {
		_ = router.Close()
	}
}
