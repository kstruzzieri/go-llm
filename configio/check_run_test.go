package configio

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

// rcRemoteBase is a remote endpoint; rcRemote's transport sends its
// connections to a stub.
const rcRemoteBase = "http://checks.example.test"

func rcFrame(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return "data: " + string(b) + "\n\n"
}

// rcCalled is a completed chat-stream-tools reply: one valid get_time call, a
// finish reason, usage and [DONE].
var rcCalled = rcFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{
	"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "get_time", "arguments": "{}"},
}}}}}}) +
	rcFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}}) +
	rcFrame(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}}) +
	"data: [DONE]\n\n"

// rcServe answers one request for model; n is how many requests for model came
// before it.
type rcServe func(w http.ResponseWriter, r *http.Request, model string, n int)

// rcStub serves both check APIs at one address, counts requests per model and
// keeps each connection's latest state.
type rcStub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	conns map[net.Conn]http.ConnState
}

// newRCStub serves every request with serve, or rcAnswer when nil.
func newRCStub(t *testing.T, serve rcServe) *rcStub {
	t.Helper()
	if serve == nil {
		serve = rcAnswer
	}
	s := &rcStub{hits: map[string]int{}, conns: map[net.Conn]http.ConnState{}}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Model, Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		model := cmp.Or(body.Model, body.Name)
		s.mu.Lock()
		n := s.hits[model]
		s.hits[model]++
		s.mu.Unlock()
		serve(w, r, model, n)
	}))
	s.srv.Config.ConnState = func(c net.Conn, state http.ConnState) {
		s.mu.Lock()
		s.conns[c] = state
		s.mu.Unlock()
	}
	s.srv.Start()
	t.Cleanup(s.srv.Close)
	return s
}

// connStates counts the connections the stub saw, those now idle and those
// closed.
func (s *rcStub) connStates() (seen, idle, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.conns {
		switch state {
		case http.StateIdle:
			idle++
		case http.StateClosed:
			closed++
		}
	}
	return len(s.conns), idle, closed
}

func (s *rcStub) count(model string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[model]
}

func (s *rcStub) sum() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, h := range s.hits {
		n += h
	}
	return n
}

// rcAnswer is a usable reply: a completed tools stream, or a show reply that
// lists tools.
func rcAnswer(w http.ResponseWriter, r *http.Request, _ string, _ int) {
	if r.URL.Path == "/api/show" {
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"completion", "tools"}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, rcCalled)
}

func rcStatus(code int, body string) rcServe {
	return func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// rcConfig is one provider at base speaking format, keyed with key, with one
// authored role per model.
func rcConfig(name, base, format, key string, models ...string) *config.Config {
	return &config.Config{
		Providers: map[string]config.ProviderConfig{name: {BaseURL: base, APIFormat: format, APIKey: key}},
		Models:    cpAuthored(name, models...),
	}
}

// rcRun runs plan and returns the results it reported, sorted by model.
func rcRun(t *testing.T, plan CheckPlan, opts RunOptions) ([]CheckResult, error) {
	t.Helper()
	var mu sync.Mutex
	var out []CheckResult
	next := opts.OnResult
	opts.OnResult = func(r CheckResult) {
		mu.Lock()
		out = append(out, r)
		mu.Unlock()
		if next != nil {
			next(r)
		}
	}
	err := RunCheck(t.Context(), plan, opts)
	mu.Lock()
	defer mu.Unlock()
	slices.SortFunc(out, func(a, b CheckResult) int { return strings.Compare(a.Model, b.Model) })
	return out, err
}

// rcRemote is a transport that sends every connection to stub, counting dials,
// and the policy that grants providerName at rcRemoteBase.
func rcRemote(t *testing.T, stub *rcStub, providerName string) (*http.Transport, provider.DestinationPolicy, *atomic.Int32) {
	t.Helper()
	dest, err := provider.NewDestination(providerName, rcRemoteBase)
	if err != nil {
		t.Fatal(err)
	}
	var dials atomic.Int32
	addr := stub.srv.Listener.Addr().String()
	tr := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return tr, provider.NewDestinationPolicy(dest), &dials
}

func rcWantCode(t *testing.T, err error, want ErrorCode) {
	t.Helper()
	if code, ok := CodeOf(err); !ok || code != want {
		t.Fatalf("RunCheck error = %v (code %q, %v); want code %q", err, code, ok, want)
	}
}

// rcToolsAnswers is what a completed tools stream with a valid call answers.
var rcToolsAnswers = []CheckAnswer{
	{Capability: "chat", State: "yes"}, {Capability: "stream", State: "yes"},
	{Capability: "tool_call", State: "yes", Detail: fingerprint.DetailToolCalled},
}

// rcWantAnswered fails unless results is one answered check of model per
// entry of models, in order.
func rcWantAnswered(t *testing.T, results []CheckResult, err error, models ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("RunCheck error = %v", err)
	}
	got := make([]string, len(results))
	for i, r := range results {
		got[i] = r.Model
		if r.Code != "" || len(r.Answers) == 0 || r.Requests < 1 {
			t.Fatalf("result %+v; want an answered check", r)
		}
	}
	if !slices.Equal(got, models) {
		t.Fatalf("results for %q; want %q", got, models)
	}
}

func TestRunCheck_KeyChangedIsStaleSendsNothing(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, newStore(t), "lc", []string{"m"}, cpTools, false)
	for name, key := range map[string]string{"another key": "sk-other-key", "no key": "", "an unexpanded key": "${GO_LLM_TEST_KEY}"} {
		t.Run(name, func(t *testing.T) {
			res, err := rcRun(t, plan, RunOptions{Config: rcConfig("lc", stub.srv.URL, "openai-compat", key, "m")})
			rcWantCode(t, err, CodePlanStale)
			if len(res) != 0 || stub.sum() != 0 {
				t.Fatalf("results %+v, server received %d; want none", res, stub.sum())
			}
		})
	}
	res, err := rcRun(t, plan, RunOptions{Config: cfg})
	rcWantAnswered(t, res, err, "m")
}

