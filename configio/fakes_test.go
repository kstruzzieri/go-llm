package configio

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// ---------------------------------------------------------------------------
// Pure seam fakes (unit tests)
// ---------------------------------------------------------------------------

// fakeLister implements ProviderLister over an explicit name order.
type fakeLister struct {
	names     []string
	providers map[string]provider.Provider
}

func (f *fakeLister) Names() []string { return append([]string(nil), f.names...) }
func (f *fakeLister) Get(name string) (provider.Provider, bool) {
	p, ok := f.providers[name]
	return p, ok
}

// fakeProvider implements provider.Provider with scripted Models results.
// onModels, when non-nil, runs before returning (cancel contexts).
type fakeProvider struct {
	name      string
	models    []provider.ModelInfo
	modelsErr error
	onModels  func()
}

func (p *fakeProvider) Name() string                      { return p.name }
func (p *fakeProvider) Capabilities() provider.Capability { return 0 }
func (p *fakeProvider) Health(context.Context) error      { return nil }
func (p *fakeProvider) Models(context.Context) ([]provider.ModelInfo, error) {
	if p.onModels != nil {
		p.onModels()
	}
	if p.modelsErr != nil {
		return nil, p.modelsErr
	}
	return append([]provider.ModelInfo(nil), p.models...), nil
}
func (p *fakeProvider) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeProvider) ChatStream(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) error {
	return errors.New("not implemented")
}
func (p *fakeProvider) Generate(context.Context, provider.GenerateRequest) (*provider.GenerateResponse, error) {
	return nil, errors.New("not implemented")
}
func (p *fakeProvider) GenerateStream(context.Context, provider.GenerateRequest, func(provider.GenerateResponse) error) error {
	return errors.New("not implemented")
}
func (p *fakeProvider) Embed(context.Context, provider.EmbedRequest) (*provider.EmbedResponse, error) {
	return nil, errors.New("not implemented")
}

// fakeProjector implements ListedProjector with a scripted transform,
// recording the infos it was handed. err is reserved for cancellation
// scripts — the real projection is total except cancellation.
type fakeProjector struct {
	mu     sync.Mutex
	seen   map[string][]provider.ModelInfo
	err    error
	onCall func(ctx context.Context)
}

func newFakeProjector() *fakeProjector {
	return &fakeProjector{seen: map[string][]provider.ModelInfo{}}
}

func (f *fakeProjector) ProjectListedModels(ctx context.Context, providerName string, infos []provider.ModelInfo) ([]provider.ListedModelFacts, error) {
	f.mu.Lock()
	f.seen[providerName] = append([]provider.ModelInfo(nil), infos...)
	onCall := f.onCall
	errOut := f.err
	f.mu.Unlock()
	if onCall != nil {
		onCall(ctx)
	}
	if errOut != nil {
		return nil, errOut
	}
	out := make([]provider.ListedModelFacts, len(infos))
	for i, info := range infos {
		out[i] = provider.ListedModelFacts{
			Key:           provider.ModelKey{Provider: providerName, Model: info.Name},
			Family:        info.Family,
			Caps:          provider.CapChat,
			KnownMask:     provider.CapChat,
			ContextWindow: info.ContextWindow,
		}
	}
	return out, nil
}

func (f *fakeProjector) seenFor(name string) []provider.ModelInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[name]
}

// fakeResolver implements ToolCallResolver with scripted outcomes.
type fakeResolver struct {
	state  fingerprint.CapProbeState
	err    error
	exp    provider.ToolCallExplanation
	expErr error
	// calls is unsynchronized: single-goroutine use only (all probe unit
	// tests drive the resolver from the test goroutine).
	calls  int
	onCall func(ctx context.Context)
}

func (f *fakeResolver) ResolveToolCall(ctx context.Context, _ provider.ModelKey) (fingerprint.CapProbeState, error) {
	f.calls++
	if f.onCall != nil {
		f.onCall(ctx)
	}
	if f.err != nil {
		return "", f.err
	}
	return f.state, nil
}

func (f *fakeResolver) ExplainToolCall(context.Context, provider.ModelKey) (provider.ToolCallExplanation, error) {
	if f.expErr != nil {
		return provider.ToolCallExplanation{}, f.expErr
	}
	return f.exp, nil
}

// ---------------------------------------------------------------------------
// Real-registry construction (integration pins)
// ---------------------------------------------------------------------------

