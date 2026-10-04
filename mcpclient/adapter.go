package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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
	return agent.ToolSpec{Name: a.prefixedName, Description: a.description, Parameters: append(json.RawMessage(nil), a.schema...)}
}

// Origin declares every MCP observation foreign (#436 spec D4): the server is
// outside the workspace trust boundary, so detectors may block rather than
// tag its output.
func (a *toolAdapter) Origin() agent.Origin { return agent.OriginForeign }

var _ agent.OriginTool = (*toolAdapter)(nil)

// Effect is the conservative upper bound for an untrusted remote tool. Class is
// the full bitset. It leaves approval unchanged, because ApprovalAlways already
// forces needsApproval to true, and it makes /tools honest. It is not inert:
// since #575 Golem's always-on egress guard (agent/interceptor Egress) tests
// Class.Has(Exec) on every run, so an MCP call whose arguments carry a
// decodable argv gets an egress finding and a badge at its approval prompt
// (unless that argv is on the classifier's quiet set).
// Narrowing Class would silently drop that classification. ApprovalAlways is
// mandatory -- Network alone is NOT "mutating" (IsMutating checks Write|Exec),
// so an ApprovalDefault network tool would skip approval entirely.
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
		return agent.ToolResult{IsError: true, Content: callFailure(err)}, nil
	}
	return agent.ToolResult{Content: flattenContent(res), IsError: res.IsError}, nil
}

// sdkLifecycleCodes are the go-sdk's own JSON-RPC sentinels (internal/jsonrpc2,
// not importable): unknown error, client closing, server closing, rejected by
// transport. They report transport state, not a server's answer.
var sdkLifecycleCodes = map[int64]bool{-32001: true, -32003: true, -32004: true, -32005: true}

// callFailure maps a call error to model-facing text by an ordered allowlist
// (spec §5.11). Transport errors can embed the full URL, query included, so
// only a server-returned JSON-RPC message is ever passed through.
func callFailure(err error) string {
	var rpcErr *jsonrpc.Error
	switch {
	case errors.Is(err, errRedirectRefused):
		return "mcp call failed: redirect refused"
	case errors.Is(err, errDestinationRefused):
		return "mcp call failed: destination refused"
	case errors.Is(err, context.Canceled):
		return "mcp call failed: canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "mcp call failed: timed out"
	case errors.As(err, &rpcErr) && !sdkLifecycleCodes[rpcErr.Code]:
		return "mcp call failed: " + rpcErr.Message
	default:
		return "mcp call failed: transport error"
	}
}
