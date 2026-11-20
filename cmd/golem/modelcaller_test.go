package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakePlan is a chatStreamer that emits one chunk carrying a RouteOutcome.
type fakePlan struct {
	outcome *provider.RouteOutcome
}

func (p fakePlan) ExecuteChatStream(ctx context.Context, fn func(provider.ChatResponse) error) error {
	return fn(provider.ChatResponse{Content: "hi", RouteOutcome: p.outcome})
}

func TestChainModelCaller_SetsChainAndCaps(t *testing.T) {
	var captured provider.RoutingRequest
	outcome := &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "ollama", Model: "m1"}}
	mc := &chainModelCaller{
		chain: []string{"ollama/m1", "ollama/m2"},
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			captured = rr
			return fakePlan{outcome: outcome}, nil
		},
	}

	res, err := mc.Chat(context.Background(),
		provider.ChatRequest{
			Messages: []provider.ChatMessage{{Role: "user", Content: "hello"}},
			Tools:    []provider.Tool{{}},
		}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if captured.UseCase != "agent" {
		t.Errorf("UseCase = %q, want agent", captured.UseCase)
	}
	wantCaps := provider.CapChat | provider.CapStream | provider.CapToolCall
	if captured.RequiredCaps != wantCaps {
		t.Errorf("RequiredCaps = %v, want %v", captured.RequiredCaps, wantCaps)
	}
	if !captured.StrictChain {
		t.Error("StrictChain = false, want true")
	}
	if len(captured.PreferredChain) != 2 || captured.PreferredChain[0] != "ollama/m1" {
		t.Errorf("PreferredChain = %v, want [ollama/m1 ollama/m2]", captured.PreferredChain)
	}
	if res.RouteOutcome == nil || res.RouteOutcome.ActualModel.Model != "m1" {
		t.Errorf("RouteOutcome not captured: %+v", res.RouteOutcome)
	}
}

func TestChainModelCaller_NoTools_DropsToolCallCap(t *testing.T) {
	var captured provider.RoutingRequest
	mc := &chainModelCaller{
		chain: []string{"ollama/m1"},
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			captured = rr
			return fakePlan{}, nil
		},
	}
	if _, err := mc.Chat(context.Background(), provider.ChatRequest{}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if captured.RequiredCaps != provider.CapChat|provider.CapStream {
		t.Errorf("RequiredCaps = %v, want chat|stream", captured.RequiredCaps)
	}
	if captured.RequiredCaps&provider.CapToolCall != 0 {
		t.Error("CapToolCall set without tools")
	}
}

func TestChainModelCaller_EmptyChain_UsesRecommend(t *testing.T) {
	var captured provider.RoutingRequest
	mc := &chainModelCaller{
		chain: nil,
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			captured = rr
			return fakePlan{}, nil
		},
	}
	if _, err := mc.Chat(context.Background(), provider.ChatRequest{Tools: []provider.Tool{{}}}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if captured.StrictChain {
		t.Error("StrictChain = true with empty chain, want false (recommend)")
	}
	if len(captured.PreferredChain) != 0 {
		t.Errorf("PreferredChain = %v, want empty", captured.PreferredChain)
	}
}

func TestChainModelCaller_UseCaseAndNoToolCap(t *testing.T) {
	var got provider.RoutingRequest
	m := &chainModelCaller{
		chain:   []string{"local/coder"},
		useCase: "coding",
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			got = rr
			return fakeChatStreamer{content: "ok"}, nil
		},
	}
	_, err := m.Chat(context.Background(), provider.ChatRequest{
		Messages: []provider.ChatMessage{{Role: "user", Content: "hi"}},
	}, nil)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got.UseCase != "coding" {
		t.Fatalf("UseCase = %q, want coding", got.UseCase)
	}
	if got.RequiredCaps&provider.CapToolCall != 0 {
		t.Fatal("no-tools request must not require CapToolCall")
	}
	if !got.StrictChain || len(got.PreferredChain) != 1 {
		t.Fatalf("expected strict chain, got %+v", got)
	}
}

