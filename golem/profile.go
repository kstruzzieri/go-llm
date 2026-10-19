package golem

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// ErrProfileUnavailable reports a profile this release cannot construct.
var ErrProfileUnavailable = errors.New("golem: profile unavailable")

// ErrProfileIncompatible reports a tool or allowlist entry the selected
// profile cannot admit.
var ErrProfileIncompatible = errors.New("golem: tool incompatible with profile")

// Profile selects the authority a Runtime grants its model. The Go zero value
// is ProfileInspect, so Options{} is the narrowest profile; an unknown value is
// an error, never an alias for ProfileTrusted.
type Profile uint8

const (
	// ProfileInspect mounts only the four runtime-owned workspace reads
	// (read_file, search, glob, list). Options.Tools must be empty.
	ProfileInspect Profile = iota
	// ProfilePreview is reserved for the staged-workspace runtime (#388/#389)
	// and is rejected at construction in this release.
	ProfilePreview
	// ProfileTrusted additionally admits explicitly supplied tools, each still
	// subject to approval.
	ProfileTrusted
)

const previewUnavailable = "Preview unavailable: staged-workspace runtime requires #388/#389"

// validateSandbox is agenttools.ValidateSandbox, replaceable only by this
// package's tests so a native policy constructs on every OS.
var validateSandbox = agenttools.ValidateSandbox

// nativeRuntime reports whether cfg selects a sandbox runtime other than host.
func nativeRuntime(cfg agenttools.SandboxConfig) bool {
	return cfg.Runtime != "" && cfg.Runtime != agenttools.SandboxRuntimeHost
}

// inspectReads is the Inspect universe: the runtime-owned read tools, in the
// order agenttools.NewFileToolsForWorkspace builds them.
var inspectReads = [...]string{"read_file", "search", "glob", "list"}

// String returns the canonical lowercase profile name, or "profile(N)" for an
// unknown value.
func (p Profile) String() string {
	switch p {
	case ProfileInspect:
		return "inspect"
	case ProfilePreview:
		return "preview"
	case ProfileTrusted:
		return "trusted"
	}
	return fmt.Sprintf("profile(%d)", uint8(p))
}

// ParseProfile maps an exact lowercase profile name back to its Profile. Any
// other text, including a differently cased name or "", wraps ErrInvalidRequest.
func ParseProfile(s string) (Profile, error) {
	for _, p := range [...]Profile{ProfileInspect, ProfilePreview, ProfileTrusted} {
		if p.String() == s {
			return p, nil
		}
	}
	return 0, fmt.Errorf("%w: unknown profile %q", ErrInvalidRequest, s)
}

// checkProfile rejects an unknown or unavailable profile, and caller tools under
// Inspect, before any tool method runs or any workspace is built.
func checkProfile(p Profile, callerTools []agent.Tool) error {
	switch p {
	case ProfileInspect:
		if len(callerTools) != 0 {
			return fmt.Errorf("%w: inspect mounts only its own read tools, got %d caller tool(s)", ErrProfileIncompatible, len(callerTools))
		}
		return nil
	case ProfileTrusted:
		return nil
	case ProfilePreview:
		return fmt.Errorf("%w: %s", ErrProfileUnavailable, previewUnavailable)
	}
	return fmt.Errorf("%w: unknown profile %s", ErrInvalidRequest, p)
}

// newCeiling validates Options.ToolAllowlist against the profile's universe and
// returns a defensive copy as the frozen ceiling. A nil allowlist yields a nil
// ceiling (the whole universe); a non-nil empty one yields an empty ceiling.
func newCeiling(p Profile, allowlist []string) (map[string]struct{}, error) {
	if allowlist == nil {
		return nil, nil
	}
	ceiling := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		if p == ProfileInspect {
			if !slices.Contains(inspectReads[:], name) {
				return nil, fmt.Errorf("%w: inspect allowlist names %q, which is not one of %s", ErrProfileIncompatible, name, strings.Join(inspectReads[:], ", "))
			}
		} else if name == "" || strings.ContainsFunc(name, unicode.IsControl) {
			return nil, fmt.Errorf("%w: allowlist entry %q is not a valid tool name", ErrInvalidRequest, name)
		}
		ceiling[name] = struct{}{}
	}
	return ceiling, nil
}

// filterOwned keeps the owned tools the ceiling names; a nil ceiling keeps all.
func filterOwned(owned []agent.Tool, ceiling map[string]struct{}) []agent.Tool {
	if ceiling == nil {
		return owned
	}
	return slices.DeleteFunc(owned, func(tool agent.Tool) bool {
		_, ok := ceiling[tool.Spec().Name]
		return !ok
	})
}

