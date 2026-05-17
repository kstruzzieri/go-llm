package ast

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSentinelsWrapTransparently(t *testing.T) {
	// Verify that a caller wrapping a sentinel with fmt.Errorf("...: %w", ...)
	// can still be matched via errors.Is — this is the contract every
	// downstream consumer (rag.Indexer, MCP tools) will rely on.
	cases := []error{
		ErrSymbolNotFound,
		ErrVectorSpaceMismatch,
		ErrInvalidGraph,
		ErrInvalidArgument,
	}
	for _, sentinel := range cases {
		t.Run(sentinel.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("rag/ast: load symbol %q: %w", "pkg#Symbol", sentinel)
			if !errors.Is(wrapped, sentinel) {
				t.Errorf("errors.Is(wrapped, %v) = false; wrapping broke sentinel identity", sentinel)
			}
			// Double-wrapping (common in layered code) must also work.
			doubled := fmt.Errorf("store.get: %w", wrapped)
			if !errors.Is(doubled, sentinel) {
				t.Errorf("errors.Is(double-wrapped, %v) = false", sentinel)
			}
		})
	}
}

func TestSentinelNamespace(t *testing.T) {
	// All sentinel messages must start with "rag/ast: " so log aggregators
	// can disambiguate from go/ast and the rag package's own sentinels.
	for _, err := range []error{ErrSymbolNotFound, ErrVectorSpaceMismatch, ErrInvalidGraph, ErrInvalidArgument} {
		if !strings.HasPrefix(err.Error(), "rag/ast: ") {
			t.Errorf("sentinel %q lacks 'rag/ast: ' namespace prefix", err.Error())
		}
	}
}
