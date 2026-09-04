package main

// gitContextBlockedKeys and gitContextBlockedPrefixes are the additional
// environment entries the read-only Git capture (#354 D5) drops on top of
// hostGitEnv: config injection (GIT_CONFIG_PARAMETERS, the GIT_CONFIG_COUNT /
// GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n family) and repository-discovery
// overrides. LC_ALL is dropped so the appended C locale is the only one, which
// makes the "not a git repository" exit classifiable from stable text (gettext
// ignores LANGUAGE under the C locale, so it needs no scrub). The filter
// deliberately keeps GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM, and GIT_CONFIG_NOSYSTEM:
// those are the user's own trust roots and carry safe.directory, so scrubbing
// them would turn a legitimate dotfiles setup into a dubious-ownership failure.
// Trusted user configuration and dubious-ownership protection keep working.
var (
	gitContextBlockedKeys = []string{
		"GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
		"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
		"LC_ALL",
	}
	gitContextBlockedPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}
)

// gitContextEnv is the environment for the read-only Git snapshot capture: the
// shared host filter plus the capture-specific scrub above, with LC_ALL=C
// appended exactly once. It is not used by Agentflow's Git calls, whose
// behavior hostGitEnv preserves unchanged.
func gitContextEnv() []string {
	return append(dropEnvKeys(hostGitEnv(), gitContextBlockedKeys, gitContextBlockedPrefixes), "LC_ALL=C")
}
