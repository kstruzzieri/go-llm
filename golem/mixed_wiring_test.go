package golem_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/golem"
)

// The two anchor payloads differ in every traced dimension (depth, rank, byte
// count) so a trace row built from the wrong group cannot pass the content pin.
const (
	wiringCardContent     = "CARD ONE"
	wiringEvidenceContent = "EVIDENCE TWO LINES"
	// wiringModel is a catalog family/variant (smallest qwen2.5, think_mode
	// none) so the routed profile carries a context window and no reasoning
	// wire quirks.
	wiringModel = "qwen2.5:0.5b"
)

// wiringContextSet is the structured payload wiringTool attaches to its result.
// It is the anchor that makes step 1's assembly a MIXED one; without it every
// assembly short-circuits to the legacy path and emits nothing.
func wiringContextSet() *agent.ContextSet {
	return &agent.ContextSet{Groups: []agent.ContextGroup{{
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "one.go"},
			Rank:    7,
		},
		Alternatives: []agent.ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL0, Kind: contextdepth.RepresentationMetadata},
			}},
			Content: wiringCardContent,
		}},
	}, {
		Desc: contextdepth.GroupDesc{
			Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "two.go"},
			Rank:    3,
		},
		Alternatives: []agent.ContextAlternative{{
			Desc: contextdepth.AlternativeDesc{Representations: []contextdepth.RepresentationDesc{
				{Depth: contextdepth.DepthL2, Kind: contextdepth.RepresentationVerbatim},
			}},
			Content: wiringEvidenceContent,
			Attrib: &agent.RetrievalAttribution{Sources: []agent.RetrievedSource{{
				StableKey: "wiring-source", Source: "one.go", StartLine: 1, EndLine: 2, Score: 0.9,
			}}},
		}},
	}}}
}

// wiringTool is a read-only tool whose result carries a structured ContextSet.
type wiringTool struct{}

func (wiringTool) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: "ctxtool", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (wiringTool) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (wiringTool) Invoke(context.Context, json.RawMessage) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "ctx:fallback", Context: wiringContextSet()}, nil
}

// assemblyHost is a consumer observer implementing the OPTIONAL
// ContextAssemblyObserver. err, when set, is returned from OnContextAssembly.
type assemblyHost struct {
	events []agent.ContextAssemblyEvent
	err    error
}

func (*assemblyHost) OnStep(context.Context, agent.StepEvent) error         { return nil }
func (*assemblyHost) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (*assemblyHost) OnToken(context.Context, agent.TokenEvent) error       { return nil }
func (h *assemblyHost) OnContextAssembly(_ context.Context, e agent.ContextAssemblyEvent) error {
	h.events = append(h.events, e)
	return h.err
}

// plainHost implements Observer and NOTHING optional.
type plainHost struct{ steps int }

func (h *plainHost) OnStep(context.Context, agent.StepEvent) error         { h.steps++; return nil }
func (h *plainHost) OnToolCall(context.Context, agent.ToolCallEvent) error { return nil }
func (h *plainHost) OnToken(context.Context, agent.TokenEvent) error       { return nil }

