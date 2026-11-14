package configio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestProbeToolCall_InvalidKey(t *testing.T) {
	r := &fakeResolver{state: fingerprint.CapProbeYes}
	for _, k := range []struct{ prov, model string }{{"", "m"}, {"p", ""}, {"", ""}} {
		out, err := ProbeToolCall(context.Background(), r, key(k.prov, k.model))
		if err == nil {
			t.Fatalf("ProbeToolCall(%q/%q) error = nil; want invalid_argument", k.prov, k.model)
		}
		if code, ok := CodeOf(err); !ok || code != CodeInvalidArgument {
			t.Fatalf("CodeOf = %q, %v; want %q", code, ok, CodeInvalidArgument)
		}
		if out != (ProbeOutcome{}) {
			t.Fatalf("outcome = %+v; want zero on error", out)
		}
	}
	if r.calls != 0 {
		t.Fatalf("resolver called %d times for invalid keys; want 0", r.calls)
	}
}

func TestProbeToolCall_FastPathAnswersSurviveUnwiredRegistry(t *testing.T) {
	// ResolveToolCall answers authoritatively from explicit overrides or
	// merged profiles BEFORE checking probe wiring; the op forwards such
	// answers (resolve-first). Explicit-source answers are durable.
	for _, want := range []fingerprint.CapProbeState{fingerprint.CapProbeYes, fingerprint.CapProbeNo} {
		r := &fakeResolver{state: want, exp: provider.ToolCallExplanation{Source: "explicit"}}
		out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
		if err != nil {
			t.Fatalf("ProbeToolCall() error: %v; a fast-path %q answer must pass through", err, want)
		}
		if out.State != want {
			t.Fatalf("state = %q; want %q", out.State, want)
		}
		if !out.Persisted {
			t.Fatalf("Persisted = false for explicit-source %q; declared knowledge is durable", want)
		}
	}
}

func TestProbeToolCall_EmptyStateNilErrorIsUnavailable(t *testing.T) {
	r := &fakeResolver{state: ""}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err == nil {
		t.Fatal("ProbeToolCall() error = nil for unresolvable model; explicit probe must be loud")
	}
	if code, ok := CodeOf(err); !ok || code != CodeProbeUnavailable {
		t.Fatalf("CodeOf = %q, %v; want %q", code, ok, CodeProbeUnavailable)
	}
	if out != (ProbeOutcome{}) {
		t.Fatalf("outcome = %+v; want zero", out)
	}
}

func TestProbeToolCall_InvalidResolverState(t *testing.T) {
	r := &fakeResolver{state: "bogus"}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if code, ok := CodeOf(err); !ok || code != CodeProbeFailed {
		t.Fatalf("CodeOf(ProbeToolCall(p/m)) = %q, %v; want %q, true", code, ok, CodeProbeFailed)
	}
	if out != (ProbeOutcome{}) {
		t.Fatalf("ProbeToolCall(p/m) outcome = %+v, want zero", out)
	}
	if r.expCalls != 0 {
		t.Fatalf("ProbeToolCall(p/m) ExplainToolCall calls = %d, want 0", r.expCalls)
	}
}

