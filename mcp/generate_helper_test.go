package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/completion"
	"github.com/kstruzzieri/go-llm/provider"
)

// fimReqFor constructs a minimal FIM-shaped GenerateRequest for tests. It
// carries a literal-zero Temperature on purpose — FIM completions in import
// blocks rely on deterministic decoding, and the provider.Ptr round-trip is
// tested at TestRoutedGenerate_RoutingRequestShape.
func fimReqFor(model string) completion.GenerateRequest {
	return completion.GenerateRequest{
		Model:       model,
		Prompt:      "func add(a, b int) int {\n\treturn ",
		Suffix:      "\n}",
		Temperature: 0.0,
		NumPredict:  64,
		NumCtx:      8192,
		Stop:        []string{"<|endoftext|>"},
	}
}

// newServerWithRouter builds a minimal *Server whose only wired field is the
// router engine. routedGenerate and the fimGenerator closure consume the
// router via routerSnapshot, so this is sufficient to exercise the FIM
// routing semantics without standing up a full provider/registry stack.
func newServerWithRouter(eng *recordingRouteEngine) *Server {
	s := &Server{}
	s.router = eng
	return s
}

func TestRoutedGenerate_RejectsEmptyModel(t *testing.T) {
	eng := newRecordingRouteEngine("done")
	s := newServerWithRouter(eng)
	req := fimReqFor("")
	_, err := s.routedGenerate(context.Background(), req, provider.PriorityHigh, 0)
	if err == nil {
		t.Fatal("routedGenerate empty-model error = nil, want non-nil")
	}
	if got := routedGenerateCategory(err); got != generateToolConfig {
		t.Errorf("category = %q, want %q", got, generateToolConfig)
	}
	if !strings.Contains(err.Error(), "FIM-family") {
		t.Errorf("error = %q, want substring %q", err.Error(), "FIM-family")
	}
	if eng.called {
		t.Error("Router.Route invoked despite empty model")
	}
}

func TestRoutedGenerate_RouterUnavailable(t *testing.T) {
	s := &Server{} // router nil
	_, err := s.routedGenerate(context.Background(), fimReqFor("qwen3:8b"), provider.PriorityHigh, 0)
	if err == nil {
		t.Fatal("routedGenerate nil-router error = nil, want non-nil")
	}
	if got := routedGenerateCategory(err); got != generateToolConfig {
		t.Errorf("category = %q, want %q", got, generateToolConfig)
	}
}

func TestRoutedGenerate_RoutingRequestShape(t *testing.T) {
	eng := newRecordingRouteEngine("done")
	s := newServerWithRouter(eng)
	req := fimReqFor("qwen3:8b")
	plan, err := s.routedGenerate(context.Background(), req, provider.PriorityHigh, 0)
	if err != nil {
		t.Fatalf("routedGenerate error: %v", err)
	}
	if plan == nil {
		t.Fatal("routedGenerate returned nil plan")
	}
	rr := eng.last
	if rr.Model != "qwen3:8b" {
		t.Errorf("Model = %q, want %q", rr.Model, "qwen3:8b")
	}
	if rr.UseCase != "fim" {
		t.Errorf("UseCase = %q, want %q", rr.UseCase, "fim")
	}
	want := provider.CapGenerate | provider.CapInsert
	if rr.RequiredCaps != want {
		t.Errorf("RequiredCaps = %v, want %v", rr.RequiredCaps, want)
	}
	if rr.Suffix != req.Suffix {
		t.Errorf("Suffix = %q, want %q", rr.Suffix, req.Suffix)
	}
	if rr.Prompt != req.Prompt {
		t.Errorf("Prompt = %q, want %q", rr.Prompt, req.Prompt)
	}
	if rr.Priority != provider.PriorityHigh {
		t.Errorf("Priority = %v, want %v", rr.Priority, provider.PriorityHigh)
	}
	if len(rr.PreferredChain) != 0 {
		t.Errorf("PreferredChain = %v, want empty (FIM pin policy)", rr.PreferredChain)
	}
	if rr.ExpectedOutput != provider.DefaultExpectedOutput("fim") {
		t.Errorf("ExpectedOutput = %d, want %d", rr.ExpectedOutput, provider.DefaultExpectedOutput("fim"))
	}
	if rr.Options.Temperature == nil {
		t.Fatal("Options.Temperature is nil, want non-nil pointer (FIM uses literal-zero temperature)")
	}
	if got := *rr.Options.Temperature; got != req.Temperature {
		t.Errorf("Options.Temperature = %v, want %v", got, req.Temperature)
	}
	if rr.Options.NumPredict != req.NumPredict {
		t.Errorf("Options.NumPredict = %d, want %d", rr.Options.NumPredict, req.NumPredict)
	}
	if rr.Options.NumCtx != req.NumCtx {
		t.Errorf("Options.NumCtx = %d, want %d", rr.Options.NumCtx, req.NumCtx)
	}
}

