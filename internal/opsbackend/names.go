package opsbackend

import "strings"

// NormalizeOllama applies Ollama's own name defaults so configured names and
// /api/ps names compare: the default registry and library namespace are
// stripped, a missing tag becomes ":latest", and comparison is
// case-insensitive.
func NormalizeOllama(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimPrefix(n, "registry.ollama.ai/")
	n = strings.TrimPrefix(n, "library/")
	last := n[strings.LastIndex(n, "/")+1:]
	if !strings.Contains(last, ":") {
		n += ":latest"
	}
	return n
}

// ResidencyOf maps a llama-swap process state to the console vocabulary.
// stopped and shutdown never appear in /running in v235; they read as
// unloaded if they ever do.
func ResidencyOf(raw string) string {
	switch raw {
	case "starting":
		return "loading"
	case "ready":
		return "loaded"
	case "stopping":
		return "unloading"
	}
	return "unloaded"
}
