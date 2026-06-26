package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/provider"
)

// ModelCaller is the model seam. The default adapter routes the "agent"
// use-case through provider.Router; tests inject a fake.
type ModelCaller interface {
	Chat(ctx context.Context, req provider.ChatRequest,
		onToken func(provider.ChatResponse) error) (ModelResult, error)
}

// ModelResult is the accumulated response plus captured route telemetry.
type ModelResult struct {
	Response     provider.ChatResponse
	RouteOutcome *provider.RouteOutcome
}

// planExecutor is the minimal slice of *provider.RoutePlan the adapter needs;
// abstracting it lets tests fake the streaming execution.
type planExecutor interface {
	ExecuteChatStream(ctx context.Context, fn func(provider.ChatResponse) error) error
}

type routerModelCaller struct {
	route func(ctx context.Context, rr provider.RoutingRequest) (planExecutor, error)
}

// NewRouterModelCaller wires the default adapter to a concrete provider.Router.
func NewRouterModelCaller(r *provider.Router) ModelCaller {
	return &routerModelCaller{
		route: func(ctx context.Context, rr provider.RoutingRequest) (planExecutor, error) {
			return r.Route(ctx, rr)
		},
	}
}

func (m *routerModelCaller) Chat(ctx context.Context, req provider.ChatRequest,
	onToken func(provider.ChatResponse) error) (ModelResult, error) {

	rr := provider.RoutingRequest{
		UseCase:        "agent",
		Messages:       req.Messages,
		Tools:          req.Tools,
		Options:        req.Options,
		ExpectedOutput: req.Options.NumPredict,
		RequiredCaps:   provider.CapChat | provider.CapStream,
	}
	if len(req.Tools) > 0 {
		rr.RequiredCaps |= provider.CapToolCall
	}

	plan, err := m.route(ctx, rr)
	if err != nil {
		return ModelResult{}, err
	}

	var outcome *provider.RouteOutcome
	wrapped, getFinal := provider.Collect(func(chunk provider.ChatResponse) error {
		if chunk.RouteOutcome != nil {
			outcome = chunk.RouteOutcome
		}
		if onToken != nil {
			return onToken(chunk)
		}
		return nil
	})
	execErr := plan.ExecuteChatStream(ctx, wrapped)
	final := getFinal()
	final.RouteOutcome = outcome
	return ModelResult{Response: final, RouteOutcome: outcome}, execErr
}

type routerSummarizer struct {
	chain []string
	route func(ctx context.Context, rr provider.RoutingRequest) (planExecutor, error)
}

// DefaultSummaryOutputReserve bounds the rolling durable summary. It is the
// summarizer's NumPredict AND the token headroom Golem reserves for the summary
// when deciding how much raw history to keep — one constant, no drift.
const DefaultSummaryOutputReserve = 512

const summarySystemPrompt = `You maintain a single rolling summary of an ongoing coding session. Rewrite the summary so it stays concise and within budget, folding in the new messages.
Do not invent facts.
If uncertain, preserve the original wording briefly.
Output ONLY these sections:
Goals:
Decisions:
Files/commands/tools:
Open tasks:
Recent outcome:`

// NewRouterSummarizer routes durable-history compression through the optional
// "summarize" model role.
func NewRouterSummarizer(r *provider.Router, chain []string) conversation.Summarizer {
	return (&routerSummarizer{
		chain: append([]string(nil), chain...),
		route: func(ctx context.Context, rr provider.RoutingRequest) (planExecutor, error) {
			return r.Route(ctx, rr)
		},
	}).Summarize
}

func (s *routerSummarizer) Summarize(ctx context.Context, prior string, msgs []conversation.Message) (string, error) {
	if s.route == nil {
		return "", fmt.Errorf("agent: summarize route is nil")
	}
	rr := provider.RoutingRequest{
		UseCase: "summarize",
		Messages: []provider.ChatMessage{
			{Role: "system", Content: summarySystemPrompt},
			{Role: "user", Content: summaryUserContent(prior, msgs)},
		},
		Options:        provider.ModelOptions{NumPredict: DefaultSummaryOutputReserve},
		ExpectedOutput: DefaultSummaryOutputReserve,
		RequiredCaps:   provider.CapChat | provider.CapStream,
	}
	if len(s.chain) > 0 {
		rr.PreferredChain = append([]string(nil), s.chain...)
		rr.StrictChain = true
	}
	plan, err := s.route(ctx, rr)
	if err != nil {
		return "", err
	}
	wrapped, getFinal := provider.Collect(nil)
	if err := plan.ExecuteChatStream(ctx, wrapped); err != nil {
		return "", err
	}
	return strings.TrimSpace(getFinal().Content), nil
}

// summaryUserContent labels the prior summary and the new transcript so the
// model rewrites one rolling blob rather than appending.
func summaryUserContent(prior string, msgs []conversation.Message) string {
	transcript := summarizeTranscript(msgs)
	if strings.TrimSpace(prior) == "" {
		return transcript
	}
	return "Current summary:\n" + prior + "\n\nNew messages:\n" + transcript
}

func summarizeTranscript(msgs []conversation.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Role != "" {
			b.WriteString(m.Role)
			b.WriteString(": ")
		}
		b.WriteString(m.Content)
		if len(m.ToolCalls) > 0 {
			b.WriteString(" tool_calls=")
			b.Write(m.ToolCalls)
		}
		if m.ToolName != "" {
			b.WriteString(" tool_name=")
			b.WriteString(m.ToolName)
		}
		if m.ToolCallID != "" {
			b.WriteString(" tool_call_id=")
			b.WriteString(m.ToolCallID)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}
