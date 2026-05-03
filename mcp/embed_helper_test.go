package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/config"
	"github.com/kstruzzieri/go-llm/provider"
)

// newEmbedHelperServer mirrors the direct Server fixture pattern used by
// mcp/tools_embed_test.go: install the recording route engine on Server.router
// and provide just enough config for chainFor("embedding") to succeed.
func newEmbedHelperServer(t *testing.T, eng routeEngine) *Server {
	t.Helper()
	return &Server{
		cfg: &config.Config{
			Defaults: map[string]string{"embedding": "embedder"},
			Models: map[string]config.ModelConfig{
				"embedder": {Name: "qwen3-embedding:8b", Provider: "ollama"},
			},
		},
		router: eng,
	}
}

func TestRoutedEmbed_EmptyInputsShortCircuits(t *testing.T) {
	eng := newRecordingRouteEngine("")
	s := newEmbedHelperServer(t, eng)

	resp, err := s.routedEmbed(context.Background(), "m", nil, provider.PriorityNormal)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if eng.called {
		t.Error("router was contacted for empty inputs; want short-circuit")
	}
}

func TestRoutedEmbed_ExplicitModelLeavesPreferredChainEmpty(t *testing.T) {
	eng := newRecordingRouteEngine("")
	s := newEmbedHelperServer(t, eng)

	_, err := s.routedEmbed(context.Background(), "m-explicit", []string{"x"}, provider.PriorityBackground)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !eng.called {
		t.Fatal("router was not contacted")
	}
	if len(eng.last.PreferredChain) != 0 {
		t.Errorf("PreferredChain = %v, want empty for explicit model", eng.last.PreferredChain)
	}
	if eng.last.Model != "m-explicit" {
		t.Errorf("Model = %q, want %q", eng.last.Model, "m-explicit")
	}
	if eng.last.Priority != provider.PriorityBackground {
		t.Errorf("Priority = %v, want %v", eng.last.Priority, provider.PriorityBackground)
	}
	if eng.last.RequiredCaps != provider.CapEmbed {
		t.Errorf("RequiredCaps = %v, want CapEmbed", eng.last.RequiredCaps)
	}
	if eng.last.UseCase != "embedding" {
		t.Errorf("UseCase = %q, want %q", eng.last.UseCase, "embedding")
	}
}

func TestRoutedEmbed_EmptyModelSetsPreferredChain(t *testing.T) {
	eng := newRecordingRouteEngine("")
	s := newEmbedHelperServer(t, eng)

	_, err := s.routedEmbed(context.Background(), "", []string{"x"}, provider.PriorityNormal)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(eng.last.PreferredChain) == 0 {
		t.Error("PreferredChain is empty; want s.chainFor(\"embedding\") result")
	}
}

func TestRoutedEmbed_LengthMismatchIsCategorisedOllama(t *testing.T) {
	// Engine returns a single embedding for a 2-input batch.
	eng := newRecordingRouteEngine("")
	eng.embedVectors = [][]float64{{1, 2, 3}}
	s := newEmbedHelperServer(t, eng)

	_, err := s.routedEmbed(context.Background(), "m", []string{"a", "b"}, provider.PriorityNormal)
	if err == nil {
		t.Fatal("expected length-mismatch error, got nil")
	}
	if got := routedEmbedCategory(err); got != embedToolOllama {
		t.Errorf("category = %q, want %q", got, embedToolOllama)
	}
	if !strings.Contains(err.Error(), "count mismatch") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "count mismatch")
	}
}

func TestRoutedEmbed_RouterErrorIsCategorisedRouter(t *testing.T) {
	eng := newRecordingRouteEngine("")
	eng.routeErr = errors.New("simulated route failure")
	s := newEmbedHelperServer(t, eng)

	_, err := s.routedEmbed(context.Background(), "m", []string{"x"}, provider.PriorityNormal)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := routedEmbedCategory(err); got != embedToolRouter {
		t.Errorf("category = %q, want %q", got, embedToolRouter)
	}
}

func TestRoutedEmbed_NilRouterIsCategorisedConfig(t *testing.T) {
	s := newEmbedHelperServer(t, nil) // explicitly nil engine

	_, err := s.routedEmbed(context.Background(), "m", []string{"x"}, provider.PriorityNormal)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := routedEmbedCategory(err); got != embedToolConfig {
		t.Errorf("category = %q, want %q", got, embedToolConfig)
	}
}

func TestRagEmbedder_EmptyModelRejected(t *testing.T) {
	eng := newRecordingRouteEngine("")
	s := newEmbedHelperServer(t, eng)
	emb := s.ragEmbedder(provider.PriorityNormal)

	_, err := emb.Embed(context.Background(), "", []string{"x"})
	if err == nil {
		t.Fatal("expected drift-guard error, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "drift") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "drift")
	}
	if eng.called {
		t.Error("router was contacted for empty-model RAG call; want fail-closed before routing")
	}
}

func TestRagEmbedder_ForwardsPriorityBackground(t *testing.T) {
	eng := newRecordingRouteEngine("")
	s := newEmbedHelperServer(t, eng)

	emb := s.ragEmbedder(provider.PriorityBackground)
	_, err := emb.Embed(context.Background(), "m", []string{"x"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if eng.last.Priority != provider.PriorityBackground {
		t.Errorf("Priority = %v, want %v", eng.last.Priority, provider.PriorityBackground)
	}
}

func TestRagEmbedder_ForwardsPriorityNormal(t *testing.T) {
	eng := newRecordingRouteEngine("")
	s := newEmbedHelperServer(t, eng)

	emb := s.ragEmbedder(provider.PriorityNormal)
	_, err := emb.Embed(context.Background(), "m", []string{"x"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if eng.last.Priority != provider.PriorityNormal {
		t.Errorf("Priority = %v, want %v", eng.last.Priority, provider.PriorityNormal)
	}
}

func TestRagEmbedder_ExtractsProvenance(t *testing.T) {
	eng := newRecordingRouteEngine("")
	eng.embedVectors = [][]float64{{1, 2, 3}}
	s := newEmbedHelperServer(t, eng)

	emb := s.ragEmbedder(provider.PriorityNormal)
	res, err := emb.Embed(context.Background(), "m", []string{"x"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Provider == "" || res.Model == "" {
		t.Errorf("expected populated Provider/Model in EmbedResult, got %+v", res)
	}
	if res.VectorSpaceID == "" {
		t.Errorf("expected populated VectorSpaceID in EmbedResult, got %+v", res)
	}
}
