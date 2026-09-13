package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/promptfence"
	"github.com/kstruzzieri/go-llm/provider"
)

// advisoryCaller records every wire request the orchestrator issues, which is
// what separates the projected copy from the State the runtime keeps.
type advisoryCaller struct{ reqs []provider.ChatRequest }

func (c *advisoryCaller) Chat(_ context.Context, req provider.ChatRequest, _ func(provider.ChatResponse) error) (ModelResult, error) {
	c.reqs = append(c.reqs, req)
	return ModelResult{Response: provider.ChatResponse{Content: "answer"}}, nil
}

func testAdvisory() *Advisory {
	return &Advisory{
		Source: "claude", Tool: "claude 2.1.240", Model: "opus",
		Digest: strings.Repeat("a", 64), Content: "Use a mutex.\nSENTINEL-ADVICE", Origin: OriginModel,
	}
}

func TestAdvisoryRendersOnlyOnTheWireCopy(t *testing.T) {
	caller := &advisoryCaller{}
	o := New(caller, ContextManager{})
	res, err := o.Run(context.Background(), Request{
		Goal:     "How do I fix the race?",
		History:  []provider.ChatMessage{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "hello"}},
		Advisory: testAdvisory(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wire := caller.reqs[0].Messages
	last := wire[len(wire)-1]
	if last.Role != "user" ||
		!strings.HasPrefix(last.Content, "How do I fix the race?\n\n<<<CONSULT_ADVICE ") ||
		!strings.Contains(last.Content, "SENTINEL-ADVICE") ||
		!strings.Contains(last.Content, `source: consultant "claude" (claude 2.1.240, model opus, sha256:aaaa`) {
		t.Fatalf("wire projection wrong: %q", last.Content)
	}
	for _, m := range wire {
		if m.Role == "tool" || len(m.ToolCalls) != 0 || m.ToolCallID != "" || m.ToolName != "" {
			t.Fatalf("fabricated tool exchange: %+v", m)
		}
	}
	for _, m := range res.Messages {
		if strings.Contains(m.Content, "SENTINEL-ADVICE") || strings.Contains(m.Content, "CONSULT_ADVICE") {
			t.Fatalf("advice leaked into Result.Messages: %q", m.Content)
		}
	}
	if res.Messages[0].Content != "How do I fix the race?" {
		t.Fatalf("raw goal not preserved: %q", res.Messages[0].Content)
	}
}

func TestAdvisoryFenceKeyIsPerRender(t *testing.T) {
	caller := &advisoryCaller{}
	o := New(caller, ContextManager{})
	for i := 0; i < 2; i++ {
		if _, err := o.Run(context.Background(), Request{Goal: "g", Advisory: testAdvisory()}, nil); err != nil {
			t.Fatal(err)
		}
	}
	key := func(req provider.ChatRequest) string {
		c := req.Messages[len(req.Messages)-1].Content
		i := strings.Index(c, "<<<CONSULT_ADVICE ")
		return strings.Fields(c[i:])[1]
	}
	if key(caller.reqs[0]) == key(caller.reqs[1]) {
		t.Fatal("fence key reused across renders")
	}
}

func TestAdvisoryCostIsChargedInBothArmsAndExhaustsBeforeCall(t *testing.T) {
	adv := testAdvisory()
	msg := Message{ChatMessage: provider.ChatMessage{Role: "user", Content: "goal"}, Segment: Pinned, Advisory: adv}
	legacy := ContextManager{}
	mixed := ContextManager{Mixed: true}
	rendered := renderAdvisoryLines(advisoryPlaceholderOpen, advisoryPlaceholderClose, "goal", *adv)
	if legacy.messageCost(msg) != legacy.estimate(rendered) || mixed.messageCost(msg) != mixed.estimate(rendered) {
		t.Fatalf("cost drift: legacy=%d mixed=%d want=%d", legacy.messageCost(msg), mixed.messageCost(msg), legacy.estimate(rendered))
	}
	if legacy.messageCost(msg) <= legacy.messageCost(Message{ChatMessage: msg.ChatMessage, Segment: Pinned}) {
		t.Fatal("projection not charged")
	}
	caller := &advisoryCaller{}
	o := New(caller, ContextManager{})
	_, err := o.Run(context.Background(), Request{Goal: "goal", Advisory: adv, Budget: Budget{InputCeiling: 8}}, nil)
	if !errors.Is(err, ErrContextExhausted) || len(caller.reqs) != 0 {
		t.Fatalf("expected exhaustion before any model call, got err=%v calls=%d", err, len(caller.reqs))
	}
}

func TestAdvisoryPlaceholderEnvelopeMatchesRealFrameLength(t *testing.T) {
	f := promptfence.New()
	adv := *testAdvisory()
	if len(renderAdvisoryLines(f.Open(advisoryRegion), f.Close(advisoryRegion), "g", adv)) !=
		len(renderAdvisoryLines(advisoryPlaceholderOpen, advisoryPlaceholderClose, "g", adv)) {
		t.Fatal("placeholder envelope length differs from a real render")
	}
}

type blockingInterceptor struct{ block bool }

func (blockingInterceptor) Name() string { return "test-block" }
func (blockingInterceptor) InspectOutput(context.Context, OutputInspection) ([]Finding, error) {
	return nil, nil
}
func (blockingInterceptor) InspectToolCall(context.Context, ToolCallInspection) ([]Finding, error) {
	return nil, nil
}
func (b blockingInterceptor) InspectInput(_ context.Context, in InputInspection) ([]Finding, error) {
	for _, m := range in.Messages {
		if strings.Contains(m.Content, "SENTINEL-ADVICE") {
			v := VerdictTag
			if b.block {
				v = VerdictBlock
			}
			return []Finding{{
				Interceptor: "test-block", Rule: "advice", Verdict: v, Risk: 10, Detail: "advice seen",
				Origin: m.Origin, Hook: HookInput, Step: in.Step, Target: TargetMessage, StateIndex: m.StateIndex,
			}}, nil
		}
	}
	return nil, nil
}

func TestAdvisoryIsReinspectedAtStepZero(t *testing.T) {
	caller := &advisoryCaller{}
	o := New(caller, ContextManager{}, WithInterceptors(blockingInterceptor{block: true}))
	res, err := o.Run(context.Background(), Request{Goal: "g", Advisory: testAdvisory()}, nil)
	if !errors.Is(err, ErrAdvisoryBlocked) || len(caller.reqs) != 0 {
		t.Fatalf("blocked advisory reached the model: err=%v calls=%d", err, len(caller.reqs))
	}
	if res.Risk == nil || res.Risk.Score != 10 {
		t.Fatalf("risk report not published on the block: %+v", res.Risk)
	}
	if _, err := o.InspectAdvisory(context.Background(), *testAdvisory()); !errors.Is(err, ErrAdvisoryBlocked) {
		t.Fatalf("consult-time inspection did not block: %v", err)
	}
	o = New(caller, ContextManager{}, WithInterceptors(blockingInterceptor{}))
	out, err := o.InspectAdvisory(context.Background(), *testAdvisory())
	if err != nil || out.Content == testAdvisory().Content || !strings.HasPrefix(out.Content, testAdvisory().Content) {
		t.Fatalf("tag trailer not appended after the content: %v %q", err, out.Content)
	}
}

func TestAdvisoryRunWithoutInterceptorsIsAllowed(t *testing.T) {
	caller := &advisoryCaller{}
	o := New(caller, ContextManager{})
	if _, err := o.Run(context.Background(), Request{Goal: "g", Advisory: testAdvisory()}, nil); err != nil {
		t.Fatalf("an advisory with no interceptors installed must run: %v", err)
	}
	if len(caller.reqs) != 1 {
		t.Fatalf("model calls = %d, want 1", len(caller.reqs))
	}
}

func TestAdvisoryStepZeroOnlyRendersOnce(t *testing.T) {
	caller := &advisoryCaller{}
	o := New(caller, ContextManager{})
	if _, err := o.Run(context.Background(), Request{Goal: "g", Advisory: testAdvisory()}, nil); err != nil {
		t.Fatal(err)
	}
	goal := caller.reqs[0].Messages[len(caller.reqs[0].Messages)-1].Content
	if n := strings.Count(goal, "<<<CONSULT_ADVICE "); n != 1 {
		t.Fatalf("open marker count = %d, want 1: %q", n, goal)
	}
	if n := strings.Count(goal, ">>>CONSULT_ADVICE "); n != 1 {
		t.Fatalf("close marker count = %d, want 1: %q", n, goal)
	}
}

func TestAdvisoryNotInHistoryOrSummary(t *testing.T) {
	caller := &advisoryCaller{}
	o := New(caller, ContextManager{})
	first, err := o.Run(context.Background(), Request{Goal: "g", Advisory: testAdvisory()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range first.Messages {
		if strings.Contains(m.Content, "CONSULT_ADVICE") || strings.Contains(m.Content, "SENTINEL-ADVICE") {
			t.Fatalf("advisory persisted into the turn transcript: %q", m.Content)
		}
	}
	// A second turn fed the first turn's messages carries nothing forward: the
	// advisory is staged for exactly one goal.
	if _, err := o.Run(context.Background(), Request{Goal: "g2", History: first.Messages, HistorySummary: "prior"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, m := range caller.reqs[1].Messages {
		if strings.Contains(m.Content, "CONSULT_ADVICE") || strings.Contains(m.Content, "SENTINEL-ADVICE") {
			t.Fatalf("advisory survived into the next turn: %q", m.Content)
		}
	}
}

func TestAdvisoryValidation(t *testing.T) {
	for name, a := range map[string]Advisory{
		"empty_content": {Source: "c", Origin: OriginModel},
		"bad_origin":    {Source: "c", Content: "x", Origin: OriginUser},
		"control_src":   {Source: "c\x00", Content: "x", Origin: OriginModel},
		"too_large":     {Source: "c", Content: strings.Repeat("x", 65537), Origin: OriginModel},
		"no_source":     {Content: "x", Origin: OriginModel},
	} {
		if err := ValidateAdvisory(&a); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := ValidateAdvisory(testAdvisory()); err != nil {
		t.Errorf("valid advisory rejected: %v", err)
	}
	if err := ValidateAdvisory(nil); err == nil {
		t.Error("nil advisory accepted")
	}
}
