package config

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestNewDocumentBootstrap(t *testing.T) {
	spec := BootstrapSpec{
		ProviderName: "llamacpp",
		Provider:     ProviderSpec{BaseURL: "http://127.0.0.1:8090", APIFormat: "openai-compat"},
		Role:         "coder",
		Model:        ModelSpec{Name: "qwen3-coder-next", Type: "moe"},
	}
	d, err := NewDocument(spec, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := d.Config()
	if cfg.Models["coder"].Provider != "llamacpp" {
		t.Fatal("constructor did not own provider linkage")
	}
	if cfg.Defaults["agent"] != "coder" {
		t.Fatal("agent route not guaranteed")
	}
	if d.Origin().Source != OriginProgrammatic {
		t.Fatalf("origin source = %q", d.Origin().Source)
	}

	_, err = NewDocument(BootstrapSpec{
		Role: "r", Provider: ProviderSpec{BaseURL: "http://h"},
		Model: ModelSpec{Name: "m", Type: "dense"},
	}, DocumentOptions{})
	assertDiag(t, err, CodeInvalidArgument, SubjectNone, "")
	_, err = NewDocument(BootstrapSpec{
		ProviderName: "p", Provider: ProviderSpec{BaseURL: "http://h"},
		Model: ModelSpec{Name: "m", Type: "dense"},
	}, DocumentOptions{})
	assertDiag(t, err, CodeInvalidArgument, SubjectNone, "")

	_, err = NewDocument(BootstrapSpec{
		ProviderName: "p", Provider: ProviderSpec{BaseURL: "nope"},
		Role: "r", Model: ModelSpec{Name: "m", Type: "dense"},
	}, DocumentOptions{})
	assertDiag(t, err, CodeProviderEndpointInvalid, SubjectProvider, "p")

	// Generic go-llm validity is deliberately weaker than Firn's agent floor.
	if _, err := NewDocument(BootstrapSpec{
		ProviderName: "p", Provider: ProviderSpec{BaseURL: "http://h"},
		Role: "embed", Model: ModelSpec{Name: "e", Type: "embedding"},
	}, DocumentOptions{}); err != nil {
		t.Fatalf("embedding must remain generically valid: %v", err)
	}
}

// TestNewDocumentFullFieldRoundTrip populates EVERY ModelSpec and
// ProviderSpec field (slot-governed combo so slot policy passes) and pins
// the effective model against a full ModelConfig literal — a dropped field
// in the constructor's copy goes red here. The effective view materializes
// nothing on a fully-populated model: applyDefaults only touches provider
// timeout/api_format and an EMPTY model Provider, and think_mode
// normalization is a lowercase no-op for "toggle".
func TestNewDocumentFullFieldRoundTrip(t *testing.T) {
	temperature, topP, topK := 0.7, 0.9, 40
	spec := BootstrapSpec{
		ProviderName: "llamacpp",
		Provider: ProviderSpec{
			BaseURL:       "http://127.0.0.1:8090",
			Timeout:       Duration{Duration: 90 * time.Second},
			APIFormat:     "openai-compat",
			SlotDiscovery: true,
		},
		Role: "coder",
		Model: ModelSpec{
			Name:          "qwen3-coder-next",
			Description:   "primary coder",
			Type:          "moe",
			Parameters:    "79.7B",
			ContextWindow: 262144,
			Dimensions:    4096,
			Capabilities:  []string{"chat"},
			Options:       &SamplingOptions{Temperature: &temperature, TopP: &topP, TopK: &topK},
			Slots:         2,
			ThinkMode:     "toggle",
			ThinkTags:     &ThinkTagsConfig{Open: "<t>", Close: "</t>"},
		},
	}
	d, err := NewDocument(spec, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := ModelConfig{
		Name:          "qwen3-coder-next",
		Provider:      "llamacpp",
		Description:   "primary coder",
		Type:          "moe",
		Parameters:    "79.7B",
		ContextWindow: 262144,
		Dimensions:    4096,
		Capabilities:  []string{"chat"},
		Options:       &SamplingOptions{Temperature: &temperature, TopP: &topP, TopK: &topK},
		Slots:         2,
		ThinkMode:     "toggle",
		ThinkTags:     &ThinkTagsConfig{Open: "<t>", Close: "</t>"},
	}
	if got := d.Config().Models["coder"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("effective model = %+v, want %+v", got, want)
	}
	gotP, err := d.AuthoredProvider("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	if gotP != spec.Provider {
		t.Fatalf("authored provider = %+v, want %+v", gotP, spec.Provider)
	}
}

// TestModelSpecParityWithModelConfig forces a conscious ModelSpec decision
// whenever ModelConfig grows a field: every ModelConfig field except the
// constructor-owned Provider and the bootstrap-excluded Fallbacks must have
// a same-named ModelSpec counterpart, and nothing extra.
func TestModelSpecParityWithModelConfig(t *testing.T) {
	skip := map[string]bool{"Provider": true, "Fallbacks": true}
	want, got := map[string]bool{}, map[string]bool{}
	cfgT := reflect.TypeOf(ModelConfig{})
	for i := 0; i < cfgT.NumField(); i++ {
		if name := cfgT.Field(i).Name; !skip[name] {
			want[name] = true
		}
	}
	specT := reflect.TypeOf(ModelSpec{})
	for i := 0; i < specT.NumField(); i++ {
		got[specT.Field(i).Name] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ModelSpec fields %v != ModelConfig fields minus {Provider, Fallbacks} %v", got, want)
	}
}

func TestNewDocumentFirstRenderCanonical(t *testing.T) {
	d, err := NewDocument(BootstrapSpec{
		ProviderName: "p", Provider: ProviderSpec{BaseURL: "http://h"},
		Role: "agent", Model: ModelSpec{Name: "m", Type: "dense"},
	}, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "boot.json")
	if err := d.SaveNew(path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	var publishes int
	original := publishReplaceFn
	publishReplaceFn = func(path string, data []byte, revision string) error {
		publishes++
		return original(path, data, revision)
	}
	t.Cleanup(func() { publishReplaceFn = original })
	if err := reloaded.SaveReplace(path, reloaded.Revision()); err != nil {
		t.Fatal(err)
	}
	if publishes != 0 {
		t.Fatalf("bootstrap required %d normalization publication(s)", publishes)
	}
}

func TestNewDocumentOwnsEveryMutableInput(t *testing.T) {
	temperature, topP, topK := 0.5, 0.8, 20
	caps := []string{"chat"}
	options := &SamplingOptions{Temperature: &temperature, TopP: &topP, TopK: &topK}
	tags := &ThinkTagsConfig{Open: "<t>", Close: "</t>"}
	d, err := NewDocument(BootstrapSpec{
		ProviderName: "p", Provider: ProviderSpec{BaseURL: "http://h"},
		Role: "agent",
		Model: ModelSpec{
			Name: "m", Type: "dense", Capabilities: caps,
			Options: options, ThinkMode: "toggle", ThinkTags: tags,
		},
	}, DocumentOptions{})
	if err != nil {
		t.Fatal(err)
	}

	caps[0] = "stream"
	temperature, topP, topK = 99, 0.1, 1
	tags.Open = "<changed>"

	got := d.Config().Models["agent"]
	if len(got.Capabilities) != 1 || got.Capabilities[0] != "chat" ||
		got.Options == nil || got.Options.Temperature == nil ||
		got.Options.TopP == nil || got.Options.TopK == nil ||
		*got.Options.Temperature != 0.5 ||
		*got.Options.TopP != 0.8 || *got.Options.TopK != 20 ||
		got.ThinkTags == nil || got.ThinkTags.Open != "<t>" {
		t.Fatal("caller-owned mutable input leaked into the document")
	}
}