// toolCallStreamServer serves an openai-compat backend whose first
// /v1/chat/completions call requests ctxtool and whose second answers. The tool
// step is what puts a structured anchor in State, so step 1's assembly is the
// mixed one under test. calls counts chat completions so a test can prove the
// assembly it asserts about actually ran.
func toolCallStreamServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"` + wiringModel + `"}]}`))
		case "/v1/chat/completions":
			n := calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("httptest response writer is not a Flusher")
				return
			}
			frames := []string{
				`{"choices":[{"index":0,"delta":{"role":"assistant","content":"done"}}]}`,
				`{"choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop"}]}`,
			}
			if n == 1 {
				frames = []string{
					`{"choices":[{"index":0,"delta":{"role":"assistant","content":"",` +
						`"tool_calls":[{"index":0,"id":"c0","type":"function",` +
						`"function":{"name":"ctxtool","arguments":"{}"}}]}}]}`,
					`{"choices":[{"index":0,"delta":{"content":""},"finish_reason":"tool_calls"}]}`,
				}
			}
			// The stream must carry a usage chunk: without one the provider
			// reports "ended before usage chunk".
			frames = append(frames,
				`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
			for _, f := range frames {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", f)
			}
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// writeAgentConfig writes a models.json wiring the agent role to baseURL.
func writeAgentConfig(t *testing.T, baseURL string) string {
	t.Helper()
	// The model name is a catalog family/variant so the profile inherits a
	// context window: config's own context_window is not a routing input, and a
	// profile without one is rejected as over budget. tool_call is never derived
	// from type, and the agent caller requires it as soon as the request carries
	// tools, so capabilities are declared explicitly.
	cfg := `{
  "providers": {"local": {"base_url": "` + baseURL + `", "api_format": "openai-compat"}},
  "models": {"agent": {"name": "` + wiringModel + `", "provider": "local", "type": "dense",
    "capabilities": ["chat", "generate", "stream", "tool_call"]}},
  "defaults": {"agent": "agent"}
}`
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// runWiringTurn drives one full config-bootstrapped turn (no injected
// Orchestrator, so Options.Progressive is the only thing that can enable mixed
// assembly) and returns the model-call count plus the emitted protocol events.
func runWiringTurn(t *testing.T, progressive bool, host agent.Observer) (int32, []golem.Event, error) {
	t.Helper()
	srv, calls := toolCallStreamServer(t)
	runtime, err := golem.New(context.Background(), golem.Options{
		Root:        t.TempDir(),
		ConfigPath:  writeAgentConfig(t, srv.URL),
		Progressive: progressive,
		Tools:       []agent.Tool{wiringTool{}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	var events []golem.Event
	_, runErr := runtime.Run(context.Background(), golem.Turn{
		RunID:    "run-mixed",
		Message:  "GOAL",
		Observer: host,
	}, func(e golem.Event) error {
		events = append(events, e)
		return nil
	})
	return calls.Load(), events, runErr
}

// TestProgressiveOptionEnablesMixedAssembly is the CONTENT pin on
// Options.Progressive -> ContextManager.Mixed: with it on, the consumer's
// ContextAssemblyObserver receives the step-1 trace carrying each fixture
// group's own depth, rank and byte count; with it off (the default) the same run
// assembles the same state on the legacy path and emits nothing.
func TestProgressiveOptionEnablesMixedAssembly(t *testing.T) {
	for _, tc := range []struct {
		name        string
		progressive bool
		wantEvents  int
	}{
		{"default off", false, 0},
		{"progressive on", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &assemblyHost{}
			calls, events, err := runWiringTurn(t, tc.progressive, host)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			// The assembly trace carries source paths, record IDs and tool call
			// IDs. It is host-observer-only: enabling mixed assembly must not add
			// a type to the versioned protocol stream consumers persist. Pinned as
			// the exact sequence, identical in both arms, so an extra emit here
			// fails rather than passing a "contains" check.
			wantTypes := []string{"run.started", "tool.started", "tool.finished", "message.delta", "run.finished"}
			if got := eventTypes(events); !slices.Equal(got, wantTypes) {
				t.Fatalf("emitted event types = %v, want %v", got, wantTypes)
			}
			// Two model calls prove the tool step and the assembly under test
			// (step 1, the first one that sees the anchor) really happened, so
			// zero events is an absence rather than a run that never assembled.
			if calls != 2 {
				t.Fatalf("model calls = %d, want 2 (one tool step, one final)", calls)
			}
			if len(host.events) != tc.wantEvents {
				t.Fatalf("assembly events = %d, want %d: %+v", len(host.events), tc.wantEvents, host.events)
			}
			if tc.wantEvents == 0 {
				return
			}
			ev := host.events[0]
			if ev.Step != 1 {
				t.Errorf("event Step = %d, want 1 (the first assembly that sees the anchor)", ev.Step)
			}
			if ev.Trace.Subjects == nil {
				t.Fatal("mixed event carries a zero trace (nil Subjects)")
			}
			for _, want := range []struct {
				id    string
				depth contextdepth.Depth
				rank  int
				bytes int
			}{
				{"one.go", contextdepth.DepthL0, 7, len(wiringCardContent)},
				{"two.go", contextdepth.DepthL2, 3, len(wiringEvidenceContent)},
			} {
				got, ok := wiringRow(ev.Trace, want.id)
				if !ok {
					t.Errorf("trace has no row for %s: %+v", want.id, ev.Trace.Subjects)
					continue
				}
				if got.EffectiveDepth != want.depth || got.Rank != want.rank || got.Bytes != want.bytes {
					t.Errorf("%s row = depth %v rank %d bytes %d, want %v/%d/%d",
						want.id, got.EffectiveDepth, got.Rank, got.Bytes, want.depth, want.rank, want.bytes)
				}
				if got.ToolCallID != "c0" {
					t.Errorf("%s row ToolCallID = %q, want %q", want.id, got.ToolCallID, "c0")
				}
			}
		})
	}
}

func wiringRow(tr agent.ContextAssemblyTrace, id string) (agent.ContextSubjectTrace, bool) {
	for _, s := range tr.Subjects {
		if s.Subject.Domain == contextdepth.DomainRAG && s.Subject.ID == id {
			return s, true
		}
	}
	return agent.ContextSubjectTrace{}, false
}

// TestAssemblyObserverErrorFailsRun: an error from the host's
// OnContextAssembly reaches Run wrapped as a host-observer failure, exactly like
// the other forwarded optional callbacks — so the run reports the stable
// observer_failed code rather than a generic internal error, and the step's
// model call never happens.
func TestAssemblyObserverErrorFailsRun(t *testing.T) {
	sentinel := errors.New("host assembly observer refused")
	host := &assemblyHost{err: sentinel}
	calls, events, runErr := runWiringTurn(t, true, host)
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("Run error = %v, want the host's assembly error", runErr)
	}
	if len(host.events) != 1 {
		t.Fatalf("host saw %d assembly event(s), want the one that failed", len(host.events))
	}
	if calls != 1 {
		t.Fatalf("model calls = %d, want 1: the abort must precede step 1's model call", calls)
	}
	last := events[len(events)-1]
	if last.Type != "run.failed" {
		t.Fatalf("last event = %q, want run.failed: %+v", last.Type, events)
	}
	var failed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(last.Payload, &failed); err != nil {
		t.Fatalf("decode run.failed: %v", err)
	}
	// observer_failed is what failureCode derives from the hostObserverError
	// wrapper; an unwrapped error would surface as "internal".
	if failed.Code != "observer_failed" {
		t.Fatalf("run.failed code = %q, want observer_failed", failed.Code)
	}
}

// TestPlainConsumerObserverUnaffectedByAssemblySeam: a host Observer that does
// not implement ContextAssemblyObserver is untouched by the forwarding seam —
// the mixed run completes and its ordinary callbacks still fire.
func TestPlainConsumerObserverUnaffectedByAssemblySeam(t *testing.T) {
	host := &plainHost{}
	if _, ok := agent.Observer(host).(agent.ContextAssemblyObserver); ok {
		t.Fatal("plainHost must not satisfy ContextAssemblyObserver")
	}
	calls, _, err := runWiringTurn(t, true, host)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
	if host.steps != 2 {
		t.Fatalf("host OnStep calls = %d, want 2", host.steps)
	}
}

func TestRetrievalPresentationTrustedHostReceivesEvent(t *testing.T) {
	host := &retrievalPresentationHost{}
	calls, events, err := runWiringTurn(t, true, host)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("model calls = %d, want 2", calls)
	}
	if len(host.events) != 1 {
		t.Fatalf("retrieval presentations = %d, want 1: %+v", len(host.events), host.events)
	}
	got := host.events[0]
	if got.Step != 1 || got.ToolCallID != "c0" || len(got.Attribution.Sources) != 1 || got.Attribution.Sources[0].StableKey != "wiring-source" {
		t.Fatalf("retrieval presentation = %+v, want step 1 c0 wiring-source", got)
	}
	if want := []string{"run.started", "tool.started", "tool.finished", "message.delta", "run.finished"}; !slices.Equal(eventTypes(events), want) {
		t.Fatalf("protocol events = %v, want %v", eventTypes(events), want)
	}
}

func TestRetrievalPresentationPlainHostIsUnaffected(t *testing.T) {
	host := &plainHost{}
	if _, ok := agent.Observer(host).(agent.RetrievalPresentationObserver); ok {
		t.Fatal("plainHost must not satisfy RetrievalPresentationObserver")
	}
	calls, _, err := runWiringTurn(t, true, host)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 2 || host.steps != 2 {
		t.Fatalf("model calls/steps = %d/%d, want 2/2", calls, host.steps)
	}
}

func TestRetrievalPresentationHostErrorUsesStableCode(t *testing.T) {
	sentinel := errors.New("host retrieval presentation refused")
	host := &retrievalPresentationHost{err: sentinel}
	calls, events, runErr := runWiringTurn(t, true, host)
	if !errors.Is(runErr, sentinel) {
		t.Fatalf("Run error = %v, want host error", runErr)
	}
	if len(host.events) != 1 || calls != 1 {
		t.Fatalf("retrieval presentations/model calls = %d/%d, want 1/1", len(host.events), calls)
	}
	last := events[len(events)-1]
	var failed struct{ Code string }
	if last.Type != "run.failed" || json.Unmarshal(last.Payload, &failed) != nil || failed.Code != "observer_failed" {
		t.Fatalf("last event = %+v, want observer_failed", last)
	}
}