func TestRunCheck_EndpointChangedIsStaleSendsNothing(t *testing.T) {
	planned, moved := newRCStub(t, nil), newRCStub(t, nil)
	cfg := rcConfig("lc", planned.srv.URL, "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
	for name, edit := range map[string]func(*config.ProviderConfig){
		"another endpoint": func(pc *config.ProviderConfig) { pc.BaseURL = moved.srv.URL },
		"another api":      func(pc *config.ProviderConfig) { pc.APIFormat = "ollama" },
		"no such provider": nil,
	} {
		t.Run(name, func(t *testing.T) {
			changed := rcConfig("lc", planned.srv.URL, "openai-compat", cpKey, "m")
			if edit == nil {
				delete(changed.Providers, "lc")
			} else {
				pc := changed.Providers["lc"]
				edit(&pc)
				changed.Providers["lc"] = pc
			}
			res, err := rcRun(t, plan, RunOptions{Config: changed})
			rcWantCode(t, err, CodePlanStale)
			if len(res) != 0 || planned.sum() != 0 || moved.sum() != 0 {
				t.Fatalf("results %+v, servers received %d and %d; want none", res, planned.sum(), moved.sum())
			}
		})
	}
	res, err := rcRun(t, plan, RunOptions{Config: cfg})
	rcWantAnswered(t, res, err, "m")
}

func TestRunCheck_RemoteWithoutGrantSendsNothing(t *testing.T) {
	stub := newRCStub(t, nil)
	tr, grant, dials := rcRemote(t, stub, "rc")
	cfg := rcConfig("rc", rcRemoteBase, "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, nil, "rc", []string{"m"}, cpTools, false)
	otherHost, err := provider.NewDestination("rc", "http://other.example.test")
	if err != nil {
		t.Fatal(err)
	}
	otherProvider, err := provider.NewDestination("other", rcRemoteBase)
	if err != nil {
		t.Fatal(err)
	}
	for name, policy := range map[string]provider.DestinationPolicy{
		"another host":     provider.NewDestinationPolicy(otherHost),
		"another provider": provider.NewDestinationPolicy(otherProvider),
	} {
		t.Run(name, func(t *testing.T) {
			res, err := rcRun(t, plan, RunOptions{Config: cfg, Policy: policy, Transport: tr})
			rcWantCode(t, err, CodeDestinationNotGranted)
			if len(res) != 0 || dials.Load() != 0 || stub.sum() != 0 {
				t.Fatalf("results %+v, dials %d, server received %d; want none", res, dials.Load(), stub.sum())
			}
		})
	}
	res, err := rcRun(t, plan, RunOptions{Config: cfg, Policy: grant, Transport: tr})
	rcWantAnswered(t, res, err, "m")
}

func TestRunCheck_InstallRefusalSendsNothing(t *testing.T) {
	stub := newRCStub(t, nil)
	tr, _, dials := rcRemote(t, stub, "rc")
	cfg := rcConfig("rc", rcRemoteBase, "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, nil, "rc", []string{"m"}, cpTools, false)
	var usage atomic.Int32
	res, err := rcRun(t, plan, RunOptions{
		Config: cfg, Policy: provider.NewDestinationPolicy(), Transport: tr, Store: newStore(t),
		OnUsage: func(string, provider.Usage) { usage.Add(1) },
	})
	rcWantCode(t, err, CodeDestinationNotGranted)
	if !errors.Is(err, provider.ErrDestinationDenied) {
		t.Fatalf("error = %v; want it to wrap ErrDestinationDenied", err)
	}
	if len(res) != 0 || dials.Load() != 0 || stub.sum() != 0 || usage.Load() != 0 {
		t.Fatalf("results %+v, dials %d, server received %d, usage %d; want none", res, dials.Load(), stub.sum(), usage.Load())
	}
}

func TestRunCheck_LocalNeedsNoGrant(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
	res, err := rcRun(t, plan, RunOptions{Config: cfg})
	rcWantAnswered(t, res, err, "m")
	want := CheckResult{Model: "m", Method: fingerprint.MethodChatStreamTools, Requests: 1, Answers: rcToolsAnswers}
	if fmt.Sprint(res[0]) != fmt.Sprint(want) || stub.count("m") != 1 {
		t.Fatalf("result %+v, server received %d; want %+v and 1", res[0], stub.count("m"), want)
	}
	if err := RunCheck(t.Context(), plan, RunOptions{Config: cfg}); err != nil || stub.count("m") != 2 {
		t.Fatalf("RunCheck with no callbacks = %v, server received %d; want nil and a second request", err, stub.count("m"))
	}
}

// rcUnreadable is a store whose reads fail with text that must not leak.
type rcUnreadable struct{ fingerprint.EvidenceStore }

func (rcUnreadable) ReadSnapshot(context.Context, string, string, *string, string, []string, time.Time) (fingerprint.EvidenceSnapshot, error) {
	return fingerprint.EvidenceSnapshot{}, errors.New("disk I/O error at SECRET-DB-TEXT")
}

func TestRunCheck_StoreReadFailureSendsNothing(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
	res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: rcUnreadable{newStore(t)}})
	if !errors.Is(err, provider.ErrEvidenceUnreadable) || strings.Contains(err.Error(), "SECRET-DB-TEXT") {
		t.Fatalf("RunCheck error = %v; want ErrEvidenceUnreadable without the store's text", err)
	}
	if code, ok := CodeOf(err); ok || len(res) != 0 || stub.sum() != 0 {
		t.Fatalf("code %q (%v), results %+v, server received %d; want no code, nothing sent", code, ok, res, stub.sum())
	}
}

func TestRunCheck_RefuseEverythingNeverExceedsPlan(t *testing.T) {
	for _, tt := range []struct {
		name, format string
		status       int
	}{
		{"tools rejected", "openai-compat", 400},
		{"tools unprocessable", "openai-compat", 422},
		{"tools server error", "openai-compat", 500},
		{"show rejected", "ollama", 400},
		{"show server error", "ollama", 503},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newRCStub(t, rcStatus(tt.status, `{"error":{"message":"no"}}`))
			models := []string{"a", "b", "c"}
			cfg := rcConfig("p", stub.srv.URL, tt.format, cpKey, models...)
			plan := cpPlan(t, cfg, nil, "p", models, cpTools, false)
			res, err := rcRun(t, plan, RunOptions{Config: cfg})
			if err != nil || len(res) != len(models) {
				t.Fatalf("RunCheck = %+v, %v; want a result per model", res, err)
			}
			for i, item := range plan.Items {
				got := stub.count(item.Model)
				if got < 1 || got > item.MaxRequests || res[i].Requests != got {
					t.Fatalf("%s: server received %d, Requests %d; want 1..%d and equal", item.Model, got, res[i].Requests, item.MaxRequests)
				}
			}
		})
	}
}

