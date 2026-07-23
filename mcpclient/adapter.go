package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kstruzzieri/go-llm/agent"
)

const (
	defaultToolTimeout   = 60 * time.Second
	defaultToolOutputCap = 32 * 1024
)

// toolCaller is the minimal slice of *gomcp.ClientSession the adapter needs, so
// tests can inject a fake without a live session.
type toolCaller interface {
	CallTool(ctx context.Context, params *gomcp.CallToolParams) (*gomcp.CallToolResult, error)
}

// toolAdapter exposes one remote MCP tool as an agent.Tool.
type toolAdapter struct {
	caller       toolCaller
	remoteName   string // unprefixed name sent to the server
	prefixedName string // namespaced model-facing name
	description  string
	schema       json.RawMessage
	timeout      time.Duration
	outputCap    int
}

var _ agent.Tool = (*toolAdapter)(nil)
var _ agent.PlanningTool = (*toolAdapter)(nil)

func (a *toolAdapter) Spec() agent.ToolSpec {
	return agent.ToolSpec{Name: a.prefixedName, Description: a.description, Parameters: a.schema}
}

// Effect is the conservative upper bound for an untrusted remote tool. Class is
// the full bitset: the only consumer of Class is needsApproval, which
// ApprovalAlways already forces to true, so the full set changes no control flow
// and only makes /tools honest. ApprovalAlways is mandatory -- Network alone is
// NOT "mutating" (IsMutating checks Write|Exec), so an ApprovalDefault network
// tool would skip approval entirely.
func (a *toolAdapter) Effect() agent.Effect {
	to, oc := a.timeout, a.outputCap
	if to <= 0 {
		to = defaultToolTimeout
	}
	if oc <= 0 {
		oc = defaultToolOutputCap
	}
	return agent.Effect{
		Class:     agent.Read | agent.Write | agent.Exec | agent.Network,
		Approval:  agent.ApprovalAlways,
		Timeout:   to,
		OutputCap: oc,
	}
}

func (a *toolAdapter) Plan(_ context.Context, raw json.RawMessage) (agent.ToolPlan, error) {
	args := strings.TrimSpace(string(raw))
	if args == "" {
		args = "{}"
	}
	return agent.ToolPlan{
		Effect: a.Effect(),
		Preview: fmt.Sprintf(
			"mcp tool call:\n  tool: %s\n  remote: %s\n  args: %s\n",
			a.prefixedName, a.remoteName, args,
		),
	}, nil
}

// Invoke calls the remote tool. Every failure mode (bad args, transport/session
// error) returns a ToolResult{IsError:true} with a nil Go error: a non-nil error
// would hard-abort the agent loop. The runtime applies Effect.Timeout and
// Effect.OutputCap around this call.
func (a *toolAdapter) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args any
	if len(raw) > 0 && string(raw) != "null" {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
		}
		args = m
	}
	res, err := a.caller.CallTool(ctx, &gomcp.CallToolParams{Name: a.remoteName, Arguments: args})
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "mcp call failed: " + err.Error()}, nil
	}
	return agent.ToolResult{Content: flattenContent(res), IsError: res.IsError}, nil
}