func TestChainModelCaller_EmptyUseCaseDefaultsAgent(t *testing.T) {
	var got provider.RoutingRequest
	m := &chainModelCaller{
		route: func(ctx context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
			got = rr
			return fakeChatStreamer{content: "ok"}, nil
		},
	}
	_, _ = m.Chat(context.Background(), provider.ChatRequest{Messages: []provider.ChatMessage{{Role: "user", Content: "x"}}}, nil)
	if got.UseCase != "agent" {
		t.Fatalf("empty useCase should default to agent, got %q", got.UseCase)
	}
}

type fakeChatStreamer struct{ content string }

func (f fakeChatStreamer) ExecuteChatStream(ctx context.Context, fn func(provider.ChatResponse) error) error {
	return fn(provider.ChatResponse{Content: f.content})
}

// Compile-time assertion: chainModelCaller satisfies agent.ModelCaller.
var _ agent.ModelCaller = (*chainModelCaller)(nil)

func TestNewActiveChainCaller_CarriesThePlansChainAndUseCase(t *testing.T) {
	// The one orchestrator caller derives from the resolved active chainPlan
	// (#476 D3): a caller routing a different use case than the admitted plan
	// is exactly the mismatch chainPlan exists to make unrepresentable.
	plan := chainPlan{chain: []string{"prov/big", "prov/small"}, useCase: "planning"}
	caller, ok := newActiveChainCaller(&provider.Router{}, plan).(*chainModelCaller)
	if !ok {
		t.Fatal("newActiveChainCaller did not return a chainModelCaller")
	}
	if caller.useCase != "planning" {
		t.Errorf("caller.useCase = %q, want %q (hard-coding agent here silently detaches the caller from the admitted plan)", caller.useCase, "planning")
	}
	if !reflect.DeepEqual(caller.chain, plan.chain) {
		t.Errorf("caller.chain = %v, want the plan's chain %v", caller.chain, plan.chain)
	}

	// And the request it routes carries both: the use case stamps the
	// RoutingRequest, and a non-empty chain rides strictly.
	var seen provider.RoutingRequest
	caller.route = func(_ context.Context, rr provider.RoutingRequest) (chatStreamer, error) {
		seen = rr
		return nil, errors.New("stop before execution")
	}
	_, _ = caller.Chat(context.Background(), provider.ChatRequest{
		Messages: []provider.ChatMessage{{Role: "user", Content: "x"}},
	}, nil)
	if seen.UseCase != "planning" {
		t.Errorf("routed UseCase = %q, want %q", seen.UseCase, "planning")
	}
	if !seen.StrictChain || !reflect.DeepEqual(seen.PreferredChain, plan.chain) {
		t.Errorf("routed chain = (strict=%v, %v), want strict %v", seen.StrictChain, seen.PreferredChain, plan.chain)
	}
}

// autoRun runs golem against e until its session is ready, then hands the
// session to ready, whose error stops the run. It returns stderr and run's
// error.
func autoRun(t *testing.T, e mcEnv, extra []string, ready func(*replSession) error) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stdin, stdout, stderr := runTestFiles(t)
	args := append([]string{"-config", e.cfg, "-root", e.root, "-no-probe", "-no-session", "-no-memory", "-no-rag",
		"-no-project-context", "-no-git-context", "-no-auto-index", "-no-editor"}, extra...)
	err := run(args, stdin, stdout, stderr, runHooks{
		startAutoIndex:    func() func() { return func() {} },
		afterSessionReady: ready,
	})
	return readRunTestFile(t, stderr), err
}

var errAutoReady = errors.New("stop: session ready")

func stopWhenReady(*replSession) error { return errAutoReady }

// rejectChecks answers every check of the listed models with a clean 400, so
// the check sends both its requests and ends inconclusive; others get mcCalled.
func rejectChecks(models ...string) func(http.ResponseWriter, *http.Request, string, int) {
	return func(w http.ResponseWriter, _ *http.Request, model string, _ int) {
		if slices.Contains(models, model) {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, mcCalled)
	}
}

const exhaustedReason = "the automatic capability checks for this run are used up; the next golem run has a fresh allowance, and golem models -probe-all checks the configured agent chain"