func TestRunCheck_RedirectFollowsNothing(t *testing.T) {
	for _, format := range []string{"openai-compat", "ollama"} {
		t.Run(format, func(t *testing.T) {
			target := newRCStub(t, nil)
			origin := newRCStub(t, func(w http.ResponseWriter, r *http.Request, _ string, _ int) {
				http.Redirect(w, r, target.srv.URL+r.URL.Path, http.StatusTemporaryRedirect)
			})
			cfg := rcConfig("p", origin.srv.URL, format, cpKey, "m")
			plan := cpPlan(t, cfg, nil, "p", []string{"m"}, cpTools, false)
			res, err := rcRun(t, plan, RunOptions{Config: cfg})
			if err != nil || len(res) != 1 {
				t.Fatalf("RunCheck = %+v, %v; want one result", res, err)
			}
			if r := res[0]; r.Code != CodeMalformedResponse || r.Requests != 1 || len(r.Answers) != 0 {
				t.Fatalf("result %+v; want malformed_response after 1 request, no answers", r)
			}
			if origin.sum() != 1 || target.sum() != 0 {
				t.Fatalf("origin received %d, target %d; want 1 and 0", origin.sum(), target.sum())
			}
		})
	}
}

// rcPeak holds every request until release closes and records the most held
// at once.
type rcPeak struct {
	inflight, peak atomic.Int32
	release        chan struct{}
}

func (p *rcPeak) serve(w http.ResponseWriter, r *http.Request, model string, n int) {
	cur := p.inflight.Add(1)
	for old := p.peak.Load(); cur > old && !p.peak.CompareAndSwap(old, cur); old = p.peak.Load() {
	}
	select {
	case <-p.release:
	case <-r.Context().Done():
	}
	p.inflight.Add(-1)
	rcAnswer(w, r, model, n)
}

func TestRunCheck_ConcurrencyLimits(t *testing.T) {
	six := []string{"a", "b", "c", "d", "e", "f"}
	for _, tt := range []struct {
		name, format string
		remote       bool
		models       []string
		want         int32
	}{
		{"remote openai-compat", "openai-compat", true, six, 4},
		{"local openai-compat", "openai-compat", false, []string{"a", "b", "c"}, 1},
		{"ollama", "ollama", false, six, 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := &rcPeak{release: make(chan struct{})}
			stub := newRCStub(t, p.serve)
			opts := RunOptions{}
			base := stub.srv.URL
			if tt.remote {
				base = rcRemoteBase
				opts.Transport, opts.Policy, _ = rcRemote(t, stub, "p")
			}
			opts.Config = rcConfig("p", base, tt.format, cpKey, tt.models...)
			plan := cpPlan(t, opts.Config, nil, "p", tt.models, cpTools, false)

			type ran struct {
				res []CheckResult
				err error
			}
			done := make(chan ran, 1)
			go func() {
				res, err := rcRun(t, plan, opts)
				done <- ran{res, err}
			}()
			deadline := time.Now().Add(10 * time.Second)
			for p.inflight.Load() < tt.want {
				if time.Now().After(deadline) {
					close(p.release)
					t.Fatalf("in flight %d; want %d at once", p.inflight.Load(), tt.want)
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond) // room for one request too many
			close(p.release)
			out := <-done
			rcWantAnswered(t, out.res, out.err, tt.models...)
			if got := p.peak.Load(); got != tt.want {
				t.Fatalf("peak in flight = %d; want %d", got, tt.want)
			}
		})
	}
}

func TestRunCheck_SkipsNewlyFreshNeverAdds(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b", "c")
	b := cpBinding(t, cfg, "lc")
	planStore := newStore(t)
	cpAppend(t, planStore, cpYes(b, "c", "chat", "stream", "tool_call")...)
	plan := cpPlan(t, cfg, planStore, "lc", []string{"a", "b", "c"}, cpTools, false)
	if got := cpPlanned(plan); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("planned %q; want [a b]", got)
	}

	// At run time a is fresh and c, fresh when planned, has no answer.
	runStore := newStore(t)
	cpAppend(t, runStore, cpYes(b, "a", "chat", "stream", "tool_call")...)
	res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: runStore})
	rcWantAnswered(t, res, err, "b")
	if stub.count("a") != 0 || stub.count("b") != 1 || stub.count("c") != 0 {
		t.Fatalf("server received a=%d b=%d c=%d; want 0, 1, 0", stub.count("a"), stub.count("b"), stub.count("c"))
	}

	// Forced, a fresh model is checked anyway.
	forced := cpPlan(t, cfg, planStore, "lc", []string{"a", "b"}, cpTools, true)
	res, err = rcRun(t, forced, RunOptions{Config: cfg, Store: runStore})
	rcWantAnswered(t, res, err, "a", "b")
}

func TestRunCheck_UsageReported(t *testing.T) {
	for _, format := range []string{"openai-compat", "ollama"} {
		t.Run(format, func(t *testing.T) {
			stub := newRCStub(t, nil)
			cfg := rcConfig("p", stub.srv.URL, format, cpKey, "m")
			plan := cpPlan(t, cfg, nil, "p", []string{"m"}, cpTools, false)
			var got []string
			res, err := rcRun(t, plan, RunOptions{Config: cfg, OnUsage: func(model string, u provider.Usage) {
				got = append(got, fmt.Sprintf("%s %d/%d/%d", model, u.PromptTokens, u.CompletionTokens, u.TotalTokens))
			}})
			rcWantAnswered(t, res, err, "m")
			want := []string{"m 11/7/18"}
			if format == "ollama" {
				want = nil // a metadata check generates nothing
			}
			if !slices.Equal(got, want) {
				t.Fatalf("usage reported %q; want %q", got, want)
			}
		})
	}
}

