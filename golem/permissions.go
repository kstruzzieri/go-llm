package golem

import (
	"slices"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
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
// backend.
const hostCommandWarning = "unsandboxed; runs with the host user's filesystem and network authority"

// Spec section 7's native backend descriptions. commandPermissions builds each
// runtime and network value's list whole, never by appending to another's.
const (
	limitSeatbeltProfile  = "deny-default profile with reviewed system reads, allowed workspace and private temp"
	limitNetworkDenied    = "network operations denied"
	limitNoQuota          = "no memory/CPU quota"
	limitNetworkAllowed   = "networking explicitly allowed by policy; filesystem limits remain"
	limitBwrapIPDenied    = "IP network namespace isolated"
	limitBwrapUnixSockets = "pathname Unix sockets reachable inside the shared workspace remain a channel"
	limitBwrapMounts      = "reviewed read-only system mounts, workspace/snapshot and private temp"
	limitBwrapCaps        = "all capabilities dropped by the backend"
	limitBwrapMemory      = "RLIMIT_AS per-process virtual address space plus separate private tmpfs quotas; not aggregate process-tree memory"
)

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
	// Runtime is "n/a" under Inspect (no commands run), otherwise the frozen
	// sandbox runtime: "host", "seatbelt" or "bwrap".
	Runtime string
	// Network is "n/a" under Inspect, "unrestricted" on host, and "denied"
	// (seatbelt), "ip-denied" (bwrap) or "allowed" on a native runtime.
	Network string
	// Warning is the required disclosure for the selected runtime, empty when
	// none applies.
	Warning string
	// Limits are the native backend's material limits, in display order; nil
	// for Inspect and host.
	Limits []string
}

// Verification describes post-write verification as the runtime would run it
// now.
type Verification struct {
	// State is "none" when no verifier is installed or the profile has no
	// post-write verification (Inspect); "absent", "disabled" or "armed" for an
	// agenttools.VerifierSlot, as its State reports; and "host-verifier" for
	// any other verifier, which only a Trusted host session runs (a native one
	// fails every turn while it is installed).
	State string
	// Target is the canonical workspace a slot's verification checks; empty
	// for "none" and "host-verifier".
	Target string
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
	// Scratch reports whether the mounted command tools run in a scratch
	// snapshot: at least one library run_command or start_command is mounted
	// and every one of them carries a scratch runtime
	// (agenttools.CommandsRunInScratch).
	Scratch bool
	// Verification describes post-write verification.
	Verification Verification
}

// EffectivePermissions returns a deep copy of the runtime's current permission
// snapshot. Tools and command policy are the state published at admission
// (Replace and ReplaceConfiguration publish the tools and this projection
// together); Verification is the installed VerifierSlot's live state, which
// /allow-write can change without a replacement. It calls no tool method and
// performs no I/O. Like ModelOptions, it works after Close.
func (r *Runtime) EffectivePermissions() EffectivePermissions {
	r.mu.Lock()
	defer r.mu.Unlock()
	return EffectivePermissions{
		Profile:      r.profile,
		Root:         r.root,
		Ceiling:      slices.Clone(r.ceilingNames),
		Tools:        slices.Clone(r.snap.permissions),
		Command:      commandPermissions(r.profile, r.sandbox),
		Scratch:      agenttools.CommandsRunInScratch(unfloored(r.snap.tools)),
		Verification: verificationOf(r.profile, r.snap.orchestrator),
	}
}

// commandPermissions projects the frozen sandbox policy (spec section 7).
func commandPermissions(p Profile, s agenttools.SandboxConfig) CommandPermissions {
	if p == ProfileInspect {
		return CommandPermissions{Runtime: "n/a", Network: "n/a"}
	}
	switch s.Runtime {
	case agenttools.SandboxRuntimeSeatbelt:
		if s.AllowNetwork {
			return CommandPermissions{Runtime: "seatbelt", Network: "allowed", Limits: []string{limitSeatbeltProfile, limitNetworkAllowed, limitNoQuota}}
		}
		return CommandPermissions{Runtime: "seatbelt", Network: "denied", Limits: []string{limitSeatbeltProfile, limitNetworkDenied, limitNoQuota}}
	case agenttools.SandboxRuntimeBwrap:
		c := CommandPermissions{Runtime: "bwrap", Network: "ip-denied", Limits: []string{limitBwrapIPDenied, limitBwrapUnixSockets, limitBwrapMounts, limitBwrapCaps}}
		if s.AllowNetwork {
			c = CommandPermissions{Runtime: "bwrap", Network: "allowed", Limits: []string{limitNetworkAllowed, limitBwrapMounts, limitBwrapCaps}}
		}
		if s.MemoryCapMB > 0 {
			c.Limits = append(c.Limits, limitBwrapMemory)
		}
		return c
	}
	return CommandPermissions{Runtime: "host", Network: "unrestricted", Warning: hostCommandWarning}
}

// verificationOf reports the verifier the orchestrator would run. It reads
// only the slot's own state: no I/O, no tool method.
func verificationOf(p Profile, orch *agent.Orchestrator) Verification {
	if p == ProfileInspect {
		return Verification{State: "none"}
	}
	switch v := orch.Verifier().(type) {
	case nil:
		return Verification{State: "none"}
	case *agenttools.VerifierSlot:
		state, target := v.State()
		return Verification{State: state, Target: target}
	default:
		return Verification{State: "host-verifier"}
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