// memCapProbeStore is an in-memory fingerprint.CapProbeStore. saveErr,
// when set, makes every SaveCapProbe fail (persistence-outcome tests).
type memCapProbeStore struct {
	mu      sync.Mutex
	rows    map[string]fingerprint.CapProbe
	saveErr error
}

func newMemCapProbeStore() *memCapProbeStore {
	return &memCapProbeStore{rows: make(map[string]fingerprint.CapProbe)}
}

func storeKey(backendID, modelName, capability string) string {
	return backendID + "\x00" + modelName + "\x00" + capability
}

func (s *memCapProbeStore) GetCapProbe(_ context.Context, backendID, modelName, capability string) (*fingerprint.CapProbe, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rows[storeKey(backendID, modelName, capability)]; ok {
		cp := row
		return &cp, nil
	}
	return nil, fingerprint.ErrNotFound
}

func (s *memCapProbeStore) SaveCapProbe(_ context.Context, probe fingerprint.CapProbe) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.rows[storeKey(probe.BackendID, probe.ModelName, probe.Capability)] = probe
	return nil
}

func (s *memCapProbeStore) DeleteCapProbes(_ context.Context, backendID, modelName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := backendID + "\x00" + modelName + "\x00"
	for k := range s.rows {
		if strings.HasPrefix(k, prefix) {
			delete(s.rows, k)
		}
	}
	return nil
}

func (s *memCapProbeStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// countingProber implements fingerprint.ModelProber + ToolCallProber with
// scripted per-model outcomes, a total call counter, an optional block
// channel (probe waits until closed or ctx dies), and a started channel
// closed on first entry (deterministic cancellation sequencing).
type countingProber struct {
	mu        sync.Mutex
	outcomes  map[string]fingerprint.CapProbeOutcome
	errs      map[string]error
	total     int
	block     chan struct{}
	started   chan struct{}
	startOnce sync.Once
}

func newCountingProber() *countingProber {
	return &countingProber{
		outcomes: make(map[string]fingerprint.CapProbeOutcome),
		errs:     make(map[string]error),
		started:  make(chan struct{}),
	}
}

func (p *countingProber) DetectKind(context.Context, string) (*fingerprint.KindDetection, error) {
	return nil, errors.New("not implemented")
}
func (p *countingProber) ProbeChat(context.Context, string, interface{}) (*fingerprint.ChatMetrics, error) {
	return nil, errors.New("not implemented")
}
func (p *countingProber) ProbeEmbedding(context.Context, string) (*fingerprint.EmbeddingMetrics, error) {
	return nil, errors.New("not implemented")
}
func (p *countingProber) ProbeToolCall(ctx context.Context, model string) (fingerprint.CapProbeOutcome, error) {
	p.mu.Lock()
	p.total++
	block := p.block
	p.mu.Unlock()
	p.startOnce.Do(func() { close(p.started) })
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return fingerprint.CapProbeOutcome{}, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.errs[model]; err != nil {
		return fingerprint.CapProbeOutcome{}, err
	}
	return p.outcomes[model], nil
}

func (p *countingProber) totalCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
}

// newRealRegistry builds a real provider.Registry + ModelRegistry over the
// given fake providers, optionally wiring the cap-probe store and prober —
// mirroring provider/capresolve_test.go's newCapResolveRegistry from
// outside the provider package.
func newRealRegistry(t *testing.T, providers []*fakeProvider, store fingerprint.CapProbeStore, prober fingerprint.ModelProber) (*provider.Registry, *provider.ModelRegistry) {
	t.Helper()
	reg := provider.NewRegistry()
	for _, p := range providers {
		if err := reg.Register(p); err != nil {
			t.Fatalf("Register(%q) error: %v", p.name, err)
		}
	}
	opts := []provider.ModelRegistryOption{}
	if store != nil {
		opts = append(opts, provider.WithCapabilityProbeStore(store))
	}
	if prober != nil {
		opts = append(opts, provider.WithCapabilityProber(
			func(_ context.Context, _ provider.ModelKey, _ *provider.ModelInfo, _ provider.Provider) (*provider.FingerprintProberSpec, error) {
				return &provider.FingerprintProberSpec{Prober: prober}, nil
			}))
	}
	mr, err := provider.NewModelRegistry(reg, nil, opts...)
	if err != nil {
		t.Fatalf("NewModelRegistry() error: %v", err)
	}
	return reg, mr
}