func TestProbeToolCall_PersistedTrueRequiresValidRow(t *testing.T) {
	// The row-match durability branch demands exp.Valid: ExplainToolCall
	// reports stale-row State separately from validity, and a stale
	// same-state row will NOT validate next session.
	valid := &fakeResolver{
		state: fingerprint.CapProbeNo,
		exp:   provider.ToolCallExplanation{Source: "probe", State: fingerprint.CapProbeNo, Valid: true},
	}
	out, err := ProbeToolCall(context.Background(), valid, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if out.State != fingerprint.CapProbeNo || !out.Persisted {
		t.Fatalf("outcome = %+v; want {no, Persisted:true} for a VALID matching row", out)
	}

	// Same state, STALE row (save failed; an old same-verdict row remains):
	// must NOT claim durability.
	stale := &fakeResolver{
		state: fingerprint.CapProbeNo,
		exp:   provider.ToolCallExplanation{Source: "probe", State: fingerprint.CapProbeNo, Valid: false},
	}
	out, err = ProbeToolCall(context.Background(), stale, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if out.State != fingerprint.CapProbeNo || out.Persisted {
		t.Fatalf("outcome = %+v; want {no, Persisted:false} — a stale same-state row is not durable", out)
	}
}

func TestProbeToolCall_PersistedFalseWhenRowLost(t *testing.T) {
	r := &fakeResolver{
		state: fingerprint.CapProbeNo,
		exp:   provider.ToolCallExplanation{Source: "unknown"},
	}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v; lost persistence is a warning, not a failure", err)
	}
	if out.State != fingerprint.CapProbeNo || out.Persisted {
		t.Fatalf("outcome = %+v; want {no, Persisted:false}", out)
	}
}

func TestProbeToolCall_PersistedTrueWhenProfileCarriesYes(t *testing.T) {
	r := &fakeResolver{
		state: fingerprint.CapProbeYes,
		exp:   provider.ToolCallExplanation{Source: "runtime", Has: true},
	}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v", err)
	}
	if !out.Persisted {
		t.Fatalf("outcome = %+v; profile-carried yes re-derives next session — durable", out)
	}
}

func TestProbeToolCall_ExplainFailureMeansPersistedFalseNotError(t *testing.T) {
	r := &fakeResolver{
		state:  fingerprint.CapProbeYes,
		expErr: errors.New("store read failed"),
	}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err != nil {
		t.Fatalf("ProbeToolCall() error: %v; the verdict stands, persistence unknown", err)
	}
	if out.State != fingerprint.CapProbeYes || out.Persisted {
		t.Fatalf("outcome = %+v; want {yes, Persisted:false} (conservative)", out)
	}
}

func TestProbeToolCall_TransientFailure(t *testing.T) {
	cause := errors.New("401 unauthorized")
	r := &fakeResolver{err: cause}
	out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
	if err == nil {
		t.Fatal("ProbeToolCall() error = nil; want probe_failed")
	}
	if code, ok := CodeOf(err); !ok || code != CodeProbeFailed {
		t.Fatalf("CodeOf = %q, %v; want %q", code, ok, CodeProbeFailed)
	}
	if !errors.Is(err, cause) {
		t.Fatal("cause lost from the error chain")
	}
	if !strings.Contains(err.Error(), "p/m") {
		t.Fatalf("error %q does not name the model key", err)
	}
	if out != (ProbeOutcome{}) {
		t.Fatalf("outcome = %+v; want zero", out)
	}
}

