package ast

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// Compile-time interface conformance checks — drift in the Extractor or
// SymbolStore signatures will fail the build here before any test runs.
var (
	_ SymbolStore = NoOpStore{}
	_ Extractor   = NoOpExtractor{}
)

func TestNoOpExtractor(t *testing.T) {
	var e NoOpExtractor
	ctx := context.Background()

	if got := e.Languages(); got != nil {
		t.Errorf("NoOpExtractor.Languages = %v, want nil", got)
	}
	g, err := e.Extract(ctx, "scope-1", "/tmp/example/../example", "vsid-1")
	if err != nil {
		t.Fatalf("NoOpExtractor.Extract: %v", err)
	}
	if g.Scope != "scope-1" {
		t.Errorf("NoOpExtractor.Extract did not stamp scope: got %q", g.Scope)
	}
	if g.VectorSpaceID != "vsid-1" {
		t.Errorf("NoOpExtractor.Extract did not stamp vsid: got %q", g.VectorSpaceID)
	}
	if g.Root != "/tmp/example" {
		t.Errorf("NoOpExtractor.Extract did not canonicalize root: got %q", g.Root)
	}
	if g.ExtractionSignature != "" {
		t.Errorf("NoOpExtractor.Extract signature = %q, want empty", g.ExtractionSignature)
	}
	if len(g.Nodes)+len(g.Calls) != 0 {
		t.Errorf("NoOpExtractor.Extract produced non-empty graph: %+v", g)
	}
	stale, err := e.Stale(ctx, "scope-1", "/tmp/example", "any-signature")
	if err != nil {
		t.Fatalf("NoOpExtractor.Stale: %v", err)
	}
	if !stale {
		t.Errorf("NoOpExtractor.Stale = false, want true (fail-closed default)")
	}
}

func TestNoOpStoreWrites(t *testing.T) {
	var s NoOpStore
	ctx := context.Background()
	if err := s.UpsertGraph(ctx, SymbolGraph{Scope: "scope-1", VectorSpaceID: "vsid-1"}); err != nil {
		t.Errorf("UpsertGraph: %v", err)
	}
	if err := s.DeleteGraph(ctx, "scope-1", "vsid-1"); err != nil {
		t.Errorf("DeleteGraph: %v", err)
	}
}

func TestNoOpStoreRejectsInvalidCallEdge(t *testing.T) {
	var s NoOpStore
	err := s.UpsertGraph(context.Background(), SymbolGraph{
		Scope:         "scope-1",
		VectorSpaceID: "vsid-1",
		Calls: []CallEdge{
			{CallerID: "caller", Resolution: CallResolutionResolved, Line: 1},
		},
	})
	if !errors.Is(err, ErrInvalidGraph) {
		t.Fatalf("UpsertGraph error = %v, want ErrInvalidGraph", err)
	}
}

func TestNoOpStoreReadsAreNotFound(t *testing.T) {
	var s NoOpStore
	ctx := context.Background()
	if _, err := s.ExtractionSignature(ctx, "scope-1", "vsid-1"); !errors.Is(err, ErrSymbolNotFound) {
		t.Errorf("ExtractionSignature error: got %v, want %v", err, ErrSymbolNotFound)
	}
	if _, err := s.GetSymbol(ctx, "scope-1", "vsid-1", "symbol"); !errors.Is(err, ErrSymbolNotFound) {
		t.Errorf("GetSymbol error: got %v, want %v", err, ErrSymbolNotFound)
	}
	if _, err := s.SymbolEnclosing(ctx, "scope-1", "vsid-1", "x.go", 10); !errors.Is(err, ErrSymbolNotFound) {
		t.Errorf("SymbolEnclosing error: got %v, want %v", err, ErrSymbolNotFound)
	}
	callers, err := s.Callers(ctx, "scope-1", "vsid-1", "symbol", 0)
	if err != nil || len(callers.Sites) != 0 || callers.Truncated {
		t.Errorf("Callers: got (%+v, %v), want empty non-truncated result", callers, err)
	}
	callees, err := s.Callees(ctx, "scope-1", "vsid-1", "symbol", 0)
	if err != nil || len(callees.Sites) != 0 || callees.Truncated {
		t.Errorf("Callees: got (%+v, %v), want empty non-truncated result", callees, err)
	}
}

// TestNoOpStoreCallLimits exercises the limit parameter contract. The no-op
// store has no entries, so non-negative limits return an empty result; negative
// limits are rejected like real stores should reject them.
func TestNoOpStoreCallLimits(t *testing.T) {
	var s NoOpStore
	ctx := context.Background()
	for _, lim := range []int{0, 1, 100} {
		callers, err := s.Callers(ctx, "scope-1", "vsid-1", "symbol", lim)
		if err != nil || len(callers.Sites) != 0 || callers.Truncated {
			t.Errorf("callers limit=%d: got (%+v, %v), want empty non-truncated result", lim, callers, err)
		}
		callees, err := s.Callees(ctx, "scope-1", "vsid-1", "symbol", lim)
		if err != nil || len(callees.Sites) != 0 || callees.Truncated {
			t.Errorf("callees limit=%d: got (%+v, %v), want empty non-truncated result", lim, callees, err)
		}
	}
	if _, err := s.Callers(ctx, "scope-1", "vsid-1", "symbol", -1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("negative Callers limit error = %v, want ErrInvalidArgument", err)
	}
	if _, err := s.Callees(ctx, "scope-1", "vsid-1", "symbol", -1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("negative Callees limit error = %v, want ErrInvalidArgument", err)
	}
}

// TestNoOpsConcurrent verifies the "safe for concurrent use" claim on the
// no-op implementations. Empty structs are trivially safe; this test
// documents the contract and lets the race detector confirm.
func TestNoOpsConcurrent(t *testing.T) {
	var (
		extractor NoOpExtractor
		store     NoOpStore
		ctx       = context.Background()
		wg        sync.WaitGroup
	)
	const workers = 32
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if _, err := extractor.Extract(ctx, "scope-1", "/tmp", "vsid-1"); err != nil {
				t.Errorf("Extract: %v", err)
			}
			if _, err := extractor.Stale(ctx, "scope-1", "/tmp", "sig"); err != nil {
				t.Errorf("Stale: %v", err)
			}
			if err := store.UpsertGraph(ctx, SymbolGraph{Scope: "scope-1", VectorSpaceID: "vsid-1"}); err != nil {
				t.Errorf("UpsertGraph: %v", err)
			}
			if _, err := store.GetSymbol(ctx, "scope-1", "vsid-1", "x"); !errors.Is(err, ErrSymbolNotFound) {
				t.Errorf("GetSymbol: %v", err)
			}
		}()
	}
	wg.Wait()
}
