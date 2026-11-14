// provider/resolve_evidence_test.go
//
// Tests for on-demand tool_call resolution on a registry with an evidence log
// (#735): the log path, the unbound and unreadable states, the cooldown, the
// automatic allowance, resolution order and progress. Fixture rule: four
// mechanisms can each keep a second check from being sent (the allowance, the
// cooldown, the singleflight and the profile cache); each test is built so the
// mechanism it is not about cannot be what held a request back.
package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/fingerprint"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const resolveEndpoint = "http://127.0.0.1:8080"

var agentCaps = CapChat | CapStream | CapToolCall

var errInjected = errors.New("injected store failure")

// hookedLog wraps a real evidence store. onRead runs before every
// ReadSnapshot and onAppend before every AppendEvidence; a non-nil error fails
// the call without reaching the store, and a hook may block. It keeps the rows
// offered to AppendEvidence, before the store sees them, and counts reads and
// GetCapProbe.
type hookedLog struct {
	fingerprint.EvidenceStore
	mu       sync.Mutex
	onRead   func(ctx context.Context) error
	onAppend func(ctx context.Context, rows []fingerprint.EvidenceRow) error
	reads    int
	capGets  int
	appended [][]fingerprint.EvidenceRow
}

func (l *hookedLog) setOnRead(fn func(ctx context.Context) error) {
	l.mu.Lock()
	l.onRead = fn
	l.mu.Unlock()
}

func (l *hookedLog) setOnAppend(fn func(ctx context.Context, rows []fingerprint.EvidenceRow) error) {
	l.mu.Lock()
	l.onAppend = fn
	l.mu.Unlock()
}

func (l *hookedLog) failReads() {
	l.setOnRead(func(context.Context) error { return errInjected })
}

func (l *hookedLog) ReadSnapshot(ctx context.Context, endpoint, adapter string, apiKey *string, legacyProvider string, models []string, now time.Time) (fingerprint.EvidenceSnapshot, error) {
	l.mu.Lock()
	l.reads++
	hook := l.onRead
	l.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return fingerprint.EvidenceSnapshot{}, err
		}
	}
	return l.EvidenceStore.ReadSnapshot(ctx, endpoint, adapter, apiKey, legacyProvider, models, now)
}

func (l *hookedLog) AppendEvidence(ctx context.Context, rows ...fingerprint.EvidenceRow) error {
	l.mu.Lock()
	l.appended = append(l.appended, slices.Clone(rows))
	hook := l.onAppend
	l.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, rows); err != nil {
			return err
		}
	}
	return l.EvidenceStore.AppendEvidence(ctx, rows...)
}

func (l *hookedLog) GetCapProbe(ctx context.Context, backendID, modelName, capability string) (*fingerprint.CapProbe, error) {
	l.mu.Lock()
	l.capGets++
	l.mu.Unlock()
	return l.EvidenceStore.GetCapProbe(ctx, backendID, modelName, capability)
}

func (l *hookedLog) appendCalls() [][]fingerprint.EvidenceRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.appended)
}

func (l *hookedLog) capGetCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.capGets
}

// capProbeFake is a fingerprint.ModelProber that implements only
// fingerprint.CapabilityProber among the probe interfaces. run scripts each
// call; nil answers a tools yes that reports 1 request. Profiling calls are
// counted and fail.
type capProbeFake struct {
	mu        sync.Mutex
	run       func(ctx context.Context, model string) (fingerprint.CapabilityProbeResult, error)
	calls     []string
	reqs      []fingerprint.CapabilityProbeRequest
	profiling int
}

func (p *capProbeFake) setRun(fn func(ctx context.Context, model string) (fingerprint.CapabilityProbeResult, error)) {
	p.mu.Lock()
	p.run = fn
	p.mu.Unlock()
}

func (p *capProbeFake) ProbeCapabilities(ctx context.Context, model string, req fingerprint.CapabilityProbeRequest) (fingerprint.CapabilityProbeResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, model)
	p.reqs = append(p.reqs, req)
	run := p.run
	p.mu.Unlock()
	if run == nil {
		return toolsResult(fingerprint.CapProbeYes, 1), nil
	}
	return run(ctx, model)
}

func (p *capProbeFake) DetectKind(context.Context, string) (*fingerprint.KindDetection, error) {
	p.mu.Lock()
	p.profiling++
	p.mu.Unlock()
	return nil, errors.New("profiling is not expected")
}

func (p *capProbeFake) ProbeChat(context.Context, string, interface{}) (*fingerprint.ChatMetrics, error) {
	p.mu.Lock()
	p.profiling++
	p.mu.Unlock()
	return nil, errors.New("profiling is not expected")
}

func (p *capProbeFake) ProbeEmbedding(context.Context, string) (*fingerprint.EmbeddingMetrics, error) {
	return nil, errors.New("profiling is not expected")
}

func (p *capProbeFake) callList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *capProbeFake) callCount() int { return len(p.callList()) }

func (p *capProbeFake) profilingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.profiling
}

// bothProber implements both probe interfaces and counts legacy calls, which
// the log path must never make.
type bothProber struct {
	*capProbeFake
	legacyCalls atomic.Int64
}

func (p *bothProber) ProbeToolCall(context.Context, string) (fingerprint.CapProbeOutcome, error) {
	p.legacyCalls.Add(1)
	return fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}, nil
}

// toolsResult is a chat-stream-tools result: chat and stream yes, and tool_call
// as state (an inconclusive is an attempt with the 24-hour TTL).
func toolsResult(state fingerprint.CapProbeState, requests int) fingerprint.CapabilityProbeResult {
	yes := func(c string) fingerprint.ProbeAnswer {
		return fingerprint.ProbeAnswer{Capability: c, Source: fingerprint.EvidenceSourceProbe, Kind: fingerprint.EvidenceKindAnswer, State: fingerprint.CapProbeYes}
	}
	tc := fingerprint.ProbeAnswer{Capability: capabilityToolCall, Source: fingerprint.EvidenceSourceProbe}
	switch state {
	case fingerprint.CapProbeInconclusive:
		tc.Kind, tc.State, tc.TTL, tc.Detail = fingerprint.EvidenceKindAttempt, state, fingerprint.CapProbeInconclusiveTTL, fingerprint.DetailToolIgnored
	case fingerprint.CapProbeYes:
		tc.Kind, tc.State, tc.Detail = fingerprint.EvidenceKindAnswer, state, fingerprint.DetailToolCalled
	default:
		tc.Kind, tc.State = fingerprint.EvidenceKindAnswer, state
	}
	return fingerprint.CapabilityProbeResult{
		Method: fingerprint.MethodChatStreamTools, Requests: requests,
		Answers: []fingerprint.ProbeAnswer{yes("chat"), yes("stream"), tc},
	}
}

func scripted(result fingerprint.CapabilityProbeResult, err error) func(context.Context, string) (fingerprint.CapabilityProbeResult, error) {
	return func(context.Context, string) (fingerprint.CapabilityProbeResult, error) { return result, err }
}

// attemptRow is an inconclusive chat-stream-tools attempt for logModel through
// b, observed at at, with the 24-hour expiry.
func attemptRow(b EvidenceBinding, digest string, at time.Time, detail string) fingerprint.EvidenceRow {
	r := logRow(b, capabilityToolCall, fingerprint.CapProbeInconclusive, digest, at)
	r.Kind, r.Detail = fingerprint.EvidenceKindAttempt, detail
	r.FreshUntil, r.ExpiresAt = fingerprint.ExpiryFor(r, 0)
	return r
}

// logProv is one provider of a log-bound fixture registry.
type logProv struct {
	name    string
	binding EvidenceBinding
	models  []ModelInfo
}

func openAIBinding(t *testing.T) EvidenceBinding {
	return mustBinding(t, resolveEndpoint, "openai-compat")
}

// oneProv is provider "local" at resolveEndpoint through openai-compat.
func oneProv(t *testing.T, models ...ModelInfo) []logProv {
	return []logProv{{name: "local", binding: openAIBinding(t), models: models}}
}

func infos(names ...string) []ModelInfo {
	out := make([]ModelInfo, len(names))
	for i, n := range names {
		out[i] = ModelInfo{Name: n, Digest: logDigest, ContextWindow: 32768}
	}
	return out
}

// newLogResolver builds a log-bound registry over provs, each bound by name,
// with prober behind WithCapabilityProber (none when nil), the chat and stream
// floor a route needs, no catalog entry, override or runtime declaration for
// the fixture models, and the clock stopped at logT0. opts apply last, so a
// test can replace the binder or the prober factory.
func newLogResolver(t *testing.T, store fingerprint.EvidenceStore, prober fingerprint.ModelProber, provs []logProv, opts ...ModelRegistryOption) (*ModelRegistry, *logClock) {
	t.Helper()
	providers := make(map[string]Provider, len(provs))
	bindings := make(map[string]EvidenceBinding, len(provs))
	for _, p := range provs {
		providers[p.name] = newStubProvider(p.name, p.models...)
		bindings[p.name] = p.binding
	}
	all := []ModelRegistryOption{WithEvidenceLog(store, func(name string) (EvidenceBinding, bool) {
		b, ok := bindings[name]
		return b, ok
	})}
	if prober != nil {
		all = append(all, WithCapabilityProber(capProberFactory(prober)))
	}
	mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: providers}, nil, append(all, opts...)...)
	if err != nil {
		t.Fatalf("NewModelRegistry: %v", err)
	}
	mr.SetCapabilityFloor(func(ModelKey) []string { return []string{"chat", "stream"} })
	return mr, newLogClock(mr, logT0)
}

func localKey(model string) ModelKey { return ModelKey{Provider: "local", Model: model} }

func allowanceOf(mr *ModelRegistry) int {
	mr.budgetMu.Lock()
	defer mr.budgetMu.Unlock()
	return mr.budgetRemaining
}

func setAllowance(mr *ModelRegistry, n int) {
	mr.budgetMu.Lock()
	mr.budgetRemaining = n
	mr.budgetMu.Unlock()
}

func storedRows(t *testing.T, store fingerprint.EvidenceStore, b EvidenceBinding, model string) []fingerprint.EvidenceRow {
	t.Helper()
	rows, err := store.EvidenceRows(t.Context(), b.Endpoint, model)
	if err != nil {
		t.Fatalf("EvidenceRows: %v", err)
	}
	return rows
}

// toolCallAnswer is what the stored rows answer for logModel's tool_call at
// digest through b at now, or "" when nothing.
func toolCallAnswer(t *testing.T, store fingerprint.EvidenceStore, b EvidenceBinding, digest string, now time.Time) fingerprint.CapProbeState {
	t.Helper()
	reading := fingerprint.ReadCapabilities(storedRows(t, store, b, logModel), b.Adapter, digest, now)[capabilityToolCall]
	if a := readingAnswer(reading); a != nil {
		return a.State
	}
	return ""
}

// holdAdmitter grants one permit per key at a time. A key with a gate in hold
// waits at acquire until the gate closes; onAcquire runs once a permit is
// granted. It records arrivals in order and counts raw release calls, so a
// double release is visible.
type holdAdmitter struct {
	mu        sync.Mutex
	held      map[ModelKey]bool
	wake      chan struct{}
	hold      map[ModelKey]chan struct{}
	onAcquire func(ModelKey)
	arrivals  []ModelKey
	releases  int
	entered   chan ModelKey
}

func newHoldAdmitter() *holdAdmitter {
	return &holdAdmitter{
		held: make(map[ModelKey]bool), wake: make(chan struct{}),
		hold: make(map[ModelKey]chan struct{}), entered: make(chan ModelKey, 64),
	}
}

func (a *holdAdmitter) holdKey(key ModelKey) chan struct{} {
	gate := make(chan struct{})
	a.mu.Lock()
	a.hold[key] = gate
	a.mu.Unlock()
	return gate
}

func (a *holdAdmitter) acquireSlot(ctx context.Context, key ModelKey) (func(), error) {
	a.mu.Lock()
	a.arrivals = append(a.arrivals, key)
	gate := a.hold[key]
	onAcquire := a.onAcquire
	a.mu.Unlock()
	a.entered <- key
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	for {
		a.mu.Lock()
		if !a.held[key] {
			a.held[key] = true
			a.mu.Unlock()
			break
		}
		wake := a.wake
		a.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if onAcquire != nil {
		onAcquire(key)
	}
	return func() {
		a.mu.Lock()
		a.releases++
		a.held[key] = false
		close(a.wake)
		a.wake = make(chan struct{})
		a.mu.Unlock()
	}, nil
}

func (a *holdAdmitter) releaseCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.releases
}

func (a *holdAdmitter) arrivalList() []ModelKey {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.arrivals)
}

