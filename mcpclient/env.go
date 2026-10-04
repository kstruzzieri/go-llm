package mcpclient

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvVar is one host-approved environment addition for a stdio server. Build
// it with InheritEnv or SetEnv; the zero value is invalid. Values are never
// persisted, rendered or fingerprinted; they are held in memory only, behind a
// pointer so reflection-based formatting prints an address, not the value.
type EnvVar struct {
	name    string
	value   *string // non-nil exactly when set
	inherit bool
	set     bool
}

// InheritEnv forwards the parent process's value for name, read when the
// server is launched. An unset variable blocks the alias with env_unset.
func InheritEnv(name string) EnvVar { return EnvVar{name: name, inherit: true} }

// SetEnv supplies value for name. name must not be a baseline variable, so
// the PATH used to resolve the launcher is always the child's PATH.
func SetEnv(name, value string) EnvVar { return EnvVar{name: name, value: &value, set: true} }

func (v EnvVar) source() string {
	if v.inherit {
		return "inherit"
	}
	return "set"
}

// Format renders only source:name whenever fmt calls it (any verb on a value
// or pointer, including through exported fields, slices and maps), so a value
// never reaches logs that way. A name that is not a valid variable name prints
// as <invalid>, and the zero value as invalid, so a mistaken name is never
// echoed. fmt cannot call Format through an unexported field or under %p;
// those paths print the raw fields, where the value is only a pointer.
func (v EnvVar) Format(f fmt.State, _ rune) {
	switch {
	case !v.inherit && !v.set:
		_, _ = io.WriteString(f, "invalid")
	case !envNameRE.MatchString(v.name):
		_, _ = fmt.Fprintf(f, "%s:<invalid>", v.source())
	default:
		_, _ = fmt.Fprintf(f, "%s:%s", v.source(), v.name)
	}
}

// envPolicy is a platform's baseline environment and name-matching rule.
type envPolicy struct {
	id       string // part of the connection identity
	baseline []string
	foldCase bool
}

var (
	unixEnvPolicy    = envPolicy{id: "unix-v1", baseline: []string{"HOME", "LANG", "PATH", "TMPDIR", "USER"}}
	windowsEnvPolicy = envPolicy{id: "windows-v1", foldCase: true, baseline: []string{
		"APPDATA", "COMSPEC", "HOME", "LANG", "LOCALAPPDATA", "PATH", "PATHEXT",
		"SYSTEMROOT", "TEMP", "TMP", "TMPDIR", "USER", "USERPROFILE",
	}}
)

// hostEnvPolicy selects the running platform's policy.
func hostEnvPolicy() envPolicy {
	if runtime.GOOS == "windows" {
		return windowsEnvPolicy
	}
	return unixEnvPolicy
}

func (p envPolicy) key(name string) string {
	if p.foldCase {
		return strings.ToUpper(name)
	}
	return name
}

// validateEnvAdditions checks names, sources and duplicates. Errors identify
// entries by position and never echo names or values.
func validateEnvAdditions(vars []EnvVar, p envPolicy) error {
	baseline := make(map[string]bool, len(p.baseline))
	for _, name := range p.baseline {
		baseline[p.key(name)] = true
	}
	seen := make(map[string]bool, len(vars))
	for i, v := range vars {
		switch {
		case !v.inherit && !v.set:
			return fmt.Errorf("mcpclient: environment entry %d was not built with InheritEnv or SetEnv", i+1)
		case !envNameRE.MatchString(v.name):
			return fmt.Errorf("mcpclient: environment entry %d is not a variable name", i+1)
		case seen[p.key(v.name)]:
			return fmt.Errorf("mcpclient: environment entry %d repeats a name", i+1)
		case v.set && baseline[p.key(v.name)]:
			return fmt.Errorf("mcpclient: environment entry %d sets a baseline variable", i+1)
		case v.set && strings.IndexByte(*v.value, 0) >= 0:
			return fmt.Errorf("mcpclient: environment entry %d value contains NUL", i+1)
		}
		seen[p.key(v.name)] = true
	}
	return nil
}

// buildServerEnv returns the child's complete environment, one NAME=value per
// matching key in key order, and the inherited additions the parent lacks.
// The result is never nil: an empty environment is empty, never inherited.
// vars must already pass validateEnvAdditions.
func buildServerEnv(p envPolicy, vars []EnvVar, lookup func(string) (string, bool)) (env []string, unset []string) {
	entries := make(map[string]string)
	for _, name := range p.baseline {
		if value, ok := lookup(name); ok {
			entries[p.key(name)] = name + "=" + value
		}
	}
	for _, v := range vars {
		if v.set {
			entries[p.key(v.name)] = v.name + "=" + *v.value
			continue
		}
		value, ok := lookup(v.name)
		if !ok {
			unset = append(unset, v.name)
			continue
		}
		entries[p.key(v.name)] = v.name + "=" + value
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env = make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, entries[k])
	}
	return env, unset
}

// envIdentity is the sorted source:KEY list that is fingerprinted for vars.
// KEY is the name as p matches it, so on Windows Path and PATH are one
// identity, as they are one variable.
func envIdentity(vars []EnvVar, p envPolicy) []string {
	out := make([]string, len(vars))
	for i, v := range vars {
		out[i] = v.source() + ":" + p.key(v.name)
	}
	sort.Strings(out)
	return out
}

// validateLaunchPolicy checks a server's launch or destination options without
// touching the filesystem or network.
func validateLaunchPolicy(s Server, p envPolicy) error {
	switch s.kind {
	case transportStdio:
		if s.tr == nil && len(s.command) == 0 {
			return errors.New("mcpclient: stdio server has an empty command")
		}
		if s.dir != "" && !filepath.IsAbs(s.dir) {
			return errors.New("mcpclient: working directory must be absolute")
		}
		return validateEnvAdditions(s.env, p)
	case transportHTTP:
		if len(s.env) > 0 || s.dir != "" {
			return errors.New("mcpclient: environment and working directory apply only to stdio servers")
		}
		if s.tr != nil {
			return nil
		}
		_, err := canonicalEndpoint(s.endpoint)
		return err
	default:
		return errors.New("mcpclient: unknown transport")
	}
}
