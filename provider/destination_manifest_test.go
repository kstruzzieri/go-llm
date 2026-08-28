package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// mustDest builds a destination or fails the test; test-input plumbing only.
func mustDest(t *testing.T, provider, raw string) Destination {
	t.Helper()
	d, err := NewDestination(provider, raw)
	if err != nil {
		t.Fatalf("NewDestination(%q, %q): %v", provider, raw, err)
	}
	return d
}

// ---------------------------------------------------------------------------
// DestinationManifest
// ---------------------------------------------------------------------------

func TestNewDestinationManifestValidates(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")

	t.Run("empty purpose rejected", func(t *testing.T) {
		_, err := NewDestinationManifest(DestinationEdge{Purpose: "", Destination: remote})
		if err == nil {
			t.Fatal("manifest accepted an empty purpose")
		}
	})

	t.Run("zero destination rejected", func(t *testing.T) {
		_, err := NewDestinationManifest(DestinationEdge{Purpose: "agent"})
		if err == nil {
			t.Fatal("manifest accepted a zero destination")
		}
	})

	// A provider key maps to exactly one base URL in an effective config, and
	// the transport is bound per provider. Two destinations under one provider
	// key would make edge lookup by (purpose, provider) ambiguous, so it is a
	// construction error, never a silent pick.
	t.Run("one provider with two destinations rejected", func(t *testing.T) {
		a := mustDest(t, "opencode", "https://opencode.ai/zen/go")
		b := mustDest(t, "opencode", "https://opencode.ai/zen/other")
		_, err := NewDestinationManifest(
			DestinationEdge{Purpose: "agent", Destination: a},
			DestinationEdge{Purpose: "summarize", Destination: b},
		)
		if err == nil {
			t.Fatal("manifest accepted two destinations for one provider")
		}
	})

	t.Run("exact duplicate edges collapse", func(t *testing.T) {
		m, err := NewDestinationManifest(
			DestinationEdge{Purpose: "agent", Destination: remote},
			DestinationEdge{Purpose: "agent", Destination: remote},
		)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(m.Edges()); got != 1 {
			t.Errorf("Edges() = %d entries, want 1", got)
		}
	})
}

// I4: one destination reachable from multiple purposes prompts once (deduped
// Destinations) while every per-purpose edge stays visible (Edges).
func TestDestinationManifestDedupesDestinationsRetainsEdges(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	local := mustDest(t, "llamacpp", "http://127.0.0.1:8090")

	m, err := NewDestinationManifest(
		DestinationEdge{Purpose: "agent", Destination: local},
		DestinationEdge{Purpose: "agent", Destination: remote, IsFallback: true},
		DestinationEdge{Purpose: "summarize", Destination: remote},
		DestinationEdge{Purpose: "embedding", Destination: local},
	)
	if err != nil {
		t.Fatal(err)
	}

	if got := len(m.Edges()); got != 4 {
		t.Errorf("Edges() = %d entries, want all 4 retained", got)
	}
	dests := m.Destinations()
	if len(dests) != 2 {
		t.Fatalf("Destinations() = %d entries, want 2 deduped", len(dests))
	}
	// Deterministic order: the same manifest must always render the same
	// prompt, or the user is asked to compare consent surfaces that shuffle.
	again := m.Destinations()
	for i := range dests {
		if dests[i] != again[i] {
			t.Fatal("Destinations() order is not deterministic")
		}
	}
	fallbackSeen := false
	for _, e := range m.Edges() {
		if e.IsFallback {
			fallbackSeen = true
		}
	}
	if !fallbackSeen {
		t.Error("IsFallback marker lost; the manifest cannot label fallback reachability")
	}
}

// ---------------------------------------------------------------------------
// DestinationGate — install / deny-all / clear
// ---------------------------------------------------------------------------

func TestDestinationGateDeniesEverythingBeforeInstall(t *testing.T) {
	g := NewDestinationGate()
	_, err := g.Bind(context.Background(), "agent", "opencode")
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("Bind before install = %v, want ErrDestinationDenied", err)
	}
}

