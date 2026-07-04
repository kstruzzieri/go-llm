package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/fingerprint"
	"github.com/kstruzzieri/go-llm/provider"
)

// fakeModelsReg is a fake modelsRegistry: canned Lookup profiles, canned
// ExplainToolCall provenance, and a ResolveToolCall that records calls (so the
// -probe-all / -reprobe tests can assert per-entry probing). When events is
// non-nil, each ResolveToolCall appends "resolve:<key>" to the shared ordered
// log so a test can assert delete-before-resolve ordering across both fakes.
type fakeModelsReg struct {
	profiles map[provider.ModelKey]*provider.ModelProfile
	explain  map[provider.ModelKey]provider.ToolCallExplanation
	states   map[provider.ModelKey]fingerprint.CapProbeState
	calls    []provider.ModelKey
	events   *[]string
}

func (f *fakeModelsReg) Lookup(_ context.Context, key provider.ModelKey) (*provider.ModelProfile, error) {
	if p, ok := f.profiles[key]; ok {
		return p, nil
	}
	return nil, context.Canceled // any non-nil error stands in for not-found
}

func (f *fakeModelsReg) ExplainToolCall(_ context.Context, key provider.ModelKey) (provider.ToolCallExplanation, error) {
	return f.explain[key], nil
}

func (f *fakeModelsReg) ResolveToolCall(_ context.Context, key provider.ModelKey) (fingerprint.CapProbeState, error) {
	f.calls = append(f.calls, key)
	if f.events != nil {
		*f.events = append(*f.events, "resolve:"+key.String())
	}
	return f.states[key], nil
}

// fakeDeleteStore records DeleteCapProbes calls for the -reprobe test. Only the
// delete path is exercised here; Get/Save satisfy the interface. When events is
// non-nil, each delete appends "delete:<provider/model>" to the shared ordered
// log so a test can pin delete-before-resolve ordering.
type fakeDeleteStore struct {
	deleted []string
	events  *[]string
}

func (s *fakeDeleteStore) GetCapProbe(context.Context, string, string, string) (*fingerprint.CapProbe, error) {
	return nil, fingerprint.ErrNotFound
}
func (s *fakeDeleteStore) SaveCapProbe(context.Context, fingerprint.CapProbe) error { return nil }
func (s *fakeDeleteStore) DeleteCapProbes(_ context.Context, backendID, modelName string) error {
	s.deleted = append(s.deleted, backendID+"/"+modelName)
	if s.events != nil {
		*s.events = append(*s.events, "delete:"+backendID+"/"+modelName)
	}
	return nil
}

func keyOf(sel string) provider.ModelKey {
	p, m, _ := strings.Cut(sel, "/")
	return provider.ModelKey{Provider: p, Model: m}
}

func TestRunModels_ListsChainWithProvenance(t *testing.T) {
	ctx := context.Background()
	aKey := keyOf("llamacpp/gemma4:31b")
	bKey := keyOf("llamacpp/byo-model")
	reg := &fakeModelsReg{
		profiles: map[provider.ModelKey]*provider.ModelProfile{
			aKey: {Key: aKey, Caps: provider.CapChat | provider.CapGenerate | provider.CapStream | provider.CapToolCall},
			bKey: {Key: bKey, Caps: provider.CapChat | provider.CapGenerate | provider.CapStream},
		},
		explain: map[provider.ModelKey]provider.ToolCallExplanation{
			aKey: {Caps: provider.CapChat | provider.CapGenerate | provider.CapStream | provider.CapToolCall, Has: true, Source: "explicit"},
			bKey: {Caps: provider.CapChat | provider.CapGenerate | provider.CapStream, Has: false, Source: "probe", State: fingerprint.CapProbeNo, TestedAt: time.Now().Add(-2 * time.Hour)},
		},
	}
	var out, errOut bytes.Buffer
	chain := []string{"llamacpp/gemma4:31b", "llamacpp/byo-model"}
	if err := runModelsWith(ctx, reg, chain, nil, nil, nil, modelsOpts{}, &out, &errOut); err != nil {
		t.Fatalf("runModelsWith() error: %v", err)
	}
	got := out.String()
	// One line per entry, with caps rendered.
	if !strings.Contains(got, "llamacpp/gemma4:31b") || !strings.Contains(got, "llamacpp/byo-model") {
		t.Fatalf("output missing chain entries:\n%s", got)
	}
	// A carries tool_call with its provenance.
	if !strings.Contains(got, "tool_call=yes") || !strings.Contains(got, "explicit") {
		t.Fatalf("output missing A tool_call provenance:\n%s", got)
	}
	// B is flagged MISSING with the probe verdict.
	if !strings.Contains(got, "MISSING") {
		t.Fatalf("output missing MISSING flag for B:\n%s", got)
	}
	// The remediation hint appears under the MISSING entry.
	if !strings.Contains(got, remediationHint("llamacpp/byo-model")) {
		t.Fatalf("output missing remediation hint for B:\n%s", got)
	}
}

