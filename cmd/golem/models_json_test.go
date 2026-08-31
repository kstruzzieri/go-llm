package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/provider"
)

func writeModelsJSON(t *testing.T, path, baseURL, model string) {
	t.Helper()
	data, err := json.Marshal(config.Config{
		Providers: map[string]config.ProviderConfig{
			"local": {BaseURL: baseURL, APIFormat: "openai-compat"},
		},
		Models: map[string]config.ModelConfig{
			"agent": {Name: model, Provider: "local", Type: "dense", Capabilities: []string{"chat", "stream"}},
		},
		Defaults: map[string]string{"agent": "agent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGolemViewRequirementsShapes(t *testing.T) {
	req := golemViewRequirements()
	if req["agent"] != provider.CapChat|provider.CapStream|provider.CapToolCall {
		t.Fatalf("agent requirements = %v", req["agent"])
	}
	// Plan authoring always registers plan tools, so its shape is the
	// tool-bearing streamed-chat shape -- the same one the agent route
	// declares, calculated by the same helper the callers use.
	if req["planning"] != provider.CapChat|provider.CapStream|provider.CapToolCall {
		t.Fatalf("planning requirements = %v", req["planning"])
	}
	if _, ok := req["embedding"]; !ok {
		t.Fatal("embedding shape missing")
	}
}

// bindingByUseCase returns the projected binding for a use case, if any.
func bindingByUseCase(snap configview.Snapshot, useCase string) (configview.RoleBinding, bool) {
	for _, b := range snap.Bindings {
		if b.UseCase == useCase {
			return b, true
		}
	}
	return configview.RoleBinding{}, false
}

func TestModelsJSONProjectsAuthoredPlanningEligibility(t *testing.T) {
	// An explicit Capabilities list REPLACES every discovered set and its
	// omissions are definitive, so this model is provably missing tool_call.
	// Binding chat and planning to the SAME model is what makes the assertion
	// meaningful: if planning's requirement were not applied, both rows would
	// read eligible, and if everything were ineligible the chat row would say
	// so too.
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"local": {BaseURL: "http://localhost:8080"}},
		Models: map[string]config.ModelConfig{
			"planner": {Name: "big", Provider: "local", Type: "dense", Capabilities: []string{"chat", "stream"}},
		},
		Defaults: map[string]string{"planning": "planner", "chat": "planner"},
	}
	snap := configview.Build(configview.BuildInput{
		Doc:          configview.DocSnapshot{Config: cfg, Origin: config.Origin{Source: config.OriginExplicit, Path: "/x"}},
		Requirements: golemViewRequirements(),
	})

	planning, ok := bindingByUseCase(snap, "planning")
	if !ok {
		t.Fatalf("no planning binding projected for an authored defaults.planning; bindings = %+v", snap.Bindings)
	}
	if planning.Role != "planner" {
		t.Errorf("planning role = %q, want %q", planning.Role, "planner")
	}
	if len(planning.Candidates) == 0 {
		t.Fatal("planning binding has no candidates")
	}
	var planningVerdict configview.Eligibility
	var planningReasons []string
	for _, c := range planning.Candidates {
		if c.Selector == "local/big" {
			planningVerdict, planningReasons = c.Eligibility, c.Reasons
		}
	}
	if planningVerdict != configview.Eligibility(provider.CapIneligible) {
		t.Errorf("planning eligibility for local/big = %q, want ineligible (declares chat+stream, not tool_call)", planningVerdict)
	}
	if !slices.Contains(planningReasons, "missing_capability:tool_call") {
		t.Errorf("planning reasons = %v, want missing_capability:tool_call", planningReasons)
	}

	chat, ok := bindingByUseCase(snap, "chat")
	if !ok {
		t.Fatal("no chat binding projected")
	}
	chatFound := false
	for _, c := range chat.Candidates {
		if c.Selector == "local/big" {
			chatFound = true
			if c.Eligibility != configview.Eligibility(provider.CapEligible) {
				t.Errorf("chat eligibility for the same model = %q, want eligible; planning's verdict must come from its own requirement, not from a model nothing can satisfy", c.Eligibility)
			}
		}
	}
	if !chatFound {
		t.Fatal("chat binding has no local/big candidate")
	}
}

func TestModelsJSONDoesNotSynthesizeAbsentPlanningBinding(t *testing.T) {
	// Golem declares a planning REQUIREMENT unconditionally, but a
	// requirement is not a binding. configview.Build projects authored
	// defaults only, so a config that never writes defaults.planning shows no
	// planning row -- even though planning WOULD resolve at runtime through
	// the reasoning/analysis/agent fallbacks. Surfacing effective fallback
	// destinations is #477's manifest, not this projection's job; synthesizing
	// a row here would claim the user authored something they did not.
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"local": {BaseURL: "http://localhost:8080"}},
		Models: map[string]config.ModelConfig{
			"agent": {Name: "m1", Provider: "local", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}},
		},
		Defaults: map[string]string{"agent": "agent"},
	}
	snap := configview.Build(configview.BuildInput{
		Doc:          configview.DocSnapshot{Config: cfg, Origin: config.Origin{Source: config.OriginExplicit, Path: "/x"}},
		Requirements: golemViewRequirements(),
	})
	if _, ok := bindingByUseCase(snap, "planning"); ok {
		t.Errorf("planning binding synthesized for a config that never authors it; bindings = %+v", snap.Bindings)
	}
	if _, ok := bindingByUseCase(snap, "agent"); !ok {
		t.Error("agent binding missing, so the absent-planning assertion above proves nothing")
	}
}