// progressRecorder records events and notes any overlap between callbacks.
// hook runs inside the callback, so a hook that blocks holds the callback.
type progressRecorder struct {
	mu      sync.Mutex
	events  []CapabilityProbeProgress
	hook    func(CapabilityProbeProgress)
	inside  atomic.Int32
	overlap atomic.Bool
}

func (p *progressRecorder) record(e CapabilityProbeProgress) {
	if p.inside.Add(1) > 1 {
		p.overlap.Store(true)
	}
	defer p.inside.Add(-1)
	p.mu.Lock()
	p.events = append(p.events, e)
	hook := p.hook
	p.mu.Unlock()
	if hook != nil {
		hook(e)
	}
}

func (p *progressRecorder) list() []CapabilityProbeProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.events)
}

func (p *progressRecorder) stages(key ModelKey) []string {
	var out []string
	for _, e := range p.list() {
		if e.Key == key {
			out = append(out, e.Stage)
		}
	}
	return out
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	var zero T
	return zero
}

type resolveOutcome struct {
	state fingerprint.CapProbeState
	err   error
}

func resolveAsync(ctx context.Context, mr *ModelRegistry, key ModelKey) <-chan resolveOutcome {
	done := make(chan resolveOutcome, 1)
	go func() {
		state, err := mr.ResolveToolCall(ctx, key)
		done <- resolveOutcome{state, err}
	}()
	return done
}

func wantState(t *testing.T, label string, got fingerprint.CapProbeState, err error, want fingerprint.CapProbeState) {
	t.Helper()
	if err != nil || got != want {
		t.Fatalf("%s: ResolveToolCall = %q, %v; want %q, nil", label, got, err, want)
	}
}

func wantErrIs(t *testing.T, label string, got fingerprint.CapProbeState, err, want error) {
	t.Helper()
	if got != "" || !errors.Is(err, want) {
		t.Fatalf("%s: ResolveToolCall = %q, %v; want \"\", %v", label, got, err, want)
	}
}

// ---------------------------------------------------------------------------
// The log path
// ---------------------------------------------------------------------------

func TestResolveToolCall_StreamedMethodWritesLogOnly(t *testing.T) {
	store := newEvidenceLog(t)
	log := &hookedLog{EvidenceStore: store}
	legacy := newFakeCapProbeStore()
	prober := &bothProber{capProbeFake: &capProbeFake{}}
	b := openAIBinding(t)
	mr, clock := newLogResolver(t, log, prober, oneProv(t, infos(logModel)...), WithCapabilityProbeStore(legacy))
	clock.set(logT0.Add(1500 * time.Microsecond)) // not on a whole millisecond

	state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
	wantState(t, "resolve", state, err, fingerprint.CapProbeYes)

	wantObserved := logT0.Add(time.Millisecond)
	appends := log.appendCalls()
	if len(appends) != 1 || len(appends[0]) != 3 {
		t.Fatalf("AppendEvidence calls = %v; want one call of 3 rows", appends)
	}
	for _, r := range appends[0] {
		if !r.ObservedAt.Equal(wantObserved) || r.ObservedAt.Nanosecond()%int(time.Millisecond) != 0 {
			t.Fatalf("appended %s ObservedAt = %v; want %v, on a whole millisecond", r.Capability, r.ObservedAt, wantObserved)
		}
	}
	rows := storedRows(t, store, b, logModel)
	if len(rows) != 3 {
		t.Fatalf("stored rows = %d; want 3", len(rows))
	}
	for _, r := range rows {
		if r.Method != fingerprint.MethodChatStreamTools || r.MethodVersion != fingerprint.MethodVersion(fingerprint.MethodChatStreamTools) ||
			r.Endpoint != b.Endpoint || r.Adapter != fingerprint.AdapterOpenAICompat || r.Digest != logDigest ||
			r.KeyVersion != fingerprint.EvidenceKeyVersion || !r.ObservedAt.Equal(wantObserved) {
			t.Fatalf("stored row = %+v; want chat-stream-tools through %s at %v", r, b.Endpoint, wantObserved)
		}
	}
	if req := prober.reqs[0]; req.Method != fingerprint.MethodChatStreamTools || !slices.Equal(req.Capabilities, []string{capabilityToolCall}) {
		t.Fatalf("probe request = %+v; want chat-stream-tools for tool_call", req)
	}
	if legacy.count() != 0 || legacy.getCount() != 0 || log.capGetCount() != 0 {
		t.Fatalf("capability_probes touched: rows=%d gets=%d log gets=%d; want none", legacy.count(), legacy.getCount(), log.capGetCount())
	}
	if n := prober.legacyCalls.Load(); n != 0 {
		t.Fatalf("legacy ProbeToolCall calls = %d; want 0", n)
	}
}

func TestResolveToolCall_DigestlessRowHasEmptyDigest(t *testing.T) {
	store := newEvidenceLog(t)
	b := openAIBinding(t)
	mr, _ := newLogResolver(t, store, &capProbeFake{}, oneProv(t, ModelInfo{Name: logModel}))

	state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
	wantState(t, "resolve", state, err, fingerprint.CapProbeYes)
	rows := storedRows(t, store, b, logModel)
	if len(rows) != 3 {
		t.Fatalf("stored rows = %d; want 3", len(rows))
	}
	for _, r := range rows {
		if r.Digest != "" {
			t.Fatalf("row %s digest = %q; want empty", r.Capability, r.Digest)
		}
		if r.Capability == capabilityToolCall && !r.FreshUntil.Equal(logT0.Add(30*logDay)) {
			t.Fatalf("name-only yes FreshUntil = %v; want 30 days after %v", r.FreshUntil, logT0)
		}
	}
	wantCaps(t, "profile after the check", lookupCaps(t, mr, "local"), agentCaps)
}

func TestResolveToolCall_EvidenceLogAloneResolves(t *testing.T) {
	store := newEvidenceLog(t)
	prober := &capProbeFake{}
	mr, _ := newLogResolver(t, store, prober, oneProv(t, infos(logModel)...))
	if !mr.canResolveToolCall() {
		t.Fatal("canResolveToolCall = false with a prober and a log; want true")
	}
	state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
	wantState(t, "resolve", state, err, fingerprint.CapProbeYes)
	if n := prober.callCount(); n != 1 {
		t.Fatalf("probe calls = %d; want 1", n)
	}
	if n := len(storedRows(t, store, openAIBinding(t), logModel)); n != 3 {
		t.Fatalf("stored rows = %d; want 3", n)
	}
}

func TestResolveToolCall_InterfaceByMode(t *testing.T) {
	key := localKey(logModel)
	t.Run("CapabilityProber with a log resolves", func(t *testing.T) {
		prober := &capProbeFake{}
		mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos(logModel)...))
		state, err := mr.ResolveToolCall(t.Context(), key)
		wantState(t, "resolve", state, err, fingerprint.CapProbeYes)
		if n := prober.callCount(); n != 1 {
			t.Fatalf("calls = %d; want 1", n)
		}
	})
	t.Run("ToolCallProber without a log resolves as before", func(t *testing.T) {
		legacy := newFakeCapProbeStore()
		prober := newFakeToolCallProber()
		prober.outcomes[logModel] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
		mr := newCapResolveRegistry(t, "local", []string{logModel}, legacy, prober)
		state, err := mr.ResolveToolCall(t.Context(), key)
		wantState(t, "resolve", state, err, fingerprint.CapProbeYes)
		if n := prober.callCount(logModel); n != 1 {
			t.Fatalf("calls = %d; want 1", n)
		}
		if _, ok := legacy.row("local", logModel, capabilityToolCall); !ok {
			t.Fatal("legacy row not saved")
		}
	})
	t.Run("ToolCallProber with a log is unknown", func(t *testing.T) {
		store := newEvidenceLog(t)
		legacy := newFakeCapProbeStore()
		prober := newFakeToolCallProber()
		prober.outcomes[logModel] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
		mr, _ := newLogResolver(t, store, prober, oneProv(t, infos(logModel)...), WithCapabilityProbeStore(legacy))
		state, err := mr.ResolveToolCall(t.Context(), key)
		wantState(t, "resolve", state, err, "")
		if n := prober.totalCalls(); n != 0 {
			t.Fatalf("calls = %d; want 0", n)
		}
		if n := len(storedRows(t, store, openAIBinding(t), logModel)); n != 0 || legacy.count() != 0 {
			t.Fatalf("rows written: log %d, legacy %d; want none", n, legacy.count())
		}
	})
}

func TestLegacyYesStreamedInconclusive_DoesNotQualifyAgent(t *testing.T) {
	ctx := t.Context()
	legacy := newFakeCapProbeStore()
	if err := legacy.SaveCapProbe(ctx, fingerprint.CapProbe{
		BackendID: "local", ModelName: logModel, Capability: capabilityToolCall, State: fingerprint.CapProbeYes,
		ModelDigest: logDigest, ProbeVersion: fingerprint.CurrentToolProbeVersion, TestedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	prober := &capProbeFake{run: scripted(toolsResult(fingerprint.CapProbeInconclusive, 1), nil)}
	mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos(logModel)...), WithCapabilityProbeStore(legacy))
	router := NewRouter(mr, NewRegistry())
	t.Cleanup(func() { _ = router.Close() })

	_, err := router.Route(ctx, RoutingRequest{Model: "local/" + logModel, UseCase: "agent", RequiredCaps: agentCaps})
	if !errors.Is(err, ErrNoViableCandidate) {
		t.Fatalf("Route = %v; want ErrNoViableCandidate", err)
	}
	if n := prober.callCount(); n != 1 {
		t.Fatalf("streamed checks = %d; want 1", n)
	}
	if n := legacy.getCount(); n != 0 {
		t.Fatalf("GetCapProbe calls = %d; want 0", n)
	}
}

func TestSecondRegistrySeesFirstProbe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fingerprints.db")
	proberA, proberB := &capProbeFake{}, &capProbeFake{}
	first, _ := newLogResolver(t, openEvidenceLog(t, path), proberA, oneProv(t, infos(logModel)...))
	state, err := first.ResolveToolCall(t.Context(), localKey(logModel))
	wantState(t, "first registry", state, err, fingerprint.CapProbeYes)

	second, _ := newLogResolver(t, openEvidenceLog(t, path), proberB, oneProv(t, infos(logModel)...))
	state, err = second.ResolveToolCall(t.Context(), localKey(logModel))
	wantState(t, "second registry", state, err, fingerprint.CapProbeYes)
	if n := proberB.callCount(); n != 0 {
		t.Fatalf("second registry calls = %d; want 0", n)
	}
	if n := allowanceOf(second); n != automaticProbeAllowance {
		t.Fatalf("second registry allowance = %d; want %d", n, automaticProbeAllowance)
	}
}

func TestResolveToolCall_PhaseThreeSeesRebuiltProfile(t *testing.T) {
	ctx := t.Context()
	prober := &capProbeFake{}
	mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos(logModel)...))
	before, err := mr.Lookup(ctx, localKey(logModel))
	if err != nil || before.Caps.Has(CapToolCall) {
		t.Fatalf("cached profile = %v, %v; want one without tool_call", before, err)
	}
	out, diags := mr.EnsureToolCallResolved(ctx, []*ModelProfile{before}, agentCaps)
	if len(diags) != 0 {
		t.Fatalf("diags = %v", diags)
	}
	if out[0] == before || !out[0].Caps.Has(CapToolCall) {
		t.Fatalf("candidate = %v; want the rebuilt profile with tool_call", out[0].Caps)
	}
	cached, err := mr.Lookup(ctx, localKey(logModel))
	if err != nil || cached != out[0] {
		t.Fatalf("candidate is not the registry's rebuilt profile")
	}
	if n := prober.callCount(); n != 1 {
		t.Fatalf("calls = %d; want 1", n)
	}
}