func TestRoutedGenerate_StreamingAddsCapStream(t *testing.T) {
	eng := newRecordingRouteEngine("done")
	s := newServerWithRouter(eng)
	_, err := s.routedGenerate(context.Background(), fimReqFor("qwen3:8b"), provider.PriorityHigh, provider.CapStream)
	if err != nil {
		t.Fatalf("routedGenerate error: %v", err)
	}
	want := provider.CapGenerate | provider.CapInsert | provider.CapStream
	if eng.last.RequiredCaps != want {
		t.Errorf("RequiredCaps = %v, want %v", eng.last.RequiredCaps, want)
	}
}

func TestRoutedGenerate_PriorityForwarded(t *testing.T) {
	for _, p := range []provider.Priority{provider.PriorityNormal, provider.PriorityHigh, provider.PriorityCritical} {
		t.Run(p.String(), func(t *testing.T) {
			eng := newRecordingRouteEngine("done")
			s := newServerWithRouter(eng)
			if _, err := s.routedGenerate(context.Background(), fimReqFor("qwen3:8b"), p, 0); err != nil {
				t.Fatalf("routedGenerate error: %v", err)
			}
			if eng.last.Priority != p {
				t.Errorf("Priority = %v, want %v", eng.last.Priority, p)
			}
		})
	}
}

func TestRoutedGenerate_RouteErrorCategorised(t *testing.T) {
	eng := newRecordingRouteEngine("done")
	eng.routeErr = errors.New("no candidates")
	s := newServerWithRouter(eng)
	_, err := s.routedGenerate(context.Background(), fimReqFor("qwen3:8b"), provider.PriorityHigh, 0)
	if err == nil {
		t.Fatal("routedGenerate route-error = nil, want non-nil")
	}
	if got := routedGenerateCategory(err); got != generateToolRouter {
		t.Errorf("category = %q, want %q", got, generateToolRouter)
	}
	if !errors.Is(err, eng.routeErr) {
		t.Errorf("Unwrap chain does not preserve original route error")
	}
}

// TestFIMGenerator_GenerateOmitsCapStream asserts the non-streaming closure
// passes extraCaps = 0 to routedGenerate, leaving CapStream OFF in the
// recorded RoutingRequest. Spec §6 makes CapStream streaming-only;
// over-requesting it on Generate would make the Router over-filter.
func TestFIMGenerator_GenerateOmitsCapStream(t *testing.T) {
	eng := newRecordingRouteEngine("hello")
	s := newServerWithRouter(eng)
	gen := s.fimGenerator(provider.PriorityHigh)
	res, err := gen.Generate(context.Background(), fimReqFor("qwen3:8b"))
	if err != nil {
		t.Fatalf("Generate error: %v", err)
	}
	if res.Response != "hello" {
		t.Errorf("Response = %q, want %q", res.Response, "hello")
	}
	if eng.last.Priority != provider.PriorityHigh {
		t.Errorf("recorded Priority = %v, want PriorityHigh", eng.last.Priority)
	}
	wantCaps := provider.CapGenerate | provider.CapInsert
	if eng.last.RequiredCaps != wantCaps {
		t.Errorf("Generate RequiredCaps = %v, want %v (must NOT include CapStream)", eng.last.RequiredCaps, wantCaps)
	}
	if eng.last.RequiredCaps&provider.CapStream != 0 {
		t.Errorf("Generate RequiredCaps included CapStream; non-streaming path must pass extraCaps = 0")
	}
	// Outcome propagation guard for the structured #82 drift telemetry seam.
	// recordingRouteEngine's RoutePlan returns a non-nil RouteOutcome on
	// successful execution (provider/route_plan.go handleResult); a regression
	// that drops translateRouteOutcome would silently leave Outcome nil here.
	if res.Outcome == nil {
		t.Fatal("res.Outcome is nil; mcpFIMGenerator must translate provider.RouteOutcome")
	}
	if want := "fake/qwen3:8b"; res.Outcome.PlannedModel != want {
		t.Errorf("Outcome.PlannedModel = %q, want %q", res.Outcome.PlannedModel, want)
	}
}