// TestGolemPreflight_ReasonsForSentinels pins the three resolution sentinels'
// reasons on both preflight paths, none of them a configured capability gap. A
// plain no from every candidate keeps today's message.
func TestGolemPreflight_ReasonsForSentinels(t *testing.T) {
	reasons := map[error]string{
		provider.ErrAutomaticProbeBudgetExhausted: exhaustedReason,
		provider.ErrEvidenceUnreadable:            "the capability store could not be read; run again",
		provider.ErrEvidenceUnbound:               "the provider has no evidence binding; check its base_url",
	}
	key := provider.ModelKey{Provider: "p", Model: "m"}
	for sentinel, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			wrapped := fmt.Errorf("provider: resolve: %w", sentinel)
			reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{key: provider.CapChat | provider.CapStream}}
			_, err := preflightToolCapable(t.Context(), reg, []string{"p/m"}, "agent", noEndpoints,
				&fakeToolResolver{errs: map[provider.ModelKey]error{key: wrapped}})
			if err == nil || !strings.Contains(err.Error(), reason) || isPreflightCapabilityError(err) {
				t.Fatalf("chain path: %v (capability error %v); want %q", err, isPreflightCapabilityError(err), reason)
			}
			reg.recommend = []provider.Capability{provider.CapChat | provider.CapStream}
			_, err = preflightToolCapable(t.Context(), reg, nil, "agent", noEndpoints,
				&fakeToolResolver{errs: map[provider.ModelKey]error{{}: wrapped}})
			if err == nil || !strings.Contains(err.Error(), reason) || strings.Contains(err.Error(), "no tool-capable model available") || isPreflightCapabilityError(err) {
				t.Fatalf("empty-chain path: %v; want %q and not a capability error", err, reason)
			}
		})
	}
	t.Run("a plain no keeps today's message", func(t *testing.T) {
		reg := fakeReg{recommend: []provider.Capability{provider.CapChat | provider.CapStream}}
		_, err := preflightToolCapable(t.Context(), reg, nil, "agent", noEndpoints,
			&fakeToolResolver{states: map[provider.ModelKey]fingerprint.CapProbeState{{}: fingerprint.CapProbeNo}})
		if err == nil || !strings.Contains(err.Error(), "no tool-capable model available") {
			t.Fatalf("err = %v; want today's message", err)
		}
	})
}

// activatedBundle bootstraps srv's provider "lc" the way golem does with
// probing on: the shared store's log attached, progress rendered by golem's
// printer into progress.
func activatedBundle(t *testing.T, e mcEnv, srv *mcServer, progress io.Writer, models ...string) *providerbootstrap.Bundle {
	t.Helper()
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{"lc": {BaseURL: srv.URL, APIFormat: "openai-compat"}}, Models: map[string]config.ModelConfig{}}
	for i, m := range models {
		cfg.Models[fmt.Sprintf("r%d", i)] = config.ModelConfig{Name: m, Provider: "lc", Type: "dense"}
	}
	h := mcOpen(t, e.root)
	t.Cleanup(func() { _ = h.Close() })
	b, err := providerbootstrap.New(t.Context(), providerbootstrap.Options{
		Config: cfg, EvidenceLog: h.Log(), CapabilityProbeProgress: probeProgressPrinter(progress),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestGolemPreflight_BudgetExhaustionIsNotACapabilityError pins that the
// registry's own exhaustion error, on a golem-wired registry whose allowance a
// first check spent, is an ordinary error with its reason on both preflight
// paths.
func TestGolemPreflight_BudgetExhaustionIsNotACapabilityError(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"a", "b"}
	srv.reply = rejectChecks("a")
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "a", "b"))
	b := activatedBundle(t, e, srv, io.Discard, "a", "b")
	if state, err := b.Models.ResolveToolCall(t.Context(), provider.ModelKey{Provider: "lc", Model: "a"}); err != nil || state != fingerprint.CapProbeInconclusive {
		t.Fatalf("spending check = %q, %v; want inconclusive", state, err)
	}
	_, err := preflightToolCapable(t.Context(), b.Models, []string{"lc/b"}, "agent", noEndpoints, b.Models)
	if err == nil || isPreflightCapabilityError(err) || !strings.Contains(err.Error(), exhaustedReason) {
		t.Fatalf("chain path: %v", err)
	}
	_, err = preflightToolCapable(t.Context(), b.Models, nil, "agent", noEndpoints, b.Models)
	if err == nil || isPreflightCapabilityError(err) || !strings.Contains(err.Error(), exhaustedReason) {
		t.Fatalf("empty-chain path: %v", err)
	}
	if got := srv.checksOf("b"); got != 0 {
		t.Fatalf("checks of b = %d; want 0", got)
	}
}