// I6: the install-time denial names the destination and the purpose that
// reached it — that pair is what the user needs to fix their invocation.
func TestDestinationGateInstallRejectsUngrantedRemoteEdge(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	m, err := NewDestinationManifest(
		DestinationEdge{Purpose: "summarize", Destination: remote},
	)
	if err != nil {
		t.Fatal(err)
	}

	g := NewDestinationGate()
	var zero DestinationPolicy
	err = g.Install(zero, m)
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("Install with ungranted remote = %v, want ErrDestinationDenied", err)
	}
	for _, want := range []string{"opencode", "https://opencode.ai/zen/go", "summarize"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("denial missing %q: %s", want, err)
		}
	}
	// A failed install must leave the gate deny-all, not half-open.
	if _, bindErr := g.Bind(context.Background(), "summarize", "opencode"); !errors.Is(bindErr, ErrDestinationDenied) {
		t.Error("gate not deny-all after failed install")
	}
}

// I12/I20: an all-loopback manifest installs under the zero policy with no
// grants at all — local-only workflows continue prompt-free.
func TestDestinationGateLoopbackInstallsUnderZeroPolicy(t *testing.T) {
	local := mustDest(t, "llamacpp", "http://127.0.0.1:8090")
	m, err := NewDestinationManifest(
		DestinationEdge{Purpose: "agent", Destination: local},
		DestinationEdge{Purpose: DestinationPurposeModelRefresh, Destination: local},
	)
	if err != nil {
		t.Fatal(err)
	}
	g := NewDestinationGate()
	var zero DestinationPolicy
	if err := g.Install(zero, m); err != nil {
		t.Fatalf("loopback-only install under zero policy: %v", err)
	}
	if _, err := g.Bind(context.Background(), "agent", "llamacpp"); err != nil {
		t.Errorf("Bind on installed loopback edge: %v", err)
	}
}

// ---------------------------------------------------------------------------
// DestinationGate — capability issue and verification
// ---------------------------------------------------------------------------

// installTestGate installs a gate with one granted remote edge per purpose
// given, plus the destinations it needs granted.
func installTestGate(t *testing.T, edges ...DestinationEdge) *DestinationGate {
	t.Helper()
	m, err := NewDestinationManifest(edges...)
	if err != nil {
		t.Fatal(err)
	}
	grants := make([]Destination, 0, len(edges))
	for _, e := range edges {
		grants = append(grants, e.Destination)
	}
	g := NewDestinationGate()
	if err := g.Install(NewDestinationPolicy(grants...), m); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestDestinationGateBindThenAuthorize(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	other := mustDest(t, "other", "https://other.example.com")
	g := installTestGate(t,
		DestinationEdge{Purpose: "agent", Destination: remote},
		DestinationEdge{Purpose: "agent", Destination: other},
	)

	ctx, err := g.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}

	if err := g.authorize(ctx, "opencode", remote); err != nil {
		t.Errorf("authorize on the bound edge: %v", err)
	}
	// The capability is bound to one provider/destination pair: it must not
	// authorize the OTHER admitted destination (a capability is not a wildcard
	// over the granted set).
	if err := g.authorize(ctx, "other", other); !errors.Is(err, ErrDestinationDenied) {
		t.Errorf("capability authorized a different provider: %v", err)
	}
	if err := g.authorize(ctx, "opencode", other); !errors.Is(err, ErrDestinationDenied) {
		t.Errorf("capability authorized a different destination: %v", err)
	}
}

// M23: a context with no capability — or one forged as a plain value rather
// than issued by the snapshot — must not authorize.
func TestDestinationGateAuthorizeRequiresIssuedCapability(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	g := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: remote})

	if err := g.authorize(context.Background(), "opencode", remote); !errors.Is(err, ErrDestinationDenied) {
		t.Errorf("bare context authorized: %v", err)
	}

	// A capability issued by a DIFFERENT gate over the same edge must not
	// transfer: authority is the snapshot's, not the string values'.
	g2 := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: remote})
	ctx2, err := g2.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.authorize(ctx2, "opencode", remote); !errors.Is(err, ErrDestinationDenied) {
		t.Errorf("foreign gate's capability authorized: %v", err)
	}
}

