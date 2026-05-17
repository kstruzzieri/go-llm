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
	g, err := e.Extract(ctx, "/tmp/example", "vsid-1")
	if err != nil {
		t.Fatalf("NoOpExtractor.Extract: %v", err)
	}
	if g.VectorSpaceID != "vsid-1" {
		t.Errorf("NoOpExtractor.Extract did not stamp vsid: got %q", g.VectorSpaceID)
	}
	if g.Root != "/tmp/example" {
		t.Errorf("NoOpExtractor.Extract did not stamp root: got %q", g.Root)
	}
	if len(g.Nodes)+len(g.Calls) != 0 {
		t.Errorf("NoOpExtractor.Extract produced non-empty graph: %+v", g)
	}
	stale, err := e.Stale(ctx, "/tmp/example", "any-signature")
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
	if err := s.UpsertGraph(ctx, SymbolGraph{VectorSpaceID: "vsid-1"}); err != nil {
		t.Errorf("UpsertGraph: %v", err)
	}
	if err := s.DeleteByVectorSpace(ctx, "vsid-1"); err != nil {
		t.Errorf("DeleteByVectorSpace: %v", err)
	}
}

func TestNoOpStoreReadsAreNotFound(t *testing.T) {
	var s NoOpStore
	ctx := context.Background()
	if _, err := s.GetSymbol(ctx, "vsid-1", "pkg#Symbol"); !errors.Is(err, ErrSymbolNotFound) {
		t.Errorf("GetSymbol error: got %v, want %v", err, ErrSymbolNotFound)
	}
	if _, err := s.SymbolEnclosing(ctx, "vsid-1", "x.go", 10); !errors.Is(err, ErrSymbolNotFound) {
		t.Errorf("SymbolEnclosing error: got %v, want %v", err, ErrSymbolNotFound)
	}
	callers, err := s.Callers(ctx, "vsid-1", "pkg#Symbol", 0)
	if err != nil || callers != nil {
		t.Errorf("Callers: got (%v, %v), want (nil, nil)", callers, err)
	}
	callees, err := s.Callees(ctx, "vsid-1", "pkg#Symbol", 0)
	if err != nil || callees != nil {
		t.Errorf("Callees: got (%v, %v), want (nil, nil)", callees, err)
	}
}

// TestNoOpStoreCallersLimit exercises the limit parameter contract — the
// no-op store has no entries so any limit returns nil, but verifying the
// signature here ensures the parameter is plumbed.
func TestNoOpStoreCallersLimit(t *testing.T) {
	var s NoOpStore
	ctx := context.Background()
	for _, lim := range []int{0, 1, 100, -1} {
		callers, err := s.Callers(ctx, "vsid-1", "pkg#Symbol", lim)
		if err != nil || callers != nil {
			t.Errorf("limit=%d: got (%v, %v), want (nil, nil)", lim, callers, err)
		}
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
			if _, err := extractor.Extract(ctx, "/tmp", "vsid-1"); err != nil {
				t.Errorf("Extract: %v", err)
			}
			if _, err := extractor.Stale(ctx, "/tmp", "sig"); err != nil {
				t.Errorf("Stale: %v", err)
			}
			if err := store.UpsertGraph(ctx, SymbolGraph{VectorSpaceID: "vsid-1"}); err != nil {
				t.Errorf("UpsertGraph: %v", err)
			}
			if _, err := store.GetSymbol(ctx, "vsid-1", "x"); !errors.Is(err, ErrSymbolNotFound) {
				t.Errorf("GetSymbol: %v", err)
			}
		}()
	}
	wg.Wait()
}