func TestRunCheck_CustomTransportUsed(t *testing.T) {
	t.Run("loopback", func(t *testing.T) {
		stub := newRCStub(t, nil)
		var dials atomic.Int32
		tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
		t.Cleanup(tr.CloseIdleConnections)
		cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
		plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
		res, err := rcRun(t, plan, RunOptions{Config: cfg, Transport: tr})
		rcWantAnswered(t, res, err, "m")
		if dials.Load() < 1 {
			t.Fatal("the supplied transport dialed nothing")
		}
	})
	t.Run("remote", func(t *testing.T) {
		stub := newRCStub(t, nil)
		tr, grant, dials := rcRemote(t, stub, "rc")
		cfg := rcConfig("rc", rcRemoteBase, "openai-compat", cpKey, "m")
		plan := cpPlan(t, cfg, nil, "rc", []string{"m"}, cpTools, false)
		res, err := rcRun(t, plan, RunOptions{Config: cfg, Policy: grant, Transport: tr})
		rcWantAnswered(t, res, err, "m")
		if dials.Load() < 1 {
			t.Fatal("the supplied transport dialed nothing")
		}
	})
}

func TestRunCheck_ModifiedPlanFieldsSendNothing(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m", "n")
	fresh := func() CheckPlan { return cpPlan(t, cfg, nil, "lc", []string{"m", "n"}, cpTools, false) }
	for name, edit := range map[string]func(*CheckPlan){
		"provider":                      func(p *CheckPlan) { p.Provider = "other" },
		"endpoint host":                 func(p *CheckPlan) { p.EndpointHost = "127.0.0.1:1" },
		"adapter":                       func(p *CheckPlan) { p.Adapter = fingerprint.AdapterOllama },
		"force":                         func(p *CheckPlan) { p.Force = true },
		"model":                         func(p *CheckPlan) { p.Items[0].Model = "x" },
		"capabilities":                  func(p *CheckPlan) { p.Items[0].Capabilities = []string{"chat"} },
		"methods":                       func(p *CheckPlan) { p.Items[0].Methods = []string{fingerprint.MethodChatStream} },
		"usual requests":                func(p *CheckPlan) { p.Items[0].UsualRequests++ },
		"max requests":                  func(p *CheckPlan) { p.Items[0].MaxRequests++ },
		"max output tokens per request": func(p *CheckPlan) { p.Items[0].MaxOutputTokensPerRequest++ },
		"model loads":                   func(p *CheckPlan) { p.Items[0].ModelLoads++ },
		"request timeout":               func(p *CheckPlan) { p.Items[0].RequestTimeout += time.Second },
		"max response bytes":            func(p *CheckPlan) { p.Items[0].MaxResponseBytes++ },
		"an item added":                 func(p *CheckPlan) { p.Items = append(p.Items, p.Items[0]) },
		"an item removed":               func(p *CheckPlan) { p.Items = p.Items[:1] },
		"items reordered":               func(p *CheckPlan) { p.Items[0], p.Items[1] = p.Items[1], p.Items[0] },
	} {
		t.Run(name, func(t *testing.T) {
			p := fresh()
			edit(&p)
			res, err := rcRun(t, p, RunOptions{Config: cfg})
			rcWantCode(t, err, CodePlanStale)
			if len(res) != 0 || stub.sum() != 0 {
				t.Fatalf("results %+v, server received %d; want none", res, stub.sum())
			}
		})
	}
	res, err := rcRun(t, fresh(), RunOptions{Config: cfg})
	rcWantAnswered(t, res, err, "m", "n")
}

func TestRunCheck_RecipeOrPolicyChangedIsStale(t *testing.T) {
	stub := newRCStub(t, nil)
	newCfg := func() *config.Config { return rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m") }
	cfg := newCfg()
	plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)

	retyped := newCfg()
	retyped.Models["lc-extra"] = config.ModelConfig{Provider: "lc", Name: "m", Type: "moe"}
	declared := newCfg()
	declared.Models["lc-extra"] = config.ModelConfig{Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}}
	timed := newCfg()
	pc := timed.Providers["lc"]
	pc.Timeout = config.Duration{Duration: time.Minute}
	timed.Providers["lc"] = pc
	for name, changed := range map[string]*config.Config{
		"another role type":         retyped,
		"a declared capability set": declared,
		"another provider timeout":  timed,
	} {
		t.Run(name, func(t *testing.T) {
			res, err := rcRun(t, plan, RunOptions{Config: changed})
			rcWantCode(t, err, CodePlanStale)
			if len(res) != 0 || stub.sum() != 0 {
				t.Fatalf("results %+v, server received %d; want none", res, stub.sum())
			}
		})
	}

	t.Run("another recipe contract", func(t *testing.T) {
		// A plan an older binary sealed under a recipe this one no longer runs.
		older := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
		st := older.state
		st.inputs.items[0].recipes[0].Version++
		st.identity = sealPlan(st.secret, cpKey, &st.inputs)
		res, err := rcRun(t, older, RunOptions{Config: cfg})
		rcWantCode(t, err, CodePlanStale)
		if len(res) != 0 || stub.sum() != 0 {
			t.Fatalf("results %+v, server received %d; want none", res, stub.sum())
		}
	})
	res, err := rcRun(t, plan, RunOptions{Config: cfg})
	rcWantAnswered(t, res, err, "m")
}

func TestRunCheck_ZeroOrJSONReconstructedPlanIsStale(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CheckPlan
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	copied := CheckPlan{Provider: plan.Provider, EndpointHost: plan.EndpointHost, Adapter: plan.Adapter, Force: plan.Force, Items: plan.Items}
	for name, p := range map[string]CheckPlan{"zero": {}, "decoded from JSON": decoded, "built from its fields": copied} {
		t.Run(name, func(t *testing.T) {
			res, err := rcRun(t, p, RunOptions{Config: cfg})
			rcWantCode(t, err, CodePlanStale)
			if len(res) != 0 || stub.sum() != 0 {
				t.Fatalf("results %+v, server received %d; want none", res, stub.sum())
			}
		})
	}
}

func TestRunCheck_NilStoreStillChecksCurrentKey(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
	res, err := rcRun(t, plan, RunOptions{Config: cfg})
	rcWantAnswered(t, res, err, "m")
	if res[0].Persisted {
		t.Fatal("Persisted = true with no store")
	}
	res, err = rcRun(t, plan, RunOptions{Config: rcConfig("lc", stub.srv.URL, "openai-compat", "sk-other-key", "m")})
	rcWantCode(t, err, CodePlanStale)
	if len(res) != 0 || stub.sum() != 1 {
		t.Fatalf("results %+v, server received %d; want none after the first check", res, stub.sum())
	}
}