// I2/M2: an omitted purpose sharing an ADMITTED endpoint is denied because
// its edge is absent — the destination being granted for another purpose
// confers nothing. This is the restaged original bug: agent admitted,
// summarize omitted, same endpoint.
func TestDestinationGateOmittedPurposeSharingAdmittedEndpointDenied(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	g := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: remote})

	_, err := g.Bind(context.Background(), "summarize", "opencode")
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("Bind for omitted purpose = %v, want ErrDestinationDenied", err)
	}
	for _, want := range []string{"summarize", "opencode"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("denial missing %q: %s", want, err)
		}
	}
}

func TestDestinationGateBindEmptyPurposeDenied(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	g := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: remote})

	if _, err := g.Bind(context.Background(), "", "opencode"); !errors.Is(err, ErrDestinationDenied) {
		t.Errorf("Bind with empty purpose = %v, want ErrDestinationDenied", err)
	}
}

// M17/I16: revocation invalidates every outstanding capability atomically.
// Re-admission issues a NEW snapshot; capabilities from the old one stay dead.
func TestDestinationGateClearInvalidatesOutstandingCapabilities(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	edge := DestinationEdge{Purpose: "agent", Destination: remote}
	m, err := NewDestinationManifest(edge)
	if err != nil {
		t.Fatal(err)
	}
	pol := NewDestinationPolicy(remote)

	g := NewDestinationGate()
	if err := g.Install(pol, m); err != nil {
		t.Fatal(err)
	}
	oldCtx, err := g.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}

	g.Clear()
	if err := g.authorize(oldCtx, "opencode", remote); !errors.Is(err, ErrDestinationDenied) {
		t.Errorf("stale capability authorized after Clear: %v", err)
	}
	if _, err := g.Bind(context.Background(), "agent", "opencode"); !errors.Is(err, ErrDestinationDenied) {
		t.Error("Bind succeeded after Clear")
	}

	// Re-admission: fresh capabilities work, the pre-Clear one stays dead.
	if err := g.Install(pol, m); err != nil {
		t.Fatal(err)
	}
	if err := g.authorize(oldCtx, "opencode", remote); !errors.Is(err, ErrDestinationDenied) {
		t.Errorf("pre-Clear capability authorized after re-install: %v", err)
	}
	freshCtx, err := g.Bind(context.Background(), "agent", "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.authorize(freshCtx, "opencode", remote); err != nil {
		t.Errorf("fresh capability after re-install: %v", err)
	}
}

// M25/D11: narrowing derives a snapshot that is a strict subset — it can
// remove candidates but cannot mint an edge the prospective set lacked.
func TestDestinationGateNarrowIsSubsetOnly(t *testing.T) {
	candA := mustDest(t, "llamacpp", "http://127.0.0.1:8080")
	candB := mustDest(t, "llamacpp-alt", "http://127.0.0.1:8081")
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	g := installTestGate(t,
		DestinationEdge{Purpose: DestinationPurposeDiscovery, Destination: candA},
		DestinationEdge{Purpose: DestinationPurposeDiscovery, Destination: candB},
		DestinationEdge{Purpose: "agent", Destination: remote},
	)

	// Narrow to the selected candidate: drop candB's discovery edge.
	if err := g.Narrow(func(e DestinationEdge) bool {
		return e.Purpose != DestinationPurposeDiscovery || e.Destination != candB
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := g.Bind(context.Background(), DestinationPurposeDiscovery, "llamacpp"); err != nil {
		t.Errorf("kept discovery edge denied after Narrow: %v", err)
	}
	if _, err := g.Bind(context.Background(), DestinationPurposeDiscovery, "llamacpp-alt"); !errors.Is(err, ErrDestinationDenied) {
		t.Error("narrowed-away discovery edge still bindable")
	}
	// Unrelated granted edges survive narrowing untouched.
	if _, err := g.Bind(context.Background(), "agent", "opencode"); err != nil {
		t.Errorf("unrelated edge lost by Narrow: %v", err)
	}

	// Narrowing an empty gate is an error, not a silent deny-all install.
	empty := NewDestinationGate()
	if err := empty.Narrow(func(DestinationEdge) bool { return true }); err == nil {
		t.Error("Narrow on an uninstalled gate must error")
	}
}

// A Clear that lands while a Narrow is in progress is a REVOCATION and must
// win: Narrow read the pre-Clear generation, and blindly storing its result
// would resurrect authority the user just revoked. The keep callback runs
// between Narrow's load and its store, so calling Clear inside it exercises
// exactly that interleaving, deterministically.
func TestDestinationGateNarrowLosesRaceToClear(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	g := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: remote})

	cleared := false
	err := g.Narrow(func(DestinationEdge) bool {
		if !cleared {
			cleared = true
			g.Clear() // revocation arrives mid-narrow
		}
		return true
	})
	if err == nil {
		t.Fatal("Narrow succeeded over a concurrent Clear; revocation was undone")
	}
	if _, bindErr := g.Bind(context.Background(), "agent", "opencode"); !errors.Is(bindErr, ErrDestinationDenied) {
		t.Errorf("gate not deny-all after Clear raced Narrow: %v", bindErr)
	}
}

