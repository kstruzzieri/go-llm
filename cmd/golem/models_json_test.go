package main

import (
	"bytes"
	"context"
	"encoding/json"
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