func TestProbeToolCall_PreCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &fakeResolver{state: fingerprint.CapProbeYes}
	out, err := ProbeToolCall(ctx, r, key("p", "m"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	if code, ok := CodeOf(err); ok {
		t.Fatalf("cancellation classified as %q; must stay unclassified", code)
	}
	if out != (ProbeOutcome{}) || r.calls != 0 {
		t.Fatalf("outcome=%+v calls=%d; pre-cancelled probe must do and publish nothing", out, r.calls)
	}
}

func TestProbeToolCall_CancelledCallerGetsNothingEvenWithADeliveredResult(t *testing.T) {
	// Singleflight followers receive the leader's completed result even
	// when their own context died. The POST-barrier withholds it: the
	// cancelled CALLER receives nothing (the #456 promise).
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeResolver{state: fingerprint.CapProbeYes}
	r.onCall = func(context.Context) { cancel() }
	out, err := ProbeToolCall(ctx, r, key("p", "m"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled (post-barrier)", err)
	}
	if code, ok := CodeOf(err); ok {
		t.Fatalf("cancellation classified as %q; must stay unclassified", code)
	}
	if out != (ProbeOutcome{}) {
		t.Fatalf("outcome = %+v; a cancelled caller must receive nothing", out)
	}
	if r.expCalls != 0 {
		t.Fatalf("ExplainToolCall called %d times after cancellation; the post-resolve barrier must stop before the durability read", r.expCalls)
	}
}

func TestProbeToolCall_CancelSurfacedAsResolveErrorStaysUnclassified(t *testing.T) {
	// The resolver may surface the caller's cancellation AS its error.
	// The post-resolve barrier must classify by the caller's ctx, not the
	// error branch: a cancelled caller gets the raw context error, never
	// probe_failed. (This is the fixture that falsifies deleting the
	// post-resolve barrier; the err=nil fixture alone is masked by the
	// post-durability barrier.)
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeResolver{err: context.Canceled}
	r.onCall = func(context.Context) { cancel() }
	out, err := ProbeToolCall(ctx, r, key("p", "m"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled", err)
	}
	if code, ok := CodeOf(err); ok {
		t.Fatalf("cancellation classified as %q; must stay unclassified", code)
	}
	if out != (ProbeOutcome{}) {
		t.Fatalf("outcome = %+v; want zero", out)
	}
}

func TestProbeToolCall_CancelDuringDurabilityReadPublishesNothing(t *testing.T) {
	// Cancellation landing inside the durability read (ExplainToolCall):
	// only the post-durability barrier stands between this and a
	// published outcome.
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeResolver{
		state:     fingerprint.CapProbeYes,
		exp:       provider.ToolCallExplanation{Source: "probe", State: fingerprint.CapProbeYes, Valid: true},
		onExplain: cancel,
	}
	out, err := ProbeToolCall(ctx, r, key("p", "m"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want context.Canceled (post-durability barrier)", err)
	}
	if code, ok := CodeOf(err); ok {
		t.Fatalf("cancellation classified as %q; must stay unclassified", code)
	}
	if out != (ProbeOutcome{}) {
		t.Fatalf("outcome = %+v; a cancelled caller must receive nothing", out)
	}
}

func TestProbeToolCall_EvidenceErrorCodes(t *testing.T) {
	cases := []struct {
		sentinel error
		want     ErrorCode
	}{
		{provider.ErrEvidenceUnbound, CodeProbeUnavailable},
		{provider.ErrEvidenceUnreadable, CodeProbeFailed},
		{provider.ErrAutomaticProbeBudgetExhausted, CodeProbeFailed},
	}
	for _, tc := range cases {
		r := &fakeResolver{err: fmt.Errorf("wrapped: %w", tc.sentinel)}
		out, err := ProbeToolCall(context.Background(), r, key("p", "m"))
		if code, ok := CodeOf(err); !ok || code != tc.want {
			t.Fatalf("%v: CodeOf = %q, %v; want %q", tc.sentinel, code, ok, tc.want)
		}
		if !errors.Is(err, tc.sentinel) {
			t.Fatalf("%v: sentinel lost from the chain: %v", tc.sentinel, err)
		}
		if out != (ProbeOutcome{}) {
			t.Fatalf("%v: outcome = %+v; want zero", tc.sentinel, out)
		}
	}
}

// TestNoProbeWithoutCallerRequest pins that RefreshInventory over a log-bound
// registry, publishing included, starts no capability check.
func TestNoProbeWithoutCallerRequest(t *testing.T) {
	const model = "mystery-model-x"
	var chats atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"`+model+`"}]}`)
		case "/v1/chat/completions":
			chats.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_time\",\"arguments\":\"{}\"}}]}}]}\n\n"+
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"+
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"+
				"data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"lc": {APIFormat: "openai-compat", BaseURL: srv.URL}},
		Models:    map[string]config.ModelConfig{"agent": {Provider: "lc", Name: model, Type: "dense"}},
	}
	store := newStore(t)
	b, err := providerbootstrap.New(t.Context(), providerbootstrap.Options{Config: cfg, EvidenceLog: store})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	inv, err := RefreshInventory(t.Context(), b.Providers, b.Models, WithPublish(store, b.Config))
	if err != nil || len(inv.Models) != 1 {
		t.Fatalf("RefreshInventory = %+v, %v; want the one model", inv.Models, err)
	}
	if n := chats.Load(); n != 0 {
		t.Fatalf("refresh sent %d chat requests; want 0", n)
	}
	// Positive control: an explicit probe reaches the stub.
	out, err := ProbeToolCall(t.Context(), b.Models, key("lc", model))
	if err != nil || out.State != fingerprint.CapProbeYes || chats.Load() != 1 {
		t.Fatalf("control: %+v, %v, %d requests; want yes after one request", out, err, chats.Load())
	}
}

// scriptedCapProber is a fingerprint.CapabilityProber whose every check
// returns result. Profiling is not expected.
type scriptedCapProber struct {
	result fingerprint.CapabilityProbeResult
	calls  atomic.Int64
}

func (p *scriptedCapProber) ProbeCapabilities(context.Context, string, fingerprint.CapabilityProbeRequest) (fingerprint.CapabilityProbeResult, error) {
	p.calls.Add(1)
	return p.result, nil
}

func (p *scriptedCapProber) DetectKind(context.Context, string) (*fingerprint.KindDetection, error) {
	return nil, errors.New("profiling is not expected")
}

func (p *scriptedCapProber) ProbeChat(context.Context, string, interface{}) (*fingerprint.ChatMetrics, error) {
	return nil, errors.New("profiling is not expected")
}

func (p *scriptedCapProber) ProbeEmbedding(context.Context, string) (*fingerprint.EmbeddingMetrics, error) {
	return nil, errors.New("profiling is not expected")
}

// unwritableLog is an evidence log whose appends always fail.
type unwritableLog struct{ fingerprint.EvidenceStore }

func (unwritableLog) AppendEvidence(context.Context, ...fingerprint.EvidenceRow) error {
	return errors.New("disk full")
}

// toolCallCheck is a completed check's result for tool_call at state. Through
// openai-compat it is the streamed check's (chat and stream too, probe
// source); through ollama it is what /api/show reported (provider source).
func toolCallCheck(apiFormat string, state fingerprint.CapProbeState) fingerprint.CapabilityProbeResult {
	source, method, requests := fingerprint.EvidenceSourceProbe, fingerprint.MethodChatStreamTools, 1
	if apiFormat == "ollama" {
		source, method, requests = fingerprint.EvidenceSourceProvider, fingerprint.MethodOllamaShow, 1
	}
	tc := fingerprint.ProbeAnswer{Capability: "tool_call", Source: source, Kind: fingerprint.EvidenceKindAnswer, State: state}
	if state == fingerprint.CapProbeInconclusive {
		tc.Kind, tc.TTL, tc.Detail = fingerprint.EvidenceKindAttempt, fingerprint.CapProbeInconclusiveTTL, fingerprint.DetailToolIgnored
	}
	answers := []fingerprint.ProbeAnswer{tc}
	if apiFormat != "ollama" {
		for _, c := range []string{"chat", "stream"} {
			answers = append(answers, fingerprint.ProbeAnswer{Capability: c, Source: source, Kind: fingerprint.EvidenceKindAnswer, State: fingerprint.CapProbeYes})
		}
	}
	return fingerprint.CapabilityProbeResult{Method: method, Requests: requests, Answers: answers}
}

// lostWriteRegistry is a real registry bound to an evidence log that cannot be
// written, serving model through provider "lc" over apiFormat, with prober.
func lostWriteRegistry(t *testing.T, apiFormat, model string, prober *scriptedCapProber) (*provider.Registry, *provider.ModelRegistry) {
	t.Helper()
	reg := provider.NewRegistry()
	if err := reg.Register(&fakeProvider{name: "lc", models: []provider.ModelInfo{{Name: model, Digest: "sha256:d", ContextWindow: 32768}}}); err != nil {
		t.Fatal(err)
	}
	binding, err := provider.EvidenceBindingFor("http://127.0.0.1:8080", apiFormat)
	if err != nil {
		t.Fatal(err)
	}
	mr, err := provider.NewModelRegistry(reg, nil,
		provider.WithEvidenceLog(unwritableLog{newStore(t)}, func(name string) (provider.EvidenceBinding, bool) { return binding, name == "lc" }),
		provider.WithCapabilityProber(func(context.Context, provider.ModelKey, *provider.ModelInfo, provider.Provider) (*provider.FingerprintProberSpec, error) {
			return &provider.FingerprintProberSpec{Prober: prober}, nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	mr.SetCapabilityFloor(func(provider.ModelKey) []string { return []string{"chat", "stream"} })
	return reg, mr
}

func TestLostWrite_NotDurable(t *testing.T) {
	const model = "mystery-model-x" // not in the catalog
	agent := provider.RoutingRequest{Model: "lc/" + model, UseCase: "agent", RequiredCaps: provider.CapChat | provider.CapStream | provider.CapToolCall}
	for _, apiFormat := range []string{"openai-compat", "ollama"} {
		for _, state := range []fingerprint.CapProbeState{fingerprint.CapProbeYes, fingerprint.CapProbeNo, fingerprint.CapProbeInconclusive} {
			t.Run(apiFormat+"/kept "+string(state), func(t *testing.T) {
				prober := &scriptedCapProber{result: toolCallCheck(apiFormat, state)}
				reg, mr := lostWriteRegistry(t, apiFormat, model, prober)
				out, err := ProbeToolCall(t.Context(), mr, key("lc", model))
				if err != nil || out.State != state || out.Persisted {
					t.Fatalf("ProbeToolCall = %+v, %v; want {State:%s Persisted:false}", out, err, state)
				}
				if state != fingerprint.CapProbeYes {
					return
				}
				router := provider.NewRouter(mr, reg)
				defer func() { _ = router.Close() }()
				if _, err := router.Route(t.Context(), agent); err != nil {
					t.Fatalf("Route in the same session: %v; want the kept yes to serve it", err)
				}
				if n := prober.calls.Load(); n != 1 {
					t.Fatalf("checks = %d; want 1", n)
				}
			})
		}
	}
	t.Run("an explicit declaration stays durable", func(t *testing.T) {
		_, mr := lostWriteRegistry(t, "openai-compat", model, &scriptedCapProber{result: toolCallCheck("openai-compat", fingerprint.CapProbeYes)})
		if out, err := ProbeToolCall(t.Context(), mr, key("lc", model)); err != nil || out.Persisted {
			t.Fatalf("precondition: ProbeToolCall = %+v, %v; want a kept yes, not persisted", out, err)
		}
		mr.SetCapabilityOverride(func(provider.ModelKey) []string { return []string{"chat", "stream", "tool_call"} })
		out, err := ProbeToolCall(t.Context(), mr, key("lc", model))
		if err != nil || out.State != fingerprint.CapProbeYes || !out.Persisted {
			t.Fatalf("ProbeToolCall = %+v, %v; want {State:yes Persisted:true}", out, err)
		}
	})
	t.Run("a catalog entry stays durable", func(t *testing.T) {
		const catalogModel = "qwen3:8b" // the catalog declares tool_call
		prober := &scriptedCapProber{result: toolCallCheck("openai-compat", fingerprint.CapProbeYes)}
		_, mr := lostWriteRegistry(t, "openai-compat", catalogModel, prober)
		out, err := ProbeToolCall(t.Context(), mr, key("lc", catalogModel))
		if err != nil || out.State != fingerprint.CapProbeYes || !out.Persisted || prober.calls.Load() != 0 {
			t.Fatalf("ProbeToolCall = %+v, %v, %d checks; want {State:yes Persisted:true} with no check", out, err, prober.calls.Load())
		}
	})
}