// Bind/authorize race Install/Clear; -race is the assertion.
func TestDestinationGateConcurrentBindInstallClear(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	edge := DestinationEdge{Purpose: "agent", Destination: remote}
	m, err := NewDestinationManifest(edge)
	if err != nil {
		t.Fatal(err)
	}
	pol := NewDestinationPolicy(remote)
	g := NewDestinationGate()
	if err := g.Install(pol, m); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 200 {
				if ctx, err := g.Bind(context.Background(), "agent", "opencode"); err == nil {
					_ = g.authorize(ctx, "opencode", remote)
				}
			}
		})
	}
	wg.Go(func() {
		for range 100 {
			g.Clear()
			_ = g.Install(pol, m)
		}
	})
	wg.Wait()
}

// ---------------------------------------------------------------------------
// RoutePlan binding
// ---------------------------------------------------------------------------

// destCaptureProvider records the ctx of every call so tests can check what
// authority actually rode to the provider — the observation point is the
// callee, not the admission code (anti-tautology).
type destCaptureProvider struct {
	name   string
	fail   bool // every call returns an infrastructure error
	models []ModelInfo

	mu    sync.Mutex
	ctxs  []context.Context
	calls int
}

var errDestInfra = &HTTPStatusError{StatusCode: 503, Status: "503 Service Unavailable"}

func (p *destCaptureProvider) record(ctx context.Context) {
	p.mu.Lock()
	p.ctxs = append(p.ctxs, ctx)
	p.calls++
	p.mu.Unlock()
}

func (p *destCaptureProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *destCaptureProvider) lastCtx(t *testing.T) context.Context {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ctxs) == 0 {
		t.Fatal("provider was never called")
	}
	return p.ctxs[len(p.ctxs)-1]
}

func (p *destCaptureProvider) Name() string { return p.name }
func (p *destCaptureProvider) Capabilities() Capability {
	return CapChat | CapStream | CapGenerate | CapEmbed
}
func (p *destCaptureProvider) Health(_ context.Context) error { return nil }
func (p *destCaptureProvider) Models(_ context.Context) ([]ModelInfo, error) {
	return p.models, nil
}

func (p *destCaptureProvider) Chat(ctx context.Context, _ ChatRequest) (*ChatResponse, error) {
	p.record(ctx)
	if p.fail {
		return nil, errDestInfra
	}
	return &ChatResponse{Content: "ok", Done: true}, nil
}

func (p *destCaptureProvider) ChatStream(ctx context.Context, _ ChatRequest, fn func(ChatResponse) error) error {
	p.record(ctx)
	if p.fail {
		return errDestInfra
	}
	return fn(ChatResponse{Content: "ok", Done: true})
}

func (p *destCaptureProvider) Generate(ctx context.Context, _ GenerateRequest) (*GenerateResponse, error) {
	p.record(ctx)
	if p.fail {
		return nil, errDestInfra
	}
	return &GenerateResponse{Response: "ok", Done: true}, nil
}

func (p *destCaptureProvider) GenerateStream(ctx context.Context, _ GenerateRequest, fn func(GenerateResponse) error) error {
	p.record(ctx)
	if p.fail {
		return errDestInfra
	}
	return fn(GenerateResponse{Response: "ok", Done: true})
}

