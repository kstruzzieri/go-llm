package opsbackend

import "strings"

// NormalizeOllama reduces a model name to Ollama's own shortest display form
// (DisplayShortest) so configured names and /api/ps names compare. Read as
// [host/][namespace/]model[:tag]: the host is dropped only when it is
// registry.ollama.ai, the namespace only when the host is that default and
// the namespace is library, a missing tag becomes ":latest", and comparison
// is case-insensitive. An empty or blank name normalizes to "".
func NormalizeOllama(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return "" // never equals a nonblank normalized name
	}
	// A tag follows the last ':' only when no '/' comes after it, so a
	// registry port is never read as a tag.
	if strings.LastIndex(n, ":") <= strings.LastIndex(n, "/") {
		n += ":latest"
	}
	if p := strings.Split(n, "/"); len(p) == 3 && p[0] == "registry.ollama.ai" {
		n = p[1] + "/" + p[2]
	}
	// Two parts are namespace/model under the default host.
	if p := strings.Split(n, "/"); len(p) == 2 && p[0] == "library" {
		n = p[1]
	}
	return n
}

// Residency words: the console vocabulary transitions are recorded in. opsview
// aliases them, so each word has one definition.
const (
	ResidencyLoading   = "loading"
	ResidencyLoaded    = "loaded"
	ResidencyUnloading = "unloading"
	ResidencyUnloaded  = "unloaded"
	ResidencyUnknown   = "unknown" // Ollama absence: not proof of unload
)

// ResidencyOf maps a llama-swap process state to the console vocabulary.
// stopped and shutdown never appear in /running in v235; they read as
// unloaded if they ever do.
func ResidencyOf(raw string) string {
	switch raw {
	case "starting":
		return ResidencyLoading
	case "ready":
		return ResidencyLoaded
	case "stopping":
		return ResidencyUnloading
	}
	return ResidencyUnloaded
}
