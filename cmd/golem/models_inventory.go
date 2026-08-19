package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/provider"
)

// golemViewRequirements declares the CLI's call shapes for configview
// eligibility. The agent path always registers tools, so tool_call is part
// of its shape (agent/model_caller.go adds CapToolCall whenever tools ride
// the request).
func golemViewRequirements() map[string]provider.Capability {
	return map[string]provider.Capability{
		"agent":      provider.CapChat | provider.CapStream | provider.CapToolCall,
		"chat":       provider.CapChat | provider.CapStream,
		"analysis":   provider.CapChat | provider.CapStream,
		"embedding":  provider.CapEmbed,
		"completion": provider.CapGenerate | provider.CapInsert,
	}
}

// renderModelsJSON emits the configview snapshot for -json.
func renderModelsJSON(w io.Writer, in configview.BuildInput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(configview.Build(in))
}

// buildInventoryFromRegistry adapts registry profiles into a configview
// Inventory. Explicit-command context only (network). A registry error is
// PROPAGATED — an empty inventory must mean "nothing found", never
// "something failed".
func buildInventoryFromRegistry(ctx context.Context, models *provider.ModelRegistry) (configview.Inventory, error) {
	profiles, err := models.All(ctx)
	if err != nil {
		return configview.Inventory{}, fmt.Errorf("list models for -json inventory: %w", err)
	}
	inv := configview.Inventory{}
	for _, p := range profiles {
		inv.Models = append(inv.Models, configview.InventoryModel{
			Key:           p.Key,
			Family:        p.Family,
			Caps:          p.Caps,
			KnownMask:     p.Caps, // merged registry knowledge; present-only mask
			ProfileSource: p.Source.String(),
			ContextWindow: p.ContextWindow,
		})
	}
	return inv, nil
}

// loadDocumentFor mirrors loadConfig's path handling for the Document form,
// including its configless mode: auto-discovery finding nothing returns
// (nil, nil), same as loadConfig, so -json projects an honest not-ready
// snapshot instead of hard-erroring where the text path would proceed.
func loadDocumentFor(configPath string) (*config.Document, error) {
	if configPath != "" {
		return config.LoadDocument(configPath)
	}
	doc, err := config.DefaultDocument()
	if err != nil {
		if errors.Is(err, config.ErrConfigNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return doc, nil
}

// modelsJSONInput assembles the BuildInput for -json. A nil doc (configless
// mode) projects as a not-ready snapshot with a config_missing diagnostic.
func modelsJSONInput(doc *config.Document, inv configview.Inventory) configview.BuildInput {
	in := configview.BuildInput{
		Inventory:    inv,
		Requirements: golemViewRequirements(),
	}
	if doc != nil {
		in.Doc = configview.DocSnapshot{
			Config:   doc.Config(),
			Origin:   doc.Origin(),
			Revision: doc.Revision(),
		}
	} else {
		in.Doc = configview.DocSnapshot{Origin: config.Origin{Source: config.OriginProgrammatic}}
	}
	return in
}