func TestRunCheck_StoreBecomesDisabledDoesNotLoseSeal(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	b := cpBinding(t, cfg, "lc")
	store := newStore(t)
	plan := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false)

	res, err := rcRun(t, plan, RunOptions{Config: cfg}) // the store is gone
	rcWantAnswered(t, res, err, "m")
	if rows, err := store.EvidenceRows(t.Context(), b.Endpoint, "m"); err != nil || len(rows) != 0 || res[0].Persisted {
		t.Fatalf("rows %d (%v), Persisted %v; want none saved", len(rows), err, res[0].Persisted)
	}
	res, err = rcRun(t, plan, RunOptions{Config: rcConfig("lc", stub.srv.URL, "openai-compat", "sk-other-key", "m")})
	rcWantCode(t, err, CodePlanStale)
	if len(res) != 0 {
		t.Fatalf("results %+v; want none", res)
	}

	res, err = rcRun(t, plan, RunOptions{Config: cfg, Store: store}) // and back
	rcWantAnswered(t, res, err, "m")
	if rows, err := store.EvidenceRows(t.Context(), b.Endpoint, "m"); err != nil || len(rows) != 3 || !res[0].Persisted {
		t.Fatalf("rows %d (%v), Persisted %v; want 3 saved", len(rows), err, res[0].Persisted)
	}
}

func TestRunCheck_OneFailureDoesNotCancelOtherModels(t *testing.T) {
	failed := make(chan struct{})
	stub := newRCStub(t, func(w http.ResponseWriter, r *http.Request, model string, n int) {
		if model == "a" {
			rcStatus(500, `{"error":"broken"}`)(w, r, model, n)
			return
		}
		select { // b and c answer only after a's failure was reported
		case <-failed:
		case <-time.After(10 * time.Second):
		}
		rcAnswer(w, r, model, n)
	})
	models := []string{"a", "b", "c"}
	cfg := rcConfig("ol", stub.srv.URL, "ollama", cpKey, models...)
	b := cpBinding(t, cfg, "ol")
	store := newStore(t)
	plan := cpPlan(t, cfg, store, "ol", models, cpTools, false)
	var once sync.Once
	res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store, OnResult: func(r CheckResult) {
		if r.Model == "a" {
			once.Do(func() { close(failed) })
		}
	}})
	if err != nil || len(res) != 3 {
		t.Fatalf("RunCheck = %+v, %v; want three results", res, err)
	}
	if res[0].Code != CodeServerError || res[0].Persisted {
		t.Fatalf("a = %+v; want server_error, nothing saved", res[0])
	}
	for _, r := range res[1:] {
		rows, _ := store.EvidenceRows(t.Context(), b.Endpoint, r.Model)
		if r.Code != "" || !r.Persisted || len(rows) == 0 {
			t.Fatalf("%s = %+v with %d rows; want answered and saved", r.Model, r, len(rows))
		}
	}
}

func TestRunCheck_CallbacksSerializedAfterCommit(t *testing.T) {
	models := []string{"a", "b", "c", "d"}
	var arrived atomic.Int32
	all := make(chan struct{})
	stub := newRCStub(t, func(w http.ResponseWriter, r *http.Request, model string, n int) {
		if arrived.Add(1) == int32(len(models)) {
			close(all)
		}
		select { // answer all four together
		case <-all:
		case <-time.After(10 * time.Second):
		}
		rcAnswer(w, r, model, n)
	})
	tr, grant, _ := rcRemote(t, stub, "rc")
	cfg := rcConfig("rc", rcRemoteBase, "openai-compat", cpKey, models...)
	b := cpBinding(t, cfg, "rc")
	store := newStore(t)
	plan := cpPlan(t, cfg, store, "rc", models, cpTools, false)

	var active, overlaps, usages atomic.Int32
	enter := func() {
		if active.Add(1) != 1 {
			overlaps.Add(1)
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
	}
	var unsaved []string
	res, err := rcRun(t, plan, RunOptions{
		Config: cfg, Store: store, Policy: grant, Transport: tr,
		OnUsage: func(string, provider.Usage) { usages.Add(1); enter() },
		OnResult: func(r CheckResult) {
			if rows, err := store.EvidenceRows(context.Background(), b.Endpoint, r.Model); err != nil || len(rows) != 3 {
				unsaved = append(unsaved, r.Model)
			}
			enter()
		},
	})
	rcWantAnswered(t, res, err, models...)
	if overlaps.Load() != 0 || usages.Load() != int32(len(models)) || len(unsaved) != 0 {
		t.Fatalf("overlapping callbacks %d, usage callbacks %d, results before their rows %q; want 0, %d, none",
			overlaps.Load(), usages.Load(), unsaved, len(models))
	}
}

func TestRunCheck_ResponseTooLargeStopsWithinRecipeLimit(t *testing.T) {
	for _, format := range []string{"openai-compat", "ollama"} {
		t.Run(format, func(t *testing.T) {
			var written atomic.Int64
			stub := newRCStub(t, func(w http.ResponseWriter, r *http.Request, _ string, _ int) {
				// An endless reply: only the response limit can end it before
				// the request timeout.
				chunk := strings.Repeat("x", 4096)
				if r.URL.Path == "/api/show" {
					_, _ = io.WriteString(w, `{"template":"`)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					chunk = rcFrame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": chunk}}}})
				}
				for r.Context().Err() == nil {
					n, err := io.WriteString(w, chunk)
					written.Add(int64(n))
					if err != nil {
						return
					}
				}
			})
			cfg := rcConfig("p", stub.srv.URL, format, cpKey, "m")
			b := cpBinding(t, cfg, "p")
			store := newStore(t)
			plan := cpPlan(t, cfg, store, "p", []string{"m"}, cpTools, false)
			start := time.Now()
			res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
			if err != nil || len(res) != 1 {
				t.Fatalf("RunCheck = %+v, %v; want one result", res, err)
			}
			if r := res[0]; r.Code != CodeResponseTooLarge || r.Requests != 1 || len(r.Answers) != 0 || r.Persisted {
				t.Fatalf("result %+v; want response_too_large after 1 request, nothing answered or saved", r)
			}
			if rows, _ := store.EvidenceRows(t.Context(), b.Endpoint, "m"); len(rows) != 0 {
				t.Fatalf("saved %d rows; want none", len(rows))
			}
			if d := time.Since(start); d > 20*time.Second || stub.count("m") != 1 {
				t.Fatalf("took %v over %d requests; want the byte limit to end one request", d, stub.count("m"))
			}
			if n := written.Load(); n > 64<<20 {
				t.Fatalf("server wrote %d bytes before the check stopped reading", n)
			}
		})
	}
}

func TestRunCheck_Invariants_SubsetAndPhysicalBounds(t *testing.T) {
	stub := newRCStub(t, func(w http.ResponseWriter, r *http.Request, model string, n int) {
		if model == "b" { // rejected twice: the tools recipe's most
			rcStatus(400, `{"error":{"message":"no"}}`)(w, r, model, n)
			return
		}
		rcAnswer(w, r, model, n)
	})
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "a", "b", "c", "d")
	bind := cpBinding(t, cfg, "lc")
	store := newStore(t)
	plan := cpPlan(t, cfg, store, "lc", []string{"c", "b", "a", "b", "c", "d", "a"}, cpTools, false)
	if same := cpPlan(t, cfg, store, "lc", []string{"d", "a", "c", "b"}, cpTools, false); fmt.Sprint(same.Items) != fmt.Sprint(plan.Items) {
		t.Fatalf("permuted inputs planned %+v; want %+v", same.Items, plan.Items)
	}
	cpAppend(t, store, cpYes(bind, "d", "chat", "stream", "tool_call")...) // d fresh by run time

	res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	planned := map[string]CheckItem{}
	allowed := 0
	for _, it := range plan.Items {
		planned[it.Model] = it
		allowed += it.MaxRequests
	}
	stub.mu.Lock()
	hits := maps.Clone(stub.hits)
	stub.mu.Unlock()
	sent := 0
	for model, n := range hits {
		it, ok := planned[model]
		if !ok || n > it.MaxRequests {
			t.Fatalf("server received %d requests for %q; planned %v with at most %d", n, model, ok, it.MaxRequests)
		}
		sent += n
	}
	if sent > allowed || hits["d"] != 0 || hits["b"] != 2 {
		t.Fatalf("sent %d of at most %d, d=%d, b=%d; want d unchecked and b at its most", sent, allowed, hits["d"], hits["b"])
	}
	seen := map[string]bool{}
	for _, r := range res {
		if _, ok := planned[r.Model]; !ok || seen[r.Model] || r.Requests != hits[r.Model] {
			t.Fatalf("result %+v: planned %v, repeated %v, server received %d", r, ok, seen[r.Model], hits[r.Model])
		}
		seen[r.Model] = true
	}
}