// admitTools returns the complete tool list a snapshot may publish: the
// runtime-owned prefix followed by extras. New, Replace and ReplaceConfiguration
// all admit through here, before any tool beyond Spec is touched, so a rejected
// extra is never planned or invoked. It enforces, in order: the profile, the
// nil/name/duplicate checks, and per extra the frozen allowlist ceiling,
// dispatch's need for all four reads and for every child it carries, and
// dispatch's incompatibility with a ScopeGuard, and in a native session the
// sandbox bindings (agenttools.CheckSandboxBindings). Only then are the admitted
// extras published, a Trusted host's through floorWrap, so every check sees the
// caller's own tool and the model sees the approval floor. A native session
// publishes its extras unwrapped: every one is a bound library tool.
//
// The third result is the scratch identity the native extras carry (always ""
// on host); New freezes it and admitReplacement enforces it.
//
// It also returns the ToolPermission projection of the published list, in the
// same order, so a snapshot can carry the two together. Provenance is decided on
// the caller's raw tool, before wrapping; Class and Approval come from the
// published tool's Effect, so Approval is the approval after the floor. This is
// the only place EffectivePermissions' data is computed: it runs only after every
// rejection, so a rejected tool never has Effect called.
func (r *Runtime) admitTools(extras []agent.Tool) ([]agent.Tool, []ToolPermission, string, error) {
	if err := checkProfile(r.profile, extras); err != nil {
		return nil, nil, "", err
	}
	combined := make([]agent.Tool, 0, len(r.fileTools)+len(extras))
	combined = append(combined, r.fileTools...)
	combined = append(combined, extras...)
	names, err := validateTools(combined)
	if err != nil {
		return nil, nil, "", err
	}
	for i, tool := range extras {
		if err := r.checkExtra(tool, names[len(r.fileTools)+i]); err != nil {
			return nil, nil, "", err
		}
	}
	native := nativeRuntime(r.sandbox)
	var scratchKey string
	if native {
		if scratchKey, err = agenttools.CheckSandboxBindings(extras, r.sandbox, r.root, r.scratchKey); err != nil {
			return nil, nil, "", fmt.Errorf("%w: %w", ErrProfileIncompatible, err)
		}
	}
	perms := make([]ToolPermission, len(combined))
	for i := range combined {
		provenance := ProvenanceRuntime
		if i >= len(r.fileTools) {
			provenance = ProvenanceHostExtension
			if libraryTool(combined[i]) {
				provenance = ProvenanceLibrary
			}
			if !native {
				combined[i] = floorWrap(combined[i])
			}
		}
		effect := combined[i].Effect()
		perms[i] = ToolPermission{Name: names[i], Class: effect.Class, Approval: effect.Approval, Provenance: provenance}
	}
	return combined, perms, scratchKey, nil
}

// admitReplacement is admitTools for Replace and ReplaceConfiguration. A native
// session's scratch identity is frozen at New, so a replacement whose command
// tools carry another one, or any one when New mounted none, is rejected. That
// check follows admitTools' Effect reads, which in a native session only ever
// reach bound library tools.
func (r *Runtime) admitReplacement(extras []agent.Tool) ([]agent.Tool, []ToolPermission, error) {
	tools, perms, scratchKey, err := r.admitTools(extras)
	if err == nil && scratchKey != r.scratchKey {
		return nil, nil, fmt.Errorf("%w: a native session's scratch policy is frozen at construction", ErrProfileIncompatible)
	}
	return tools, perms, err
}

// checkVerifier enforces spec 3.3's verifier binding for a native session: the
// orchestrator's verifier must be nil or a VerifierSlot bound to the session's
// root and frozen policy. A Trusted host session, and a nil orchestrator (one
// New is about to bootstrap, which installs no verifier), pass unchecked.
func (r *Runtime) checkVerifier(orch *agent.Orchestrator) error {
	if !nativeRuntime(r.sandbox) || orch == nil {
		return nil
	}
	switch v := orch.Verifier().(type) {
	case nil:
		return nil
	case *agenttools.VerifierSlot:
		if v.BoundTo(r.root, r.sandbox) {
			return nil
		}
	}
	return fmt.Errorf("%w: the verifier is not a verifier slot bound to this session's root and %s policy", ErrProfileIncompatible, r.sandbox.Runtime)
}

// checkExtra applies the ceiling rules to one caller tool named name.
func (r *Runtime) checkExtra(tool agent.Tool, name string) error {
	if r.ceiling != nil {
		if _, ok := r.ceiling[name]; !ok {
			return fmt.Errorf("%w: tool %q is outside the frozen allowlist", ErrProfileIncompatible, name)
		}
		// dispatch builds its own child readers, which the allowlist cannot
		// narrow, so the allowlist must already admit every read.
		if name == agenttools.DispatchToolName {
			for _, read := range inspectReads {
				if _, ok := r.ceiling[read]; !ok {
					return fmt.Errorf("%w: allowlist admits %s without %q, but its child readers are not bound by the allowlist", ErrProfileIncompatible, name, read)
				}
			}
		}
		// The library dispatch also hands unscoped children an optional
		// retrieve, so every child it carries must lie inside the ceiling.
		if d, ok := tool.(*agenttools.Dispatch); ok {
			for _, child := range d.ChildToolNames() {
				if _, ok := r.ceiling[child]; !ok {
					return fmt.Errorf("%w: %s child tool %q is outside the frozen allowlist", ErrProfileIncompatible, name, child)
				}
			}
		}
	}
	if _, ok := tool.(*agenttools.Dispatch); ok && r.guarded {
		return fmt.Errorf("%w: %s cannot run beside a ScopeGuard, its child readers do not share the guarded workspace", ErrProfileIncompatible, agenttools.DispatchToolName)
	}
	return nil
}
