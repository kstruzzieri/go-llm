package golem

import (
	"slices"

	"github.com/kstruzzieri/go-llm/agent"
)

// Provenance says who built a mounted tool, which is what a consumer needs to
// judge how far to trust its declared effect.
type Provenance string

const (
	// ProvenanceRuntime marks the runtime's own workspace reads.
	ProvenanceRuntime Provenance = "runtime"
	// ProvenanceLibrary marks a tool whose concrete type agent/tools builds, so
	// its Effect is the library's own. It is decided by type, never by name.
	ProvenanceLibrary Provenance = "library"
	// ProvenanceHostExtension marks any other caller-supplied tool: opaque to
	// this package, its Effect is whatever the host declared, floored.
	ProvenanceHostExtension Provenance = "host-extension"
)

// hostCommandWarning is spec section 7's required description of the host
// backend, the only command runtime this release can construct.
const hostCommandWarning = "unsandboxed; runs with the host user's filesystem and network authority"

// ToolPermission describes one mounted tool as the model is offered it.
type ToolPermission struct {
	// Name is the tool's name.
	Name string
	// Class is the tool's static effect class.
	Class agent.EffectClass
	// Approval is the static Effect's approval policy after the Trusted
	// approval floor, not the one the host declared. A PlanningTool's per-call
	// plan carries its own Effect, floored separately, so the policy the loop
	// applies to a given call may differ.
	Approval agent.ApprovalPolicy
	// Provenance says who built the tool.
	Provenance Provenance
}

// CommandPermissions describes where commands run and what network they get.
type CommandPermissions struct {
	// Runtime is "n/a" under Inspect (no commands run) and "host" otherwise.
	Runtime string
	// Network is "n/a" under Inspect and "unrestricted" on the host runtime.
	Network string
	// Warning is the required disclosure for the selected runtime, empty when
	// none applies.
	Warning string
}

// EffectivePermissions is a copied snapshot of what a Runtime currently grants,
// separating the policy ceiling from the tools mounted now.
type EffectivePermissions struct {
	// Profile is the normalized profile.
	Profile Profile
	// Root is the canonical workspace root.
	Root string
	// Ceiling is the sorted allowlist names, nil when no allowlist was given. A
	// non-nil empty Ceiling means no model tools; a name here may not be mounted.
	Ceiling []string
	// Tools lists what is mounted now, in the order the model sees it.
	Tools []ToolPermission
	// Command describes command execution.
	Command CommandPermissions
}

// EffectivePermissions returns a deep copy of the runtime's current permission
// snapshot. It reads state published at admission, so it calls no tool method
// and performs no I/O; Replace and ReplaceConfiguration publish the tools and
// this projection together. Like ModelOptions, it works after Close.
func (r *Runtime) EffectivePermissions() EffectivePermissions {
	r.mu.Lock()
	defer r.mu.Unlock()
	command := CommandPermissions{Runtime: "host", Network: "unrestricted", Warning: hostCommandWarning}
	if r.profile == ProfileInspect {
		command = CommandPermissions{Runtime: "n/a", Network: "n/a"}
	}
	return EffectivePermissions{
		Profile: r.profile,
		Root:    r.root,
		Ceiling: slices.Clone(r.ceilingNames),
		Tools:   slices.Clone(r.snap.permissions),
		Command: command,
	}
}

// sortedCeiling returns the ceiling's names in order, nil for a nil ceiling and
// a non-nil empty slice for an empty one.
func sortedCeiling(ceiling map[string]struct{}) []string {
	if ceiling == nil {
		return nil
	}
	names := make([]string, 0, len(ceiling))
	for name := range ceiling {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