func (p *destCaptureProvider) Embed(ctx context.Context, _ EmbedRequest) (*EmbedResponse, error) {
	p.record(ctx)
	if p.fail {
		return nil, errDestInfra
	}
	return &EmbedResponse{Embeddings: [][]float64{{0.1}}}, nil
}

// destPlan builds a gated plan for one primary provider with UseCase useCase.
func destPlan(prov *destCaptureProvider, useCase string, gate *DestinationGate) *RoutePlan {
	key := ModelKey{Provider: prov.name, Model: "test-model"}
	plan := &RoutePlan{
		Kind:     RouteKindChat,
		Provider: prov,
		Model:    "test-model",
		Profile: &ModelProfile{
			Key:           key,
			Name:          "test-model",
			Family:        "test",
			Provider:      prov.name,
			ContextWindow: 32768,
		},
		Request: RoutingRequest{
			Model:    "test-model",
			UseCase:  useCase,
			Messages: []ChatMessage{{Role: "user", Content: "hello"}},
			Prompt:   "hello",
			Input:    []string{"hello"},
		},
		Budget: BudgetResult{Decision: BudgetOK},
	}
	plan.setDestinationGate(gate)
	return plan
}

// executeKinds runs one plan through every execution kind.
var executeKinds = []struct {
	name string
	run  func(context.Context, *RoutePlan) error
}{
	{"ExecuteChat", func(ctx context.Context, rp *RoutePlan) error {
		_, err := rp.ExecuteChat(ctx)
		return err
	}},
	{"ExecuteChatStream", func(ctx context.Context, rp *RoutePlan) error {
		return rp.ExecuteChatStream(ctx, func(ChatResponse) error { return nil })
	}},
	{"ExecuteGenerate", func(ctx context.Context, rp *RoutePlan) error {
		_, err := rp.ExecuteGenerate(ctx)
		return err
	}},
	{"ExecuteGenerateStream", func(ctx context.Context, rp *RoutePlan) error {
		return rp.ExecuteGenerateStream(ctx, func(GenerateResponse) error { return nil })
	}},
	{"ExecuteEmbed", func(ctx context.Context, rp *RoutePlan) error {
		_, err := rp.ExecuteEmbed(ctx)
		return err
	}},
}

// I5-adjacent, plan level: every execution kind binds a capability for
// {UseCase, provider} that the gate's current snapshot authorizes.
func TestRoutePlanEveryExecutionKindBindsCapability(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	for _, kind := range executeKinds {
		t.Run(kind.name, func(t *testing.T) {
			gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: remote})
			prov := &destCaptureProvider{name: "opencode"}
			plan := destPlan(prov, "agent", gate)

			if err := kind.run(context.Background(), plan); err != nil {
				t.Fatalf("%s: %v", kind.name, err)
			}
			got := prov.lastCtx(t)
			if err := gate.authorize(got, "opencode", remote); err != nil {
				t.Errorf("provider ctx does not carry an authorizing capability: %v", err)
			}
		})
	}
}

// I2 at the plan level, with the delegate-side counter: a gated plan whose
// use case has no edge produces ZERO provider calls, for every kind.
func TestRoutePlanMissingEdgeFailsBeforeProviderContact(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	for _, kind := range executeKinds {
		t.Run(kind.name, func(t *testing.T) {
			// Gate admits the endpoint for "agent" only; the plan routes
			// "summarize" — the restaged original bug.
			gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: remote})
			prov := &destCaptureProvider{name: "opencode"}
			plan := destPlan(prov, "summarize", gate)

			err := kind.run(context.Background(), plan)
			if !errors.Is(err, ErrDestinationDenied) {
				t.Fatalf("%s = %v, want ErrDestinationDenied", kind.name, err)
			}
			if got := prov.callCount(); got != 0 {
				t.Errorf("%s: provider called %d times after denial, want 0", kind.name, got)
			}
		})
	}
}

func TestRoutePlanEmptyUseCaseOnGatedPlanDenied(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: remote})
	prov := &destCaptureProvider{name: "opencode"}
	plan := destPlan(prov, "", gate)

	_, err := plan.ExecuteChat(context.Background())
	if !errors.Is(err, ErrDestinationDenied) {
		t.Fatalf("empty use case = %v, want ErrDestinationDenied", err)
	}
	if got := prov.callCount(); got != 0 {
		t.Errorf("provider called %d times, want 0", got)
	}
}

