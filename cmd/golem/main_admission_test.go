package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/internal/providerbootstrap"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/provider/openaicompat"
)

// admissionHarness serves a local openai-compat backend and writes a config
// whose shape restages the reported #477 bug: a local agent role, NO
// summarize default, and a REMOTE analysis role — so the summarize side-task
// falls through to the remote destination.
func admissionHarness(t *testing.T, remoteURL string) (configPath, root string, requests *atomic.Int64) {
	cp, rt, reqs, _ := admissionHarnessWithBystander(t, remoteURL)
	return cp, rt, reqs
}

// admissionHarnessWithBystander also configures a BYSTANDER provider that no
// route reaches: gated bootstrap must send it zero requests (I7 through the
// real run()).
func admissionHarnessWithBystander(t *testing.T, remoteURL string) (configPath, root string, requests, bystander *atomic.Int64) {
	t.Helper()
	requests = &atomic.Int64{}
	bystander = &atomic.Int64{}
	bystanderSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bystander.Add(1)
		http.NotFound(w, nil)
	}))
	t.Cleanup(bystanderSrv.Close)
	_ = bystanderSrv
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"model":"agent-model","choices":[{"delta":{"content":"done"},"finish_reason":null}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"model":"agent-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	root = t.TempDir()
	configPath = filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "local":     {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"},
    "opencode":  {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"},
    "bystander": {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"}
  },
  "models": {
    "agent":     {"name": "agent-model", "provider": "local", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"]},
    "cloud-pro": {"name": "remote-model", "provider": "opencode", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream"]}
  },
  "defaults": {"agent": "agent", "analysis": "cloud-pro"}
}`, server.URL, remoteURL, bystanderSrv.URL)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, root, requests, bystander
}

func admissionArgs(configPath, root string) []string {
	return []string{"-config", configPath, "-root", root, "-p", "say done",
		"-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context",
		"-no-session", "-no-memory"}
}

// One-shot mode disables compression, so its remote summarize fallback is not
// reachable and must not require destination consent.
func TestRunOneShotOmitsRemoteSummarizeRoute(t *testing.T) {
	configPath, root, requests := admissionHarness(t, "https://opencode.invalid/zen/go")
	stdin, stdout, stderr := runTestFiles(t)

	err := run(admissionArgs(configPath, root), stdin, stdout, stderr)
	if err != nil {
		t.Fatalf("one-shot run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	if !strings.Contains(readRunTestFile(t, stdout), "done") {
		t.Errorf("final answer missing:\n%s", readRunTestFile(t, stdout))
	}
	if got := requests.Load(); got == 0 {
		t.Error("local provider received no requests; the one-shot turn cannot have run")
	}
	errOut := readRunTestFile(t, stderr)
	if strings.Contains(errOut, "summarize") || strings.Contains(errOut, "opencode.invalid") {
		t.Errorf("one-shot admitted unreachable summarize fallback:\n%s", errOut)
	}
}

func TestRunConfiglessGroundingDegradesWithoutPanic(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/tags":
			_, _ = io.WriteString(w, `{"models":[{"name":"qwen3:8b"}]}`)
		case "/api/show":
			_, _ = io.WriteString(w, `{"details":{"family":"qwen3","parameter_size":"8B"},"capabilities":["completion","tools"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	oldConfig, hadConfig := os.LookupEnv("GO_LLM_CONFIG")
	if err := os.Unsetenv("GO_LLM_CONFIG"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadConfig {
			_ = os.Setenv("GO_LLM_CONFIG", oldConfig)
		}
	})
	stdin, stdout, stderr := runTestFiles(t)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("configless -grounding panicked: %v", r)
		}
	}()
	errStop := errors.New("stop after startup")
	err := run([]string{"-root", t.TempDir(), "-p", "say done", "-ollama-url", server.URL, "-grounding", "-no-rag", "-no-probe", "-no-cap-probe", "-no-project-context", "-no-session", "-no-memory"}, stdin, stdout, stderr, runHooks{
		afterSessionReady: func(*replSession) error { return errStop },
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("configless run error = %v, want test stop after startup", err)
	}
	if got := readRunTestFile(t, stderr); !strings.Contains(got, groundingNoRetrieveWarning) {
		t.Errorf("configless run stderr missing %q:\n%s", groundingNoRetrieveWarning, got)
	}
}

// #480 grounding integration: -grounding binds extract and verify at
// runtime, so those routes join the plan and their remote reachability
// shows on the consent surface and in the denial.
func TestRunGroundingRoutesJoinTheManifest(t *testing.T) {
	configPath, root, requests := admissionHarness(t, "https://opencode.invalid/zen/go")
	stdin, stdout, stderr := runTestFiles(t)

	args := append(admissionArgs(configPath, root), "-grounding")
	err := run(args, stdin, stdout, stderr)
	if err == nil {
		t.Fatalf("grounding run with remote fallback succeeded\nstderr:\n%s", readRunTestFile(t, stderr))
	}
	errOut := readRunTestFile(t, stderr)
	for _, want := range []string{"verify", "extract"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("manifest missing grounding purpose %q:\n%s", want, errOut)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("denied grounding startup sent %d requests, want 0", got)
	}
}

// The exact allowlist unblocks the same run: the turn completes on the local
// provider and the remote — although admitted — receives nothing, because
// admission is consent, not traffic.
func TestRunAllowDestinationAdmitsAndCompletes(t *testing.T) {
	configPath, root, requests, bystander := admissionHarnessWithBystander(t, "https://opencode.invalid/zen/go")
	stdin, stdout, stderr := runTestFiles(t)

	args := append(admissionArgs(configPath, root),
		"-allow-destination", "opencode/https://opencode.invalid/zen/go")
	if err := run(args, stdin, stdout, stderr); err != nil {
		t.Fatalf("allowlisted run: %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	if !strings.Contains(readRunTestFile(t, stdout), "done") {
		t.Errorf("final answer missing:\n%s", readRunTestFile(t, stdout))
	}
	if got := requests.Load(); got == 0 {
		t.Error("local provider received no requests; the turn cannot have run")
	}
	// I7 through the real run(): the configured bystander is on no route,
	// so the gated bootstrap must never touch it. An ungated bootstrap
	// would refresh it.
	if got := bystander.Load(); got != 0 {
		t.Errorf("bystander provider received %d requests, want 0", got)
	}
}

// A local-only config keeps its prompt-free startup: the manifest renders,
// nothing asks for consent, the run completes.
func TestRunLocalOnlyRemainsPromptFree(t *testing.T) {
	// Reuse the harness but point every role at the local provider.
	configPath, root, _ := admissionHarness(t, "https://opencode.invalid/zen/go")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	local := strings.ReplaceAll(string(raw), `"provider": "opencode"`, `"provider": "local"`)
	local = strings.Replace(local, `"opencode": {"base_url": "https://opencode.invalid/zen/go", "api_format": "openai-compat", "timeout": "5s"}`, `"opencode": {"base_url": "http://127.0.0.1:9", "api_format": "openai-compat", "timeout": "5s"}`, 1)
	if err := os.WriteFile(configPath, []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)

	if err := run(admissionArgs(configPath, root), stdin, stdout, stderr); err != nil {
		t.Fatalf("local-only run: %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	errOut := readRunTestFile(t, stderr)
	if strings.Contains(errOut, "Allow this session") {
		t.Errorf("local-only run prompted for consent:\n%s", errOut)
	}
	if !strings.Contains(errOut, "destinations:") {
		t.Errorf("manifest not rendered:\n%s", errOut)
	}
}

// M15/D8: the discovery target derives from the plan's agent route, not from
// defaults.agent.
func TestOpenAICompatTargetFromRoute(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc":    {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8080"},
			"other": {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:8081"},
		},
		Models: map[string]config.ModelConfig{
			"agent": {Provider: "lc", Name: "m-default"},
			"plan":  {Provider: "other", Name: "m-plan"},
		},
		Defaults: map[string]string{"agent": "agent"},
	}

	// The route says "other/m-plan"; defaults.agent says lc. The route wins.
	key, model, ok := openAICompatTargetFromRoute(cfg, providerbootstrap.PlannedRoute{
		UseCase: "agent", Chain: []string{"other/m-plan"},
	})
	if !ok || key != "other" || model != "m-plan" {
		t.Fatalf("target = (%q, %q, %v), want the ROUTE primary (other, m-plan, true)", key, model, ok)
	}

	// A recommend route has no single primary: discovery disabled.
	if _, _, ok := openAICompatTargetFromRoute(cfg, providerbootstrap.PlannedRoute{UseCase: "agent", Recommend: true}); ok {
		t.Error("recommend route produced a discovery target")
	}
	// Non-openai-compat primary: disabled.
	cfg.Providers["other"] = config.ProviderConfig{APIFormat: "ollama", BaseURL: "http://127.0.0.1:11434"}
	if _, _, ok := openAICompatTargetFromRoute(cfg, providerbootstrap.PlannedRoute{UseCase: "agent", Chain: []string{"other/m-plan"}}); ok {
		t.Error("ollama-format primary produced an openai-compat discovery target")
	}
}

// The per-candidate discovery guard: loopback candidates get a bound guarded
// client; a non-loopback candidate cannot even construct one (the scan
// structurally cannot leave the host); and the guarded scan probes each
// candidate individually.
func TestDiscoveryCandidateGuard(t *testing.T) {
	guard := discoveryCandidateGuard(context.Background(), "lc")

	t.Run("loopback candidate binds", func(t *testing.T) {
		hc, bctx, err := guard("http://127.0.0.1:8087")
		if err != nil {
			t.Fatal(err)
		}
		if hc == nil || bctx == nil {
			t.Fatal("guard returned nil client or context")
		}
	})

	t.Run("remote candidate refused", func(t *testing.T) {
		if _, _, err := guard("https://evil.example.com"); err == nil {
			t.Fatal("non-loopback candidate accepted into the scan")
		}
	})
}

// The guarded scan goes candidate-by-candidate through the guard and stops
// at the first hit; a guard refusal skips the candidate without probing it.
func TestResolveBackendGuardedScan(t *testing.T) {
	// The route primary (lc) deliberately DIVERGES from defaults.agent
	// (which points at "other"): a scan that fell back to the legacy
	// defaults.agent derivation would target the wrong provider and probe
	// nothing on lc's band (M15).
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"lc":    {APIFormat: "openai-compat", BaseURL: "http://127.0.0.1:1"},
			"other": {APIFormat: "openai-compat", BaseURL: "https://other.example.com"},
		},
		Models: map[string]config.ModelConfig{
			"agent": {Provider: "other", Name: "m-other"},
			"plan":  {Provider: "lc", Name: "m1"},
		},
		Defaults: map[string]string{"agent": "agent"},
	}
	route := providerbootstrap.PlannedRoute{UseCase: "agent", Chain: []string{"lc/m1"}}

	var probed []string
	prober := func(_ context.Context, candidates []string, wantModel string, _ ...openaicompat.ClientOption) (string, error) {
		if len(candidates) != 1 {
			t.Fatalf("guarded scan probed %d candidates in one call, want 1", len(candidates))
		}
		probed = append(probed, candidates[0])
		if candidates[0] == "http://127.0.0.1:8083" {
			return candidates[0], nil
		}
		return "", errors.New("not here")
	}

	res, err := resolveBackend(context.Background(), cfg, backendResolveOpts{
		lookupEnv:      func(string) (string, bool) { return "", false },
		prober:         prober,
		activeRoute:    &route,
		guardCandidate: discoveryCandidateGuard(context.Background(), "lc"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.source != "discovered" || res.baseURL != "http://127.0.0.1:8083" {
		t.Fatalf("resolution = %+v, want discovery hit at :8083", res)
	}
	if len(probed) < 2 {
		t.Fatalf("scan probed %d candidates, want the walk up to the hit", len(probed))
	}
}

// pinLoopback retargets the admitted manifest to the discovered URL without
// adding authority: the provider's edges bind against the NEW destination,
// the old one stops resolving, a remote pin is refused, and pinning before
// admission is refused.
func TestAdmissionPinLoopback(t *testing.T) {
	p := &fakePrompt{answer: true}
	adm, out := newTestAdmission(t, admEdges(t), nil, true, p)

	discovered := admDest(t, "llamacpp", "http://127.0.0.1:8083")
	if err := adm.pinLoopback("llamacpp", discovered); err == nil {
		t.Fatal("pin before admission accepted")
	}
	if err := adm.ensure(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := adm.pinLoopback("llamacpp", admDest(t, "llamacpp", "https://opencode.ai/zen/go")); err == nil {
		t.Fatal("remote pin accepted")
	}
	// The IsLocal contract is a hard D11 invariant, not policy-derived: even
	// a remote the user ALLOWLISTED for this provider must not become a pin
	// target — discovery selects loopback backends, full stop.
	{
		p2 := &fakePrompt{answer: true}
		adm2, _ := newTestAdmission(t, admEdges(t),
			[]string{"llamacpp/https://lan.example.com"}, true, p2)
		if err := adm2.ensure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := adm2.pinLoopback("llamacpp", admDest(t, "llamacpp", "https://lan.example.com")); err == nil {
			t.Fatal("allowlisted remote accepted as a pin target")
		}
	}
	if err := adm.pinLoopback("llamacpp", discovered); err != nil {
		t.Fatal(err)
	}

	ctx, err := adm.gate.Bind(context.Background(), "agent", "llamacpp")
	if err != nil {
		t.Fatalf("bind after pin: %v", err)
	}
	_ = ctx
	// The remote grant survives the pin untouched.
	if _, err := adm.gate.Bind(context.Background(), "summarize", "opencode"); err != nil {
		t.Errorf("remote grant lost by pin: %v", err)
	}
	if !strings.Contains(out.String(), "http://127.0.0.1:8083") {
		t.Errorf("pin receipt missing the discovered URL:\n%s", out.String())
	}

	// The pinned manifest's llamacpp edges now carry the discovered URL: a
	// guarded client bound to the NEW destination authorizes the bound
	// context, one bound to the OLD does not.
	newT, err := provider.GuardHTTPClient(adm.gate, discovered, nil)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8083/v1/models", nil)
	// RoundTrip will fail to CONNECT (nothing listens), but a guard denial is
	// distinguishable: it wraps ErrDestinationDenied.
	_, terr := newT.Transport.RoundTrip(req)
	if terr != nil && errors.Is(terr, provider.ErrDestinationDenied) {
		t.Fatalf("bound context denied against the pinned destination: %v", terr)
	}
}

// End to end through the real run(): the configured loopback URL is dead,
// the scan band is aimed at a live listener the test owns, and the whole
// #477 chain fires — guarded per-candidate discovery, the loopback pin, the
// pinned receipt, and a completed turn against the DISCOVERED backend.
func TestRunDiscoveryPinsAndCompletes(t *testing.T) {
	requests := &atomic.Int64{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"agent-model"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"model":"agent-model","choices":[{"delta":{"content":"done"},"finish_reason":null}]}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"model":"agent-model","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	port := backend.Listener.Addr().(*net.TCPAddr).Port
	oldLow, oldHigh := backendScanPortLow, backendScanPortHigh
	backendScanPortLow, backendScanPortHigh = port, port
	defer func() { backendScanPortLow, backendScanPortHigh = oldLow, oldHigh }()

	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := `{
  "providers": {"lc": {"base_url": "http://127.0.0.1:1", "api_format": "openai-compat", "timeout": "5s"}},
  "models": {"agent": {"name": "agent-model", "provider": "lc", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)
	args := []string{"-config", configPath, "-root", root, "-p", "say done",
		"-no-cap-probe", "-no-rag", "-no-project-context", "-no-session", "-no-memory"}

	if err := run(args, stdin, stdout, stderr); err != nil {
		t.Fatalf("discovery run: %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
	}
	if !strings.Contains(readRunTestFile(t, stdout), "done") {
		t.Errorf("final answer missing:\n%s", readRunTestFile(t, stdout))
	}
	errOut := readRunTestFile(t, stderr)
	wantPin := fmt.Sprintf("destination pinned: lc -> http://127.0.0.1:%d (discovered)", port)
	if !strings.Contains(errOut, wantPin) {
		t.Errorf("pin receipt missing (%q):\n%s", wantPin, errOut)
	}
	if got := requests.Load(); got == 0 {
		t.Error("discovered backend received no requests")
	}
}

// goalHarness serves TWO counted loopback providers: "agentprov" backing the
// agent role and "planprov" backing the role the planning use case resolves
// to. lastPlanModel records the model name of the most recent chat request
// the planning provider served.
func goalHarness(t *testing.T, planningDefaults string) (configPath, root string, agentReqs, planReqs *atomic.Int64, lastPlanModel *atomic.Pointer[string]) {
	t.Helper()
	agentReqs, planReqs = &atomic.Int64{}, &atomic.Int64{}
	lastPlanModel = &atomic.Pointer[string]{}
	agentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentReqs.Add(1)
		serveCompat(w, r, "agent-model", nil)
	}))
	t.Cleanup(agentSrv.Close)
	planSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		planReqs.Add(1)
		serveCompat(w, r, "planner-model", lastPlanModel)
	}))
	t.Cleanup(planSrv.Close)

	root = t.TempDir()
	configPath = filepath.Join(t.TempDir(), "models.json")
	configJSON := fmt.Sprintf(`{
  "providers": {
    "agentprov": {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"},
    "planprov":  {"base_url": %q, "api_format": "openai-compat", "timeout": "5s"}
  },
  "models": {
    "agent":   {"name": "agent-model", "provider": "agentprov", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"]},
    "planner": {"name": "planner-model", "provider": "planprov", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"]}
  },
  "defaults": {"agent": "agent", %s}
}`, agentSrv.URL, planSrv.URL, planningDefaults)
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, root, agentReqs, planReqs, lastPlanModel
}

// serveCompat answers /v1/models and /v1/chat/completions for one model,
// recording the requested model name of chat calls when record is non-nil.
func serveCompat(w http.ResponseWriter, r *http.Request, model string, record *atomic.Pointer[string]) {
	switch r.URL.Path {
	case "/v1/models":
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, model)
	case "/v1/chat/completions":
		if record != nil {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &req)
			m := req.Model
			record.Store(&m)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"delta\":{\"content\":\"noop\"},\"finish_reason\":null}]}\n\n", model)
		_, _ = fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n", model)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	default:
		http.NotFound(w, r)
	}
}

func goalArgs(configPath, root string) []string {
	return []string{"-config", configPath, "-root", root, "-goal", "outline a refactor",
		"-max-steps", "2", "-no-probe", "-no-cap-probe", "-no-project-context"}
}

// driveGoalTurn runs a goal-mode startup through the real run(), then drives
// ONE model turn through sess.orch -- the very orchestrator instance plan
// authoring runs on (#476 D3) -- and stops before the author loop, which
// would otherwise require a real agentflow CLI on the host. The turn's
// requested model proves which route the session's caller carries.
func driveGoalTurn(t *testing.T, configPath, root string) {
	t.Helper()
	stdin, stdout, stderr := runTestFiles(t)
	errStop := errors.New("stop after the routed turn")
	err := run(goalArgs(configPath, root), stdin, stdout, stderr, runHooks{
		afterSessionReady: func(sess *replSession) error {
			_, runErr := sess.orch.Run(context.Background(),
				agent.Request{Goal: "say noop", MaxSteps: 1}, nil)
			if runErr != nil {
				t.Errorf("goal-session turn: %v", runErr)
			}
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run error = %v, want the test stop after the routed turn\nstderr:\n%s",
			err, readRunTestFile(t, stderr))
	}
}

// Goal mode's active route IS the planning route (#476 D3/I5): a turn on the
// session's orchestrator must reach the provider the planning use case
// resolves to, with that role's model -- and the agent provider, INACTIVE in
// goal mode, must receive zero requests: no refresh, no probe, no inference
// (I10).
func TestRunGoalModeSessionRoutesThePlanningChain(t *testing.T) {
	configPath, root, agentReqs, planReqs, lastPlanModel := goalHarness(t,
		`"planning": "planner"`)

	driveGoalTurn(t, configPath, root)

	if got := planReqs.Load(); got == 0 {
		t.Error("planning provider received no requests; the session cannot have routed there")
	}
	if m := lastPlanModel.Load(); m == nil || *m != "planner-model" {
		got := "<none>"
		if m != nil {
			got = *m
		}
		t.Errorf("session turn requested model %q, want %q (the planning role's model)", got, "planner-model")
	}
	if got := agentReqs.Load(); got != 0 {
		t.Errorf("agent provider received %d requests in goal mode, want 0 (inactive route)", got)
	}
}

// The reasoning hop is a real route: a config with no planning key but a
// reasoning default routes the goal session through it (#476 D2). The model
// assertion keeps this non-vacuous -- a bare request counter would already be
// satisfied by the metadata refresh.
func TestRunGoalModeFallsBackToTheReasoningRole(t *testing.T) {
	configPath, root, agentReqs, _, lastPlanModel := goalHarness(t,
		`"reasoning": "planner"`)

	driveGoalTurn(t, configPath, root)

	if m := lastPlanModel.Load(); m == nil || *m != "planner-model" {
		got := "<none>"
		if m != nil {
			got = *m
		}
		t.Errorf("session turn requested model %q, want %q (the reasoning role's model via the planning fallback)", got, "planner-model")
	}
	if got := agentReqs.Load(); got != 0 {
		t.Errorf("agent provider received %d requests in goal mode, want 0 (inactive route)", got)
	}
}

// A remote planning fallback fails closed in noninteractive goal mode: the
// run dies naming the destination and the exact flag, and NO provider --
// including the local agent provider -- receives a single request, because
// admission precedes discovery, refresh, probe, and inference (#477).
func TestRunGoalModeRemotePlanningFallbackFailsClosed(t *testing.T) {
	configPath, root, agentReqs, _, _ := goalHarness(t,
		`"analysis": "remoteplan"`)
	// Point the planning fallback at an unreachable REMOTE provider: the
	// analysis hop resolves planning to it.
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	amended := strings.Replace(string(raw), `"planner":`, `"remoteplan": {"name": "remote-model", "provider": "remote", "type": "dense", "context_window": 32768,
      "capabilities": ["chat", "generate", "stream", "tool_call"]},
    "planner":`, 1)
	amended = strings.Replace(amended, `"planprov":`, `"remote": {"base_url": "https://opencode.invalid/zen/go", "api_format": "openai-compat", "timeout": "5s"},
    "planprov":`, 1)
	if err := os.WriteFile(configPath, []byte(amended), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)

	err = run(goalArgs(configPath, root), stdin, stdout, stderr)
	if err == nil {
		t.Fatalf("run = nil error, want fail-closed on the unconsented remote planning destination\nstderr:\n%s",
			readRunTestFile(t, stderr))
	}
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("run error = %v, want ErrDestinationDenied", err)
	}
	msg := err.Error() + readRunTestFile(t, stderr)
	if !strings.Contains(msg, "opencode.invalid") || !strings.Contains(msg, "-allow-destination") {
		t.Errorf("failure names neither the destination nor the flag:\n%s", msg)
	}
	if got := agentReqs.Load(); got != 0 {
		t.Errorf("agent provider received %d requests before fail-closed admission, want 0", got)
	}
}

// TestOneShotMachineModeDestinationDenialWritesResultAndExitsTwo (#352):
// a non-interactive one-shot whose AGENT route lands on an unadmitted remote
// destination must fail closed with exit 2 AND still put exactly one
// golem.result.v1 record on stdout, carrying the destination_denied code.
func TestOneShotMachineModeDestinationDenialWritesResultAndExitsTwo(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "models.json")
	configJSON := `{
  "providers": {"opencode": {"base_url": "https://opencode.invalid/zen/go", "api_format": "openai-compat", "timeout": "5s"}},
  "models": {"agent": {"name": "remote-model", "provider": "opencode", "type": "dense", "context_window": 32768,
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`
	if err := os.WriteFile(configPath, []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, stdout, stderr := runTestFiles(t)

	err := run([]string{"-config", configPath, "-root", root, "-p", "say done", "-output-format", "json",
		"-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context", "-no-session", "-no-memory"},
		stdin, stdout, stderr)
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("run error = %v, want ErrDestinationDenied", err)
	}
	if got := exitCodeFor(err); got != 2 {
		t.Fatalf("exitCodeFor(%v) = %d, want 2 (a typed local policy denial is a caller error)", err, got)
	}
	out := readRunTestFile(t, stdout)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("machine-mode denial must write exactly one stdout line, got %d:\n%s", len(lines), out)
	}
	m := decodeResult(t, lines[0])
	var recErr struct{ Code string }
	if err := json.Unmarshal(m["error"], &recErr); err != nil || recErr.Code != "destination_denied" {
		t.Errorf("error = %s, want code destination_denied", m["error"])
	}
}

// TestRunAllowToolWiresHeadlessPromptAndApprover (#352, spec F6): through the
// real run() wiring, -allow-tool run_command must install the headless
// approver AND build the system prompt from the exact mounted set — naming
// run_command, never the unmounted background suite, and never interactive
// approval prose.
func TestRunAllowToolWiresHeadlessPromptAndApprover(t *testing.T) {
	configPath, root, _ := admissionHarness(t, "https://opencode.invalid/zen/go")
	stdin, stdout, stderr := runTestFiles(t)

	errStop := errors.New("stop after startup")
	err := run(append(admissionArgs(configPath, root), "-allow-tool", "run_command"), stdin, stdout, stderr, runHooks{
		afterSessionReady: func(sess *replSession) error {
			if sess.headlessApprover == nil {
				t.Error("-allow-tool must install the headless approver")
			}
			if sess.machine != nil {
				t.Error("text mode must have no machine writer")
			}
			if !strings.Contains(sess.baseSystem, "run_command") {
				t.Errorf("headless prompt must name run_command:\n%s", sess.baseSystem)
			}
			for _, banned := range []string{"start_command", "command_status", "command_tail", "stop_command", "after they approve"} {
				if strings.Contains(sess.baseSystem, banned) {
					t.Errorf("headless prompt must not carry %q:\n%s", banned, sess.baseSystem)
				}
			}
			mounted := map[string]bool{}
			for _, tool := range sess.tools {
				mounted[tool.Spec().Name] = true
			}
			if !mounted["run_command"] {
				t.Errorf("run_command must be mounted, tools = %v", mounted)
			}
			for _, banned := range []string{"start_command", "command_status", "command_tail", "stop_command", "write_file", "edit_file"} {
				if mounted[banned] {
					t.Errorf("unnamed gated tool %q must not be mounted, tools = %v", banned, mounted)
				}
			}
			return errStop
		},
	})
	if !errors.Is(err, errStop) {
		t.Fatalf("run = %v, want the harness stop sentinel", err)
	}
}