func TestRunCheck_LoopbackDialsWithoutDNSOrProxy(t *testing.T) {
	stub := newRCStub(t, nil)
	port := stub.srv.Listener.Addr().(*net.TCPAddr).Port
	var proxied atomic.Int32
	var mu sync.Mutex
	var addrs []string
	tr := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) { proxied.Add(1); return nil, nil },
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			addrs = append(addrs, addr)
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	cfg := rcConfig("lc", fmt.Sprintf("http://localhost:%d", port), "openai-compat", cpKey, "m")
	plan := cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false)
	res, err := rcRun(t, plan, RunOptions{Config: cfg, Transport: tr})
	rcWantAnswered(t, res, err, "m")
	mu.Lock()
	defer mu.Unlock()
	want := []string{fmt.Sprintf("127.0.0.1:%d", port), fmt.Sprintf("[::1]:%d", port)}
	if proxied.Load() != 0 || len(addrs) == 0 {
		t.Fatalf("proxy consulted %d times, dials %q; want none and at least one", proxied.Load(), addrs)
	}
	for _, a := range addrs {
		if !slices.Contains(want, a) {
			t.Fatalf("dialed %q; want only %q", a, want)
		}
	}
}

func TestRunCheck_NilTransportUsesDefault(t *testing.T) {
	for _, format := range []string{"openai-compat", "ollama"} {
		t.Run(format, func(t *testing.T) {
			stub := newRCStub(t, nil)
			cfg := rcConfig("p", stub.srv.URL, format, cpKey, "m")
			plan := cpPlan(t, cfg, nil, "p", []string{"m"}, cpTools, false)
			var none *http.Transport
			res, err := rcRun(t, plan, RunOptions{Config: cfg, Transport: none})
			rcWantAnswered(t, res, err, "m")
		})
	}
}

func TestRunCheck_DigestOrder(t *testing.T) {
	const listed, runtime, reply = "sha256:listed", "sha256:runtime", "sha256:reply"
	for _, tt := range []struct {
		name                          string
		reply, runtime, listing, want string
	}{
		{"the reply's digest first", reply, runtime, listed, reply},
		{"then the runtime's", "", runtime, listed, runtime},
		{"then the listing's", "", "", listed, listed},
		{"else name only", "", "", "", ""},
	} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s force=%v", tt.name, force), func(t *testing.T) {
				stub := newRCStub(t, func(w http.ResponseWriter, _ *http.Request, _ string, _ int) {
					body := map[string]any{"capabilities": []string{"completion", "tools"}}
					if tt.reply != "" {
						body["digest"] = tt.reply
					}
					_ = json.NewEncoder(w).Encode(body)
				})
				cfg := rcConfig("ol", stub.srv.URL, "ollama", cpKey, "m")
				b := cpBinding(t, cfg, "ol")
				store := newStore(t)
				if tt.listing != "" {
					pub := fingerprint.ListingPublish{Endpoint: b.Endpoint, Adapter: b.Adapter, APIKey: cpKey, StartedAt: time.Now().Add(-time.Minute),
						Members: []fingerprint.ListingMember{{Model: "m", Digest: tt.listing}}}
					if err := store.PublishListings(t.Context(), []fingerprint.ListingPublish{pub}); err != nil {
						t.Fatal(err)
					}
				}
				var digests map[string]string
				if tt.runtime != "" {
					digests = map[string]string{"m": tt.runtime}
				}
				plan, err := PlanCheck(t.Context(), "ol", []string{"m"}, cpTools, CheckOptions{Config: cfg, Store: store, Force: force, RuntimeDigests: digests})
				if err != nil {
					t.Fatal(err)
				}
				before := time.Now().Truncate(time.Millisecond)
				res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
				rcWantAnswered(t, res, err, "m")
				rows, err := store.EvidenceRows(t.Context(), b.Endpoint, "m")
				if err != nil || len(rows) == 0 || !res[0].Persisted {
					t.Fatalf("rows %d (%v), Persisted %v; want rows saved", len(rows), err, res[0].Persisted)
				}
				for _, r := range rows {
					want := r
					want.FreshUntil, want.ExpiresAt = fingerprint.ExpiryFor(r, 0)
					if r.Digest != tt.want || r.Model != "m" || r.Endpoint != b.Endpoint || r.Adapter != b.Adapter ||
						r.KeyVersion != fingerprint.EvidenceKeyVersion || r.Method != fingerprint.MethodOllamaShow ||
						r.MethodVersion != fingerprint.MethodVersion(fingerprint.MethodOllamaShow) ||
						r.Source != fingerprint.EvidenceSourceProvider || r.ObservedAt.Before(before) ||
						r.ObservedAt.Nanosecond()%int(time.Millisecond) != 0 ||
						!r.FreshUntil.Equal(want.FreshUntil) || !r.ExpiresAt.Equal(want.ExpiresAt) {
						t.Fatalf("row %+v; want an ollama-show row for m at digest %q, observed on a whole millisecond, with ExpiryFor's expiry", r, tt.want)
					}
				}
			})
		}
	}
}