// TestGolemPreflight_EmptyChainChecksOneModel pins that with no agent chain and
// five unknown recommended models, startup checks one model with at most two
// requests and fails with the run-again reason, not "no tool-capable model".
func TestGolemPreflight_EmptyChainChecksOneModel(t *testing.T) {
	srv := newMCServer(t)
	models := []string{"m1", "m2", "m3", "m4", "m5"}
	srv.ids = models
	srv.reply = rejectChecks(models...)
	cfg := mcConfig(srv.URL, "openai-compat", models...)
	cfg = strings.Replace(cfg, `"defaults": {"agent": "r0"}`, `"defaults": {}`, 1)
	e := newMCEnv(t, cfg)
	stderr, err := autoRun(t, e, nil, stopWhenReady)
	if err == nil || errors.Is(err, errAutoReady) || !strings.Contains(err.Error(), exhaustedReason) ||
		strings.Contains(err.Error(), "no tool-capable model available") {
		t.Fatalf("run = %v; want the exhaustion reason\nstderr:\n%s", err, stderr)
	}
	checked := 0
	for _, m := range models {
		if srv.checksOf(m) > 0 {
			checked++
		}
	}
	if total := srv.totalChecks(); checked != 1 || total > 2 {
		t.Fatalf("checked %d models with %d requests; want 1 model, at most 2", checked, total)
	}
}

// TestGolemPreflight_SecondEntryRefusedThenNextRunSucceeds pins the chain
// [a, b] case: a comes back inconclusive and spends the allowance, b is
// refused it and the run fails naming golem models -probe-all; the next run
// sends nothing for a (cooldown), checks b and proceeds.
func TestGolemPreflight_SecondEntryRefusedThenNextRunSucceeds(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"a", "b"}
	srv.reply = rejectChecks("a")
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "a", "b"))

	stderr, err := autoRun(t, e, nil, stopWhenReady)
	if err == nil || errors.Is(err, errAutoReady) || !strings.Contains(err.Error(), "golem models -probe-all") || isPreflightCapabilityError(err) {
		t.Fatalf("first run = %v\nstderr:\n%s", err, stderr)
	}
	if a, b := srv.checksOf("a"), srv.checksOf("b"); a != 2 || b != 0 {
		t.Fatalf("first run checks a=%d b=%d; want 2 and 0", a, b)
	}
	if got := lines(stderr, `"lc/b" not checked`); len(got) != 1 {
		t.Fatalf("budget lines for b %q; want one\nstderr:\n%s", got, stderr)
	}

	stderr, err = autoRun(t, e, nil, stopWhenReady)
	if !errors.Is(err, errAutoReady) {
		t.Fatalf("second run = %v; want a ready session\nstderr:\n%s", err, stderr)
	}
	if a, b := srv.checksOf("a"), srv.checksOf("b"); a != 2 || b != 1 {
		t.Fatalf("after the second run checks a=%d b=%d; want 2 and 1", a, b)
	}
}

// failChecks answers every check of model with status; others get mcCalled.
func failChecks(model string, status int) func(http.ResponseWriter, *http.Request, string, int) {
	return func(w http.ResponseWriter, _ *http.Request, m string, _ int) {
		if m == model {
			http.Error(w, "check failed", status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, mcCalled)
	}
}

// TestGolemPreflight_ServerErrorFirstEntryThenNextRunChecksSecond pins the
// chain [a, b] case where a's check fails with a 500 after its first request:
// the run fails with b unchecked, and the check_failed attempt a's failure
// leaves lets the next run send nothing for a, check b and proceed, naming
// golem models -probe-all for a.
func TestGolemPreflight_ServerErrorFirstEntryThenNextRunChecksSecond(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"a", "b"}
	srv.reply = failChecks("a", http.StatusInternalServerError)
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "a", "b"))

	stderr, err := autoRun(t, e, nil, stopWhenReady)
	if err == nil || errors.Is(err, errAutoReady) || isPreflightCapabilityError(err) || !strings.Contains(err.Error(), "500") {
		t.Fatalf("first run = %v; want the 500\nstderr:\n%s", err, stderr)
	}
	if a, b := srv.checksOf("a"), srv.checksOf("b"); a != 1 || b != 0 {
		t.Fatalf("first run checks a=%d b=%d; want 1 and 0", a, b)
	}
	if got := lines(stderr, `"lc/b" not checked`); len(got) != 1 {
		t.Fatalf("budget lines for b %q; want one\nstderr:\n%s", got, stderr)
	}

	stderr, err = autoRun(t, e, nil, stopWhenReady)
	if !errors.Is(err, errAutoReady) {
		t.Fatalf("second run = %v; want a ready session\nstderr:\n%s", err, stderr)
	}
	if a, b := srv.checksOf("a"), srv.checksOf("b"); a != 1 || b != 1 {
		t.Fatalf("after the second run checks a=%d b=%d; want 1 and 1", a, b)
	}
	hinted := false
	for _, l := range lines(stderr, `"lc/a"`) {
		hinted = hinted || strings.Contains(l, "golem models -probe-all")
	}
	if !hinted {
		t.Fatalf("no line for \"lc/a\" names golem models -probe-all\nstderr:\n%s", stderr)
	}
}

