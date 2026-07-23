package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kstruzzieri/go-llm/provider"
)

// Tool is the effect-aware unit the loop drives.
type Tool interface {
	Spec() ToolSpec
	Effect() Effect
	Invoke(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// PlanningTool is optionally implemented by tools that can pre-compute a
// per-call effect/preview BEFORE invocation so approval can gate mutation.
type PlanningTool interface {
	Plan(ctx context.Context, args json.RawMessage) (ToolPlan, error)
}

// ToolSpec is the model-facing schema; maps to provider.ToolFunction.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema
}

// ToolPlan is the narrowed per-call effect plus an optional preview/diff.
type ToolPlan struct {
	Effect  Effect
	Preview string
}

// ToolResult is the outcome fed back as a tool-role observation.
type ToolResult struct {
	Content   string
	IsError   bool
	Preview   string
	Truncated bool
	Attrib    *RetrievalAttribution // set by retrieval-style tools; copied to Message.Attrib
	// RouteOutcome is set by tools that make their own model call (e.g.
	// delegate_code) so the run can record which model authored the result.
	// Nil for ordinary tools. Display-only fields (Preview) and the model-facing
	// observation (Content) are unaffected by it.
	RouteOutcome *provider.RouteOutcome
}

// Effect is the static, conservative upper bound for a tool.
type Effect struct {
	Class EffectClass
	// Scope is ADVISORY ONLY: the runtime does NOT enforce path/cwd limits in
	// this version. A tool must enforce its own scope. It is metadata for
	// consumers and future mutating-tool gating.
	Scope     Scope
	Timeout   time.Duration
	OutputCap int
	Approval  ApprovalPolicy
}

// Scope bounds the paths / cwd a tool may touch. Advisory only — see Effect.Scope;
// the runtime does not enforce it.
type Scope struct {
	Paths []string
	CWD   string
}

// EffectClass is an orthogonal bitset.
type EffectClass uint8

const (
	Read EffectClass = 1 << iota
	Write
	Exec
	Network
)

// Has reports whether every bit in x is set on c.
func (c EffectClass) Has(x EffectClass) bool { return c&x != 0 }

// IsMutating reports whether the class includes Write or Exec.
func (c EffectClass) IsMutating() bool { return c.Has(Write) || c.Has(Exec) }

// ApprovalPolicy decides whether a call needs consumer approval.
type ApprovalPolicy uint8

const (
	// ApprovalDefault is the safe zero value: mutating tools require approval,
	// read-only tools do not.
	ApprovalDefault ApprovalPolicy = iota
	// ApprovalNever explicitly bypasses approval.
	ApprovalNever
	ApprovalOnWrite
	ApprovalAlways
)

// Approver gates a tool call before invocation.
type Approver interface {
	Approve(ctx context.Context, call provider.ToolCall, preview string) (bool, error)
}

func needsApproval(p ApprovalPolicy, c EffectClass) bool {
	switch p {
	case ApprovalAlways:
		return true
	case ApprovalNever:
		return false
	case ApprovalOnWrite:
		return c.IsMutating()
	default:
		return c.IsMutating()
	}
}

// normalizeEffect fills zero values with safe defaults. A zero timeout/cap
// must never mean "cancel immediately" / "return nothing".
func normalizeEffect(e Effect) Effect {
	if e.Timeout <= 0 {
		e.Timeout = defaultToolTimeout
	}
	if e.OutputCap <= 0 {
		e.OutputCap = defaultOutputCap
	}
	return e
}

// toolRegistry indexes tools by name and converts specs to provider.Tool.
type toolRegistry struct {
	byName map[string]Tool
	order  []string
}

func newToolRegistry(tools []Tool) (*toolRegistry, error) {
	r := &toolRegistry{byName: make(map[string]Tool, len(tools))}
	for i, t := range tools {
		if t == nil {
			return nil, fmt.Errorf("agent: nil tool at index %d", i)
		}
		name := t.Spec().Name
		if name == "" {
			return nil, fmt.Errorf("agent: tool with empty name")
		}
		if _, dup := r.byName[name]; dup {
			return nil, fmt.Errorf("agent: duplicate tool name %q", name)
		}
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return r, nil
}

func (r *toolRegistry) lookup(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

func (r *toolRegistry) providerSpecs() []provider.Tool {
	specs := make([]provider.Tool, 0, len(r.order))
	for _, name := range r.order {
		s := r.byName[name].Spec()
		specs = append(specs, provider.Tool{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        s.Name,
				Description: s.Description,
				Parameters:  s.Parameters,
			},
		})
	}
	return specs
}
