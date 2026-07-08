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
	caller agent.ModelCaller
}

// NewDelegateCode builds the delegate_code tool over a caller pinned to the
// delegate role chain.
func NewDelegateCode(caller agent.ModelCaller) *DelegateCode {
	return &DelegateCode{caller: caller}
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
// call, and Network keeps it off the read-only parallel dispatch path.
func (*DelegateCode) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read | agent.Network, Timeout: delegateTimeout}
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
		Stream: true,
	}
	result, err := t.caller.Chat(ctx, req, nil)
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "delegate failed: " + err.Error()}, nil
	}
	content := strings.TrimSpace(result.Response.Content)
	if content == "" {
		return agent.ToolResult{IsError: true, Content: "delegate returned no content"}, nil
	}
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
