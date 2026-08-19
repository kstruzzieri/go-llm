package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/configview"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestGolemViewRequirementsShapes(t *testing.T) {
	req := golemViewRequirements()
	if req["agent"] != provider.CapChat|provider.CapStream|provider.CapToolCall {
		t.Fatalf("agent requirements = %v", req["agent"])
	}
	if _, ok := req["embedding"]; !ok {
		t.Fatal("embedding shape missing")
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
