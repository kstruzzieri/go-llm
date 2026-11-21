package tools

// Declared toolchain policy (#722): the closed tables behind SandboxConfig.Env
// and TempEnv, and the pure checks on the declared roots. Nothing here touches
// the filesystem.

import (
	"fmt"
	"slices"
	"unicode/utf8"
)

// sandboxEnvFixed is SandboxConfig.Env's closed table: each name with its one
// accepted value. GOMODCACHE, the remaining row, takes a path inside a
// declared root instead (validateToolchainFields). Adding a row needs its own
// review.
var sandboxEnvFixed = map[string]string{
	"GOPROXY":     "off",
	"GOTOOLCHAIN": "local",
	"GOFLAGS":     "-buildvcs=false",
}

// sandboxTempEnv is SandboxConfig.TempEnv's closed table: each name with its
// own directory under the command's private temp.
var sandboxTempEnv = map[string]string{
	"GOCACHE": "gocache",
}

// developerDirectoryEnv is added whenever DeveloperDirectory is set: git
// reads no system or global config and python writes no bytecode beside its
// sources.
var developerDirectoryEnv = []string{
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_CONFIG_NOSYSTEM=1",
	"PYTHONDONTWRITEBYTECODE=1",
}

// toolchainPath reports whether p is a clean absolute POSIX path of valid
// UTF-8 with no control characters: the spelling every declared root and
// GOMODCACHE must have.
func toolchainPath(p string) bool {
	return posixCleanAbs(p) && utf8.ValidString(p) && !containsControl(p)
}

// validateToolchainFields checks c's #722 fields without I/O: roots and the
// developer directory are clean absolute paths, Env and TempEnv come from
// their tables with no name repeated, and GOMODCACHE lies in a declared root
// (the developer directory counts). Errors name the field and index, never
// the refused value.
func validateToolchainFields(c SandboxConfig) error {
	for i, root := range c.ReadOnlyRoots {
		if !toolchainPath(root) {
			return fmt.Errorf("tools: sandbox ReadOnlyRoots[%d] must be a clean absolute path of valid UTF-8 with no control characters", i)
		}
	}
	if c.DeveloperDirectory != "" && !toolchainPath(c.DeveloperDirectory) {
		return fmt.Errorf("tools: sandbox DeveloperDirectory must be a clean absolute path of valid UTF-8 with no control characters")
	}
	seen := make(map[string]bool, len(c.Env))
	for i, e := range c.Env {
		if e.Name == "GOMODCACHE" {
			inRoot := slices.ContainsFunc(c.ReadOnlyRoots, func(r string) bool { return pathCovers(r, e.Value) }) ||
				(c.DeveloperDirectory != "" && pathCovers(c.DeveloperDirectory, e.Value))
			if !toolchainPath(e.Value) || !inRoot {
				return fmt.Errorf("tools: sandbox Env[%d] GOMODCACHE must be a clean absolute path inside a declared root", i)
			}
		} else if want, ok := sandboxEnvFixed[e.Name]; !ok {
			return fmt.Errorf("tools: sandbox Env[%d] names a variable outside the allowed table", i)
		} else if e.Value != want {
			return fmt.Errorf("tools: sandbox Env[%d] %s accepts only %q", i, e.Name, want)
		}
		if seen[e.Name] {
			return fmt.Errorf("tools: sandbox Env[%d] repeats %s", i, e.Name)
		}
		seen[e.Name] = true
	}
	seen = make(map[string]bool, len(c.TempEnv))
	for i, name := range c.TempEnv {
		if _, ok := sandboxTempEnv[name]; !ok {
			return fmt.Errorf("tools: sandbox TempEnv[%d] names a variable outside the allowed table", i)
		}
		if seen[name] {
			return fmt.Errorf("tools: sandbox TempEnv[%d] repeats %s", i, name)
		}
		seen[name] = true
	}
	return nil
}
