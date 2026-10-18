package golem

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// libraryTool reports whether t's concrete type is one agent/tools builds. It
// is a type switch, never a name or Effect test: a caller cannot claim library
// status by naming a tool, declaring a Read effect or embedding a library tool
// (an embedding type is a different concrete type). A typed nil pointer is not
// library: it reaches no library code, so it takes the floor.
func libraryTool(t agent.Tool) bool {
	if rv := reflect.ValueOf(t); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return false
	}
	switch t.(type) {
	case *agenttools.ReadFile, *agenttools.Search, *agenttools.Glob, *agenttools.List,
		*agenttools.WriteFile, *agenttools.EditFile,
		*agenttools.RunCommand, *agenttools.StartCommand, *agenttools.CommandStatus,
		*agenttools.CommandTail, *agenttools.StopCommand,
		*agenttools.PromoteArtifact, *agenttools.DelegateCode, *agenttools.Dispatch:
		return true
	case agenttools.ScratchChanges, agenttools.Retrieve, agenttools.MemorySearch,
		agenttools.AgentMemorySearch, agenttools.AgentMemoryCreate, agenttools.AgentMemoryPromote:
		return true
	}
	return false
}

// needsApproval mirrors the loop's own gate (agent.needsApproval, unexported):
// Always gates, Never does not, and every other policy gates exactly the
// mutating classes.
func needsApproval(p agent.ApprovalPolicy, c agent.EffectClass) bool {
	switch p {
	case agent.ApprovalAlways:
		return true
	case agent.ApprovalNever:
		return false
	default:
		return c.IsMutating()
	}
}

// floorEffect is the Trusted approval floor for an opaque tool (#387, spec
// 3.3). A plain Read effect is unchanged. Anything else that the loop would let
// run without approval, including a Write or Exec that asks for ApprovalNever
// and a Network effect, is forced to ApprovalAlways. A stronger existing
// approval is kept, and every other Effect field is preserved.
func floorEffect(e agent.Effect) agent.Effect {
	if e.Class == agent.Read {
		return e
	}
	if !needsApproval(e.Approval, e.Class) {
		e.Approval = agent.ApprovalAlways
	}
	return e
}

// floorTool publishes an opaque tool with its static Effect floored. It
// forwards Spec and Invoke untouched, and Origin as the wrapped tool declares
// it: embedding does not promote Origin, and a wrapper must never upgrade
// trust, so an undeclared tool stays OriginUnknown (#514).
type floorTool struct{ inner agent.Tool }

func (f floorTool) Spec() agent.ToolSpec { return f.inner.Spec() }

func (f floorTool) Effect() agent.Effect { return floorEffect(f.inner.Effect()) }

func (f floorTool) Invoke(ctx context.Context, args json.RawMessage) (agent.ToolResult, error) {
	return f.inner.Invoke(ctx, args)
}

func (f floorTool) Origin() agent.Origin {
	if ot, ok := f.inner.(agent.OriginTool); ok {
		return ot.Origin()
	}
	return agent.OriginUnknown
}

// floorPlanningTool is floorTool for a PlanningTool. The loop replaces the
// static Effect with the plan's, so the floor must cover Plan as well, or a
// planning tool could request ApprovalNever per call.
type floorPlanningTool struct{ floorTool }

func (f floorPlanningTool) Plan(ctx context.Context, args json.RawMessage) (agent.ToolPlan, error) {
	plan, err := f.inner.(agent.PlanningTool).Plan(ctx, args)
	plan.Effect = floorEffect(plan.Effect)
	return plan, err
}

// floorWrap returns t unchanged when it is a library tool and otherwise a
// floor wrapper that is a PlanningTool exactly when t is. Those two optional
// interfaces (PlanningTool, OriginTool) are the only ones the loop consults.
func floorWrap(t agent.Tool) agent.Tool {
	if libraryTool(t) {
		return t
	}
	if _, ok := t.(agent.PlanningTool); ok {
		return floorPlanningTool{floorTool{inner: t}}
	}
	return floorTool{inner: t}
}