func TestResolveToolCall_AnswerFromLogRefreshesProfile(t *testing.T) {
	b := openAIBinding(t)
	t.Run("two provider names in one candidate list", func(t *testing.T) {
		ctx := t.Context()
		prober := &capProbeFake{}
		mr, _ := newLogResolver(t, newEvidenceLog(t), prober, []logProv{
			{name: "p1", binding: b, models: infos(logModel)},
			{name: "p2", binding: b, models: infos(logModel)},
		})
		var candidates []*ModelProfile
		for _, name := range []string{"p1", "p2"} {
			p, err := mr.Lookup(ctx, ModelKey{Provider: name, Model: logModel})
			if err != nil {
				t.Fatal(err)
			}
			candidates = append(candidates, p)
		}
		out, diags := mr.EnsureToolCallResolved(ctx, candidates, agentCaps)
		if len(diags) != 0 {
			t.Fatalf("diags = %v", diags)
		}
		for i, p := range out {
			if !p.Caps.Has(CapToolCall) {
				t.Fatalf("candidate %d (%s) caps = %v; want tool_call", i, p.Key, p.Caps)
			}
		}
		if n := prober.callCount(); n != 1 {
			t.Fatalf("calls = %d; want 1 (the second name reads the first one's answer)", n)
		}
	})
	// The boundary sync drops a profile cached before a write it sees, so the
	// yes found by the slow path's own read must drop the one cached before a
	// write that lands after the sync.
	t.Run("an answer written after the lookup", func(t *testing.T) {
		ctx := t.Context()
		path := filepath.Join(t.TempDir(), "fingerprints.db")
		other := openEvidenceLog(t, path)
		prober := &capProbeFake{}
		factory := func(context.Context, ModelKey, *ModelInfo, Provider) (*FingerprintProberSpec, error) {
			appendRows(t, other, logRow(b, capabilityToolCall, fingerprint.CapProbeYes, logDigest, logT0))
			return &FingerprintProberSpec{Prober: prober}, nil
		}
		mr, _ := newLogResolver(t, openEvidenceLog(t, path), nil, oneProv(t, infos(logModel)...), WithCapabilityProber(factory))
		before, err := mr.Lookup(ctx, localKey(logModel))
		if err != nil {
			t.Fatal(err)
		}
		out, diags := mr.EnsureToolCallResolved(ctx, []*ModelProfile{before}, agentCaps)
		if len(diags) != 0 || !out[0].Caps.Has(CapToolCall) {
			t.Fatalf("candidate caps = %v, diags = %v; want tool_call from the refreshed profile", out[0].Caps, diags)
		}
		if n := prober.callCount(); n != 0 {
			t.Fatalf("calls = %d; want 0", n)
		}
	})
}

// holdFirstAppend makes the log's first append wait until release closes,
// signalling entered when it arrives.
func holdFirstAppend(log *hookedLog) (entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	log.setOnAppend(func(context.Context, []fingerprint.EvidenceRow) error {
		once.Do(func() {
			close(entered)
			<-release
		})
		return nil
	})
	return entered, release
}

func TestResolveToolCall_ObservedAtSurvivesLateAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fingerprints.db")
	store, other := openEvidenceLog(t, path), openEvidenceLog(t, path)
	log := &hookedLog{EvidenceStore: store}
	b := openAIBinding(t)
	prober := &capProbeFake{run: scripted(toolsResult(fingerprint.CapProbeNo, 1), nil)}
	mr, clock := newLogResolver(t, log, prober, oneProv(t, infos(logModel)...))
	entered, release := holdFirstAppend(log)

	done := resolveAsync(t.Context(), mr, localKey(logModel))
	waitFor(t, entered, "the held append")
	appendRows(t, other, logRow(b, capabilityToolCall, fingerprint.CapProbeYes, logDigest, logT0.Add(time.Second)))
	clock.set(logT0.Add(2 * time.Second))
	close(release)
	out := waitFor(t, done, "the resolution")
	wantState(t, "the older check", out.state, out.err, fingerprint.CapProbeNo)

	if got := toolCallAnswer(t, store, b, logDigest, logT0.Add(2*time.Second)); got != fingerprint.CapProbeYes {
		t.Fatalf("answer after the late append = %q; want the newer yes", got)
	}
}

func TestResolveToolCall_CompletionTimeTakenBeforeCallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fingerprints.db")
	store, other := openEvidenceLog(t, path), openEvidenceLog(t, path)
	b := openAIBinding(t)
	rec := &progressRecorder{}
	held, release := make(chan struct{}), make(chan struct{})
	rec.hook = func(e CapabilityProbeProgress) {
		if e.Stage == probeStageFinished {
			close(held)
			<-release
		}
	}
	prober := &capProbeFake{run: scripted(toolsResult(fingerprint.CapProbeNo, 1), nil)}
	mr, clock := newLogResolver(t, store, prober, oneProv(t, infos(logModel)...), WithCapabilityProbeProgress(rec.record))

	done := resolveAsync(t.Context(), mr, localKey(logModel))
	waitFor(t, held, "the finished callback")
	clock.set(logT0.Add(2 * time.Second))
	appendRows(t, other, logRow(b, capabilityToolCall, fingerprint.CapProbeYes, logDigest, logT0.Add(time.Second)))
	close(release)
	out := waitFor(t, done, "the resolution")
	wantState(t, "the check", out.state, out.err, fingerprint.CapProbeNo)

	for _, r := range storedRows(t, store, b, logModel) {
		if r.Method == fingerprint.MethodChatStreamTools && r.State == fingerprint.CapProbeNo && !r.ObservedAt.Equal(logT0) {
			t.Fatalf("probe row ObservedAt = %v; want %v, from before the callback", r.ObservedAt, logT0)
		}
	}
	if got := toolCallAnswer(t, store, b, logDigest, logT0.Add(2*time.Second)); got != fingerprint.CapProbeYes {
		t.Fatalf("answer = %q; want the newer yes", got)
	}
}

// switchBinder answers a binding for "local" that a test can change.
type switchBinder struct {
	current atomic.Pointer[EvidenceBinding]
}

func newSwitchBinder(b EvidenceBinding) *switchBinder {
	s := &switchBinder{}
	s.current.Store(&b)
	return s
}

func (s *switchBinder) set(b EvidenceBinding) { s.current.Store(&b) }

func (s *switchBinder) bind(name string) (EvidenceBinding, bool) {
	return *s.current.Load(), name == "local"
}

func TestResolveToolCall_BindingChangedDuringPreparation(t *testing.T) {
	a := openAIBinding(t)
	other := mustBinding(t, "http://127.0.0.1:9090", "openai-compat")
	key := localKey(logModel)

	t.Run("after the prober is built", func(t *testing.T) {
		store := newEvidenceLog(t)
		binder := newSwitchBinder(a)
		prober := &capProbeFake{}
		factory := func(context.Context, ModelKey, *ModelInfo, Provider) (*FingerprintProberSpec, error) {
			binder.set(other)
			return &FingerprintProberSpec{Prober: prober}, nil
		}
		mr, _ := newLogResolver(t, store, nil, oneProv(t, infos(logModel)...),
			WithEvidenceLog(store, binder.bind), WithCapabilityProber(factory))
		state, err := mr.ResolveToolCall(t.Context(), key)
		wantErrIs(t, "resolve", state, err, ErrEvidenceUnbound)
		if n := prober.callCount(); n != 0 {
			t.Fatalf("calls = %d; want 0", n)
		}
		if n := len(storedRows(t, store, a, logModel)) + len(storedRows(t, store, other, logModel)); n != 0 {
			t.Fatalf("rows = %d; want 0", n)
		}
	})
	t.Run("during the admission wait", func(t *testing.T) {
		store := newEvidenceLog(t)
		binder := newSwitchBinder(a)
		prober := &capProbeFake{}
		mr, _ := newLogResolver(t, store, prober, oneProv(t, infos(logModel)...), WithEvidenceLog(store, binder.bind))
		adm := newHoldAdmitter()
		adm.onAcquire = func(ModelKey) { binder.set(other) }
		mr.setSlotAdmitter(adm)
		state, err := mr.ResolveToolCall(t.Context(), key)
		wantErrIs(t, "resolve", state, err, ErrEvidenceUnbound)
		if n := prober.callCount(); n != 0 {
			t.Fatalf("calls = %d; want 0", n)
		}
		if n := len(storedRows(t, store, a, logModel)) + len(storedRows(t, store, other, logModel)); n != 0 {
			t.Fatalf("rows = %d; want 0", n)
		}
		if n := adm.releaseCount(); n != 1 {
			t.Fatalf("releases = %d; want 1", n)
		}
	})
}

func TestResolveToolCall_PermitReleasedOnEveryExit(t *testing.T) {
	a := openAIBinding(t)
	moved := mustBinding(t, "http://127.0.0.1:9090", "openai-compat")
	type fixture struct {
		mr     *ModelRegistry
		adm    *holdAdmitter
		log    *hookedLog
		other  fingerprint.EvidenceStore
		binder *switchBinder
		prober *capProbeFake
		cancel context.CancelFunc
	}
	cases := []struct {
		name      string
		arrange   func(t *testing.T, f *fixture)
		wantState fingerprint.CapProbeState
		wantErr   error
	}{
		{"answer found after admission", func(t *testing.T, f *fixture) {
			f.adm.onAcquire = func(ModelKey) {
				appendRows(t, f.other, logRow(a, capabilityToolCall, fingerprint.CapProbeYes, logDigest, logT0))
			}
		}, fingerprint.CapProbeYes, nil},
		{"cooldown found after admission", func(t *testing.T, f *fixture) {
			f.adm.onAcquire = func(ModelKey) {
				appendRows(t, f.other, attemptRow(a, logDigest, logT0, fingerprint.DetailToolIgnored))
			}
		}, fingerprint.CapProbeInconclusive, nil},
		{"log unreadable after admission", func(t *testing.T, f *fixture) {
			f.adm.onAcquire = func(ModelKey) { f.log.failReads() }
		}, "", ErrEvidenceUnreadable},
		{"binding changed", func(t *testing.T, f *fixture) {
			f.adm.onAcquire = func(ModelKey) { f.binder.set(moved) }
		}, "", ErrEvidenceUnbound},
		{"reservation refused", func(t *testing.T, f *fixture) {
			f.adm.onAcquire = func(ModelKey) { setAllowance(f.mr, 0) }
		}, "", ErrAutomaticProbeBudgetExhausted},
		{"prober error", func(t *testing.T, f *fixture) {
			f.prober.setRun(scripted(fingerprint.CapabilityProbeResult{Requests: 1}, errInjected))
		}, "", errInjected},
		{"cancellation", func(t *testing.T, f *fixture) {
			f.prober.setRun(func(ctx context.Context, _ string) (fingerprint.CapabilityProbeResult, error) {
				f.cancel()
				<-ctx.Done()
				return fingerprint.CapabilityProbeResult{Requests: 1}, ctx.Err()
			})
		}, "", context.Canceled},
		{"success", func(*testing.T, *fixture) {}, fingerprint.CapProbeYes, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fingerprints.db")
			f := &fixture{
				log:    &hookedLog{EvidenceStore: openEvidenceLog(t, path)},
				other:  openEvidenceLog(t, path),
				binder: newSwitchBinder(a),
				prober: &capProbeFake{},
				adm:    newHoldAdmitter(),
			}
			f.mr, _ = newLogResolver(t, f.log, f.prober, oneProv(t, infos(logModel)...), WithEvidenceLog(f.log, f.binder.bind))
			f.mr.setSlotAdmitter(f.adm)
			releasesAtAppend := -1
			f.log.setOnAppend(func(context.Context, []fingerprint.EvidenceRow) error {
				releasesAtAppend = f.adm.releaseCount()
				return nil
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.cancel = cancel
			tc.arrange(t, f)

			state, err := f.mr.ResolveToolCall(ctx, localKey(logModel))
			if state != tc.wantState || (tc.wantErr == nil) != (err == nil) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("ResolveToolCall = %q, %v; want %q, %v", state, err, tc.wantState, tc.wantErr)
			}
			if n := f.adm.releaseCount(); n != 1 {
				t.Fatalf("releases = %d; want exactly 1", n)
			}
			if len(f.log.appendCalls()) > 0 && releasesAtAppend != 1 {
				t.Fatalf("releases at the append = %d; want 1 (release before the append)", releasesAtAppend)
			}
			actx, acancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer acancel()
			rel, err := f.adm.acquireSlot(actx, localKey(logModel))
			if err != nil {
				t.Fatalf("another caller could not acquire the slot: %v", err)
			}
			rel()
		})
	}
}

// ---------------------------------------------------------------------------
// Unbound and unreadable
// ---------------------------------------------------------------------------

func TestResolveToolCall_UnboundProviderStartsNoProbe(t *testing.T) {
	log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
	prober := &capProbeFake{}
	rec := &progressRecorder{}
	mr, _ := newLogResolver(t, log, prober, oneProv(t, infos(logModel)...),
		WithEvidenceLog(log, func(string) (EvidenceBinding, bool) { return EvidenceBinding{}, false }),
		WithCapabilityProbeProgress(rec.record))
	state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
	wantErrIs(t, "resolve", state, err, ErrEvidenceUnbound)
	if prober.callCount() != 0 || allowanceOf(mr) != automaticProbeAllowance || len(log.appendCalls()) != 0 || len(rec.list()) != 0 {
		t.Fatalf("calls=%d allowance=%d appends=%d events=%d; want 0, full, 0, 0",
			prober.callCount(), allowanceOf(mr), len(log.appendCalls()), len(rec.list()))
	}
}