func TestRenderModelsJSON(t *testing.T) {
	in := configview.BuildInput{
		Doc: configview.DocSnapshot{Config: &config.Config{
			Providers: map[string]config.ProviderConfig{"local": {BaseURL: "http://localhost:8080"}},
			Models:    map[string]config.ModelConfig{"agent": {Name: "m1", Provider: "local", Type: "dense"}},
			Defaults:  map[string]string{"agent": "agent"},
		}, Origin: config.Origin{Source: config.OriginExplicit, Path: "/private/x"}},
		Requirements: golemViewRequirements(),
	}
	var buf bytes.Buffer
	if err := renderModelsJSON(&buf, in); err != nil {
		t.Fatal(err)
	}
	var snap configview.Snapshot
	if err := json.Unmarshal(buf.Bytes(), &snap); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if !snap.Ready || len(snap.Bindings) != 1 || snap.Bindings[0].UseCase != "agent" {
		t.Fatalf("snapshot shape wrong: ready=%v bindings=%d", snap.Ready, len(snap.Bindings))
	}
	if bytes.Contains(buf.Bytes(), []byte("/private/x")) {
		t.Fatal("json output leaks config path")
	}
}

// Configless mode parity with loadConfig: discovery finding nothing is
// (nil, nil), and the snapshot projects not-ready with config_missing.
func TestModelsJSONConfiglessParity(t *testing.T) {
	if prev, ok := os.LookupEnv("GO_LLM_CONFIG"); ok {
		t.Cleanup(func() { _ = os.Setenv("GO_LLM_CONFIG", prev) })
		if err := os.Unsetenv("GO_LLM_CONFIG"); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(t.TempDir()) // no models.json in cwd
	doc, err := loadDocumentFor("")
	if err != nil {
		t.Fatalf("configless loadDocumentFor err = %v, want nil (loadConfig parity)", err)
	}
	// doc may be non-nil if the machine has a user-config/legacy models.json;
	// only the nil-doc projection is assertable portably.
	in := modelsJSONInput(nil, configview.Inventory{})
	snap := configview.Build(in)
	if snap.Ready {
		t.Fatal("nil-doc snapshot must not be ready")
	}
	found := false
	for _, d := range snap.Diagnostics {
		if d.Code == "config_missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("nil-doc snapshot missing config_missing diagnostic: %+v", snap.Diagnostics)
	}
	_ = doc
}

// -json refuses the active-probe switches: a view must not probe.
func TestModelsJSONRejectsProbeFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-json", "-probe-all"},
		{"-json", "-reprobe"},
	} {
		var out, errOut bytes.Buffer
		err := runModels(context.Background(), args, &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "-json cannot be combined") {
			t.Fatalf("args %v: err = %v, want combination rejection", args, err)
		}
	}
}

