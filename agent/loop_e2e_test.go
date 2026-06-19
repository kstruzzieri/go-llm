package agent_test

import (
	"context"
	"encoding/json"
	"testing"

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
	// observer saw: step (tool turn), tool_call, token, step (final turn).
	if len(rec.Kinds) < 4 || rec.Kinds[0] != "step" || rec.Kinds[1] != "tool_call" || rec.Kinds[2] != "token" {
		t.Fatalf("unexpected event order: %v", rec.Kinds)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "retrieve" {
		t.Fatalf("expected one retrieve call: %+v", res.ToolCalls)
	}
}