func TestResolveToolCall_UnreadableLogStartsNoProbe(t *testing.T) {
	cases := map[string]func(log *hookedLog, adm *holdAdmitter){
		"first read":           func(log *hookedLog, _ *holdAdmitter) { log.failReads() },
		"read after admission": func(log *hookedLog, adm *holdAdmitter) { adm.onAcquire = func(ModelKey) { log.failReads() } },
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
			prober := &capProbeFake{}
			rec := &progressRecorder{}
			mr, _ := newLogResolver(t, log, prober, oneProv(t, infos(logModel)...), WithCapabilityProbeProgress(rec.record))
			adm := newHoldAdmitter()
			mr.setSlotAdmitter(adm)
			arrange(log, adm)
			state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
			wantErrIs(t, "resolve", state, err, ErrEvidenceUnreadable)
			if prober.callCount() != 0 || allowanceOf(mr) != automaticProbeAllowance || len(log.appendCalls()) != 0 || len(rec.list()) != 0 {
				t.Fatalf("calls=%d allowance=%d appends=%d events=%d; want 0, full, 0, 0",
					prober.callCount(), allowanceOf(mr), len(log.appendCalls()), len(rec.list()))
			}
		})
	}
}

func TestResolveToolCall_UnreadableLogCancelledCaller(t *testing.T) {
	log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
	prober := &capProbeFake{}
	mr, _ := newLogResolver(t, log, prober, oneProv(t, infos(logModel)...))
	if _, err := mr.Lookup(t.Context(), localKey(logModel)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	log.setOnRead(func(context.Context) error {
		cancel()
		return errInjected
	})
	state, err := mr.ResolveToolCall(ctx, localKey(logModel))
	if state != "" || !errors.Is(err, context.Canceled) || errors.Is(err, ErrEvidenceUnreadable) {
		t.Fatalf("ResolveToolCall = %q, %v; want the caller's cancellation", state, err)
	}
	if n := prober.callCount(); n != 0 {
		t.Fatalf("calls = %d; want 0", n)
	}
}

func TestResolveToolCall_DeclarationsAnswerWhileUnreadable(t *testing.T) {
	cases := []struct {
		name  string
		info  ModelInfo
		setup func(mr *ModelRegistry, key ModelKey)
	}{
		{"explicit", ModelInfo{Name: logModel}, func(mr *ModelRegistry, key ModelKey) {
			mr.SetCapabilityOverride(func(k ModelKey) []string {
				if k == key {
					return []string{"chat", "stream", "tool_call"}
				}
				return nil
			})
		}},
		{"catalog", ModelInfo{Name: "qwen2.5:7b"}, func(*ModelRegistry, ModelKey) {}},
		{"runtime", ModelInfo{Name: logModel, Capabilities: []string{"completion", "tools"}}, func(*ModelRegistry, ModelKey) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
			log.failReads()
			prober := &capProbeFake{}
			mr, _ := newLogResolver(t, log, prober, oneProv(t, tc.info))
			key := localKey(tc.info.Name)
			tc.setup(mr, key)
			state, err := mr.ResolveToolCall(t.Context(), key)
			wantState(t, tc.name, state, err, fingerprint.CapProbeYes)
			if n := prober.callCount(); n != 0 {
				t.Fatalf("calls = %d; want 0", n)
			}
		})
	}
}

func TestResolveToolCall_RecoversWhenLogReadsAgain(t *testing.T) {
	log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
	prober := &capProbeFake{}
	mr, _ := newLogResolver(t, log, prober, oneProv(t, infos(logModel)...))
	log.failReads()
	state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
	wantErrIs(t, "unreadable", state, err, ErrEvidenceUnreadable)
	log.setOnRead(nil)
	state, err = mr.ResolveToolCall(t.Context(), localKey(logModel))
	wantState(t, "readable again", state, err, fingerprint.CapProbeYes)
	if n := prober.callCount(); n != 1 {
		t.Fatalf("calls = %d; want 1", n)
	}
}

func TestResolveToolCall_MemoryOnlyStoreWorks(t *testing.T) {
	root := t.TempDir()
	getenv := func(k string) string {
		if k == "XDG_DATA_HOME" {
			return filepath.Join(root, "data") // inside the workspace, so memory-only
		}
		return ""
	}
	shared, err := fingerprint.OpenSharedStore(t.Context(), getenv, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.Close() })
	if shared.Status() != fingerprint.StoreMemoryOnly {
		t.Fatalf("store status = %q; want memory-only", shared.Status())
	}
	prober := &capProbeFake{}
	mr, _ := newLogResolver(t, shared.Log(), prober, oneProv(t, infos(logModel)...))
	state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
	wantState(t, "resolve", state, err, fingerprint.CapProbeYes)
	if n := len(storedRows(t, shared.Log(), openAIBinding(t), logModel)); n != 3 {
		t.Fatalf("rows = %d; want 3", n)
	}
	wantCaps(t, "profile", lookupCaps(t, mr, "local"), agentCaps)
}

// ---------------------------------------------------------------------------
// No step without a caller request
// ---------------------------------------------------------------------------

func TestNoProbeWithoutCallerRequest(t *testing.T) {
	t.Run("probing off", func(t *testing.T) {
		prober := &capProbeFake{}
		mr, err := NewModelRegistry(&mrMockProviderRegistry{providers: map[string]Provider{
			"local": newStubProvider("local", infos(logModel)...),
		}}, nil, WithCapabilityProber(capProberFactory(prober)))
		if err != nil {
			t.Fatal(err)
		}
		mr.SetCapabilityFloor(func(ModelKey) []string { return []string{"chat", "stream"} })
		router := NewRouter(mr, NewRegistry())
		t.Cleanup(func() { _ = router.Close() })
		_, _ = router.Route(t.Context(), RoutingRequest{Model: "local/" + logModel, RequiredCaps: agentCaps})
		state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
		wantState(t, "resolve", state, err, "")
		if n := prober.callCount(); n != 0 {
			t.Fatalf("calls = %d; want 0", n)
		}
	})
	// Each step runs on its own fresh registry, so it meets cold profiles and a
	// full allowance and stands alone. Profiling from lookups is
	// TestLogBoundRegistryDoesNotProfile's.
	type fixture struct {
		mr     *ModelRegistry
		router *Router
		log    *hookedLog
		prober *capProbeFake
	}
	key := localKey(logModel)
	steps := []struct {
		name string
		run  func(ctx context.Context, f fixture) error
	}{
		{"Lookup", func(ctx context.Context, f fixture) error { _, err := f.mr.Lookup(ctx, key); return err }},
		{"LookupAny", func(ctx context.Context, f fixture) error { _, err := f.mr.LookupAny(ctx, logModel); return err }},
		{"All", func(ctx context.Context, f fixture) error { _, err := f.mr.All(ctx); return err }},
		{"Recommend", func(ctx context.Context, f fixture) error { _, err := f.mr.Recommend(ctx, RecommendOpts{}); return err }},
		{"Refresh", func(ctx context.Context, f fixture) error { _, err := f.mr.Refresh(ctx, key); return err }},
		{"ExplainToolCall", func(ctx context.Context, f fixture) error { _, err := f.mr.ExplainToolCall(ctx, key); return err }},
		{"listing projection", func(ctx context.Context, f fixture) error {
			_, err := f.mr.ProjectListedModels(ctx, "local", infos(logModel))
			return err
		}},
		{"route without tool calls", func(ctx context.Context, f fixture) error {
			_, err := f.router.Route(ctx, RoutingRequest{Model: "local/" + logModel, RequiredCaps: CapChat | CapStream})
			return err
		}},
	}
	for _, step := range steps {
		t.Run("enabled/"+step.name, func(t *testing.T) {
			ctx := t.Context()
			f := fixture{log: &hookedLog{EvidenceStore: newEvidenceLog(t)}, prober: &capProbeFake{}}
			provReg := NewRegistry()
			if err := provReg.Register(newStubProvider("local", infos(logModel)...)); err != nil {
				t.Fatal(err)
			}
			if err := provReg.RefreshModels(ctx, "local"); err != nil {
				t.Fatal(err)
			}
			b := openAIBinding(t)
			var err error
			f.mr, err = NewModelRegistry(provReg, nil,
				WithEvidenceLog(f.log, func(string) (EvidenceBinding, bool) { return b, true }),
				WithCapabilityProber(capProberFactory(f.prober)))
			if err != nil {
				t.Fatal(err)
			}
			f.mr.SetCapabilityFloor(func(ModelKey) []string { return []string{"chat", "stream"} })
			f.router = NewRouter(f.mr, provReg)
			t.Cleanup(func() { _ = f.router.Close() })

			if err := step.run(ctx, f); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
			if n := f.prober.callCount(); n != 0 || allowanceOf(f.mr) != automaticProbeAllowance || len(f.log.appendCalls()) != 0 {
				t.Fatalf("%s: calls=%d allowance=%d appends=%d; want 0, full, 0", step.name, n, allowanceOf(f.mr), len(f.log.appendCalls()))
			}
			// Positive control: the counter sees a caller's request.
			if _, err := f.mr.ResolveToolCall(ctx, key); err != nil || f.prober.callCount() != 1 {
				t.Fatalf("control: ResolveToolCall err=%v calls=%d; want one check", err, f.prober.callCount())
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cooldown: the allowance is full and every registry is new, so only the
// cooldown can hold a check back.
// ---------------------------------------------------------------------------

func testCooldown(t *testing.T, detail string) {
	b := openAIBinding(t)
	key := localKey(logModel)
	assertCooled := func(t *testing.T, mr *ModelRegistry, prober *capProbeFake) {
		t.Helper()
		if n := allowanceOf(mr); n != automaticProbeAllowance {
			t.Fatalf("precondition: allowance = %d; want full", n)
		}
		state, err := mr.ResolveToolCall(t.Context(), key)
		wantState(t, "in cooldown", state, err, fingerprint.CapProbeInconclusive)
		if n := prober.callCount(); n != 0 {
			t.Fatalf("calls = %d; want 0", n)
		}
		exp, err := mr.ExplainToolCall(t.Context(), key)
		if err != nil || exp.Has || exp.State != fingerprint.CapProbeInconclusive {
			t.Fatalf("explanation = %+v, %v; want no tool_call, inconclusive", exp, err)
		}
		if lookupCaps(t, mr, "local").Has(CapToolCall) {
			t.Fatal("profile gained tool_call")
		}
	}
	t.Run("seeded attempt", func(t *testing.T) {
		store := newEvidenceLog(t)
		appendRows(t, store, attemptRow(b, logDigest, logT0, detail))
		prober := &capProbeFake{}
		mr, clock := newLogResolver(t, store, prober, oneProv(t, infos(logModel)...))
		clock.set(logT0.Add(time.Hour))
		assertCooled(t, mr, prober)
	})
	t.Run("attempt made by another registry", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "fingerprints.db")
		res := toolsResult(fingerprint.CapProbeInconclusive, 1)
		res.Answers[2].Detail = detail
		first, _ := newLogResolver(t, openEvidenceLog(t, path), &capProbeFake{run: scripted(res, nil)}, oneProv(t, infos(logModel)...))
		state, err := first.ResolveToolCall(t.Context(), key)
		wantState(t, "first registry", state, err, fingerprint.CapProbeInconclusive)
		prober := &capProbeFake{}
		second, _ := newLogResolver(t, openEvidenceLog(t, path), prober, oneProv(t, infos(logModel)...))
		assertCooled(t, second, prober)
	})
}

func TestResolveToolCall_InconclusiveCooldown(t *testing.T) {
	testCooldown(t, fingerprint.DetailToolIgnored)
}

func TestResolveToolCall_InvalidToolCallCooldown(t *testing.T) {
	testCooldown(t, fingerprint.DetailToolCallInvalid)
}

func TestResolveToolCall_InconclusiveExpiresAndRetries(t *testing.T) {
	store := newEvidenceLog(t)
	b := openAIBinding(t)
	row := attemptRow(b, logDigest, logT0, fingerprint.DetailToolIgnored)
	appendRows(t, store, row)
	prober := &capProbeFake{}
	mr, clock := newLogResolver(t, store, prober, oneProv(t, infos(logModel)...))

	clock.set(row.ExpiresAt)
	state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
	wantState(t, "at ExpiresAt", state, err, fingerprint.CapProbeInconclusive)
	if n := prober.callCount(); n != 0 {
		t.Fatalf("calls at ExpiresAt = %d; want 0", n)
	}
	clock.set(row.ExpiresAt.Add(time.Nanosecond))
	state, err = mr.ResolveToolCall(t.Context(), localKey(logModel))
	wantState(t, "after ExpiresAt", state, err, fingerprint.CapProbeYes)
	if n := prober.callCount(); n != 1 {
		t.Fatalf("calls after ExpiresAt = %d; want 1", n)
	}
}

func TestResolveToolCall_CooldownDoesNotCrossIdentity(t *testing.T) {
	b := openAIBinding(t)
	native := mustBinding(t, resolveEndpoint, "ollama")
	otherAdapter := attemptRow(native, logDigest, logT0, "")
	otherAdapter.Method, otherAdapter.Source = fingerprint.MethodOllamaShow, fingerprint.EvidenceSourceProvider
	cases := map[string]fingerprint.EvidenceRow{
		"another digest":    attemptRow(b, "sha256:other", logT0, fingerprint.DetailToolIgnored),
		"the other adapter": otherAdapter,
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			store := newEvidenceLog(t)
			appendRows(t, store, row)
			prober := &capProbeFake{}
			mr, clock := newLogResolver(t, store, prober, oneProv(t, infos(logModel)...))
			clock.set(logT0.Add(time.Hour))
			state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
			wantState(t, name, state, err, fingerprint.CapProbeYes)
			if n := prober.callCount(); n != 1 {
				t.Fatalf("calls = %d; want 1", n)
			}
		})
	}
}

func TestResolveToolCall_RechecksEvidenceAfterAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fingerprints.db")
	other := openEvidenceLog(t, path)
	b := openAIBinding(t)
	prober := &capProbeFake{}
	mr, _ := newLogResolver(t, openEvidenceLog(t, path), prober, oneProv(t, infos(logModel)...))
	adm := newHoldAdmitter()
	gate := adm.holdKey(localKey(logModel))
	mr.setSlotAdmitter(adm)

	done := resolveAsync(t.Context(), mr, localKey(logModel))
	waitFor(t, adm.entered, "the admission wait")
	appendRows(t, other, logRow(b, capabilityToolCall, fingerprint.CapProbeYes, logDigest, logT0))
	close(gate)
	out := waitFor(t, done, "the resolution")
	wantState(t, "after admission", out.state, out.err, fingerprint.CapProbeYes)
	if n := prober.callCount(); n != 0 {
		t.Fatalf("calls = %d; want 0", n)
	}
	if n := allowanceOf(mr); n != automaticProbeAllowance {
		t.Fatalf("allowance = %d; want untouched", n)
	}
}

