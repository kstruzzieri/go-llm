package agentflow

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// agentflowBaselineEnv names the parent variables every Agentflow child
// receives when they are set. It is Agentflow-owned on purpose: the exec
// tool's allowlist is bound to bwrap argv policy and changes for other reasons.
var agentflowBaselineEnv = []string{"PATH", "HOME", "USER", "TMPDIR", "LANG"}

// windowsBaselineEnv extends the baseline on Windows. Forwarded only when set;
// Windows runtime behavior is built but not exercised in CI. LOCALAPPDATA and
// APPDATA play HOME's role there: Go reads its build cache and saved settings
// from them, and nothing else substitutes.
var windowsBaselineEnv = []string{"SYSTEMROOT", "TEMP", "TMP", "PATHEXT", "USERPROFILE", "COMSPEC", "LOCALAPPDATA", "APPDATA"}

// strictEnvName is Agentflow's strict-mode switch. Upstream enables strict mode
// only for the exact value "1", so only that value is forwarded, as a constant.
const strictEnvName = "AGENTFLOW_STRICT"

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// childEnvPolicy is everything that decides an Agentflow child's environment.
type childEnvPolicy struct {
	baseline []string // forwarded when set in the parent
	approved []string // host-approved names; each must be set at launch
	owned    []string // runner-controlled NAME=VALUE entries; always win
	foldCase bool     // Windows: names compare case-insensitively
}

// childEnvPolicyFor is the policy on goos (runtime.GOOS at launch). It takes
// the platform as input so the Windows policy is testable on every host.
func childEnvPolicyFor(goos string, approved, owned []string) childEnvPolicy {
	p := childEnvPolicy{baseline: agentflowBaselineEnv, approved: approved, owned: owned}
	if goos == "windows" {
		p.baseline = append(slices.Clone(agentflowBaselineEnv), windowsBaselineEnv...)
		p.foldCase = true
	}
	return p
}

// EnvNotSetError reports an approved environment variable that is unset when a
// child is launched. It carries only the validated name, never a value.
type EnvNotSetError struct{ Name string }

func (e *EnvNotSetError) Error() string {
	return fmt.Sprintf("agentflow: approved environment variable %q is not set", e.Name)
}

// buildChildEnv returns a child's complete environment: never nil, one entry
// per name, sorted by name. Layers apply in order and later layers win:
// baseline, approved names, strict mode, runner-owned entries. Nothing else
// from the parent is forwarded. Errors name a variable, never a value.
func buildChildEnv(p childEnvPolicy, lookup func(string) (string, bool)) ([]string, error) {
	key := func(name string) string {
		if p.foldCase {
			return strings.ToUpper(name)
		}
		return name
	}
	entries := make(map[string]string)
	for _, name := range p.baseline {
		if value, ok := lookup(name); ok {
			entries[key(name)] = name + "=" + value
		}
	}
	for _, name := range p.approved {
		value, ok := lookup(name)
		if !ok {
			return nil, &EnvNotSetError{Name: name}
		}
		entries[key(name)] = name + "=" + value
	}
	if value, ok := lookup(strictEnvName); ok && value == "1" {
		entries[key(strictEnvName)] = strictEnvName + "=1"
	}
	for _, kv := range p.owned {
		name, _, _ := strings.Cut(kv, "=")
		entries[key(name)] = kv
	}
	env := make([]string, 0, len(entries))
	for _, k := range slices.Sorted(maps.Keys(entries)) {
		env = append(env, entries[k])
	}
	return env, nil
}

// ValidateEnvNames reports whether every name may be approved for Agentflow
// children. Names must be plain variable names; runner-owned Python settings
// and Agentflow control variables (AGENTFLOW_*) are reserved, compared without
// regard to case. Errors identify an entry by position and never echo it, so a
// mistyped NAME=VALUE cannot leak its value.
func ValidateEnvNames(names []string) error {
	for i, name := range names {
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("agentflow: environment name #%d is not a variable name (names only; values are read from the environment)", i+1)
		}
		upper := strings.ToUpper(name)
		switch {
		case upper == "PYTHONPATH" || upper == "PYTHONDONTWRITEBYTECODE":
			return fmt.Errorf("agentflow: environment name #%d is runner-owned", i+1)
		case strings.HasPrefix(upper, "AGENTFLOW_"):
			return fmt.Errorf("agentflow: environment name #%d is an Agentflow control variable", i+1)
		}
	}
	return nil
}
