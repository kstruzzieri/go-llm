package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// delegateTimeout bounds a single delegated code-generation sub-call. It is
// deliberately longer than the default tool timeout: a specialist model
// generating a whole file routinely exceeds the read-tool budget.
const delegateTimeout = 5 * time.Minute

// delegateSystemPrompt frames the specialist sub-call. The delegate returns a
// proposal; it must not claim disk writes or tool use.
const delegateSystemPrompt = "You are a code-generation specialist invoked for one scoped sub-task. " +
	"Return only the requested code or text, directly, with no preamble or explanation. " +
	"Do not claim to write files, run commands, or use tools; your output is a proposal the orchestrator will review and apply."

// DelegateCode routes one scoped code-generation sub-task to a specialist model
// (the caller is pinned to the configured delegate role chain) and returns the
// generated text as a NON-MUTATING tool result. It never touches disk: the
// orchestrator integrates the result through the approval-gated write tools.
type DelegateCode struct {
	caller  agent.ModelCaller
	onToken func(string)
}

// DelegateOption configures optional DelegateCode behavior.
type DelegateOption func(*DelegateCode)

// WithStream streams the specialist's output tokens to sink as they arrive, so a
// caller can show progress during a long delegate call. A nil sink (or no option)
// leaves streaming off — the tool still returns the full result. The sink is
// display-only; it never affects the ToolResult.
func WithStream(sink func(string)) DelegateOption {
	return func(d *DelegateCode) { d.onToken = sink }
}

// NewDelegateCode builds the delegate_code tool over a caller pinned to the
// delegate role chain. Pass WithStream to surface generation progress.
func NewDelegateCode(caller agent.ModelCaller, opts ...DelegateOption) *DelegateCode {
	d := &DelegateCode{caller: caller}
	for _, o := range opts {
		o(d)
	}
	return d
}

type delegateArgs struct {
	Prompt string `json:"prompt"`
}

func (*DelegateCode) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "delegate_code",
		Description: "Delegate one self-contained code-generation sub-task to a specialist coding model and return the generated code. " +
			"Use it for bulk or boilerplate generation, not for planning or decisions. The result is a proposal: review it and write it to disk with write_file or edit_file. This tool does not modify any file.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "prompt":{"type":"string","description":"a precise, self-contained description of the code to generate"}
  },
  "required":["prompt"]
}`),
	}
}

// Effect is Read|Network (not plain Read): the tool makes an outbound model
// call, and Network keeps it off the read-only parallel dispatch path. The
// OutputCap matches the write tools' ceiling (mutateMaxBytes) so a large
// generated proposal is not truncated below what write_file/edit_file accept.
func (*DelegateCode) Effect() agent.Effect {
	return agent.Effect{
		Class:     agent.Read | agent.Network,
		Timeout:   delegateTimeout,
		OutputCap: mutateMaxBytes,
		Approval:  agent.ApprovalNever,
	}
}

func (t *DelegateCode) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args delegateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return agent.ToolResult{IsError: true, Content: "delegate_code requires a non-empty prompt"}, nil
	}

	req := provider.ChatRequest{
		Messages: []provider.ChatMessage{
			{Role: "system", Content: delegateSystemPrompt},
			{Role: "user", Content: args.Prompt},
		},
	}
	var onTok func(provider.ChatResponse) error
	if t.onToken != nil {
		onTok = func(chunk provider.ChatResponse) error {
			if chunk.Content != "" {
				t.onToken(chunk.Content)
			}
			return nil
		}
	}
	result, err := t.caller.Chat(ctx, req, onTok)
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "delegate failed: " + err.Error()}, nil
	}
	content := strings.TrimSpace(result.Response.Content)
	if content == "" {
		return agent.ToolResult{IsError: true, Content: "delegate returned no content"}, nil
	}
	content = stripCodeFence(content)
	return agent.ToolResult{
		Content:      content,
		Preview:      delegatePreview(result.RouteOutcome, content),
		RouteOutcome: result.RouteOutcome,
	}, nil
}

// delegatePreview renders the display-only summary line: the authoring model and
// output size. It never includes generated text.
func delegatePreview(outcome *provider.RouteOutcome, content string) string {
	model := "specialist model"
	if outcome != nil {
		if s := outcome.ActualModel.String(); s != "/" {
			model = s
		}
	}
	lines := strings.Count(content, "\n") + 1
	return fmt.Sprintf("delegated to %s · %d lines", model, lines)
}

// stripCodeFence removes a single outer Markdown code fence when the ENTIRE
// content is one fenced block (```lang\n...\n```), the common way local models
// wrap generated code despite instructions. It strips only when the content
// starts with a fence line, ends with a closing fence, the inner body is
// non-empty, and the body contains no further fence — so a genuine multi-block
// Markdown document (which does not reduce to a single fence pair) is left
// untouched.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s // single line starting with ``` — not a real block
	}
	inner := strings.TrimRight(s[nl+1:], " \t\n")
	if !strings.HasSuffix(inner, "```") {
		return s
	}
	inner = strings.TrimRight(strings.TrimSuffix(inner, "```"), " \t\n")
	if strings.TrimSpace(inner) == "" {
		return s // empty block — leave original so the empty-content guard can catch it
	}
	if strings.Contains(inner, "```") {
		return s // multi-block document — not a single wrapping fence
	}
	return inner
}