// ---------------------------------------------------------------------------
// Allowance
// ---------------------------------------------------------------------------

// spendAll is a run that reports 2 requests, the whole allowance.
func spendAll(context.Context, string) (fingerprint.CapabilityProbeResult, error) {
	return toolsResult(fingerprint.CapProbeYes, 2), nil
}

func TestAutomaticProbeBudget_SharedAcrossModelsAndTurns(t *testing.T) {
	ctx := t.Context()
	prober := &capProbeFake{run: spendAll}
	provReg := NewRegistry()
	if err := provReg.Register(newStubProvider("local", infos("probe-a", "probe-b")...)); err != nil {
		t.Fatal(err)
	}
	mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos("probe-a", "probe-b")...))
	router := NewRouter(mr, provReg)
	t.Cleanup(func() { _ = router.Close() })

	if _, err := router.Route(ctx, RoutingRequest{Model: "local/probe-a", RequiredCaps: agentCaps}); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	// The later turn uses a new unresolved key.
	// The route error joins its diagnostics as text, so the sentinel is matched
	// by its fixed message there.
	_, err := router.Route(ctx, RoutingRequest{Model: "local/probe-b", RequiredCaps: agentCaps})
	if !errors.Is(err, ErrNoViableCandidate) || !strings.Contains(err.Error(), ErrAutomaticProbeBudgetExhausted.Error()) {
		t.Fatalf("second turn: %v; want a refusal carrying the exhaustion", err)
	}
	if got := prober.callList(); !slices.Equal(got, []string{"probe-a"}) {
		t.Fatalf("checks = %v; want only probe-a", got)
	}
}

func TestAutomaticProbeBudget_DirectResolveToolCallSpendsIt(t *testing.T) {
	ensure := func(t *testing.T, mr *ModelRegistry, model string) error {
		p, err := mr.Lookup(t.Context(), localKey(model))
		if err != nil {
			t.Fatal(err)
		}
		_, diags := mr.EnsureToolCallResolved(t.Context(), []*ModelProfile{p}, agentCaps)
		return errors.Join(diags...)
	}
	direct := func(t *testing.T, mr *ModelRegistry, model string) error {
		_, err := mr.ResolveToolCall(t.Context(), localKey(model))
		return err
	}
	orders := map[string][2]func(*testing.T, *ModelRegistry, string) error{
		"direct then ensure": {direct, ensure},
		"ensure then direct": {ensure, direct},
	}
	for name, order := range orders {
		t.Run(name, func(t *testing.T) {
			prober := &capProbeFake{run: spendAll}
			mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos("probe-a", "probe-b")...))
			if err := order[0](t, mr, "probe-a"); err != nil {
				t.Fatalf("first: %v", err)
			}
			if err := order[1](t, mr, "probe-b"); !errors.Is(err, ErrAutomaticProbeBudgetExhausted) {
				t.Fatalf("second: %v; want the exhaustion error", err)
			}
			if n := prober.callCount(); n != 1 {
				t.Fatalf("calls = %d; want 1", n)
			}
		})
	}
}

func TestAutomaticProbeBudget_OneToolsCheckPerInvocation(t *testing.T) {
	prober := &capProbeFake{} // a yes reporting 1 request
	mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos("probe-a", "probe-b")...))
	state, err := mr.ResolveToolCall(t.Context(), localKey("probe-a"))
	wantState(t, "first model", state, err, fingerprint.CapProbeYes)
	if n := allowanceOf(mr); n != 1 {
		t.Fatalf("remaining = %d; want exactly 1", n)
	}
	state, err = mr.ResolveToolCall(t.Context(), localKey("probe-b"))
	wantErrIs(t, "second model", state, err, ErrAutomaticProbeBudgetExhausted)
	if n := prober.callCount(); n != 1 {
		t.Fatalf("calls = %d; want 1", n)
	}
}

func TestAutomaticProbeBudget_ConcurrentReservationAndRefund(t *testing.T) {
	for _, requests := range []int{0, 1, 2} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d requests, error %v", requests, fail), func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				prober := &capProbeFake{}
				prober.setRun(func(_ context.Context, model string) (fingerprint.CapabilityProbeResult, error) {
					if model != "probe-a" {
						return toolsResult(fingerprint.CapProbeYes, 1), nil
					}
					close(entered)
					<-release
					res := toolsResult(fingerprint.CapProbeYes, requests)
					if fail {
						return fingerprint.CapabilityProbeResult{Requests: requests}, errInjected
					}
					return res, nil
				})
				mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos("probe-a", "probe-b", "probe-c")...))
				done := resolveAsync(t.Context(), mr, localKey("probe-a"))
				waitFor(t, entered, "probe-a's check")
				// Overlapped on purpose: probe-a holds the whole reservation.
				state, err := mr.ResolveToolCall(t.Context(), localKey("probe-b"))
				wantErrIs(t, "probe-b during probe-a", state, err, ErrAutomaticProbeBudgetExhausted)
				close(release)
				<-done
				if got, want := allowanceOf(mr), automaticProbeAllowance-requests; got != want {
					t.Fatalf("remaining = %d; want %d", got, want)
				}
				state, err = mr.ResolveToolCall(t.Context(), localKey("probe-c"))
				if requests == 0 {
					wantState(t, "probe-c after a full refund", state, err, fingerprint.CapProbeYes)
				} else {
					wantErrIs(t, "probe-c", state, err, ErrAutomaticProbeBudgetExhausted)
				}
			})
		}
	}
}

func TestAutomaticProbeBudget_ExhaustedReportedOncePerModel(t *testing.T) {
	rec := &progressRecorder{}
	prober := &capProbeFake{run: spendAll}
	mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos("probe-a", "probe-b")...), WithCapabilityProbeProgress(rec.record))
	if _, err := mr.ResolveToolCall(t.Context(), localKey("probe-a")); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		state, err := mr.ResolveToolCall(t.Context(), localKey("probe-b"))
		wantErrIs(t, fmt.Sprintf("attempt %d", i), state, err, ErrAutomaticProbeBudgetExhausted)
	}
	if got := rec.stages(localKey("probe-b")); !slices.Equal(got, []string{probeStageBudgetExhausted}) {
		t.Fatalf("probe-b events = %v; want exactly one budget_exhausted", got)
	}
	e := rec.list()[len(rec.list())-1]
	if e.EndpointHost != "127.0.0.1:8080" || e.Method != fingerprint.MethodChatStreamTools || e.RemainingRequests != 0 || e.MaxOutputTokensPerRequest != 2048 {
		t.Fatalf("budget_exhausted event = %+v", e)
	}
}

func TestAutomaticProbeBudget_ExhaustedIsUnknownAndUnsaved(t *testing.T) {
	store := newEvidenceLog(t)
	prober := &capProbeFake{run: spendAll}
	mr, _ := newLogResolver(t, store, prober, oneProv(t, infos("probe-a", "probe-b")...))
	if _, err := mr.ResolveToolCall(t.Context(), localKey("probe-a")); err != nil {
		t.Fatal(err)
	}
	state, err := mr.ResolveToolCall(t.Context(), localKey("probe-b"))
	wantErrIs(t, "refused", state, err, ErrAutomaticProbeBudgetExhausted)
	if n := len(storedRows(t, store, openAIBinding(t), "probe-b")); n != 0 {
		t.Fatalf("rows for probe-b = %d; want 0", n)
	}
	exp, err := mr.ExplainToolCall(t.Context(), localKey("probe-b"))
	if err != nil || exp.State != "" || exp.Has {
		t.Fatalf("explanation = %+v, %v; want no state", exp, err)
	}
	// No cooldown: with the allowance back, the next call checks.
	setAllowance(mr, automaticProbeAllowance)
	state, err = mr.ResolveToolCall(t.Context(), localKey("probe-b"))
	wantState(t, "after a refill", state, err, fingerprint.CapProbeYes)
	if got := prober.callList(); !slices.Equal(got, []string{"probe-a", "probe-b"}) {
		t.Fatalf("checks = %v", got)
	}
}

func TestAutomaticProbeBudget_ExhaustedDoesNotWaitForAdmission(t *testing.T) {
	prober := &capProbeFake{run: spendAll}
	mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos("probe-a", "probe-b")...))
	if _, err := mr.ResolveToolCall(t.Context(), localKey("probe-a")); err != nil {
		t.Fatal(err)
	}
	adm := newHoldAdmitter()
	mr.setSlotAdmitter(adm)
	held, err := adm.acquireSlot(t.Context(), localKey("probe-b")) // the test holds B's slot
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	<-adm.entered

	done := resolveAsync(t.Context(), mr, localKey("probe-b"))
	select {
	case out := <-done:
		wantErrIs(t, "probe-b", out.state, out.err, ErrAutomaticProbeBudgetExhausted)
	case <-time.After(2 * time.Second):
		t.Fatal("probe-b waited for admission")
	}
	if got := adm.arrivalList(); len(got) != 1 {
		t.Fatalf("admission arrivals = %v; want only the test's own", got)
	}
}

func TestAutomaticProbeBudget_ClockDoesNotRefill(t *testing.T) {
	prober := &capProbeFake{run: spendAll}
	mr, clock := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos("probe-a", "probe-b")...))
	if _, err := mr.ResolveToolCall(t.Context(), localKey("probe-a")); err != nil {
		t.Fatal(err)
	}
	clock.set(logT0.Add(logDay + time.Hour))
	state, err := mr.ResolveToolCall(t.Context(), localKey("probe-b"))
	wantErrIs(t, "a day later", state, err, ErrAutomaticProbeBudgetExhausted)
}

