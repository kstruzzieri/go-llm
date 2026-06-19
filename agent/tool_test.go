package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type fakeTool struct {
	name   string
	effect Effect
}

func (f fakeTool) Spec() ToolSpec {
	return ToolSpec{Name: f.name, Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (f fakeTool) Effect() Effect { return f.effect }
func (f fakeTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	return ToolResult{Content: "ok"}, nil
}

func TestEffectClassHasAndMutating(t *testing.T) {
	e := Effect{Class: Read | Network}
	if !e.Class.Has(Read) || !e.Class.Has(Network) {
		t.Fatal("expected Read|Network bits set")
	}
	if e.Class.Has(Write) || e.Class.IsMutating() {
		t.Fatal("Read|Network must not be mutating")
	}
	if !(Effect{Class: Write}).Class.IsMutating() || !(Effect{Class: Exec}).Class.IsMutating() {
		t.Fatal("Write and Exec must be mutating")
	}
}

func TestNormalizeEffectDefaults(t *testing.T) {
	got := normalizeEffect(Effect{Class: Read})
	if got.Timeout != defaultToolTimeout {
		t.Fatalf("timeout = %v, want default %v", got.Timeout, defaultToolTimeout)
	}
	if got.OutputCap != defaultOutputCap {
		t.Fatalf("cap = %d, want default %d", got.OutputCap, defaultOutputCap)
	}
	keep := normalizeEffect(Effect{Class: Read, Timeout: time.Second, OutputCap: 10})
	if keep.Timeout != time.Second || keep.OutputCap != 10 {
		t.Fatal("explicit non-zero values must be preserved")
	}
}

func TestNeedsApproval(t *testing.T) {
	if needsApproval(ApprovalNever, Write) {
		t.Fatal("Never => no approval")
	}
	if !needsApproval(ApprovalAlways, Read) {
		t.Fatal("Always => approval even for Read")
	}
	if needsApproval(ApprovalOnWrite, Read) {
		t.Fatal("OnWrite + Read => no approval")
	}
	if !needsApproval(ApprovalOnWrite, Write) {
		t.Fatal("OnWrite + Write => approval")
	}
}

func TestRegistryLookupAndProviderSpecs(t *testing.T) {
	reg, err := newToolRegistry([]Tool{fakeTool{name: "a"}, fakeTool{name: "b"}})
	if err != nil {
		t.Fatalf("newToolRegistry: %v", err)
	}
	if _, ok := reg.lookup("a"); !ok {
		t.Fatal("expected tool a")
	}
	if _, ok := reg.lookup("missing"); ok {
		t.Fatal("missing must not resolve")
	}
	specs := reg.providerSpecs()
	if len(specs) != 2 || specs[0].Type != "function" || specs[0].Function.Name == "" {
		t.Fatalf("bad provider specs: %+v", specs)
	}
}

func TestRegistryRejectsDuplicateNames(t *testing.T) {
	if _, err := newToolRegistry([]Tool{fakeTool{name: "x"}, fakeTool{name: "x"}}); err == nil {
		t.Fatal("duplicate tool names must error")
	}
}