// rcRouteProber is a registry's capability prober that counts the checks asked
// of it and answers tool_call yes.
type rcRouteProber struct{ calls atomic.Int32 }

func (p *rcRouteProber) DetectKind(context.Context, string) (*fingerprint.KindDetection, error) {
	return nil, errors.New("not expected")
}

func (p *rcRouteProber) ProbeChat(context.Context, string, interface{}) (*fingerprint.ChatMetrics, error) {
	return nil, errors.New("not expected")
}

func (p *rcRouteProber) ProbeEmbedding(context.Context, string) (*fingerprint.EmbeddingMetrics, error) {
	return nil, errors.New("not expected")
}

func (p *rcRouteProber) ProbeCapabilities(_ context.Context, _ string, req fingerprint.CapabilityProbeRequest) (fingerprint.CapabilityProbeResult, error) {
	p.calls.Add(1)
	return fingerprint.CapabilityProbeResult{Method: req.Method, Requests: 1, Answers: []fingerprint.ProbeAnswer{{
		Capability: "tool_call", Source: fingerprint.EvidenceSourceProvider, Kind: fingerprint.EvidenceKindAnswer, State: fingerprint.CapProbeYes,
	}}}, nil
}

// TestRunCheck_FiledWhereRoutingReads wires RunCheck, the store and a log-bound
// registry together, mocking only the Ollama server. The check's reply names no
// digest, so its rows are filed under the host's runtime digest; routing, whose
// runtime reports that digest and which has no live, catalog or floor knowledge
// of the model, reads tool_call from them and checks nothing.
func TestRunCheck_FiledWhereRoutingReads(t *testing.T) {
	const model, planned = "zz-runcheck-model:7b", "sha256:runtime-d2"
	for _, tt := range []struct {
		name, runtime string
		read          bool
	}{
		{"routing's runtime reports the planned digest", planned, true},
		{"routing's runtime reports another digest", "sha256:runtime-d1", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var routing atomic.Bool // after the check, show reports nothing live
			stub := newRCStub(t, func(w http.ResponseWriter, r *http.Request, _ string, _ int) {
				switch {
				case r.URL.Path == "/api/tags":
					_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"name": model, "digest": tt.runtime}}})
				case routing.Load():
					_ = json.NewEncoder(w).Encode(map[string]any{})
				default:
					_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": []string{"completion", "tools"}})
				}
			})
			cfg := rcConfig("ol", stub.srv.URL, "ollama", cpKey, model)
			b := cpBinding(t, cfg, "ol")
			store := newStore(t)
			plan, err := PlanCheck(t.Context(), "ol", []string{model}, provider.CapToolCall,
				CheckOptions{Config: cfg, Store: store, RuntimeDigests: map[string]string{model: planned}})
			if err != nil {
				t.Fatal(err)
			}
			res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: store})
			rcWantAnswered(t, res, err, model)
			if !res[0].Persisted {
				t.Fatalf("result %+v; want it saved", res[0])
			}
			routing.Store(true)

			reg := provider.NewRegistry()
			if err := reg.Register(provider.NewOllamaProvider(ollama.NewClient(ollama.WithBaseURL(stub.srv.URL)), provider.WithProviderName("ol"))); err != nil {
				t.Fatal(err)
			}
			prober := &rcRouteProber{}
			mr, err := provider.NewModelRegistry(reg, nil,
				provider.WithEvidenceLog(store, func(name string) (provider.EvidenceBinding, bool) { return b, name == "ol" }),
				provider.WithCapabilityProber(func(context.Context, provider.ModelKey, *provider.ModelInfo, provider.Provider) (*provider.FingerprintProberSpec, error) {
					return &provider.FingerprintProberSpec{Prober: prober}, nil
				}))
			if err != nil {
				t.Fatal(err)
			}
			key := provider.ModelKey{Provider: "ol", Model: model}
			profile, err := mr.Lookup(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if got := profile.Caps.Has(provider.CapToolCall); got != tt.read {
				t.Fatalf("Lookup tool_call = %v; want %v", got, tt.read)
			}
			state, err := mr.ResolveToolCall(t.Context(), key)
			if err != nil || state != fingerprint.CapProbeYes {
				t.Fatalf("ResolveToolCall = %q, %v; want yes", state, err)
			}
			if want := map[bool]int32{true: 0, false: 1}[tt.read]; prober.calls.Load() != want {
				t.Fatalf("routing checked %d times; want %d", prober.calls.Load(), want)
			}
		})
	}
}

// rcAppendFails is a store whose check appends fail with text that must not
// leak; it reads as the store it wraps.
type rcAppendFails struct{ fingerprint.EvidenceStore }