func TestAutomaticProbeBudget_NoLogPathUnbudgeted(t *testing.T) {
	models := []string{"probe-a", "probe-b", "probe-c"}
	prober := newGatedToolCallProber()
	release := make(chan struct{})
	for _, m := range models {
		prober.outcomes[m] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
		prober.gates[m] = release
	}
	mr := newCapResolveRegistry(t, "local", models, newFakeCapProbeStore(), prober)
	var candidates []*ModelProfile
	for _, m := range models {
		p, err := mr.Lookup(t.Context(), localKey(m))
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, p)
	}
	done := make(chan []*ModelProfile, 1)
	go func() {
		out, _ := mr.EnsureToolCallResolved(t.Context(), candidates, CapToolCall)
		done <- out
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(prober.startedModels()) < len(models) {
		if time.Now().After(deadline) {
			t.Fatalf("started = %v; want all three in flight at once", prober.startedModels())
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	out := waitFor(t, done, "resolution")
	for _, p := range out {
		if !p.Caps.Has(CapToolCall) {
			t.Fatalf("%s not resolved", p.Key)
		}
	}
}

func TestAutomaticProbeBudget_MetadataCheckReservesNothing(t *testing.T) {
	native := mustBinding(t, "http://127.0.0.1:11434", "ollama")
	for _, remaining := range []int{0, 1} {
		t.Run(fmt.Sprintf("remaining %d", remaining), func(t *testing.T) {
			rec := &progressRecorder{}
			prober := &capProbeFake{run: scripted(fingerprint.CapabilityProbeResult{
				Method: fingerprint.MethodOllamaShow, Requests: 1,
				Answers: []fingerprint.ProbeAnswer{{Capability: capabilityToolCall, Source: fingerprint.EvidenceSourceProvider, Kind: fingerprint.EvidenceKindAnswer, State: fingerprint.CapProbeYes}},
			}, nil)}
			mr, _ := newLogResolver(t, newEvidenceLog(t), prober,
				[]logProv{{name: "local", binding: native, models: infos(logModel)}}, WithCapabilityProbeProgress(rec.record))
			setAllowance(mr, remaining)
			state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
			wantState(t, "metadata check", state, err, fingerprint.CapProbeYes)
			if n := prober.callCount(); n != 1 {
				t.Fatalf("calls = %d; want 1", n)
			}
			if req := prober.reqs[0]; req.Method != fingerprint.MethodOllamaShow {
				t.Fatalf("method = %q; want ollama-show", req.Method)
			}
			if n := allowanceOf(mr); n != remaining {
				t.Fatalf("allowance = %d; want unchanged %d", n, remaining)
			}
			if n := len(rec.list()); n != 0 {
				t.Fatalf("progress events = %d; want 0", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Order
// ---------------------------------------------------------------------------

func TestAutomaticProbeBudget_FirstCandidateReservesFirst(t *testing.T) {
	models := []string{"probe-a", "probe-b", "probe-c", "probe-d"}
	prober := &capProbeFake{run: spendAll}
	mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos(models...)...))
	var candidates []*ModelProfile
	for _, m := range models {
		p, err := mr.Lookup(t.Context(), localKey(m))
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, p)
	}
	adm := newHoldAdmitter()
	gate := adm.holdKey(localKey("probe-a"))
	mr.setSlotAdmitter(adm)

	type ensured struct {
		out   []*ModelProfile
		diags []error
	}
	done := make(chan ensured, 1)
	go func() {
		out, diags := mr.EnsureToolCallResolved(t.Context(), candidates, agentCaps)
		done <- ensured{out, diags}
	}()
	if got := waitFor(t, adm.entered, "the first key's admission"); got != localKey("probe-a") {
		t.Fatalf("first admission = %v; want probe-a", got)
	}
	time.Sleep(50 * time.Millisecond) // room for a parallel resolver to show itself
	if got := adm.arrivalList(); len(got) != 1 {
		t.Fatalf("admission arrivals while the first key is held = %v; want only probe-a", got)
	}
	if prober.callCount() != 0 || allowanceOf(mr) != automaticProbeAllowance {
		t.Fatalf("calls=%d allowance=%d while the first key is held; want 0, full", prober.callCount(), allowanceOf(mr))
	}
	close(gate)
	res := waitFor(t, done, "resolution")
	if got := prober.callList(); !slices.Equal(got, []string{"probe-a"}) {
		t.Fatalf("checks = %v; want probe-a only", got)
	}
	if !res.out[0].Caps.Has(CapToolCall) || len(res.diags) != 3 {
		t.Fatalf("probe-a caps = %v, diags = %v; want probe-a qualified and three refusals", res.out[0].Caps, res.diags)
	}
}

func TestRoute_BareChainEntryChecksInProviderIndexOrder(t *testing.T) {
	ctx := t.Context()
	const model = "probe-m"
	provReg := NewRegistry()
	for _, name := range []string{"zeta", "alpha"} { // reverse name order
		if err := provReg.Register(newStubProvider(name, infos(model)...)); err != nil {
			t.Fatal(err)
		}
		if err := provReg.RefreshModels(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	bindings := map[string]EvidenceBinding{
		"zeta":  mustBinding(t, "http://127.0.0.1:8081", "openai-compat"),
		"alpha": mustBinding(t, "http://127.0.0.1:8082", "openai-compat"),
	}
	probers := map[string]*capProbeFake{"zeta": {run: spendAll}, "alpha": {run: spendAll}}
	mr, err := NewModelRegistry(provReg, nil,
		WithEvidenceLog(newEvidenceLog(t), func(name string) (EvidenceBinding, bool) { b, ok := bindings[name]; return b, ok }),
		WithCapabilityProber(func(_ context.Context, key ModelKey, _ *ModelInfo, _ Provider) (*FingerprintProberSpec, error) {
			return &FingerprintProberSpec{Prober: probers[key.Provider]}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	mr.SetCapabilityFloor(func(ModelKey) []string { return []string{"chat", "stream"} })
	listed, err := mr.LookupAny(ctx, model)
	if err != nil || len(listed) != 2 || listed[0].Key.Provider != "zeta" {
		t.Fatalf("precondition: LookupAny order = %v, %v; want zeta first", listed, err)
	}
	router := NewRouter(mr, provReg)
	t.Cleanup(func() { _ = router.Close() })
	_, _ = router.Route(ctx, RoutingRequest{PreferredChain: []string{model}, UseCase: "agent", RequiredCaps: agentCaps})
	if probers["zeta"].callCount() != 1 || probers["alpha"].callCount() != 0 {
		t.Fatalf("checks: zeta %d, alpha %d; want zeta, the first in provider-index order", probers["zeta"].callCount(), probers["alpha"].callCount())
	}
}

// ---------------------------------------------------------------------------
// Progress
// ---------------------------------------------------------------------------

func TestAutomaticProbeProgress_StartedBeforeProbeAndCancelReported(t *testing.T) {
	t.Run("started first, cancelled refunds by the reported count", func(t *testing.T) {
		rec := &progressRecorder{}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var startedAtCall []string
		prober := &capProbeFake{}
		prober.setRun(func(ctx context.Context, _ string) (fingerprint.CapabilityProbeResult, error) {
			startedAtCall = rec.stages(localKey(logModel))
			cancel()
			return fingerprint.CapabilityProbeResult{Requests: 1}, ctx.Err()
		})
		mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos(logModel)...), WithCapabilityProbeProgress(rec.record))
		state, err := mr.ResolveToolCall(ctx, localKey(logModel))
		wantErrIs(t, "cancelled", state, err, context.Canceled)
		if !slices.Equal(startedAtCall, []string{probeStageStarted}) {
			t.Fatalf("events when the prober was called = %v; want [started]", startedAtCall)
		}
		events := rec.list()
		if len(events) != 2 {
			t.Fatalf("events = %+v; want started, cancelled", events)
		}
		started, ended := events[0], events[1]
		if started.ReservedRequests != 2 || started.RemainingRequests != 0 || started.Requests != 0 ||
			started.EndpointHost != "127.0.0.1:8080" || started.Method != fingerprint.MethodChatStreamTools || started.MaxOutputTokensPerRequest != 2048 {
			t.Fatalf("started = %+v", started)
		}
		if ended.Stage != probeStageCancelled || ended.Requests != 1 || ended.ReservedRequests != 2 || ended.RemainingRequests != 1 {
			t.Fatalf("cancelled = %+v; want 1 sent and 1 refunded", ended)
		}
		if n := allowanceOf(mr); n != 1 {
			t.Fatalf("allowance = %d; want 1", n)
		}
	})
	t.Run("callbacks never overlap", func(t *testing.T) {
		rec := &progressRecorder{hook: func(CapabilityProbeProgress) { time.Sleep(5 * time.Millisecond) }}
		entered, release := make(chan struct{}), make(chan struct{})
		prober := &capProbeFake{}
		prober.setRun(func(context.Context, string) (fingerprint.CapabilityProbeResult, error) {
			close(entered)
			<-release
			return toolsResult(fingerprint.CapProbeYes, 2), nil
		})
		models := []string{"probe-a", "probe-b", "probe-c", "probe-d"}
		mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos(models...)...), WithCapabilityProbeProgress(rec.record))
		first := resolveAsync(t.Context(), mr, localKey("probe-a"))
		<-entered
		var refused []<-chan resolveOutcome
		for _, m := range models[1:] {
			refused = append(refused, resolveAsync(t.Context(), mr, localKey(m)))
		}
		for _, ch := range refused {
			out := waitFor(t, ch, "a refused resolution")
			wantErrIs(t, "refused", out.state, out.err, ErrAutomaticProbeBudgetExhausted)
		}
		close(release)
		waitFor(t, first, "the check")
		if rec.overlap.Load() {
			t.Fatal("progress callbacks overlapped")
		}
		if n := len(rec.list()); n != 5 {
			t.Fatalf("events = %d; want started, three budget_exhausted, finished", n)
		}
	})
}

func TestAutomaticProbeBudget_PanicRefundsReservation(t *testing.T) {
	prober := &capProbeFake{}
	prober.setRun(func(_ context.Context, model string) (fingerprint.CapabilityProbeResult, error) {
		if model == "probe-a" {
			panic("prober exploded")
		}
		return toolsResult(fingerprint.CapProbeYes, 1), nil
	})
	mr, _ := newLogResolver(t, newEvidenceLog(t), prober, oneProv(t, infos("probe-a", "probe-b")...))
	adm := newHoldAdmitter()
	mr.setSlotAdmitter(adm)

	recovered := func() (r any) {
		defer func() { r = recover() }()
		_, _ = mr.ResolveToolCall(t.Context(), localKey("probe-a"))
		return nil
	}()
	if recovered == nil {
		t.Fatal("the prober's panic did not reach the caller")
	}
	if n := adm.releaseCount(); n != 1 {
		t.Fatalf("releases after the panic = %d; want exactly 1", n)
	}
	if n := allowanceOf(mr); n != automaticProbeAllowance {
		t.Fatalf("allowance after the panic = %d; want the whole reservation back", n)
	}
	state, err := mr.ResolveToolCall(t.Context(), localKey("probe-b"))
	wantState(t, "a different key after the panic", state, err, fingerprint.CapProbeYes)
}

// ---------------------------------------------------------------------------
// Lost write: a completed check whose append fails is kept for the registry's
// life. Checks report 0 requests unless a case says otherwise, so the
// allowance stays full and cannot be what holds a second check back; each case
// uses a fresh registry, so a profile cached elsewhere cannot answer.
// ---------------------------------------------------------------------------

// failAppends makes every append through the log fail without reaching the
// store.
func (l *hookedLog) failAppends() {
	l.setOnAppend(func(context.Context, []fingerprint.EvidenceRow) error { return errInjected })
}

// keptRows is a copy of the registry's kept rows.
func keptRows(mr *ModelRegistry) []fingerprint.EvidenceRow {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	return slices.Clone(mr.kept)
}

// lostWriteResolver is newLogResolver over a log whose appends fail, with a
// prober whose checks report 0 requests.
func lostWriteResolver(t *testing.T, state fingerprint.CapProbeState, provs []logProv, opts ...ModelRegistryOption) (*ModelRegistry, *logClock, *hookedLog, *capProbeFake) {
	t.Helper()
	log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
	log.failAppends()
	prober := &capProbeFake{run: scripted(toolsResult(state, 0), nil)}
	mr, clock := newLogResolver(t, log, prober, provs, opts...)
	return mr, clock, log, prober
}

// routerOver is a router for mr whose provider registry serves "local" with
// models.
func routerOver(t *testing.T, mr *ModelRegistry, models ...string) *Router {
	t.Helper()
	provReg := NewRegistry()
	if err := provReg.Register(newStubProvider("local", infos(models...)...)); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(mr, provReg)
	t.Cleanup(func() { _ = router.Close() })
	return router
}

func wantCalls(t *testing.T, label string, prober *capProbeFake, want int) {
	t.Helper()
	if n := prober.callCount(); n != want {
		t.Fatalf("%s: calls = %d; want %d", label, n, want)
	}
}

func TestLostWrite_SameCallQualifies(t *testing.T) {
	mr, _, log, prober := lostWriteResolver(t, fingerprint.CapProbeYes, oneProv(t, infos(logModel)...))
	mr.SetCapabilityFloor(nil) // chat and stream come only from the check's rows
	if caps := lookupCaps(t, mr, "local"); caps.Has(CapChat) || caps.Has(CapStream) {
		t.Fatalf("precondition: caps = %v; want neither chat nor stream", caps)
	}
	router := routerOver(t, mr, logModel)
	if _, err := router.Route(t.Context(), RoutingRequest{Model: "local/" + logModel, UseCase: "agent", RequiredCaps: agentCaps}); err != nil {
		t.Fatalf("Route: %v; want the checked model accepted in this call", err)
	}
	wantCalls(t, "route", prober, 1)
	if len(log.appendCalls()) != 1 || len(storedRows(t, log.EvidenceStore, openAIBinding(t), logModel)) != 0 {
		t.Fatal("precondition: want one failed append and nothing stored")
	}
}

func TestLostWrite_LaterCallsReadKeptRows(t *testing.T) {
	ctx := t.Context()
	mr, _, _, prober := lostWriteResolver(t, fingerprint.CapProbeYes, oneProv(t, infos(logModel)...))
	router := routerOver(t, mr, logModel)
	state, err := mr.ResolveToolCall(ctx, localKey(logModel))
	wantState(t, "the check", state, err, fingerprint.CapProbeYes)
	allowance := allowanceOf(mr)

	state, err = mr.ResolveToolCall(ctx, localKey(logModel))
	wantState(t, "a later direct call", state, err, fingerprint.CapProbeYes)
	if _, err := router.Route(ctx, RoutingRequest{Model: "local/" + logModel, UseCase: "agent", RequiredCaps: agentCaps}); err != nil {
		t.Fatalf("Route after a direct call: %v", err)
	}
	wantCalls(t, "later calls", prober, 1)
	if n := allowanceOf(mr); n != allowance {
		t.Fatalf("allowance = %d; want unchanged %d", n, allowance)
	}
	exp, err := mr.ExplainToolCall(ctx, localKey(logModel))
	if err != nil || exp.State != fingerprint.CapProbeYes || !exp.Has || !exp.Unsaved {
		t.Fatalf("explanation = %+v, %v; want a yes marked Unsaved", exp, err)
	}
}

func TestLostWrite_FollowerAndDuplicateSeeIt(t *testing.T) {
	t.Run("a follower of a direct leader", func(t *testing.T) {
		ctx := t.Context()
		mr, _, _, prober := lostWriteResolver(t, fingerprint.CapProbeYes, oneProv(t, infos(logModel)...))
		entered, release := make(chan struct{}), make(chan struct{})
		prober.setRun(func(context.Context, string) (fingerprint.CapabilityProbeResult, error) {
			close(entered)
			<-release
			return toolsResult(fingerprint.CapProbeYes, 0), nil
		})
		candidate, err := mr.Lookup(ctx, localKey(logModel))
		if err != nil {
			t.Fatal(err)
		}
		leader := resolveAsync(ctx, mr, localKey(logModel))
		waitFor(t, entered, "the leader's check")
		type ensured struct {
			out   []*ModelProfile
			diags []error
		}
		follower := make(chan ensured, 1)
		go func() {
			out, diags := mr.EnsureToolCallResolved(ctx, []*ModelProfile{candidate}, agentCaps)
			follower <- ensured{out, diags}
		}()
		// Nothing outside the singleflight shows a join; this is room for the
		// follower to reach it. One that arrived late reads the kept rows instead.
		time.Sleep(50 * time.Millisecond)
		close(release)
		out := waitFor(t, leader, "the leader")
		wantState(t, "leader", out.state, out.err, fingerprint.CapProbeYes)
		got := waitFor(t, follower, "the follower")
		if len(got.diags) != 0 || !got.out[0].Caps.Has(CapToolCall) {
			t.Fatalf("follower candidate caps = %v, diags = %v; want tool_call", got.out[0].Caps, got.diags)
		}
		wantCalls(t, "leader and follower", prober, 1)
	})
	t.Run("the same key twice in one candidate list", func(t *testing.T) {
		ctx := t.Context()
		mr, _, _, prober := lostWriteResolver(t, fingerprint.CapProbeYes, oneProv(t, infos(logModel)...))
		p, err := mr.Lookup(ctx, localKey(logModel))
		if err != nil {
			t.Fatal(err)
		}
		out, diags := mr.EnsureToolCallResolved(ctx, []*ModelProfile{p, p}, agentCaps)
		if len(diags) != 0 || !out[0].Caps.Has(CapToolCall) || !out[1].Caps.Has(CapToolCall) {
			t.Fatalf("caps = %v, %v, diags = %v; want both with tool_call", out[0].Caps, out[1].Caps, diags)
		}
		wantCalls(t, "duplicate", prober, 1)
	})
}

func TestLostWrite_OverrideStillWins(t *testing.T) {
	ctx := t.Context()
	mr, _, _, prober := lostWriteResolver(t, fingerprint.CapProbeYes, oneProv(t, infos(logModel)...))
	prober.setRun(func(context.Context, string) (fingerprint.CapabilityProbeResult, error) {
		mr.SetCapabilityOverride(func(ModelKey) []string { return []string{"chat", "stream"} })
		return toolsResult(fingerprint.CapProbeYes, 0), nil
	})
	p, err := mr.Lookup(ctx, localKey(logModel))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := mr.EnsureToolCallResolved(ctx, []*ModelProfile{p}, agentCaps)
	if out[0].Caps.Has(CapToolCall) {
		t.Fatalf("candidate caps = %v; the override withholds tool_call", out[0].Caps)
	}
	// Control: the kept yes is what the override was hiding.
	mr.SetCapabilityOverride(nil)
	wantCaps(t, "without the override", lookupCaps(t, mr, "local"), agentCaps)
}

func TestLostWrite_KeptAttemptHoldsCooldown(t *testing.T) {
	ctx := t.Context()
	mr, clock, _, prober := lostWriteResolver(t, fingerprint.CapProbeInconclusive, oneProv(t, infos(logModel)...))
	state, err := mr.ResolveToolCall(ctx, localKey(logModel))
	wantState(t, "the check", state, err, fingerprint.CapProbeInconclusive)
	clock.set(logT0.Add(time.Hour))
	state, err = mr.ResolveToolCall(ctx, localKey(logModel))
	wantState(t, "in the cooldown", state, err, fingerprint.CapProbeInconclusive)
	wantCalls(t, "in the cooldown", prober, 1)
	if n := allowanceOf(mr); n != automaticProbeAllowance {
		t.Fatalf("allowance = %d; want full, so only the cooldown held the check", n)
	}
	exp, err := mr.ExplainToolCall(ctx, localKey(logModel))
	if err != nil || exp.State != fingerprint.CapProbeInconclusive || !exp.Unsaved {
		t.Fatalf("explanation = %+v, %v; want an inconclusive marked Unsaved", exp, err)
	}
}

func TestLostWrite_ScopedToEndpoint(t *testing.T) {
	b := openAIBinding(t)
	t.Run("a name re-bound to another endpoint reads nothing", func(t *testing.T) {
		ctx := t.Context()
		binder := newSwitchBinder(b)
		log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
		log.failAppends()
		prober := &capProbeFake{run: scripted(toolsResult(fingerprint.CapProbeYes, 0), nil)}
		mr, _ := newLogResolver(t, log, prober, oneProv(t, infos(logModel)...), WithEvidenceLog(log, binder.bind))
		state, err := mr.ResolveToolCall(ctx, localKey(logModel))
		wantState(t, "the check", state, err, fingerprint.CapProbeYes)
		if exp, _ := mr.ExplainToolCall(ctx, localKey(logModel)); exp.State != fingerprint.CapProbeYes {
			t.Fatalf("precondition: explanation state = %q; want the kept yes", exp.State)
		}
		binder.set(mustBinding(t, "http://127.0.0.1:9090", "openai-compat"))
		if rows := mr.readEvidence(ctx, "local", []string{logModel}).rows[logModel]; len(rows) != 0 {
			t.Fatalf("rows read through the new endpoint = %d; want 0", len(rows))
		}
		if exp, err := mr.ExplainToolCall(ctx, localKey(logModel)); err != nil || exp.State != "" || exp.Has {
			t.Fatalf("explanation = %+v, %v; want nothing", exp, err)
		}
	})
	t.Run("a second name at the same endpoint reads the rows", func(t *testing.T) {
		ctx := t.Context()
		mr, _, _, prober := lostWriteResolver(t, fingerprint.CapProbeYes, []logProv{
			{name: "p1", binding: b, models: infos(logModel)},
			{name: "p2", binding: b, models: infos(logModel)},
		})
		state, err := mr.ResolveToolCall(ctx, ModelKey{Provider: "p1", Model: logModel})
		wantState(t, "p1", state, err, fingerprint.CapProbeYes)
		state, err = mr.ResolveToolCall(ctx, ModelKey{Provider: "p2", Model: logModel})
		wantState(t, "p2", state, err, fingerprint.CapProbeYes)
		wantCalls(t, "p2", prober, 1)
		wantCaps(t, "p2's profile", lookupCaps(t, mr, "p2"), agentCaps)
	})
}

// toolCallReading reduces rows to their tool_call reading, and its answer's
// state, for the fixture route.
func toolCallReading(rows []fingerprint.EvidenceRow, now time.Time) (fingerprint.CapabilityReading, fingerprint.CapProbeState) {
	reading := fingerprint.ReadCapabilities(rows, fingerprint.AdapterOpenAICompat, logDigest, now)[capabilityToolCall]
	if a := readingAnswer(reading); a != nil {
		return reading, a.State
	}
	return reading, ""
}

func TestLostWrite_OrderingMatchesStoredRows(t *testing.T) {
	b := openAIBinding(t)
	at := logT0.Add(time.Second)
	key := []string{logModel}
	// both answers the reading of view's rows in the given order and reversed.
	both := func(t *testing.T, mr *ModelRegistry) (fingerprint.CapProbeState, fingerprint.CapProbeState) {
		t.Helper()
		rows := mr.readEvidence(t.Context(), "local", key).rows[logModel]
		_, forward := toolCallReading(rows, at)
		reversed := slices.Clone(rows)
		slices.Reverse(reversed)
		_, backward := toolCallReading(reversed, at)
		return forward, backward
	}

	t.Run("a newer stored row beats a kept one", func(t *testing.T) {
		ctx := t.Context()
		mr, clock, log, _ := lostWriteResolver(t, fingerprint.CapProbeYes, oneProv(t, infos(logModel)...))
		clock.set(logT0.Add(1500 * time.Microsecond))
		state, err := mr.ResolveToolCall(ctx, localKey(logModel))
		wantState(t, "the check", state, err, fingerprint.CapProbeYes)
		if exp, _ := mr.ExplainToolCall(ctx, localKey(logModel)); exp.State != fingerprint.CapProbeYes || !exp.Unsaved {
			t.Fatalf("precondition: explanation = %+v; want the kept yes", exp)
		}
		appendRows(t, log.EvidenceStore, logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, logT0.Add(1900*time.Microsecond)))
		exp, err := mr.ExplainToolCall(ctx, localKey(logModel))
		if err != nil || exp.State != fingerprint.CapProbeNo || exp.Unsaved {
			t.Fatalf("explanation = %+v, %v; want the stored no, not Unsaved", exp, err)
		}
	})
	t.Run("two kept rows at one instant: the later wins", func(t *testing.T) {
		mr, _ := newLogResolver(t, newEvidenceLog(t), nil, oneProv(t, infos(logModel)...))
		mr.keepUnsavedEvidence([]fingerprint.EvidenceRow{logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, at)})
		mr.keepUnsavedEvidence([]fingerprint.EvidenceRow{logRow(b, capabilityToolCall, fingerprint.CapProbeYes, logDigest, at)})
		kept := keptRows(mr)
		if len(kept) != 2 || kept[0].ID != math.MinInt64 || kept[1].ID != math.MinInt64+1 {
			t.Fatalf("kept IDs = %v; want MinInt64, MinInt64+1", kept)
		}
		if f, r := both(t, mr); f != fingerprint.CapProbeYes || r != fingerprint.CapProbeYes {
			t.Fatalf("answers = %q, %q; want yes in both orders", f, r)
		}
	})
	t.Run("a kept and a stored row at one instant: stored wins", func(t *testing.T) {
		store := newEvidenceLog(t)
		appendRows(t, store, logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, at))
		mr, _ := newLogResolver(t, store, nil, oneProv(t, infos(logModel)...))
		mr.keepUnsavedEvidence([]fingerprint.EvidenceRow{logRow(b, capabilityToolCall, fingerprint.CapProbeYes, logDigest, at)})
		if n := len(mr.readEvidence(t.Context(), "local", key).rows[logModel]); n != 2 {
			t.Fatalf("rows read = %d; want the stored and the kept row", n)
		}
		if f, r := both(t, mr); f != fingerprint.CapProbeNo || r != fingerprint.CapProbeNo {
			t.Fatalf("answers = %q, %q; want the stored no in both orders", f, r)
		}
	})
	t.Run("across slots the choice is the same as for stored rows", func(t *testing.T) {
		probe := logRow(b, capabilityToolCall, fingerprint.CapProbeYes, logDigest, at)
		observed := logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, at)
		observed.Source = fingerprint.EvidenceSourceObserved
		store := newEvidenceLog(t)
		appendRows(t, store, observed)
		mr, _ := newLogResolver(t, store, nil, oneProv(t, infos(logModel)...))
		mr.keepUnsavedEvidence([]fingerprint.EvidenceRow{probe})

		mirror := newEvidenceLog(t)
		appendRows(t, mirror, probe, observed)
		_, want := toolCallReading(storedRows(t, mirror, b, logModel), at)
		if f, r := both(t, mr); f != want || r != want || want == "" {
			t.Fatalf("answers = %q, %q; want %q, as for two stored rows", f, r, want)
		}
	})
}

// withoutIDs is readings with every row's ID zeroed, to compare kept rows with
// the same rows stored.
func withoutIDs(readings map[string]fingerprint.CapabilityReading) map[string]fingerprint.CapabilityReading {
	strip := func(r *fingerprint.EvidenceRow) *fingerprint.EvidenceRow {
		if r == nil {
			return nil
		}
		c := *r
		c.ID = 0
		return &c
	}
	out := make(map[string]fingerprint.CapabilityReading, len(readings))
	for k, c := range readings {
		c.Probe, c.Observed, c.Provider = strip(c.Probe), strip(c.Observed), strip(c.Provider)
		c.Attempt, c.Flag, c.OtherAdapter = strip(c.Attempt), strip(c.Flag), strip(c.OtherAdapter)
		out[k] = c
	}
	return out
}

func TestLostWrite_LateOlderFailureKeepsNewerAttempt(t *testing.T) {
	ctx := t.Context()
	b := openAIBinding(t)
	log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool // not sync.Once: a second append must not wait for the first
	log.setOnAppend(func(context.Context, []fingerprint.EvidenceRow) error {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return errInjected
	})
	prober := &capProbeFake{run: scripted(toolsResult(fingerprint.CapProbeInconclusive, 0), nil)}
	mr, clock := newLogResolver(t, log, prober, []logProv{
		{name: "p1", binding: b, models: infos(logModel)},
		{name: "p2", binding: b, models: infos(logModel)},
	})
	older, newer := logT0, logT0.Add(time.Hour)

	olderDone := resolveAsync(ctx, mr, ModelKey{Provider: "p1", Model: logModel})
	waitFor(t, entered, "the older check's append")
	clock.set(newer)
	state, err := mr.ResolveToolCall(ctx, ModelKey{Provider: "p2", Model: logModel})
	wantState(t, "the newer check", state, err, fingerprint.CapProbeInconclusive)
	close(release)
	out := waitFor(t, olderDone, "the older check")
	wantState(t, "the older check", out.state, out.err, fingerprint.CapProbeInconclusive)
	if n := len(keptRows(mr)); n != 6 {
		t.Fatalf("kept rows = %d; want 6", n)
	}

	// The older attempt has expired; the newer one has not.
	clock.set(older.Add(fingerprint.CapProbeInconclusiveTTL + time.Nanosecond))
	state, err = mr.ResolveToolCall(ctx, ModelKey{Provider: "p1", Model: logModel})
	wantState(t, "past the older expiry", state, err, fingerprint.CapProbeInconclusive)
	wantCalls(t, "past the older expiry", prober, 2)

	mirror := newEvidenceLog(t)
	appendRows(t, mirror, keptRows(mr)...)
	stored := storedRows(t, mirror, b, logModel)
	for _, now := range []time.Time{newer, older.Add(fingerprint.CapProbeInconclusiveTTL + time.Nanosecond), newer.Add(fingerprint.CapProbeInconclusiveTTL + time.Nanosecond)} {
		mr.now = func() time.Time { return now }
		view := mr.readEvidence(ctx, "p1", []string{logModel})
		got := withoutIDs(view.readings(logModel, logDigest))
		want := withoutIDs(fingerprint.ReadCapabilities(stored, b.Adapter, logDigest, now))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("at %v: readings of kept rows = %+v; want %+v, as stored", now, got, want)
		}
		if g, w := view.deadline(logModel, logDigest), fingerprint.NextReadingTransition(stored, b.Adapter, logDigest, now); !g.Equal(w) {
			t.Fatalf("at %v: NextReadingTransition = %v; want %v, as stored", now, g, w)
		}
	}
}

func TestLostWrite_ExternalWriterStillCounts(t *testing.T) {
	b := openAIBinding(t)
	cases := []struct {
		name string
		at   time.Time
		want fingerprint.CapProbeState
	}{
		{"a newer observation", logT0.Add(2 * time.Second), fingerprint.CapProbeNo},
		{"an older observation", logT0, fingerprint.CapProbeYes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "fingerprints.db")
			log := &hookedLog{EvidenceStore: openEvidenceLog(t, path)}
			log.failAppends()
			mr, clock := newLogResolver(t, log, &capProbeFake{run: scripted(toolsResult(fingerprint.CapProbeYes, 0), nil)}, oneProv(t, infos(logModel)...))
			clock.set(logT0.Add(time.Second))
			state, err := mr.ResolveToolCall(ctx, localKey(logModel))
			wantState(t, "the kept check", state, err, fingerprint.CapProbeYes)
			appendRows(t, openEvidenceLog(t, path), logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, tc.at))
			clock.set(logT0.Add(3 * time.Second))
			exp, err := mr.ExplainToolCall(ctx, localKey(logModel))
			if err != nil || exp.State != tc.want {
				t.Fatalf("explanation = %+v, %v; want %q, the newest of both", exp, err, tc.want)
			}
		})
	}
}

