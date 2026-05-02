package mcp

import (
	"context"

	"github.com/kstruzzieri/go-llm/provider"
)

// recordingRouteEngine is a routeEngine implementation that captures the last
// RoutingRequest it was handed and returns a deterministic plan/response.
// Used by handler tests to assert on RoutingRequest shape without standing
// up a real provider/registry stack.
type recordingRouteEngine struct {
	last         provider.RoutingRequest
	called       bool
	closeCalled  bool
	chatContent  string
	embedVectors [][]float64
	genResponse  string
	routeErr     error
}

func newRecordingRouteEngine(reply string) *recordingRouteEngine {
	return &recordingRouteEngine{chatContent: reply, genResponse: reply}
}

// fakeRouteProvider is a minimal Provider satisfying the methods that
// RoutePlan.Execute* call. Other Provider methods are no-ops.
type fakeRouteProvider struct {
	name         string
	chatContent  string
	embedVectors [][]float64
	genResponse  string
}

func (f *fakeRouteProvider) Name() string             { return f.name }
func (f *fakeRouteProvider) Capabilities() provider.Capability {
	return provider.CapChat | provider.CapGenerate | provider.CapEmbed | provider.CapStream
}
func (f *fakeRouteProvider) Health(ctx context.Context) error { return nil }
func (f *fakeRouteProvider) Models(ctx context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}
func (f *fakeRouteProvider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{
		Provider: f.name, Model: req.Model, Content: f.chatContent, Done: true,
	}, nil
}
func (f *fakeRouteProvider) ChatStream(ctx context.Context, req provider.ChatRequest, fn func(provider.ChatResponse) error) error {
	return fn(provider.ChatResponse{Provider: f.name, Model: req.Model, Content: f.chatContent, Done: true})
}
func (f *fakeRouteProvider) Generate(ctx context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
	return &provider.GenerateResponse{
		Provider: f.name, Model: req.Model, Response: f.genResponse, Done: true,
	}, nil
}
func (f *fakeRouteProvider) GenerateStream(ctx context.Context, req provider.GenerateRequest, fn func(provider.GenerateResponse) error) error {
	return fn(provider.GenerateResponse{Provider: f.name, Model: req.Model, Response: f.genResponse, Done: true})
}
func (f *fakeRouteProvider) Embed(ctx context.Context, req provider.EmbedRequest) (*provider.EmbedResponse, error) {
	vectors := f.embedVectors
	if vectors == nil {
		vectors = make([][]float64, len(req.Input))
		for i := range vectors {
			vectors[i] = []float64{1, 2, 3}
		}
	}
	return &provider.EmbedResponse{Provider: f.name, Model: req.Model, Embeddings: vectors}, nil
}

func (e *recordingRouteEngine) Route(ctx context.Context, req provider.RoutingRequest) (*provider.RoutePlan, error) {
	e.last = req
	e.called = true
	if e.routeErr != nil {
		return nil, e.routeErr
	}
	model := req.Model
	if model == "" && len(req.PreferredChain) > 0 {
		model = req.PreferredChain[0]
	}
	prov := &fakeRouteProvider{
		name:         "fake",
		chatContent:  e.chatContent,
		embedVectors: e.embedVectors,
		genResponse:  e.genResponse,
	}
	plan := &provider.RoutePlan{
		Provider: prov,
		Model:    model,
		Profile: &provider.ModelProfile{
			Key:           provider.ModelKey{Provider: "fake", Model: model},
			Caps:          provider.CapChat | provider.CapGenerate | provider.CapEmbed | provider.CapStream,
			ContextWindow: 8192,
		},
		Request: req,
		Budget:  provider.BudgetResult{Decision: provider.BudgetOK},
	}
	return plan, nil
}

func (e *recordingRouteEngine) Close() error {
	e.closeCalled = true
	return nil
}

func (e *recordingRouteEngine) BreakerInfo(string) (provider.BreakerInfo, bool) {
	return provider.BreakerInfo{}, false
}

func (e *recordingRouteEngine) WarmthSnapshot() []provider.WarmModel { return nil }

func (e *recordingRouteEngine) StickyRoutes() map[string]provider.StickyRouteInfo { return nil }
