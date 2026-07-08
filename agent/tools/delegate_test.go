package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

type fakeCaller struct {
	resp    provider.ChatResponse
	outcome *provider.RouteOutcome
	err     error
	gotReq  provider.ChatRequest
}

func (f *fakeCaller) Chat(ctx context.Context, req provider.ChatRequest, onToken func(provider.ChatResponse) error) (agent.ModelResult, error) {
	f.gotReq = req
	if f.err != nil {
		return agent.ModelResult{}, f.err
	}
	return agent.ModelResult{Response: f.resp, RouteOutcome: f.outcome}, nil
}

func rawPrompt(p string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"prompt": p})
	return b
}

func TestDelegateCode_Success(t *testing.T) {
	fc := &fakeCaller{
		resp:    provider.ChatResponse{Content: "func add(a, b int) int { return a + b }\n"},
		outcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "coder"}},
	}
	tool := NewDelegateCode(fc)

	out, err := tool.Invoke(context.Background(), rawPrompt("write an add function"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if out.IsError {
		t.Fatalf("unexpected error result: %s", out.Content)
	}
	if !strings.Contains(out.Content, "func add") {
		t.Fatalf("content missing generated code: %q", out.Content)
	}
	if out.RouteOutcome == nil || out.RouteOutcome.ActualModel.Model != "coder" {
		t.Fatalf("RouteOutcome not set: %+v", out.RouteOutcome)
	}
	if !strings.Contains(out.Preview, "local/coder") {
		t.Fatalf("Preview should name the model: %q", out.Preview)
	}
	if len(fc.gotReq.Tools) != 0 {
		t.Fatalf("delegate sub-request must have no tools, got %d", len(fc.gotReq.Tools))
	}
}

func TestDelegateCode_EmptyContent(t *testing.T) {
	fc := &fakeCaller{resp: provider.ChatResponse{Content: "   \n"}}
	out, err := NewDelegateCode(fc).Invoke(context.Background(), rawPrompt("x"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !out.IsError {
		t.Fatal("empty content should be IsError")
	}
}

func TestDelegateCode_CallerError(t *testing.T) {
	fc := &fakeCaller{err: errors.New("route unreachable")}
	out, err := NewDelegateCode(fc).Invoke(context.Background(), rawPrompt("x"))
	if err != nil {
		t.Fatalf("Invoke should not return a Go error: %v", err)
	}
	if !out.IsError || !strings.Contains(out.Content, "route unreachable") {
		t.Fatalf("caller error should be a model-visible IsError result: %+v", out)
	}
}

func TestDelegateCode_EmptyPrompt(t *testing.T) {
	out, _ := NewDelegateCode(&fakeCaller{}).Invoke(context.Background(), rawPrompt("  "))
	if !out.IsError {
		t.Fatal("empty prompt should be IsError")
	}
}

func TestDelegateCode_EffectAndShape(t *testing.T) {
	tool := NewDelegateCode(&fakeCaller{})
	if tool.Spec().Name != "delegate_code" {
		t.Fatalf("name = %q", tool.Spec().Name)
	}
	eff := tool.Effect()
	if eff.Class != (agent.Read | agent.Network) {
		t.Fatalf("effect class = %v, want Read|Network", eff.Class)
	}
	if eff.Class == agent.Read {
		t.Fatal("effect must not be exactly Read (would be parallel-dispatched)")
	}
	if _, isPlanning := any(tool).(agent.PlanningTool); isPlanning {
		t.Fatal("delegate_code must not be a PlanningTool")
	}
}

func TestDelegateCode_SubRequestMessages(t *testing.T) {
	fc := &fakeCaller{resp: provider.ChatResponse{Content: "x"}}
	_, _ = NewDelegateCode(fc).Invoke(context.Background(), rawPrompt("generate a parser"))
	msgs := fc.gotReq.Messages
	if len(msgs) != 2 {
		t.Fatalf("expected system+user messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, "proposal") {
		t.Fatalf("first message must be the delegate system prompt, got %+v", msgs[0])
	}
	if msgs[1].Role != "user" || msgs[1].Content != "generate a parser" {
		t.Fatalf("second message must be the user prompt verbatim, got %+v", msgs[1])
	}
}

func TestDelegateCode_PreviewFallbackNoOutcome(t *testing.T) {
	// nil RouteOutcome -> fallback label, never a bare slash.
	fc := &fakeCaller{resp: provider.ChatResponse{Content: "code\nmore\n"}}
	out, _ := NewDelegateCode(fc).Invoke(context.Background(), rawPrompt("x"))
	if !strings.Contains(out.Preview, "specialist model") {
		t.Fatalf("nil outcome should fall back to 'specialist model', got %q", out.Preview)
	}
	if strings.Contains(out.Preview, "delegated to /") {
		t.Fatalf("must not render a bare slash for an empty model key: %q", out.Preview)
	}
}
