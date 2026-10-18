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
// library: it reaches no library code, so it takes the floor. Being a library
// tool is not the approval exemption; floorExempt names the four of those.
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

// floorEffect is the Trusted approval floor for a tool that is not floorExempt
// (#387, spec 3.3). A plain Read effect is unchanged. Anything else that the
// loop would let run without approval, including a Write or Exec that asks for
// ApprovalNever and a Network effect, is forced to ApprovalAlways. A stronger
// existing approval is kept, and every other Effect field is preserved.
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

// floorExempt reports whether t is one of the four library types that spec 3.3
// exempts from the floor and lets keep its own ApprovalNever: the two
// agent-memory writes (they write only the memory DB, D-P1 option A) and
// delegate_code and dispatch (their Network effect is model-provider traffic
// under the destination policy). It is not "any library tool": every other
// library tool goes through the floor, which leaves it unchanged only because it
// already gates or is plain Read. A typed nil pointer is not exempt.
func floorExempt(t agent.Tool) bool {
	if !libraryTool(t) {
		return false
	}
	switch t.(type) {
	case agenttools.AgentMemoryCreate, agenttools.AgentMemoryPromote, *agenttools.DelegateCode, *agenttools.Dispatch:
		return true
	}
	return false
}

// floorWrap returns t unchanged when it is one of the four spec-exempt library
// types and otherwise a floor wrapper that is a PlanningTool exactly when t is.
// Those two optional interfaces (PlanningTool, OriginTool) are the only ones the
// loop consults.
func floorWrap(t agent.Tool) agent.Tool {
	if floorExempt(t) {
		return t
	}
	if _, ok := t.(agent.PlanningTool); ok {
		return floorPlanningTool{floorTool{inner: t}}
	}
	return floorTool{inner: t}
}