// TestGolemPreflight_KeyErrorFirstEntryBlocksEveryRun pins the known issue: a
// 401 or 429 belongs to the key, not the shared endpoint, so a's failed check
// records nothing and every run re-sends it, leaving b unchecked.
func TestGolemPreflight_KeyErrorFirstEntryBlocksEveryRun(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := newMCServer(t)
			srv.ids = []string{"a", "b"}
			srv.reply = failChecks("a", status)
			e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "a", "b"))
			for run := 1; run <= 2; run++ {
				stderr, err := autoRun(t, e, nil, stopWhenReady)
				if err == nil || errors.Is(err, errAutoReady) {
					t.Fatalf("run %d = %v; want a failure\nstderr:\n%s", run, err, stderr)
				}
				if a, b := srv.checksOf("a"), srv.checksOf("b"); a != run || b != 0 {
					t.Fatalf("after run %d checks a=%d b=%d; want %d and 0", run, a, b, run)
				}
			}
		})
	}
}

// TestGolemAutomaticBudget_SharedAcrossTurnsAndCandidates pins that one
// allowance spans the invocation: startup's check of a sends one request and
// refunds one, which cannot fund b's check after a turn, so /model set to b is
// refused it, reported once however often it is asked.
func TestGolemAutomaticBudget_SharedAcrossTurnsAndCandidates(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"a", "b"}
	e := newMCEnv(t, strings.Replace(mcConfig(srv.URL, "openai-compat", "a"), `"context_window": 32768}}`,
		`"context_window": 32768}, "rb": {"name": "b", "provider": "lc", "type": "dense", "context_window": 32768}}`, 1))
	var outs []string
	stderr, err := autoRun(t, e, nil, func(sess *replSession) error {
		if _, err := runOnce(t.Context(), io.Discard, nil, sess, "hello", nil); err != nil {
			return err
		}
		for range 2 {
			var out strings.Builder
			_, _ = dispatchSlash(t.Context(), &out, sess, "/model set rb")
			outs = append(outs, out.String())
		}
		return errAutoReady
	})
	if !errors.Is(err, errAutoReady) {
		t.Fatalf("run = %v\nstderr:\n%s", err, stderr)
	}
	for _, out := range outs {
		if !strings.Contains(out, exhaustedReason) {
			t.Fatalf("/model set rb = %q; want the exhaustion reason", out)
		}
	}
	if a, b := srv.checksOf("a"), srv.checksOf("b"); a != 1 || b != 0 {
		t.Fatalf("checks a=%d b=%d; want 1 and 0", a, b)
	}
	if got := lines(stderr, `"lc/b" not checked`); len(got) != 1 {
		t.Fatalf("budget lines for b %q; want exactly one\nstderr:\n%s", got, stderr)
	}
}

