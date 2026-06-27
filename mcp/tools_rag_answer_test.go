package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

func TestQuoteInChunk(t *testing.T) {
	chunk := rag.Chunk{Content: "func BuildContext(results []SearchResult)  {\n\treturn x\n}"}
	tests := []struct {
		name  string
		quote string
		want  bool
	}{
		{"exact", "func BuildContext(results []SearchResult)", true},
		{"whitespace reflow", "func BuildContext(results []SearchResult) {", true}, // double space collapsed
		{"newline reflow", "[]SearchResult) { return x }", true},
		{"case mismatch", "func buildcontext", false},
		{"absent", "func NeverHere()", false},
		{"empty quote", "", false},
		{"line-anchor prefix not present", "42| func BuildContext", false}, // verify vs raw Content, not BuildContext output
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteInChunk(chunk, tc.quote); got != tc.want {
				t.Errorf("quoteInChunk(%q) = %v, want %v", tc.quote, got, tc.want)
			}
		})
	}
}

func TestExtractJSONObjects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain", `{"a":1}`, []string{`{"a":1}`}},
		{"prose around", "sure:\n{\"a\":1}\nthanks", []string{`{"a":1}`}},
		{"two objects", `{"a":1} then {"b":2}`, []string{`{"a":1}`, `{"b":2}`}},
		{"nested", `{"a":{"b":2}}`, []string{`{"a":{"b":2}}`}},
		{"brace in string", `{"a":"}{"}`, []string{`{"a":"}{"}`}},
		{"escaped quote in string", `{"a":"x\"}"}`, []string{`{"a":"x\"}"}`}},
		{"none", `no json here`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSONObjects(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d objects %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if string(got[i]) != tc.want[i] {
					t.Errorf("obj[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseModelAnswer(t *testing.T) {
	t.Run("single valid", func(t *testing.T) {
		ma, ok := parseModelAnswer(`{"answer":"yes","evidence":[{"id":"E1","quote":"q"}]}`)
		if !ok || ma.Answer != "yes" || len(ma.Evidence) != 1 || ma.Evidence[0].ID != "E1" {
			t.Fatalf("got %+v ok=%v", ma, ok)
		}
	})
	t.Run("echo then real picks last", func(t *testing.T) {
		in := `Here is the format {"answer":"","evidence":[]} and my answer: {"answer":"real","evidence":[]}`
		ma, ok := parseModelAnswer(in)
		if !ok || ma.Answer != "real" {
			t.Fatalf("got %+v ok=%v, want answer=real", ma, ok)
		}
	})
	t.Run("object without answer or evidence skipped", func(t *testing.T) {
		ma, ok := parseModelAnswer(`{"foo":1} {"answer":"x"}`)
		if !ok || ma.Answer != "x" {
			t.Fatalf("got %+v ok=%v", ma, ok)
		}
	})
	t.Run("no valid object", func(t *testing.T) {
		if _, ok := parseModelAnswer(`no json {nope}`); ok {
			t.Fatal("expected ok=false")
		}
	})
}

func TestDeriveAnswer(t *testing.T) {
	blocks := []evidenceBlock{
		{ID: "E1", Chunk: rag.Chunk{Content: "func Foo() int { return 1 }", Source: "a.go", StartLine: 10, EndLine: 12}},
		{ID: "E2", Chunk: rag.Chunk{Content: "var Bar = 2", Source: "b.go", StartLine: 3, EndLine: 3}},
	}

	t.Run("verified -> answer_found", func(t *testing.T) {
		ma := modelAnswer{Answer: "Foo returns 1", Evidence: []modelEvidence{{ID: "E1", Quote: "return 1"}}}
		got := deriveAnswer(ma, blocks)
		if got.Status != statusAnswerFound || !got.AnswerFound || !got.Verified {
			t.Fatalf("status=%s found=%v verified=%v", got.Status, got.AnswerFound, got.Verified)
		}
		if got.Answer != "Foo returns 1" {
			t.Errorf("answer = %q, want populated", got.Answer)
		}
		if len(got.Sources) != 1 || got.Sources[0].Source != "a.go" || got.Sources[0].StartLine != 10 {
			t.Errorf("sources = %+v", got.Sources)
		}
		if len(got.Quotes) != 1 || got.Quotes[0].ID != "E1" {
			t.Errorf("quotes = %+v", got.Quotes)
		}
	})

	t.Run("invented quote -> unverified, answer suppressed", func(t *testing.T) {
		ma := modelAnswer{Answer: "Foo returns 99", Evidence: []modelEvidence{{ID: "E1", Quote: "return 99"}}}
		got := deriveAnswer(ma, blocks)
		if got.Status != statusUnverifiedAnswer || got.AnswerFound || got.Verified {
			t.Fatalf("status=%s found=%v", got.Status, got.AnswerFound)
		}
		if got.Answer != "" {
			t.Errorf("answer = %q, want empty (suppressed)", got.Answer)
		}
		if len(got.VerificationErrors) != 1 {
			t.Errorf("verification_errors = %v", got.VerificationErrors)
		}
	})

	t.Run("unknown id -> unverified with error", func(t *testing.T) {
		ma := modelAnswer{Answer: "x", Evidence: []modelEvidence{{ID: "E9", Quote: "return 1"}}}
		got := deriveAnswer(ma, blocks)
		if got.Status != statusUnverifiedAnswer {
			t.Fatalf("status=%s", got.Status)
		}
		if len(got.VerificationErrors) != 1 {
			t.Errorf("verification_errors = %v", got.VerificationErrors)
		}
	})

	t.Run("empty answer -> not_in_retrieved_context", func(t *testing.T) {
		got := deriveAnswer(modelAnswer{Answer: "  "}, blocks)
		if got.Status != statusNotInRetrievedContext || got.AnswerFound {
			t.Fatalf("status=%s found=%v", got.Status, got.AnswerFound)
		}
	})
}

func TestBuildEvidenceBlocks(t *testing.T) {
	results := []rag.SearchResult{
		{Chunk: rag.Chunk{Source: "a.go", StartLine: 1, EndLine: 1, Content: "alpha"}},
		{Chunk: rag.Chunk{Source: "b.go", StartLine: 2, EndLine: 2, Content: "beta"}},
	}
	t.Run("labels and format", func(t *testing.T) {
		text, blocks := buildEvidenceBlocks(results, 4096)
		if len(blocks) != 2 || blocks[0].ID != "E1" || blocks[1].ID != "E2" {
			t.Fatalf("blocks = %+v", blocks)
		}
		if !strings.Contains(text, "[E1] a.go (lines 1-1)") || !strings.Contains(text, "alpha") {
			t.Errorf("text missing E1 block:\n%s", text)
		}
		if !strings.Contains(text, "[E2] b.go (lines 2-2)") {
			t.Errorf("text missing E2 block:\n%s", text)
		}
	})
	t.Run("budget drops tail but keeps first", func(t *testing.T) {
		// maxTokens=1 -> maxChars=4, far smaller than one block; first still included.
		text, blocks := buildEvidenceBlocks(results, 1)
		if len(blocks) != 1 || blocks[0].ID != "E1" {
			t.Fatalf("blocks = %+v", blocks)
		}
		if strings.Contains(text, "E2") {
			t.Errorf("E2 should have been dropped:\n%s", text)
		}
	})
}

// queuedRouteEngine returns successive chat contents on each Route call, so a
// test can drive the repair retry (call 1 malformed, call 2 valid). Each
// rag_answer model turn performs exactly one Route+ExecuteChat. Modeled on
// recordingRouteEngine / fakeRouteProvider in mcp/router_seam_test.go.
type queuedRouteEngine struct {
	responses []string
	calls     int
}

func (e *queuedRouteEngine) Route(ctx context.Context, req provider.RoutingRequest) (*provider.RoutePlan, error) {
	content := ""
	if e.calls < len(e.responses) {
		content = e.responses[e.calls]
	}
	e.calls++
	model := req.Model
	if model == "" && len(req.PreferredChain) > 0 {
		model = req.PreferredChain[0]
	}
	prov := &fakeRouteProvider{name: "fake", chatContent: content}
	return &provider.RoutePlan{
		Provider: prov,
		Model:    model,
		Profile: &provider.ModelProfile{
			Key:           provider.ModelKey{Provider: "fake", Model: model},
			Caps:          provider.CapChat | provider.CapGenerate | provider.CapEmbed | provider.CapStream,
			ContextWindow: 8192,
		},
		Request: req,
		Budget:  provider.BudgetResult{Decision: provider.BudgetOK},
	}, nil
}

func (e *queuedRouteEngine) Close() error { return nil }
func (e *queuedRouteEngine) BreakerInfo(string) (provider.BreakerInfo, bool) {
	return provider.BreakerInfo{}, false
}
func (e *queuedRouteEngine) WarmthSnapshot() []provider.WarmModel              { return nil }
func (e *queuedRouteEngine) StickyRoutes() map[string]provider.StickyRouteInfo { return nil }

func newAnswerTestServer(t *testing.T, router routeEngine) *Server {
	t.Helper()
	if router == nil {
		router = &queuedRouteEngine{}
	}
	return &Server{
		cfg: &config.Config{
			Defaults: map[string]string{"chat": "primary"},
			Models: map[string]config.ModelConfig{
				"primary": {Name: "qwen3:8b", Provider: "ollama"},
			},
		},
		router: router,
	}
}

func TestCallForAnswer_RepairRetry(t *testing.T) {
	s := newAnswerTestServer(t, nil)
	good := `{"answer":"ok","evidence":[]}`

	t.Run("first valid, no retry", func(t *testing.T) {
		eng := &queuedRouteEngine{responses: []string{good}}
		ma, ok, err := s.callForAnswer(context.Background(), eng, "", "q", "E1...")
		if err != nil || !ok || ma.Answer != "ok" {
			t.Fatalf("ma=%+v ok=%v err=%v", ma, ok, err)
		}
		if eng.calls != 1 {
			t.Errorf("calls = %d, want 1", eng.calls)
		}
	})

	t.Run("malformed then valid", func(t *testing.T) {
		eng := &queuedRouteEngine{responses: []string{"not json", good}}
		ma, ok, err := s.callForAnswer(context.Background(), eng, "", "q", "E1...")
		if err != nil || !ok || ma.Answer != "ok" {
			t.Fatalf("ma=%+v ok=%v err=%v", ma, ok, err)
		}
		if eng.calls != 2 {
			t.Errorf("calls = %d, want 2 (one repair)", eng.calls)
		}
	})

	t.Run("malformed twice -> not ok", func(t *testing.T) {
		eng := &queuedRouteEngine{responses: []string{"nope", "still nope"}}
		_, ok, err := s.callForAnswer(context.Background(), eng, "", "q", "E1...")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if ok {
			t.Fatal("expected ok=false after failed repair")
		}
		if eng.calls != 2 {
			t.Errorf("calls = %d, want 2", eng.calls)
		}
	})
}

func TestHandleRAGAnswer(t *testing.T) {
	good := `{"answer":"computeFoo returns 1","evidence":[{"id":"E1","quote":"return 1"}]}`

	t.Run("answer_found", func(t *testing.T) {
		s := answerEnv(t, &queuedRouteEngine{responses: []string{good}})
		res := callAnswer(t, s, `{"question":"what does computeFoo return?"}`)
		if res.Status != statusAnswerFound || !res.AnswerFound || res.Answer == "" {
			t.Fatalf("res = %+v", res)
		}
	})

	t.Run("unverified answer suppresses public answer", func(t *testing.T) {
		bad := `{"answer":"Foo returns 7","evidence":[{"id":"E1","quote":"return 7"}]}`
		s := answerEnv(t, &queuedRouteEngine{responses: []string{bad}})
		res := callAnswer(t, s, `{"question":"q","include_diagnostics":true}`)
		if res.Status != statusUnverifiedAnswer || res.Answer != "" {
			t.Fatalf("res = %+v", res)
		}
		if res.Diagnostics == nil || res.Diagnostics.CandidateAnswer == "" {
			t.Errorf("candidate answer should be in diagnostics: %+v", res.Diagnostics)
		}
	})

	t.Run("model declines -> not_in_retrieved_context", func(t *testing.T) {
		s := answerEnv(t, &queuedRouteEngine{responses: []string{`{"answer":"","evidence":[]}`}})
		res := callAnswer(t, s, `{"question":"unrelated?"}`)
		if res.Status != statusNotInRetrievedContext {
			t.Fatalf("status = %s", res.Status)
		}
	})

	t.Run("malformed twice -> malformed_output", func(t *testing.T) {
		s := answerEnv(t, &queuedRouteEngine{responses: []string{"no", "still no"}})
		res := callAnswer(t, s, `{"question":"q"}`)
		if res.Status != statusMalformedOutput {
			t.Fatalf("status = %s", res.Status)
		}
	})

	t.Run("zero retrieved chunks skips model", func(t *testing.T) {
		eng := &queuedRouteEngine{responses: []string{good}}
		s := answerEnvWithStore(t, eng, &answerStore{})
		res := callAnswer(t, s, `{"question":"q"}`)
		if res.Status != statusNotInRetrievedContext {
			t.Fatalf("status = %s, want not_in_retrieved_context", res.Status)
		}
		if eng.calls != 0 {
			t.Fatalf("model calls = %d, want 0", eng.calls)
		}
	})

	t.Run("missing question -> validation error", func(t *testing.T) {
		s := answerEnv(t, &queuedRouteEngine{responses: []string{good}})
		out, err := s.handleRAGAnswer(context.Background(), rawArgs(t, `{}`))
		if err != nil || out == nil || !out.IsError {
			t.Fatalf("expected validation error result, got out=%v err=%v", out, err)
		}
	})
}

// callAnswer invokes the handler and unmarshals the JSON tool result.
func callAnswer(t *testing.T, s *Server, args string) ragAnswerResult {
	t.Helper()
	out, err := s.handleRAGAnswer(context.Background(), rawArgs(t, args))
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected tool error: %s", extractText(out))
	}
	var res ragAnswerResult
	if uerr := json.Unmarshal([]byte(extractText(out)), &res); uerr != nil {
		t.Fatalf("unmarshal result: %v (%s)", uerr, extractText(out))
	}
	return res
}

// rawArgs wraps a JSON arguments string in a CallToolRequest.
func rawArgs(t *testing.T, args string) *gomcp.CallToolRequest {
	t.Helper()
	return &gomcp.CallToolRequest{Params: &gomcp.CallToolParamsRaw{Arguments: json.RawMessage(args)}}
}

type answerStore struct {
	results []rag.SearchResult
}

func (s *answerStore) Store(context.Context, []rag.Chunk, [][]float64) error { return nil }

func (s *answerStore) Search(_ context.Context, _ []float64, k int) ([]rag.SearchResult, error) {
	if k > 0 && len(s.results) > k {
		return s.results[:k], nil
	}
	return s.results, nil
}

func (s *answerStore) DeleteBySource(context.Context, string) error  { return nil }
func (s *answerStore) Stats(context.Context) (rag.StoreStats, error) { return rag.StoreStats{}, nil }
func (s *answerStore) Close() error                                  { return nil }

func answerEnv(t *testing.T, eng routeEngine) *Server {
	t.Helper()
	chunk := rag.Chunk{
		ID: "c1", Source: "a.go", StartLine: 10, EndLine: 12,
		Content: "func computeFoo() int { return 1 }",
	}
	store := &answerStore{results: []rag.SearchResult{{Chunk: chunk, Score: 1}}}
	return answerEnvWithStore(t, eng, store)
}

func answerEnvWithStore(t *testing.T, eng routeEngine, store rag.VectorStore) *Server {
	t.Helper()
	ret, err := rag.NewRetrieverWithEmbedder(rag.EmbedderFunc(func(context.Context, string, []string) (rag.EmbedResult, error) {
		return rag.EmbedResult{Embeddings: [][]float64{{1}}}, nil
	}), store)
	if err != nil {
		t.Fatalf("NewRetrieverWithEmbedder: %v", err)
	}
	s := newAnswerTestServer(t, eng)
	s.store = store
	s.retriever = ret
	return s
}