func TestRunModels_SharedKeyNamesDeclaringRole(t *testing.T) {
	ctx := context.Background()
	// The agent chain uses key llamacpp/shared with NO explicit caps of its
	// own; role "chat" in cfg declares tool_call for the SAME key.
	key := keyOf("llamacpp/shared")
	reg := &fakeModelsReg{
		profiles: map[provider.ModelKey]*provider.ModelProfile{
			key: {Key: key, Caps: provider.CapChat | provider.CapStream | provider.CapToolCall},
		},
		explain: map[provider.ModelKey]provider.ToolCallExplanation{
			key: {Caps: provider.CapChat | provider.CapStream | provider.CapToolCall, Has: true, Source: "explicit"},
		},
	}
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"chat":  {Name: "shared", Provider: "llamacpp", Type: "dense", Capabilities: []string{"chat", "stream", "tool_call"}},
			"agent": {Name: "shared", Provider: "llamacpp", Type: "dense"},
		},
	}
	var out, errOut bytes.Buffer
	if err := runModelsWith(ctx, reg, []string{"llamacpp/shared"}, cfg, nil, nil, modelsOpts{}, &out, &errOut); err != nil {
		t.Fatalf("runModelsWith() error: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `declared by model entry "chat"`) {
		t.Fatalf("output missing shared-key declaring-role annotation:\n%s", got)
	}
}

func TestRunModels_ProbeAllProbesEveryEntry(t *testing.T) {
	ctx := context.Background()
	sels := []string{"llamacpp/m1", "llamacpp/m2", "llamacpp/m3"}
	reg := &fakeModelsReg{
		profiles: map[provider.ModelKey]*provider.ModelProfile{},
		explain:  map[provider.ModelKey]provider.ToolCallExplanation{},
		states:   map[provider.ModelKey]fingerprint.CapProbeState{},
	}
	for _, s := range sels {
		k := keyOf(s)
		// Non-explicit, tool_call absent => all three are probe candidates.
		reg.profiles[k] = &provider.ModelProfile{Key: k, Caps: provider.CapChat | provider.CapStream}
		reg.explain[k] = provider.ToolCallExplanation{Caps: provider.CapChat | provider.CapStream, Has: false, Source: "unknown"}
		reg.states[k] = fingerprint.CapProbeNo
	}
	var out, errOut bytes.Buffer
	if err := runModelsWith(ctx, reg, sels, nil, nil, nil, modelsOpts{probeAll: true}, &out, &errOut); err != nil {
		t.Fatalf("runModelsWith() error: %v", err)
	}
	if len(reg.calls) != 3 {
		t.Fatalf("ResolveToolCall calls = %d, want 3 (probe every non-explicit entry, no bounded-eager stop)", len(reg.calls))
	}
}

func TestRunModels_ReprobeDeletesRowsFirst(t *testing.T) {
	ctx := context.Background()
	sels := []string{"llamacpp/m1", "llamacpp/m2"}
	// Shared ordered log so the two fakes agree on a single timeline; this is
	// how delete-BEFORE-resolve is pinned (each fake alone has no ordering).
	var events []string
	reg := &fakeModelsReg{
		profiles: map[provider.ModelKey]*provider.ModelProfile{},
		explain:  map[provider.ModelKey]provider.ToolCallExplanation{},
		states:   map[provider.ModelKey]fingerprint.CapProbeState{},
		events:   &events,
	}
	for _, s := range sels {
		k := keyOf(s)
		reg.profiles[k] = &provider.ModelProfile{Key: k, Caps: provider.CapChat | provider.CapStream}
		reg.explain[k] = provider.ToolCallExplanation{Caps: provider.CapChat | provider.CapStream, Has: false, Source: "unknown"}
		reg.states[k] = fingerprint.CapProbeYes
	}
	store := &fakeDeleteStore{events: &events}
	var out, errOut bytes.Buffer
	if err := runModelsWith(ctx, reg, sels, nil, store, nil, modelsOpts{reprobe: true}, &out, &errOut); err != nil {
		t.Fatalf("runModelsWith() error: %v", err)
	}
	if len(store.deleted) != 2 {
		t.Fatalf("DeleteCapProbes calls = %d (%v), want 2 (per non-explicit entry)", len(store.deleted), store.deleted)
	}
	if len(reg.calls) != 2 {
		t.Fatalf("ResolveToolCall calls = %d, want 2 (resolve after delete)", len(reg.calls))
	}
	// Every delete for a key must precede that key's resolve on the shared
	// timeline. Locate each event's index and compare per key.
	idx := func(want string) int {
		for i, e := range events {
			if e == want {
				return i
			}
		}
		return -1
	}
	for _, s := range sels {
		k := keyOf(s)
		di := idx("delete:" + k.String())
		ri := idx("resolve:" + k.String())
		if di < 0 || ri < 0 {
			t.Fatalf("missing events for %s: got %v", s, events)
		}
		if di >= ri {
			t.Fatalf("%s: delete (idx %d) must precede resolve (idx %d); timeline=%v", s, di, ri, events)
		}
	}
}