func (rcAppendFails) AppendCheckResult(context.Context, string, string, string, []fingerprint.EvidenceRow, *fingerprint.ModelAccess) error {
	return errors.New("database is locked at SECRET-DB-APPEND")
}

func TestRunCheck_AppendFailureReportsUnsaved(t *testing.T) {
	stub := newRCStub(t, nil)
	cfg := rcConfig("lc", stub.srv.URL, "openai-compat", cpKey, "m")
	b := cpBinding(t, cfg, "lc")
	store := newStore(t)
	plan := cpPlan(t, cfg, store, "lc", []string{"m"}, cpTools, false)
	res, err := rcRun(t, plan, RunOptions{Config: cfg, Store: rcAppendFails{store}})
	rcWantAnswered(t, res, err, "m")
	if r := res[0]; r.Persisted || fmt.Sprint(r.Answers) != fmt.Sprint(rcToolsAnswers) {
		t.Fatalf("result %+v; want its answers, not persisted", r)
	}
	shown, _ := json.Marshal(res[0])
	if text := fmt.Sprintf("%+v %s", res[0], shown); strings.Contains(text, "SECRET-DB-APPEND") {
		t.Fatalf("result carries the store's text: %s", text)
	}
	if rows, err := store.EvidenceRows(t.Context(), b.Endpoint, "m"); err != nil || len(rows) != 0 {
		t.Fatalf("rows %d (%v); want none", len(rows), err)
	}
}

// TestRunCheck_ClosesRunOwnedConnections: when RunCheck returns, the idle
// connections of a transport the run made are closed: the guard's clone at a
// loopback endpoint, and its copy of the default at a remote one. A host's
// remote transport, which the guard uses as is, keeps its idle connection.
// Ollama's metadata reply has a length, so a kept connection goes idle.
func TestRunCheck_ClosesRunOwnedConnections(t *testing.T) {
	run := func(t *testing.T, stub *rcStub, base string, opts RunOptions) {
		t.Helper()
		opts.Config = rcConfig("ol", base, "ollama", cpKey, "m")
		plan := cpPlan(t, opts.Config, nil, "ol", []string{"m"}, cpTools, false)
		res, err := rcRun(t, plan, opts)
		rcWantAnswered(t, res, err, "m")
	}
	wantClosed := func(t *testing.T, stub *rcStub) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			seen, _, closed := stub.connStates()
			if seen > 0 && closed == seen {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("connections seen %d, closed %d; want every one closed when RunCheck returns", seen, closed)
			}
		}
	}

	t.Run("loopback", func(t *testing.T) {
		stub := newRCStub(t, nil)
		run(t, stub, stub.srv.URL, RunOptions{})
		wantClosed(t, stub)
	})
	t.Run("loopback with a host transport", func(t *testing.T) {
		stub := newRCStub(t, nil)
		tr := &http.Transport{}
		t.Cleanup(tr.CloseIdleConnections)
		run(t, stub, stub.srv.URL, RunOptions{Transport: tr})
		wantClosed(t, stub)
	})
	t.Run("remote with the default transport", func(t *testing.T) {
		stub := newRCStub(t, nil)
		_, grant, _ := rcRemote(t, stub, "ol")
		// The guard copies http.DefaultTransport for a remote endpoint; this one
		// dials the stub. Not parallel: it swaps a global.
		addr := stub.srv.Listener.Addr().String()
		saved := http.DefaultTransport
		http.DefaultTransport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}}
		t.Cleanup(func() { http.DefaultTransport = saved })
		run(t, stub, rcRemoteBase, RunOptions{Policy: grant})
		wantClosed(t, stub)
	})
	t.Run("remote with a host transport", func(t *testing.T) {
		stub := newRCStub(t, nil)
		tr, grant, _ := rcRemote(t, stub, "ol")
		run(t, stub, rcRemoteBase, RunOptions{Policy: grant, Transport: tr})
		time.Sleep(200 * time.Millisecond) // room for a close that must not come
		if seen, idle, closed := stub.connStates(); seen != 1 || idle != 1 || closed != 0 {
			t.Fatalf("connections seen %d, idle %d, closed %d; want the host's one connection kept idle", seen, idle, closed)
		}
	})
}

// TestCheckJSON_FieldNames pins the wire names hosts read. A duration is Go's
// integer nanoseconds.
func TestCheckJSON_FieldNames(t *testing.T) {
	cfg := rcConfig("lc", cpLocal, "openai-compat", cpKey, "m")
	declared := rcConfig("lc", cpLocal, "openai-compat", cpKey) // an explicit list is never planned
	declared.Models = map[string]config.ModelConfig{"lc-00": {Provider: "lc", Name: "m", Type: "dense", Capabilities: []string{"chat"}}}
	for _, tt := range []struct {
		name string
		v    any
		want string
	}{
		{"plan", cpPlan(t, cfg, nil, "lc", []string{"m"}, cpTools, false),
			`{"provider":"lc","endpoint_host":"127.0.0.1:8080","adapter":"openai-compat","force":false,"items":[` +
				`{"model":"m","capabilities":["chat","stream","tool_call"],"methods":["chat-stream-tools"],"usual_requests":1,` +
				`"max_requests":2,"max_output_tokens_per_request":2048,"model_loads":1,"request_timeout":180000000000,"max_response_bytes":1048576}]}`},
		{"empty plan", cpPlan(t, declared, nil, "lc", []string{"m"}, cpTools, false).Items, `[]`},
		{"answered", CheckResult{Model: "m", Method: fingerprint.MethodChatStreamTools, Requests: 1, Answers: rcToolsAnswers, Persisted: true},
			`{"model":"m","method":"chat-stream-tools","requests":1,"answers":[{"capability":"chat","state":"yes"},` +
				`{"capability":"stream","state":"yes"},{"capability":"tool_call","state":"yes","detail":"tool_called"}],"persisted":true}`},
		{"failed", CheckResult{Model: "m", Method: fingerprint.MethodChatStreamTools, Code: CodeServerError, Requests: 1},
			`{"model":"m","method":"chat-stream-tools","code":"server_error","requests":1,"persisted":false}`},
	} {
		got, err := json.Marshal(tt.v)
		if err != nil || string(got) != tt.want {
			t.Errorf("%s = %s, %v; want %s", tt.name, got, err, tt.want)
		}
	}
}