// I20: an ungated plan (nil gate) neither fails nor smuggles a capability.
func TestRoutePlanUngatedPassesNoCapability(t *testing.T) {
	remote := mustDest(t, "opencode", "https://opencode.ai/zen/go")
	prov := &destCaptureProvider{name: "opencode"}
	plan := destPlan(prov, "agent", nil)

	if _, err := plan.ExecuteChat(context.Background()); err != nil {
		t.Fatalf("ungated ExecuteChat: %v", err)
	}
	// The ctx that reached the provider holds no capability: any gate must
	// refuse it.
	other := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: remote})
	if err := other.authorize(prov.lastCtx(t), "opencode", remote); !errors.Is(err, ErrDestinationDenied) {
		t.Errorf("ungated plan's ctx authorized against a gate: %v", err)
	}
}

// The fallback attempt binds the FALLBACK's provider and destination, not the
// primary's — a capability for the primary must not ride to the fallback.
func TestRoutePlanFallbackBindsFallbackDestination(t *testing.T) {
	primaryDest := mustDest(t, "primary", "https://primary.example.com")
	fbDest := mustDest(t, "fallback", "https://fallback.example.com")

	for _, kind := range executeKinds {
		t.Run(kind.name, func(t *testing.T) {
			gate := installTestGate(t,
				DestinationEdge{Purpose: "agent", Destination: primaryDest},
				DestinationEdge{Purpose: "agent", Destination: fbDest, IsFallback: true},
			)
			primary := &destCaptureProvider{name: "primary", fail: true}
			fb := &destCaptureProvider{name: "fallback"}
			plan := destPlan(primary, "agent", gate)
			fbPlan := *destPlan(fb, "agent", gate)
			plan.Fallbacks = []RoutePlan{fbPlan}

			if err := kind.run(context.Background(), plan); err != nil {
				t.Fatalf("%s with fallback: %v", kind.name, err)
			}
			fbCtx := fb.lastCtx(t)
			if err := gate.authorize(fbCtx, "fallback", fbDest); err != nil {
				t.Errorf("fallback ctx does not authorize the fallback destination: %v", err)
			}
			if err := gate.authorize(fbCtx, "primary", primaryDest); !errors.Is(err, ErrDestinationDenied) {
				t.Errorf("fallback ctx authorized the PRIMARY destination: %v", err)
			}
		})
	}
}

// A gated plan whose FALLBACK edge is missing stops the walk at the fallback
// without contacting it; the primary attempt stands. Every execution kind has
// its own fallback bind site, so every kind is exercised — a single-kind test
// here would leave the other four sites free to ignore the denial.
func TestRoutePlanFallbackMissingEdgeStopsWalkWithoutContact(t *testing.T) {
	primaryDest := mustDest(t, "primary", "https://primary.example.com")
	for _, kind := range executeKinds {
		t.Run(kind.name, func(t *testing.T) {
			gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: primaryDest})
			primary := &destCaptureProvider{name: "primary", fail: true}
			fb := &destCaptureProvider{name: "fallback"} // no edge for this provider
			plan := destPlan(primary, "agent", gate)
			fbPlan := *destPlan(fb, "agent", gate)
			plan.Fallbacks = []RoutePlan{fbPlan}

			err := kind.run(context.Background(), plan)
			if !errors.Is(err, ErrDestinationDenied) {
				t.Fatalf("%s fallback without edge = %v, want ErrDestinationDenied", kind.name, err)
			}
			if got := fb.callCount(); got != 0 {
				t.Errorf("ungated-edge fallback called %d times, want 0", got)
			}
			if got := primary.callCount(); got != 1 {
				t.Errorf("primary called %d times, want exactly 1", got)
			}
		})
	}
}

