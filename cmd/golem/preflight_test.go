package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

type fakeReg struct {
	byKey     map[provider.ModelKey]provider.Capability
	recommend []provider.Capability
}

func (f fakeReg) Lookup(ctx context.Context, key provider.ModelKey) (*provider.ModelProfile, error) {
	c, ok := f.byKey[key]
	if !ok {
		return nil, context.Canceled // any non-nil error; preflight treats lookup error as "not capable"
	}
	return &provider.ModelProfile{Caps: c}, nil
}

func (f fakeReg) LookupAny(ctx context.Context, model string) ([]*provider.ModelProfile, error) {
	var out []*provider.ModelProfile
	for k, c := range f.byKey {
		if k.Model == model {
			out = append(out, &provider.ModelProfile{Caps: c})
		}
	}
	return out, nil
}

func (f fakeReg) Recommend(ctx context.Context, opts provider.RecommendOpts) ([]*provider.ModelProfile, error) {
	var out []*provider.ModelProfile
	for _, c := range f.recommend {
		out = append(out, &provider.ModelProfile{Caps: c})
	}
	return out, nil
}

const fullCaps = provider.CapChat | provider.CapStream | provider.CapToolCall

func TestPreflight_HeadCapable(t *testing.T) {
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "m1"}: fullCaps,
	}}
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ollama/m1"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(warns) != 0 {
		t.Errorf("warnings = %v, want none", warns)
	}
}

func TestPreflight_PrimaryIncapableFallbackCapable(t *testing.T) {
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "m1"}: provider.CapChat | provider.CapStream, // no tool_call
		{Provider: "ollama", Model: "m2"}: fullCaps,
	}}
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ollama/m1", "ollama/m2"})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "ollama/m1") {
		t.Errorf("warnings = %v, want one naming ollama/m1", warns)
	}
}

func TestPreflight_NoneCapable(t *testing.T) {
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "m1"}: provider.CapChat | provider.CapStream,
	}}
	_, err := preflightToolCapable(context.Background(), reg, []string{"ollama/m1"})
	if err == nil {
		t.Fatal("err = nil, want failure (no tool-capable entry)")
	}
}

func TestPreflight_EmptyChain_UsesRecommend(t *testing.T) {
	ok := fakeReg{recommend: []provider.Capability{fullCaps}}
	if _, err := preflightToolCapable(context.Background(), ok, nil); err != nil {
		t.Fatalf("recommend-capable err = %v, want nil", err)
	}
	empty := fakeReg{recommend: nil}
	if _, err := preflightToolCapable(context.Background(), empty, nil); err == nil {
		t.Fatal("empty recommend err = nil, want failure")
	}
}

// TestPreflight_BareModelNotFound exercises the unparseable-selector branch: a
// bare model name (no "/") routes through LookupAny, which returns nothing here,
// so the entry is not tool-capable and — being the only entry — fails the gate.
func TestPreflight_BareModelNotFound(t *testing.T) {
	reg := fakeReg{byKey: map[provider.ModelKey]provider.Capability{
		{Provider: "ollama", Model: "other"}: fullCaps,
	}}
	warns, err := preflightToolCapable(context.Background(), reg, []string{"ghost"})
	if err == nil {
		t.Fatal("err = nil for bare model with no capable match, want failure")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "ghost") {
		t.Errorf("warnings = %v, want one naming ghost", warns)
	}
}
