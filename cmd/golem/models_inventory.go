package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/provider"
)

// golemViewRequirements declares the CLI's call shapes for configview
// eligibility. The agent path always registers tools, so tool_call is part
// of its shape (agent/model_caller.go adds CapToolCall whenever tools ride
// the request). AgentFlow plan authoring passes plan tools unconditionally,
// so planning carries the same tool-bearing shape (#476 D6).
//
// This is Golem's CONSUMER declaration, not a library floor table:
// configview.BuildInput.Requirements is consumer-declared by contract, and
// MCP declares its own operation-specific shapes. A declared requirement is
// also not a binding -- configview.Build projects authored defaults only, so
// a config that never writes defaults.planning shows no planning row.
func golemViewRequirements() map[string]provider.Capability {
	return map[string]provider.Capability{
		"agent":      agent.ModelCallCapabilities(true),
		"planning":   agent.ModelCallCapabilities(true),
		"chat":       agent.ModelCallCapabilities(false),
		"analysis":   agent.ModelCallCapabilities(false),
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
// Inventory. Explicit-command context only (network). Every provider/list
// error is propagated — an empty inventory must mean "nothing found", never
// "something failed".
// bind attaches the model-refresh destination capability per provider when a
// gate is armed (#477 I14: the inventory sweep is repository-owned metadata
// traffic through guarded clients); nil keeps the ungated context.
func buildInventoryFromRegistry(ctx context.Context, providers *provider.Registry, models *provider.ModelRegistry, bind func(context.Context, string) (context.Context, error)) (configview.Inventory, error) {
	inv := configview.Inventory{}
	for _, name := range providers.Names() {
		p, ok := providers.Get(name)
		if !ok {
			return configview.Inventory{}, fmt.Errorf("list models for -json inventory: provider %q disappeared", name)
		}
		pctx := ctx
		if bind != nil {
			var err error
			pctx, err = bind(ctx, name)
			if err != nil {
				return configview.Inventory{}, fmt.Errorf("list models for -json inventory from provider %q: %w", name, err)
			}
		}
		infos, err := p.Models(pctx)
		if err != nil {
			return configview.Inventory{}, fmt.Errorf("list models for -json inventory from provider %q: %w", name, err)
		}
		for _, info := range infos {
			profile, err := models.Lookup(ctx, provider.ModelKey{Provider: name, Model: info.Name})
			if err != nil {
				return configview.Inventory{}, fmt.Errorf("load model %q from provider %q for -json inventory: %w", info.Name, name, err)
			}
			inv.Models = append(inv.Models, configview.InventoryModel{
				Key:           profile.Key,
				Family:        profile.Family,
				Caps:          profile.Caps,
				KnownMask:     profile.Caps, // merged registry knowledge; present-only mask
				ProfileSource: profile.Source.String(),
				ContextWindow: profile.ContextWindow,
			})
		}
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