// TestGolemAutomaticProgress_StderrOnlyAndBeforeHTTP pins that an automatic
// check's progress is on stderr before its first request reaches the server,
// and that none of it reaches the headless stream on stdout.
func TestGolemAutomaticProgress_StderrOnlyAndBeforeHTTP(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stdin, stdout, stderr := runTestFiles(t)
	var sawStarted bool
	srv.reply = func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
		got, _ := os.ReadFile(stderr.Name())
		sawStarted = strings.Contains(string(got), `capability check: "lc/mystery" at `)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, mcCalled)
	}
	err := run([]string{"-config", e.cfg, "-root", e.root, "-no-probe", "-no-session", "-no-memory", "-no-rag",
		"-no-project-context", "-no-git-context", "-no-auto-index", "-no-editor", "-p", "hello", "-output-format", "stream-json"},
		stdin, stdout, stderr, runHooks{startAutoIndex: func() func() { return func() {} }})
	errText := readRunTestFile(t, stderr)
	if err != nil {
		t.Fatalf("run = %v\nstderr:\n%s", err, errText)
	}
	if !sawStarted {
		t.Fatalf("the check's request arrived before its progress line\nstderr:\n%s", errText)
	}
	started := lines(errText, `capability check: "lc/mystery" at `)
	if len(started) != 1 || !strings.Contains(started[0], "chat-stream-tools") || !strings.Contains(started[0], "2 requests") ||
		!strings.Contains(started[0], "requesting up to 2048 output tokens") {
		t.Fatalf("started line %q", started)
	}
	if got := lines(errText, `capability check: "lc/mystery" finished: 1 request sent`); len(got) != 1 {
		t.Fatalf("finished lines %q\nstderr:\n%s", got, errText)
	}
	out := readRunTestFile(t, stdout)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !json.Valid([]byte(line)) || strings.Contains(line, "capability check") {
			t.Fatalf("stdout line %q is not a clean machine record", line)
		}
	}
}

// TestGolemAutomaticCancel_StopsR2AndPreservesCommittedRows pins that a
// caller's cancellation during an automatic check on golem's registry sends no
// second request, saves nothing for the cancelled check, keeps rows committed
// before it, and reports the cancellation on golem's progress printer.
func TestGolemAutomaticCancel_StopsR2AndPreservesCommittedRows(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv, e := cancelFixture(t, cancel)
	srv.ids = []string{"mystery", "kept"}
	kept := mcToolAnswers(t, srv.URL, "kept", fingerprint.CapProbeYes, time.Now().Add(-time.Hour))
	mcSeed(t, e.root, kept...)
	var progress strings.Builder
	b := activatedBundle(t, e, srv, &progress, "mystery", "kept")
	_, err := preflightToolCapable(ctx, b.Models, []string{"lc/mystery"}, "agent", noEndpoints, b.Models)
	if err == nil {
		t.Fatal("preflight passed a cancelled check")
	}
	if got := srv.checksOf("mystery"); got != 1 {
		t.Fatalf("check requests = %d; want 1", got)
	}
	if rows := mcRows(t, e.root, srv.URL, "openai-compat", "mystery"); len(rows) != 0 {
		t.Fatalf("rows for the cancelled check = %d; want 0", len(rows))
	}
	if rows := mcRows(t, e.root, srv.URL, "openai-compat", "kept"); len(rows) != len(kept) {
		t.Fatalf("committed rows = %d; want %d", len(rows), len(kept))
	}
	if got := lines(progress.String(), `capability check: "lc/mystery" cancelled`); len(got) != 1 {
		t.Fatalf("progress:\n%s", progress.String())
	}
}

// TestGolemNoCapProbe_SendsNoAutomaticGeneration pins that -no-cap-probe sends
// no check: an undeclared model fails preflight as a configured gap. The
// control without the flag checks it once.
func TestGolemNoCapProbe_SendsNoAutomaticGeneration(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"mystery"}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "mystery"))
	stderr, err := autoRun(t, e, []string{"-no-cap-probe"}, stopWhenReady)
	if !isPreflightCapabilityError(err) || srv.totalChecks() != 0 {
		t.Fatalf("-no-cap-probe: run = %v, %d checks\nstderr:\n%s", err, srv.totalChecks(), stderr)
	}
	if stderr, err := autoRun(t, e, nil, stopWhenReady); !errors.Is(err, errAutoReady) || srv.totalChecks() != 1 {
		t.Fatalf("control: run = %v, %d checks\nstderr:\n%s", err, srv.totalChecks(), stderr)
	}
}

