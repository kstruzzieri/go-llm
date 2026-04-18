package main

import (
	"fmt"
	"strings"
)

const defaultBenchProvider = "ollama"

// ModelTarget is a benchmark candidate model selector. Display preserves the
// user-facing identifier while Model is the provider-native model name.
type ModelTarget struct {
	Display  string
	Provider string
	Model    string
}

func parseModelTargets(csv string) ([]ModelTarget, error) {
	parts := strings.Split(csv, ",")
	targets := make([]ModelTarget, 0, len(parts))
	for _, part := range parts {
		target, err := parseModelTarget(part)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, nil
}

func parseModelTarget(raw string) (ModelTarget, error) {
	selector := strings.TrimSpace(raw)
	if selector == "" {
		return ModelTarget{}, fmt.Errorf("empty model selector")
	}

	target := ModelTarget{
		Display:  selector,
		Provider: defaultBenchProvider,
		Model:    selector,
	}

	if strings.Contains(selector, "/") {
		parts := strings.SplitN(selector, "/", 2)
		target.Provider = strings.TrimSpace(parts[0])
		target.Model = strings.TrimSpace(parts[1])
		if target.Provider == "" || target.Model == "" {
			return ModelTarget{}, fmt.Errorf("invalid model selector %q", selector)
		}
	}

	if target.Provider != defaultBenchProvider {
		return ModelTarget{}, fmt.Errorf("%w: %q", errUnsupportedProv, target.Provider)
	}

	return target, nil
}
