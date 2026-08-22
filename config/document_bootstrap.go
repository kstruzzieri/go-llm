package config

import "fmt"

// ModelSpec is the provider-free model description for bootstrap: the
// constructor owns the provider linkage. No Fallbacks — a single-model
// document has no valid fallback target.
type ModelSpec struct {
	Name          string
	Description   string
	Type          string // dense | moe | embedding
	Parameters    string
	ContextWindow int
	Dimensions    int
	Capabilities  []string
	Options       *SamplingOptions
	Slots         int
	ThinkMode     string
	ThinkTags     *ThinkTagsConfig
}

// BootstrapSpec describes an initially valid single-provider, single-model
// document with a guaranteed agent route. ProviderSpec has no api_key
// field; creation flows follow with SetProviderAPIKey as a second draft
// mutation.
type BootstrapSpec struct {
	ProviderName string
	Provider     ProviderSpec
	Role         string
	Model        ModelSpec
}

// NewDocument creates one generic-schema-valid provider/model document with
// an agent route (spec §4, #410 criterion 10). "Initially valid" means
// go-llm schema-valid — consumer capability floors (e.g. Firn's
// chat|stream|tool_call agent requirement) remain host-owned. No
// filesystem I/O; pair with SaveNew for managed-file creation.
func NewDocument(spec BootstrapSpec, opts DocumentOptions) (*Document, error) {
	if spec.ProviderName == "" || spec.Role == "" {
		return nil, diagWrap(CodeInvalidArgument, SubjectNone, "",
			fmt.Errorf("config: new document: provider name and role are required"))
	}
	m := spec.Model
	authored := &Config{
		Providers: map[string]ProviderConfig{
			spec.ProviderName: {
				BaseURL: spec.Provider.BaseURL, Timeout: spec.Provider.Timeout,
				APIFormat:     spec.Provider.APIFormat,
				SlotDiscovery: spec.Provider.SlotDiscovery,
			},
		},
		Models: map[string]ModelConfig{
			spec.Role: {
				Name: m.Name, Provider: spec.ProviderName,
				Description: m.Description, Type: m.Type, Parameters: m.Parameters,
				ContextWindow: m.ContextWindow, Dimensions: m.Dimensions,
				Capabilities: m.Capabilities, Options: m.Options, Slots: m.Slots,
				ThinkMode: m.ThinkMode, ThinkTags: m.ThinkTags,
			},
		},
		Defaults: map[string]string{"agent": spec.Role},
	}
	// Render→reparse gives ParseDocument-owned copies of every
	// caller-mutable input, runs the full finalize gate, and seeds rawBytes
	// canonical so the first save publishes byte-identical.
	raw, err := renderCanonical([]byte("{}"), authored)
	if err != nil {
		return nil, err
	}
	return ParseDocument(raw, Origin{Source: OriginProgrammatic}, opts)
}