// A denied PRIMARY fails loud without falling through to an admitted
// fallback. A missing primary edge is a manifest-wiring bug, not a runtime
// availability condition — silently serving from the fallback would mask it
// exactly the way the original #477 leak was masked, so neither provider may
// be contacted.
func TestRoutePlanDeniedPrimaryDoesNotFallThroughToAdmittedFallback(t *testing.T) {
	fbDest := mustDest(t, "fallback", "https://fallback.example.com")
	for _, kind := range executeKinds {
		t.Run(kind.name, func(t *testing.T) {
			// Only the FALLBACK's edge is admitted; the primary has none.
			gate := installTestGate(t, DestinationEdge{Purpose: "agent", Destination: fbDest, IsFallback: true})
			primary := &destCaptureProvider{name: "primary", fail: true}
			fb := &destCaptureProvider{name: "fallback"}
			plan := destPlan(primary, "agent", gate)
			fbPlan := *destPlan(fb, "agent", gate)
			plan.Fallbacks = []RoutePlan{fbPlan}

			err := kind.run(context.Background(), plan)
			if !errors.Is(err, ErrDestinationDenied) {
				t.Fatalf("%s denied primary = %v, want ErrDestinationDenied", kind.name, err)
			}
			if got := primary.callCount(); got != 0 {
				t.Errorf("denied primary contacted %d times, want 0", got)
			}
			if got := fb.callCount(); got != 0 {
				t.Errorf("fallback contacted %d times after primary denial, want 0", got)
			}
		})
	}
}

// Router stamping, strict-chain path: golem routes exclusively through
// PreferredChain+StrictChain (routeChain), so the stamp must hold there —
// covering only the scored Route path would leave the production route
// unverified.
func TestRouterStampsDestinationGateOntoStrictChainPlans(t *testing.T) {
	local := mustDest(t, "cap", "http://127.0.0.1:9999")
	gate := installTestGate(t, DestinationEdge{Purpose: "chat", Destination: local})

	prov := &destCaptureProvider{name: "cap", models: []ModelInfo{{
		Name:          "test-model",
		Family:        "test",
		ParameterSize: "8B",
		ContextWindow: 32768,
		Capabilities:  []string{"completion"},
	}}}
	reg := NewRegistry()
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	if err := reg.RefreshModels(context.Background(), "cap"); err != nil {
		t.Fatal(err)
	}
	mr, err := NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(mr, reg, WithDestinationGate(gate))
	t.Cleanup(func() { _ = router.Close() })

	plan, err := router.Route(context.Background(), RoutingRequest{
		UseCase:        "chat",
		Messages:       []ChatMessage{{Role: "user", Content: "hi"}},
		PreferredChain: []string{"cap/test-model"},
		StrictChain:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ExecuteChat(context.Background()); err != nil {
		t.Fatalf("gated strict-chain plan: %v", err)
	}
	if err := gate.authorize(prov.lastCtx(t), "cap", local); err != nil {
		t.Errorf("strict-chain plan's provider ctx does not authorize: %v", err)
	}
}

// Router stamping: a Router built WithDestinationGate stamps the gate onto
// the plans it builds, so gating does not depend on callers remembering a
// per-plan setter.
func TestRouterStampsDestinationGateOntoPlans(t *testing.T) {
	local := mustDest(t, "cap", "http://127.0.0.1:9999")
	gate := installTestGate(t, DestinationEdge{Purpose: "chat", Destination: local})

	prov := &destCaptureProvider{name: "cap", models: []ModelInfo{{
		Name:          "test-model",
		Family:        "test",
		ParameterSize: "8B",
		ContextWindow: 32768,
		Capabilities:  []string{"completion"},
	}}}
	reg := NewRegistry()
	if err := reg.Register(prov); err != nil {
		t.Fatal(err)
	}
	if err := reg.RefreshModels(context.Background(), "cap"); err != nil {
		t.Fatal(err)
	}
	mr, err := NewModelRegistry(reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(mr, reg, WithDestinationGate(gate))
	t.Cleanup(func() { _ = router.Close() })

	plan, err := router.Route(context.Background(), RoutingRequest{
		Model:    "test-model",
		UseCase:  "chat",
		Messages: []ChatMessage{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.ExecuteChat(context.Background()); err != nil {
		t.Fatalf("gated routed plan: %v", err)
	}
	if err := gate.authorize(prov.lastCtx(t), "cap", local); err != nil {
		t.Errorf("routed plan's provider ctx does not authorize: %v", err)
	}
}
