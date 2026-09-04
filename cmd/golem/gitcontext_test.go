package main

import (
	"strings"
	"testing"
)

// envValues returns every value carried for key in env, matched the way the
// process environment is consumed on every platform golem ships to:
// case-insensitively on the key.
func envValues(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, key) {
			out = append(out, v)
		}
	}
	return out
}

// hostGitLocationKeys are the repository-location overrides every host Git
// call (Agentflow's and #354's) must drop so cmd.Dir alone selects the
// repository. GIT_TERMINAL_PROMPT is listed because the helper owns its
// value: an inherited one must not survive beside the appended =0.
var hostGitLocationKeys = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_COMMON_DIR", "GIT_NAMESPACE", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX",
	"GIT_TERMINAL_PROMPT",
}

func seedEnv(t *testing.T, keys []string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "seeded-"+k)
		t.Setenv(strings.ToLower(k), "seeded-lower-"+k)
	}
	t.Setenv("GOLEM_UNRELATED_KEEP", "kept")
}

func TestHostGitEnvStripsLocationOverridesCaseInsensitively(t *testing.T) {
	seedEnv(t, hostGitLocationKeys)
	env := hostGitEnv()
	for _, k := range hostGitLocationKeys {
		if k == "GIT_TERMINAL_PROMPT" {
			continue
		}
		if got := envValues(env, k); len(got) != 0 {
			t.Fatalf("%s survived the host Git environment filter: %q", k, got)
		}
	}
	if got := envValues(env, "GIT_TERMINAL_PROMPT"); len(got) != 1 || got[0] != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT = %q, want exactly [0]", got)
	}
	if got := envValues(env, "GOLEM_UNRELATED_KEEP"); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("unrelated variable did not survive: %q", got)
	}
}

// gitContextEnv is the capture-only filter (#354 D5): on top of the host
// filter it removes config injection and discovery overrides and pins the C
// locale so the non-repository exit is classified from stable text.
func TestGitContextEnvStripsConfigAndDiscoveryOverrides(t *testing.T) {
	extra := []string{
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		"GIT_CONFIG_KEY_17", "GIT_CONFIG_VALUE_17",
		"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM", "LC_ALL",
	}
	seedEnv(t, append(append([]string(nil), hostGitLocationKeys...), extra...))
	env := gitContextEnv()
	for _, k := range append(append([]string(nil), hostGitLocationKeys...), extra...) {
		if k == "GIT_TERMINAL_PROMPT" || k == "LC_ALL" {
			continue
		}
		if got := envValues(env, k); len(got) != 0 {
			t.Fatalf("%s survived the capture environment filter: %q", k, got)
		}
	}
	if got := envValues(env, "GIT_TERMINAL_PROMPT"); len(got) != 1 || got[0] != "0" {
		t.Fatalf("GIT_TERMINAL_PROMPT = %q, want exactly [0]", got)
	}
	if got := envValues(env, "LC_ALL"); len(got) != 1 || got[0] != "C" {
		t.Fatalf("LC_ALL = %q, want exactly [C]", got)
	}
	if got := envValues(env, "GOLEM_UNRELATED_KEEP"); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("unrelated variable did not survive: %q", got)
	}
	// The capture filter is strictly a superset of the host filter: everything
	// the host filter keeps survives unless it is one of the capture-only keys
	// seeded above. The predicate is spelled out here rather than borrowed from
	// production so the test cannot agree with a broken implementation.
	captureOnly := func(k string) bool {
		up := strings.ToUpper(k)
		if strings.HasPrefix(up, "GIT_CONFIG_KEY_") || strings.HasPrefix(up, "GIT_CONFIG_VALUE_") {
			return true
		}
		for _, x := range extra {
			if strings.EqualFold(k, x) {
				return true
			}
		}
		return false
	}
	for _, kv := range hostGitEnv() {
		k, _, _ := strings.Cut(kv, "=")
		if !captureOnly(k) && len(envValues(env, k)) == 0 {
			t.Fatalf("capture filter dropped %q, which the host filter keeps", k)
		}
	}
}
