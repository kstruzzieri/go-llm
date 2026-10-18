package golem

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/kstruzzieri/go-llm/agent"
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
// runtime-owned prefix followed by extras, after nil/name/duplicate validation.
func (r *Runtime) admitTools(extras []agent.Tool) ([]agent.Tool, error) {
	combined := make([]agent.Tool, 0, len(r.fileTools)+len(extras))
	combined = append(combined, r.fileTools...)
	combined = append(combined, extras...)
	if err := validateTools(combined); err != nil {
		return nil, err
	}
	return combined, nil
}
