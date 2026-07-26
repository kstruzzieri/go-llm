package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/agenttest"
	"github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

type scripted struct {
	responses []agent.ModelResult
	calls     int
}

func (s *scripted) Chat(_ context.Context, _ provider.ChatRequest,
	onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	r := s.responses[s.calls]
	s.calls++
	if onToken != nil && r.Response.Content != "" {
		if err := onToken(provider.ChatResponse{Content: r.Response.Content}); err != nil {
			return agent.ModelResult{}, err
		}
	}
	return r, nil
}

type fakeRetriever struct{}

func (fakeRetriever) Retrieve(context.Context, string, int) ([]rag.SearchResult, error) {
	return []rag.SearchResult{{Chunk: rag.Chunk{StableKey: "k", Content: "ctx"}, Score: 1}}, nil
}
func (fakeRetriever) BuildContext([]rag.SearchResult, int) string { return "ctx" }

func TestEndToEndRetrieveThenAnswer(t *testing.T) {
	mc := &scripted{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "retrieve", Arguments: json.RawMessage(`{"query":"x"}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "final", Done: true}},
	}}
	o := agent.New(mc, agent.ContextManager{
		Compactor: agent.RecencyCompactor{},
	})
	rec := &agenttest.RecorderObserver{}
	res, err := o.Run(context.Background(), agent.Request{
		Goal:  "explain x",
		Tools: []agent.Tool{tools.Retrieve{R: fakeRetriever{}, K: 3}},
	}, rec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Answer != "final" || res.StopReason != agent.Completed {
		t.Fatalf("got answer=%q stop=%v", res.Answer, res.StopReason)
	}
	// observer saw: pressure (step 0 assembly), step (tool turn), tool_call,
	// pressure (step 1 assembly), token, step (final turn).
	if len(rec.Kinds) < 6 || rec.Kinds[0] != "pressure" || rec.Kinds[1] != "step" || rec.Kinds[2] != "tool_call" || rec.Kinds[3] != "pressure" || rec.Kinds[4] != "token" {
		t.Fatalf("unexpected event order: %v", rec.Kinds)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "retrieve" {
		t.Fatalf("expected one retrieve call: %+v", res.ToolCalls)
	}
}

// progressiveCapRetriever renders output near RetrieveOutputCap with
// multi-byte runes to prove the runtime capOutput is a no-op end to end.
type progressiveCapRetriever struct{ payload string }

func (p progressiveCapRetriever) Retrieve(context.Context, string, int) ([]rag.SearchResult, error) {
	return []rag.SearchResult{{Chunk: rag.Chunk{ID: "c1", Source: "a.go", Content: "x"}, Score: 1}}, nil
}
func (p progressiveCapRetriever) BuildContext([]rag.SearchResult, int) string { return "unused" }
func (p progressiveCapRetriever) RenderProgressive(_ context.Context, req rag.ProgressiveRenderRequest) (string, rag.ProgressiveTrace, error) {
	// Honor the contract the real renderer guarantees: never exceed MaxBytes,
	// never split a rune.
	out := p.payload
	for len(out) > req.MaxBytes {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	return out, rag.ProgressiveTrace{}, nil
}

func TestEndToEndProgressiveOutputSurvivesCapExactly(t *testing.T) {
	// Payload deliberately larger than the cap; the tool-side renderer trims
	// to MaxBytes = RetrieveOutputCap, so the runtime capOutput must change
	// nothing. The tool observation in the transcript must equal the renderer
	// output byte for byte.
	payload := strings.Repeat("héllo wörld ", tools.RetrieveOutputCap/12+10)
	fake := progressiveCapRetriever{payload: payload}
	mc := &scripted{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{{
			ID: "1", Type: "function",
			Function: provider.ToolCallFunction{Name: "retrieve", Arguments: json.RawMessage(`{"query":"x"}`)},
		}}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	o := agent.New(mc, agent.ContextManager{Compactor: agent.RecencyCompactor{}})
	res, err := o.Run(context.Background(), agent.Request{
		Goal:   "big retrieval",
		Budget: agent.Budget{InputCeiling: 1 << 20},
		Tools:  []agent.Tool{tools.Retrieve{R: fake, Progressive: true}},
	}, &agenttest.RecorderObserver{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want, _, _ := fake.RenderProgressive(context.Background(),
		rag.ProgressiveRenderRequest{MaxBytes: tools.RetrieveOutputCap})
	var toolMsg string
	for _, msg := range res.Messages {
		if msg.Role == "tool" {
			toolMsg = msg.Content
		}
	}
	if toolMsg != want {
		t.Fatalf("dispatch altered tool output: got %d bytes, want %d — capOutput is not a no-op",
			len(toolMsg), len(want))
	}
	if !utf8.ValidString(toolMsg) {
		t.Fatal("dispatch split a multi-byte rune")
	}
}