// TestFIMGenerator_GenerateStreamRequiresCapStream asserts the streaming
// closure passes extraCaps = provider.CapStream to routedGenerate, and
// that the chunk stream observed by the user callback is byte-identical
// to the upstream provider chunk sequence with no wrapping. Together with
// TestFIMGenerator_GenerateOmitsCapStream this proves the streaming bit
// is set ONLY by GenerateStream and never leaks into the non-streaming
// path.
func TestFIMGenerator_GenerateStreamRequiresCapStream(t *testing.T) {
	// recordingRouteEngine returns a single-chunk stream by default; the
	// chunk parity assertion verifies the user-callback sequence is the
	// same as the upstream provider chunk sequence with no extra wrapping.
	eng := newRecordingRouteEngine("hello")
	s := newServerWithRouter(eng)
	gen := s.fimGenerator(provider.PriorityHigh)

	var (
		got       []string
		doneSeen  bool
		doneCount int
	)
	res, err := gen.GenerateStream(context.Background(), fimReqFor("qwen3:8b"), func(ch completion.GenerateChunk) error {
		got = append(got, ch.Response)
		if ch.Done {
			doneSeen = true
			doneCount++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("GenerateStream error: %v", err)
	}
	if want := []string{"hello"}; !equalStrings(got, want) {
		t.Errorf("chunks = %v, want %v", got, want)
	}
	if res.Response != "hello" {
		t.Errorf("aggregated Response = %q, want %q", res.Response, "hello")
	}
	if !doneSeen {
		t.Error("no chunk with Done=true; Generator contract requires final chunk Done=true on success")
	}
	if doneCount != 1 {
		t.Errorf("Done=true chunks = %d, want exactly 1", doneCount)
	}
	wantCaps := provider.CapGenerate | provider.CapInsert | provider.CapStream
	if eng.last.RequiredCaps != wantCaps {
		t.Errorf("GenerateStream RequiredCaps = %v, want %v", eng.last.RequiredCaps, wantCaps)
	}
	if eng.last.RequiredCaps&provider.CapStream == 0 {
		t.Errorf("GenerateStream RequiredCaps missing CapStream; streaming path must pass extraCaps = provider.CapStream")
	}
	// Outcome propagation guard: ExecuteGenerateStream stamps RouteOutcome on
	// the terminal Done chunk; mcpFIMGenerator captures it and translates it
	// into completion.RouteOutcome via resultFromAggregatedStream. A regression
	// that drops the capture would leave res.Outcome nil silently.
	if res.Outcome == nil {
		t.Fatal("res.Outcome is nil; streaming path must capture and translate provider.RouteOutcome from the terminal chunk")
	}
	if want := "fake/qwen3:8b"; res.Outcome.PlannedModel != want {
		t.Errorf("Outcome.PlannedModel = %q, want %q", res.Outcome.PlannedModel, want)
	}
}

func TestFIMGenerator_GenerateRejectsEmptyModelEarly(t *testing.T) {
	eng := newRecordingRouteEngine("done")
	s := newServerWithRouter(eng)
	gen := s.fimGenerator(provider.PriorityHigh)
	_, err := gen.Generate(context.Background(), fimReqFor(""))
	if err == nil {
		t.Fatal("Generate empty-model error = nil, want non-nil")
	}
	if got := routedGenerateCategory(err); got != generateToolConfig {
		t.Errorf("category = %q, want %q", got, generateToolConfig)
	}
	if eng.called {
		t.Error("Router.Route invoked despite empty model")
	}
}

// equalStrings is redeclared locally to keep mcp tests from depending on
// completion test fixtures (different package). Trivial slice equality.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