// keptYes is the rows of a tools yes for logModel through b at at.
func keptYes(b EvidenceBinding, at time.Time) []fingerprint.EvidenceRow {
	return []fingerprint.EvidenceRow{
		logRow(b, "chat", fingerprint.CapProbeYes, logDigest, at),
		logRow(b, "stream", fingerprint.CapProbeYes, logDigest, at),
		logRow(b, capabilityToolCall, fingerprint.CapProbeYes, logDigest, at),
	}
}

func TestLostWrite_FencesBuildInFlight(t *testing.T) {
	b := openAIBinding(t)
	t.Run("a build that read the log before the rows were kept does not publish", func(t *testing.T) {
		ctx := t.Context()
		mr, prov := logRegistry(t, newEvidenceLog(t), "local", b, logDigest)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		prov.onModels = func() {
			once.Do(func() {
				close(entered)
				<-release
			})
		}
		built := make(chan *ModelProfile, 1)
		go func() {
			p, _ := mr.Lookup(ctx, ModelKey{Provider: "local", Model: logModel})
			built <- p
		}()
		waitFor(t, entered, "the build after its log read")
		mr.keepUnsavedEvidence(keptYes(b, logT0))
		close(release)
		if stale := waitFor(t, built, "the held build"); stale == nil || stale.Caps.Has(CapToolCall) {
			t.Fatalf("precondition: the held build = %v; want one without tool_call", stale)
		}
		wantCaps(t, "the next lookup", lookupCaps(t, mr, "local"), agentCaps)
	})
	t.Run("keeping races no reader", func(t *testing.T) {
		ctx := t.Context()
		mr, _ := newLogResolver(t, newEvidenceLog(t), nil, oneProv(t, infos(logModel)...))
		// Readers run until the keeper is done, so they overlap it. The keeper is
		// bounded: every read reduces all of a model's kept rows.
		const checks = 30
		done := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			defer close(done)
			for i := range checks {
				mr.keepUnsavedEvidence(keptYes(b, logT0.Add(time.Duration(i)*time.Millisecond)))
				time.Sleep(time.Millisecond)
			}
		})
		readers := []func(){
			func() { _, _ = mr.Lookup(ctx, localKey(logModel)) },
			func() { _, _ = mr.ExplainToolCall(ctx, localKey(logModel)) },
			func() { _, _ = mr.ProjectListedModels(ctx, "local", infos(logModel)) },
		}
		for _, read := range readers {
			wg.Go(func() {
				for {
					select {
					case <-done:
						return
					default:
						read()
					}
				}
			})
		}
		wg.Wait()
		if n := len(keptRows(mr)); n != 3*checks {
			t.Fatalf("kept rows = %d; want %d", n, 3*checks)
		}
	})
}

