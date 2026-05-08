package completion

import (
	"context"
	"strings"
	"testing"
)

func TestProvider_CompleteRoutesThroughGenerator(t *testing.T) {
	gen := &fakeGenerator{
		result: GenerateResult{Response: "return a + b", Tokens: 4, Model: "qwen3:8b", Provider: "ollama"},
	}
	p, err := NewProviderWithGenerator(gen, "qwen3:8b", testProviderConfig())
	if err != nil {
		t.Fatalf("NewProviderWithGenerator: %v", err)
	}
	resp, err := p.Complete(context.Background(), FIMRequest{
		Prefix:   "func add(a, b int) int {\n\t",
		Suffix:   "\n}",
		FilePath: "math.go",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !gen.called {
		t.Fatal("Generator.Generate was not invoked")
	}
	if got := gen.lastReq.Model; got != "qwen3:8b" {
		t.Errorf("GenerateRequest.Model = %q, want %q", got, "qwen3:8b")
	}
	if gen.lastReq.Suffix != "\n}" {
		t.Errorf("GenerateRequest.Suffix = %q, want %q", gen.lastReq.Suffix, "\n}")
	}
	if gen.lastReq.NumCtx <= 0 {
		t.Errorf("GenerateRequest.NumCtx = %d, want > 0 (planRequest must populate)", gen.lastReq.NumCtx)
	}
	if gen.lastReq.NumPredict <= 0 {
		t.Errorf("GenerateRequest.NumPredict = %d, want > 0", gen.lastReq.NumPredict)
	}
	if resp.Completion != "return a + b" {
		t.Errorf("Completion = %q, want %q", resp.Completion, "return a + b")
	}
	if resp.Tokens != 4 {
		t.Errorf("Tokens = %d, want 4", resp.Tokens)
	}
}

// TestProvider_CompleteStreamSuppressesStopTokensAcrossChunks proves the
// trailing-buffer logic surfaces stop-stripped tokens to fn even when the
// upstream stream splits a stop token across chunk boundaries — the
// existing TestCompleteStreamUsesGeneratorRequestPlan covers the "stop
// token arrives in a single chunk" case; this test covers the harder
// "stop token straddles two chunks" case.
func TestProvider_CompleteStreamSuppressesStopTokensAcrossChunks(t *testing.T) {
	gen := &fakeGenerator{
		chunks: []GenerateChunk{
			{Response: "return ", Done: false},
			{Response: "1<|", Done: false},
			{Response: "endoftext|>", Done: true},
		},
		result: GenerateResult{Response: "return 1<|endoftext|>", Tokens: 4, Model: "qwen3:8b", Provider: "ollama"},
	}
	cfg := testProviderConfig()
	p, err := NewProviderWithGenerator(gen, "qwen3:8b", cfg)
	if err != nil {
		t.Fatalf("NewProviderWithGenerator: %v", err)
	}
	var emitted []string
	if err := p.CompleteStream(context.Background(), FIMRequest{
		Prefix:   "func add(a, b int) int {\n\t",
		Suffix:   "\n}",
		FilePath: "math.go",
	}, func(token string) error {
		emitted = append(emitted, token)
		return nil
	}); err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	joined := strings.Join(emitted, "")
	if strings.Contains(joined, "<|endoftext|>") {
		t.Fatalf("emitted contained literal stop token: %q", joined)
	}
	if !strings.Contains(joined, "return 1") {
		t.Errorf("emitted = %q, want substring %q", joined, "return 1")
	}
}

func TestProvider_CompleteWithGeneratorRejectsNilGenerator(t *testing.T) {
	// NewProvider(nil, ...) preserves the legacy nil-client compat path
	// (generatorFromOllamaClient(nil) returns a nil Generator); Complete
	// must reject with "generator is required" when the field is nil.
	p, err := NewProvider(nil, "qwen3:8b", testProviderConfig())
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_, err = p.Complete(context.Background(), FIMRequest{Prefix: "x", Suffix: "y"})
	if err == nil || !strings.Contains(err.Error(), "generator is required") {
		t.Fatalf("Complete error = %v, want substring %q", err, "generator is required")
	}
}

func TestProvider_CompleteStreamWithGeneratorRejectsNilCallback(t *testing.T) {
	gen := &fakeGenerator{}
	p, err := NewProviderWithGenerator(gen, "qwen3:8b", testProviderConfig())
	if err != nil {
		t.Fatalf("NewProviderWithGenerator: %v", err)
	}
	if err := p.CompleteStream(context.Background(), FIMRequest{Prefix: "x", Suffix: "y"}, nil); err == nil {
		t.Fatal("CompleteStream with nil callback error = nil, want non-nil")
	}
	if gen.streamCalled {
		t.Error("Generator.GenerateStream invoked despite nil callback")
	}
}

// TestProvider_CompleteStreamWithResult_ReturnsAggregatedResult proves the
// additive entry point captures and returns the underlying Generator's
// aggregated GenerateResult — Tokens, Model, Provider, and the route
// metadata (Outcome) on success. CompleteStream(...) error preserves
// backwards compatibility but discards this; CompleteStreamWithResult
// surfaces it for #82 drift telemetry consumers.
func TestProvider_CompleteStreamWithResult_ReturnsAggregatedResult(t *testing.T) {
	wantResult := GenerateResult{
		Response: "fmt.Println()",
		Tokens:   5,
		Model:    "qwen3:8b",
		Provider: "ollama",
	}
	gen := &fakeGenerator{
		chunks: []GenerateChunk{
			{Response: "fmt.", Done: false},
			{Response: "Println()", Done: true},
		},
		result: wantResult,
	}
	p, err := NewProviderWithGenerator(gen, "qwen3:8b", testProviderConfig())
	if err != nil {
		t.Fatalf("NewProviderWithGenerator: %v", err)
	}
	var emitted []string
	got, err := p.CompleteStreamWithResult(context.Background(), FIMRequest{
		Prefix: "func main() {\n\t",
		Suffix: "\n}",
	}, func(token string) error {
		emitted = append(emitted, token)
		return nil
	})
	if err != nil {
		t.Fatalf("CompleteStreamWithResult: %v", err)
	}
	if got.Response != wantResult.Response {
		t.Errorf("Response = %q, want %q", got.Response, wantResult.Response)
	}
	if got.Tokens != wantResult.Tokens {
		t.Errorf("Tokens = %d, want %d", got.Tokens, wantResult.Tokens)
	}
	if got.Model != wantResult.Model {
		t.Errorf("Model = %q, want %q", got.Model, wantResult.Model)
	}
	if got.Provider != wantResult.Provider {
		t.Errorf("Provider = %q, want %q", got.Provider, wantResult.Provider)
	}
	// The aggregated chunk content the user fn observed should match the
	// chunks the fake fed in (no stop-token suppression in this test
	// because the fake's chunks contain none).
	if want := "fmt.Println()"; strings.Join(emitted, "") != want {
		t.Errorf("emitted joined = %q, want %q", strings.Join(emitted, ""), want)
	}
}

// TestProvider_CompleteStreamWithResult_PropagatesOutcomeVerbatim asserts
// the structured RouteOutcome the underlying Generator places on its
// GenerateResult reaches the CompleteStreamWithResult caller unmodified.
// This is the seam #82 drift telemetry consumes.
func TestProvider_CompleteStreamWithResult_PropagatesOutcomeVerbatim(t *testing.T) {
	wantOutcome := &RouteOutcome{
		RouteHint:     "fake/qwen3:8b",
		PlannedModel:  "fake/qwen3:8b",
		Score:         1.5,
		FallbacksUsed: 2,
		WasSticky:     true,
	}
	gen := &fakeGenerator{
		chunks: []GenerateChunk{{Response: "ok", Done: true}},
		result: GenerateResult{
			Response: "ok",
			Tokens:   1,
			Model:    "qwen3:8b",
			Provider: "fake",
			Outcome:  wantOutcome,
		},
	}
	p, err := NewProviderWithGenerator(gen, "qwen3:8b", testProviderConfig())
	if err != nil {
		t.Fatalf("NewProviderWithGenerator: %v", err)
	}
	got, err := p.CompleteStreamWithResult(context.Background(), FIMRequest{
		Prefix: "x",
		Suffix: "y",
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("CompleteStreamWithResult: %v", err)
	}
	if got.Outcome == nil {
		t.Fatal("got.Outcome is nil; CompleteStreamWithResult must propagate Outcome verbatim")
	}
	if got.Outcome != wantOutcome {
		t.Errorf("got.Outcome = %+v, want pointer-equal to %+v (must be verbatim, no copy/translate)", got.Outcome, wantOutcome)
	}
}

// TestProvider_CompleteStreamWithResult_NilOutcomeWhenGeneratorOmitsIt
// asserts the legacy ollama-adapter shape (Outcome nil) round-trips
// unchanged — CompleteStreamWithResult does NOT synthesize a placeholder
// Outcome from Model/Provider when the underlying Generator returns nil.
// Keeping nil-as-nil is the explicit signal "no router-side metadata
// available" that downstream telemetry consumers (#82) rely on.
func TestProvider_CompleteStreamWithResult_NilOutcomeWhenGeneratorOmitsIt(t *testing.T) {
	gen := &fakeGenerator{
		chunks: []GenerateChunk{{Response: "ok", Done: true}},
		result: GenerateResult{
			Response: "ok",
			Tokens:   1,
			Model:    "qwen3:8b",
			Provider: "ollama",
			// Outcome intentionally nil — the legacy adapter shape.
		},
	}
	p, err := NewProviderWithGenerator(gen, "qwen3:8b", testProviderConfig())
	if err != nil {
		t.Fatalf("NewProviderWithGenerator: %v", err)
	}
	got, err := p.CompleteStreamWithResult(context.Background(), FIMRequest{
		Prefix: "x",
		Suffix: "y",
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("CompleteStreamWithResult: %v", err)
	}
	if got.Outcome != nil {
		t.Errorf("got.Outcome = %+v, want nil (legacy ollama-adapter shape must round-trip unchanged)", got.Outcome)
	}
	if got.Model != "qwen3:8b" {
		t.Errorf("got.Model = %q, want qwen3:8b (Model must still be populated even when Outcome is nil)", got.Model)
	}
}

// TestProvider_CompleteStreamWithResult_ResponseStopsStripped proves that
// CompleteStreamWithResult.Response is the byte concatenation of the
// stop-stripped tokens fn observed, NOT the raw backend payload the
// underlying Generator returned. Without this guarantee, callers would
// see a Response containing literal stop tokens (e.g. "<|endoftext|>")
// even though fn was correctly stripped — two inconsistent views of the
// same stream. This is the post-fix invariant for the P2 review finding
// on inline.go's completeStream.
func TestProvider_CompleteStreamWithResult_ResponseStopsStripped(t *testing.T) {
	cfg := testProviderConfig()
	// Sanity: the fixture must declare a stop token, otherwise the
	// stripping path is never exercised and this test would tautologically
	// pass.
	if len(cfg.FIM.StopTokens) == 0 {
		t.Fatal("testProviderConfig().FIM.StopTokens must be non-empty for this test to be meaningful")
	}
	stop := cfg.FIM.StopTokens[0]

	gen := &fakeGenerator{
		// Chunks split the stop token across boundaries to also exercise
		// the trailing-buffer logic.
		chunks: []GenerateChunk{
			{Response: "fmt.", Done: false},
			{Response: "Println()" + stop[:2], Done: false},
			{Response: stop[2:], Done: true},
		},
		// The fake's raw result.Response carries the stop token exactly
		// like Ollama would; if completeStream returned this verbatim,
		// callers would observe a leak.
		result: GenerateResult{
			Response: "fmt.Println()" + stop,
			Tokens:   3,
			Model:    "qwen3:8b",
			Provider: "ollama",
		},
	}
	p, err := NewProviderWithGenerator(gen, "qwen3:8b", cfg)
	if err != nil {
		t.Fatalf("NewProviderWithGenerator: %v", err)
	}
	got, err := p.CompleteStreamWithResult(context.Background(), FIMRequest{
		Prefix: "func main() {\n\t",
		Suffix: "\n}",
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("CompleteStreamWithResult: %v", err)
	}
	if strings.Contains(got.Response, stop) {
		t.Errorf("Response = %q contains literal stop token %q; CompleteStreamWithResult must return the stop-stripped aggregate", got.Response, stop)
	}
	if want := "fmt.Println()"; got.Response != want {
		t.Errorf("Response = %q, want %q (cleaned aggregate matching fn-observed bytes)", got.Response, want)
	}
}

// TestProvider_CompleteStream_ChunkParityWithCompleteStreamWithResult
// proves the two streaming entry points produce byte-identical fn-observed
// chunk sequences for the same fake-Generator input. This is the
// structural guarantee that legacy callers and Outcome-aware callers see
// identical streams; only the post-stream return value differs.
func TestProvider_CompleteStream_ChunkParityWithCompleteStreamWithResult(t *testing.T) {
	chunks := []GenerateChunk{
		{Response: "fmt.", Done: false},
		{Response: "Println(\"hi\")", Done: false},
		{Response: "<|endoftext|>", Done: true},
	}
	result := GenerateResult{Response: "fmt.Println(\"hi\")<|endoftext|>", Tokens: 3, Model: "qwen3:8b", Provider: "ollama"}

	collect := func(t *testing.T, run func(p *Provider, fn func(token string) error) error) []string {
		t.Helper()
		gen := &fakeGenerator{
			chunks: append([]GenerateChunk(nil), chunks...), // fresh per run; stop-suppression mutates buffer
			result: result,
		}
		p, err := NewProviderWithGenerator(gen, "qwen3:8b", testProviderConfig())
		if err != nil {
			t.Fatalf("NewProviderWithGenerator: %v", err)
		}
		var got []string
		if err := run(p, func(token string) error {
			got = append(got, token)
			return nil
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
		return got
	}

	gotLegacy := collect(t, func(p *Provider, fn func(token string) error) error {
		return p.CompleteStream(context.Background(), FIMRequest{Prefix: "x", Suffix: "y"}, fn)
	})
	gotResult := collect(t, func(p *Provider, fn func(token string) error) error {
		_, err := p.CompleteStreamWithResult(context.Background(), FIMRequest{Prefix: "x", Suffix: "y"}, fn)
		return err
	})

	if strings.Join(gotLegacy, "") != strings.Join(gotResult, "") {
		t.Errorf("chunk-byte parity violated:\n  CompleteStream           emitted = %q\n  CompleteStreamWithResult emitted = %q",
			strings.Join(gotLegacy, ""), strings.Join(gotResult, ""))
	}
	// Both must also produce stop-stripped output (no leak across the
	// buffer boundary).
	for _, joined := range []string{strings.Join(gotLegacy, ""), strings.Join(gotResult, "")} {
		if strings.Contains(joined, "<|endoftext|>") {
			t.Errorf("emitted contained literal stop token: %q", joined)
		}
	}
}