func TestModelsJSONSkipsBackendDiscovery(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, `{"data":[{"id":"m1"}]}`)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	configPath := filepath.Join(root, "models.json")
	writeModelsJSON(t, configPath, server.URL, "m1")
	var out, errOut bytes.Buffer
	if err := runModels(context.Background(), []string{"-json", "-config", configPath, "-root", root}, &out, &errOut); err != nil {
		t.Fatalf("runModels: %v\nstderr:\n%s", err, errOut.String())
	}

	if got := requests.Load(); got != 3 {
		t.Fatalf("backend requests = %d, want 3 inventory requests and no discovery request", got)
	}
}

func TestModelsJSONUsesOneConfigDocument(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "models.json")
	var server *httptest.Server
	var rewrites atomic.Int32
	rewriteErr := make(chan error, 1)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if rewrites.Add(1) == 1 {
			data, err := json.Marshal(config.Config{
				Providers: map[string]config.ProviderConfig{
					"local": {BaseURL: server.URL, APIFormat: "openai-compat"},
				},
				Models: map[string]config.ModelConfig{
					"agent": {Name: "after", Provider: "local", Type: "dense"},
				},
				Defaults: map[string]string{"agent": "agent"},
			})
			if err == nil {
				err = os.WriteFile(configPath, data, 0o600)
			}
			if err != nil {
				rewriteErr <- err
				http.Error(w, "rewrite failed", http.StatusInternalServerError)
				return
			}
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"before"}]}`)
	}))
	t.Cleanup(server.Close)
	writeModelsJSON(t, configPath, server.URL, "before")

	var out, errOut bytes.Buffer
	commandErr := runModels(context.Background(), []string{"-json", "-config", configPath, "-root", root}, &out, &errOut)
	select {
	case err := <-rewriteErr:
		t.Fatalf("rewrite config: %v", err)
	default:
	}
	if commandErr != nil {
		t.Fatalf("runModels: %v\nstderr:\n%s", commandErr, errOut.String())
	}
	var snap configview.Snapshot
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	for _, model := range snap.Models {
		if model.Selector == "local/after" {
			t.Fatal("snapshot projected a config revision loaded after bootstrap")
		}
	}
}

func TestModelsJSONMissingConfigNeedsNoProvider(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	if previous, ok := os.LookupEnv("GO_LLM_CONFIG"); ok {
		t.Cleanup(func() { _ = os.Setenv("GO_LLM_CONFIG", previous) })
	}
	if err := os.Unsetenv("GO_LLM_CONFIG"); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := runModels(context.Background(), []string{
		"-json", "-root", root, "-ollama-url", "http://127.0.0.1:1",
	}, &out, &errOut); err != nil {
		t.Fatalf("runModels: %v\nstderr:\n%s", err, errOut.String())
	}
	var snap configview.Snapshot
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Ready || len(snap.Diagnostics) != 1 || snap.Diagnostics[0].Code != "config_missing" {
		t.Fatalf("snapshot = %+v, want config_missing", snap)
	}
}

func TestModelsJSONRejectsPartialInventory(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"data":[{"id":"good-model"}]}`)
	}))
	t.Cleanup(good.Close)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "inventory unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(bad.Close)

	root := t.TempDir()
	configPath := filepath.Join(root, "models.json")
	data, err := json.Marshal(config.Config{
		Providers: map[string]config.ProviderConfig{
			"good": {BaseURL: good.URL, APIFormat: "openai-compat"},
			"bad":  {BaseURL: bad.URL, APIFormat: "openai-compat"},
		},
		Models: map[string]config.ModelConfig{
			"agent": {Name: "good-model", Provider: "good", Type: "dense"},
			"other": {Name: "bad-model", Provider: "bad", Type: "dense"},
		},
		Defaults: map[string]string{"agent": "agent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	err = runModels(context.Background(), []string{"-json", "-config", configPath, "-root", root}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), `provider "bad"`) {
		t.Fatalf("runModels error = %v, want partial-inventory error naming bad provider", err)
	}
}

func TestModelsJSONRejectsPartialLookupInventory(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 4 {
			http.Error(w, "model lookup unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprint(w, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	configPath := filepath.Join(root, "models.json")
	writeModelsJSON(t, configPath, server.URL, "m1")
	var out, errOut bytes.Buffer
	err := runModels(context.Background(), []string{"-json", "-config", configPath, "-root", root}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), `model "m2"`) {
		t.Fatalf("runModels error = %v, want partial-inventory error naming m2", err)
	}
}