func TestLostWrite_ExpiryRebuilds(t *testing.T) {
	store := newEvidenceLog(t)
	b := openAIBinding(t)
	appendRows(t, store, logRow(b, capabilityToolCall, fingerprint.CapProbeYes, logDigest, logT0))
	mr, clock := newLogResolver(t, store, nil, oneProv(t, infos(logModel)...))
	wantCaps(t, "the stored yes", lookupCaps(t, mr, "local"), agentCaps)

	no := logRow(b, capabilityToolCall, fingerprint.CapProbeNo, logDigest, logT0.Add(time.Second))
	no.ExpiresAt = no.ObservedAt.Add(time.Hour)
	mr.keepUnsavedEvidence([]fingerprint.EvidenceRow{no})
	clock.set(no.ExpiresAt)
	if lookupCaps(t, mr, "local").Has(CapToolCall) {
		t.Fatal("at ExpiresAt the kept no does not count")
	}
	clock.set(no.ExpiresAt.Add(time.Nanosecond))
	wantCaps(t, "the instant after ExpiresAt", lookupCaps(t, mr, "local"), agentCaps)
}

func TestLostWrite_CancelledAppendIsKept(t *testing.T) {
	t.Run("a completed check", func(t *testing.T) {
		log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
		log.setOnAppend(func(ctx context.Context, _ []fingerprint.EvidenceRow) error { return ctx.Err() })
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		prober := &capProbeFake{run: func(context.Context, string) (fingerprint.CapabilityProbeResult, error) {
			cancel()
			return toolsResult(fingerprint.CapProbeYes, 0), nil
		}}
		mr, _ := newLogResolver(t, log, prober, oneProv(t, infos(logModel)...))
		_, _ = mr.ResolveToolCall(ctx, localKey(logModel))
		if n := len(keptRows(mr)); n != 3 {
			t.Fatalf("kept rows = %d; want the check's 3", n)
		}
		state, err := mr.ResolveToolCall(t.Context(), localKey(logModel))
		wantState(t, "a later call", state, err, fingerprint.CapProbeYes)
		wantCalls(t, "a later call", prober, 1)
	})
	t.Run("an interrupted check", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		prober := &capProbeFake{run: func(ctx context.Context, _ string) (fingerprint.CapabilityProbeResult, error) {
			cancel()
			return fingerprint.CapabilityProbeResult{}, ctx.Err()
		}}
		log := &hookedLog{EvidenceStore: newEvidenceLog(t)}
		log.failAppends()
		mr, _ := newLogResolver(t, log, prober, oneProv(t, infos(logModel)...))
		state, err := mr.ResolveToolCall(ctx, localKey(logModel))
		wantErrIs(t, "interrupted", state, err, context.Canceled)
		if n := len(keptRows(mr)); n != 0 {
			t.Fatalf("kept rows = %d; want 0", n)
		}
	})
}

func TestLostWrite_SecondRegistrySeesNothing(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "fingerprints.db")
	log := &hookedLog{EvidenceStore: openEvidenceLog(t, path)}
	log.failAppends()
	first, _ := newLogResolver(t, log, &capProbeFake{run: scripted(toolsResult(fingerprint.CapProbeYes, 0), nil)}, oneProv(t, infos(logModel)...))
	state, err := first.ResolveToolCall(ctx, localKey(logModel))
	wantState(t, "first registry", state, err, fingerprint.CapProbeYes)
	if exp, _ := first.ExplainToolCall(ctx, localKey(logModel)); exp.State != fingerprint.CapProbeYes {
		t.Fatalf("precondition: first registry explanation state = %q; want the kept yes", exp.State)
	}

	second, _ := newLogResolver(t, openEvidenceLog(t, path), nil, oneProv(t, infos(logModel)...))
	if exp, err := second.ExplainToolCall(ctx, localKey(logModel)); err != nil || exp.State != "" || exp.Has {
		t.Fatalf("second registry explanation = %+v, %v; want nothing", exp, err)
	}
	if n := len(storedRows(t, log.EvidenceStore, openAIBinding(t), logModel)); n != 0 {
		t.Fatalf("stored rows = %d; want 0", n)
	}
}

func TestLostWrite_NoLogPathKeepsItsPatch(t *testing.T) {
	store := newFakeCapProbeStore()
	store.saveErr = errInjected
	prober := newFakeToolCallProber()
	prober.outcomes[logModel] = fingerprint.CapProbeOutcome{State: fingerprint.CapProbeYes}
	mr := newCapResolveRegistry(t, "local", []string{logModel}, store, prober)
	p, err := mr.Lookup(t.Context(), localKey(logModel))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := mr.EnsureToolCallResolved(t.Context(), []*ModelProfile{p}, CapToolCall)
	if !out[0].Caps.Has(CapToolCall) || out[0] == p || p.Caps.Has(CapToolCall) {
		t.Fatalf("out caps = %v (same pointer %v), input caps = %v; want a patched copy", out[0].Caps, out[0] == p, p.Caps)
	}
	if n := len(keptRows(mr)); n != 0 {
		t.Fatalf("kept rows = %d; the legacy path keeps none", n)
	}
	if lookupCaps(t, mr, "local").Has(CapToolCall) {
		t.Fatal("the patch reached the registry's profile; it is the caller's copy only")
	}
}