// TestGolemNoCapProbe_AttachesNoLog pins that -no-cap-probe attaches no log:
// a stored no is ignored, as legacy rows were, so the Ollama model's live
// tools yes passes preflight; and the store still opens for the input ceiling.
// Neither the config, the catalog nor the runtime gives the model a window, so
// 22528 (a stored profile's 24576 less the agent reserve) can come only from
// the store. The control without the flag honors the stored no.
func TestGolemNoCapProbe_AttachesNoLog(t *testing.T) {
	const digest = "sha256:d1"
	srv := newMCServer(t)
	srv.tags = []mcTag{{"mystery", digest}}
	srv.setCaps(`["completion","tools"]`)
	e := newMCEnv(t, strings.Replace(mcConfig(srv.URL, "ollama", "mystery"), `, "context_window": 32768`, "", 1))
	no := fingerprint.ProbeAnswer{Capability: "tool_call", Source: fingerprint.EvidenceSourceProvider, Kind: fingerprint.EvidenceKindAnswer, State: fingerprint.CapProbeNo}
	mcSeed(t, e.root, mcAnswers(t, srv.URL, "ollama", "mystery", digest, fingerprint.MethodOllamaShow, time.Now().Add(-time.Hour), no)...)
	h := mcOpen(t, e.root)
	if err := h.Profiles().Save(context.Background(), fingerprint.Profile{
		BackendID: "lc", ModelName: "mystery", ModelDigest: digest,
		ProfileVersion: fingerprint.CurrentProfileVersion, EffectiveContext: 24_576, TestedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	stderr, err := autoRun(t, e, []string{"-no-cap-probe"}, stopWhenReady)
	if !errors.Is(err, errAutoReady) {
		t.Fatalf("-no-cap-probe: run = %v\nstderr:\n%s", err, stderr)
	}
	if !strings.Contains(stderr, "input ceiling: 22528 tokens (chain minimum)") {
		t.Fatalf("the input ceiling is not the stored profile's:\n%s", stderr)
	}
	if stderr, err = autoRun(t, e, nil, stopWhenReady); err == nil || errors.Is(err, errAutoReady) {
		t.Fatalf("control: run = %v; want the stored no to fail preflight\nstderr:\n%s", err, stderr)
	}
}

// TestGolemPreflight_ProbeErrorTextIsQuoted pins that a failed check's error,
// which can carry a server's body, reaches the preflight message quoted, so an
// escape sequence in a 500 body is escaped rather than sent to the terminal.
func TestGolemPreflight_ProbeErrorTextIsQuoted(t *testing.T) {
	srv := newMCServer(t)
	srv.ids = []string{"a"}
	srv.reply = func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "\x1b[31mboom\x1b[0m")
	}
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "a"))
	b := activatedBundle(t, e, srv, io.Discard, "a")
	warns, err := preflightToolCapable(t.Context(), b.Models, []string{"lc/a"}, "agent", noEndpoints, b.Models)
	if err == nil || len(warns) != 1 || !strings.Contains(warns[0], "tool-capability probe failed: ") {
		t.Fatalf("preflight = %q, %v; want the failed check's warning", warns, err)
	}
	if strings.ContainsRune(err.Error(), 0x1b) || !strings.Contains(warns[0], `\x1b[31mboom`) {
		t.Fatalf("warning %q; want the body's escape sequence quoted", warns[0])
	}
}

// TestGolemPreflight_LookupErrorTextIsQuoted pins that a failed lookup's
// error, which can carry a server's body, reaches the connectivity warning
// quoted, so an escape sequence in the body is escaped rather than sent to the
// terminal.
func TestGolemPreflight_LookupErrorTextIsQuoted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "\x1b[31mdown\x1b[0m")
	}))
	t.Cleanup(srv.Close)
	e := newMCEnv(t, mcConfig(srv.URL, "openai-compat", "a"))
	b := activatedBundle(t, e, &mcServer{Server: srv}, io.Discard, "a")
	warns, err := preflightToolCapable(t.Context(), b.Models, []string{"lc/a"}, "agent", noEndpoints, b.Models)
	if err == nil || len(warns) != 1 || !strings.Contains(warns[0], "provider lookup failed: ") {
		t.Fatalf("preflight = %q, %v; want the lookup failure's warning", warns, err)
	}
	if strings.ContainsRune(err.Error(), 0x1b) || !strings.Contains(warns[0], `\x1b[31mdown`) {
		t.Fatalf("warning %q; want the body's escape sequence quoted", warns[0])
	}
}
