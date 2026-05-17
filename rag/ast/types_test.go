package ast

import (
	"errors"
	"strings"
	"testing"
)

func TestSymbolID(t *testing.T) {
	tests := []struct {
		name string
		key  SymbolKey
	}{
		{
			name: "go free function",
			key: SymbolKey{
				Language:  "go",
				Kind:      SymbolKindFunction,
				Namespace: "github.com/kstruzzieri/go-llm/rag",
				Name:      "NewIndexer",
			},
		},
		{
			name: "go method",
			key: SymbolKey{
				Language:  "go",
				Kind:      SymbolKindMethod,
				Namespace: "github.com/kstruzzieri/go-llm/rag",
				Receiver:  "Indexer",
				Name:      "IndexFile",
			},
		},
		{
			name: "overloaded method",
			key: SymbolKey{
				Language:      "java",
				Kind:          SymbolKindMethod,
				Namespace:     "com.example.Service",
				Receiver:      "Service",
				Name:          "Handle",
				Disambiguator: "(Request)Response",
			},
		},
		{
			name: "punctuation from non-go syntax",
			key: SymbolKey{
				Language:      "typescript",
				Kind:          SymbolKindFunction,
				Namespace:     "src/routes/admin#users.ts",
				Name:          "GET /admin/:id",
				Disambiguator: "export const GET = async (...)",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := SymbolID(tt.key)
			got, ok := decodeSymbolID(id)
			if !ok {
				t.Fatalf("decodeSymbolID(%q) failed", id)
			}
			if got != tt.key {
				t.Errorf("SymbolID round-trip = %+v, want %+v", got, tt.key)
			}
		})
	}
}

func TestSymbolIDWireFormat(t *testing.T) {
	key := SymbolKey{
		Language:  "go",
		Kind:      SymbolKindFunction,
		Namespace: "pkg",
		Name:      "Foo",
	}
	const want = "symbol-v1#Z28#ZnVuY3Rpb24#cGtn##Rm9v#"
	if got := SymbolID(key); got != want {
		t.Errorf("SymbolID wire format = %q, want %q", got, want)
	}
}

func TestSymbolIDNoCollisionAcrossAmbiguousInputs(t *testing.T) {
	cases := []SymbolKey{
		{Language: "go", Kind: SymbolKindFunction, Namespace: "foo#bar", Name: "baz"},
		{Language: "go", Kind: SymbolKindFunction, Namespace: "foo", Receiver: "bar", Name: "baz"},
		{Language: "go", Kind: SymbolKindMethod, Namespace: "foo", Receiver: "bar", Name: "baz"},
		{Language: "java", Kind: SymbolKindMethod, Namespace: "foo", Receiver: "bar", Name: "baz"},
		{Language: "java", Kind: SymbolKindMethod, Namespace: "foo", Receiver: "bar", Name: "baz", Disambiguator: "(int)"},
		{Language: "java", Kind: SymbolKindMethod, Namespace: "foo", Receiver: "bar", Name: "baz", Disambiguator: "(string)"},
	}
	seen := make(map[string]SymbolKey, len(cases))
	for _, key := range cases {
		id := SymbolID(key)
		if prev, ok := seen[id]; ok {
			t.Errorf("SymbolID collision: %+v and %+v both produced %q", prev, key, id)
		}
		seen[id] = key
	}
}

// FuzzSymbolID asserts the bijection property for arbitrary component text,
// including characters that would be separators in simpler encodings.
func FuzzSymbolID(f *testing.F) {
	f.Add("go", "function", "pkg", "", "Name", "")
	f.Add("go", "method", "github.com/x/y", "Recv", "Method", "")
	f.Add("typescript", "function", "src/a#b.ts", "", "GET /:id", "export const GET")
	f.Add("", "", "", "", "", "")
	f.Fuzz(func(t *testing.T, lang, kind, namespace, recv, name, disambiguator string) {
		key := SymbolKey{
			Language:      lang,
			Kind:          SymbolKind(kind),
			Namespace:     namespace,
			Receiver:      recv,
			Name:          name,
			Disambiguator: disambiguator,
		}
		id := SymbolID(key)
		parts := strings.Split(id, "#")
		if len(parts) != 7 {
			t.Fatalf("SymbolID(%+v) = %q split to %d parts, want 7", key, id, len(parts))
		}
		for _, part := range parts[1:] {
			if strings.Contains(part, "#") {
				t.Fatalf("encoded SymbolID part %q contains separator", part)
			}
		}
		got, ok := decodeSymbolID(id)
		if !ok {
			t.Fatalf("decodeSymbolID(%q) failed", id)
		}
		if got != key {
			t.Errorf("SymbolID(%+v) = %q failed round-trip: %+v", key, id, got)
		}
	})
}

func BenchmarkSymbolID(b *testing.B) {
	key := SymbolKey{
		Language:  "go",
		Kind:      SymbolKindMethod,
		Namespace: "github.com/kstruzzieri/go-llm/rag",
		Receiver:  "Indexer",
		Name:      "IndexFile",
	}
	for i := 0; i < b.N; i++ {
		_ = SymbolID(key)
	}
}

func TestSymbolNodeZeroValue(t *testing.T) {
	var n SymbolNode
	if n.Kind != "" {
		t.Errorf("zero SymbolNode.Kind = %q, want \"\" (Kind is left unset on zero value)", n.Kind)
	}
	if n.Kind == SymbolKindUnknown {
		t.Errorf("zero SymbolNode.Kind should NOT equal SymbolKindUnknown (%q); unknown is an explicit value, not the default", SymbolKindUnknown)
	}
	if n.Declaration != "" {
		t.Errorf("zero SymbolNode.Declaration = %q, want empty", n.Declaration)
	}
}

func TestSymbolGraphZeroValue(t *testing.T) {
	var g SymbolGraph
	if len(g.Nodes) != 0 || len(g.Calls) != 0 {
		t.Errorf("zero SymbolGraph should have empty slices, got nodes=%d calls=%d", len(g.Nodes), len(g.Calls))
	}
	if g.Scope != "" || g.VectorSpaceID != "" || g.ExtractionSignature != "" || g.Root != "" {
		t.Errorf("zero SymbolGraph should have empty scope+vsid+signature+root, got scope=%q vsid=%q signature=%q root=%q",
			g.Scope, g.VectorSpaceID, g.ExtractionSignature, g.Root)
	}
}

func TestSymbolKindConstants(t *testing.T) {
	// Pin the wire encoding for every defined kind. If any of these
	// strings changes, serialized graphs from older indexes will fail
	// to round-trip — make the change loud.
	wireFormat := map[SymbolKind]string{
		SymbolKindUnknown:   "unknown",
		SymbolKindFunction:  "function",
		SymbolKindMethod:    "method",
		SymbolKindStruct:    "struct",
		SymbolKindInterface: "interface",
		SymbolKindVar:       "var",
		SymbolKindConst:     "const",
		SymbolKindType:      "type",
	}
	for k, want := range wireFormat {
		if got := string(k); got != want {
			t.Errorf("SymbolKind wire format drift: got %q, want %q", got, want)
		}
	}
}

func TestCallResolutionConstants(t *testing.T) {
	wireFormat := map[CallResolution]string{
		CallResolutionResolved:     "resolved",
		CallResolutionUnresolved:   "unresolved",
		CallResolutionNotAttempted: "not_attempted",
	}
	for k, want := range wireFormat {
		if got := string(k); got != want {
			t.Errorf("CallResolution wire format drift: got %q, want %q", got, want)
		}
	}
}

func TestCallEdgeValidate(t *testing.T) {
	validResolved := CallEdge{
		CallerID:   "caller",
		CalleeID:   "callee",
		CalleeRaw:  "Do",
		Resolution: CallResolutionResolved,
		File:       "x.go",
		Line:       10,
	}
	if err := validResolved.Validate(); err != nil {
		t.Fatalf("valid resolved edge failed validation: %v", err)
	}

	validUnresolved := CallEdge{
		CallerID:   "caller",
		CalleeRaw:  "unknown.Do",
		Resolution: CallResolutionUnresolved,
		File:       "x.go",
		Line:       11,
	}
	if err := validUnresolved.Validate(); err != nil {
		t.Fatalf("valid unresolved edge failed validation: %v", err)
	}

	tests := []struct {
		name string
		edge CallEdge
	}{
		{
			name: "missing caller",
			edge: CallEdge{CalleeID: "callee", Resolution: CallResolutionResolved, Line: 1},
		},
		{
			name: "invalid line",
			edge: CallEdge{CallerID: "caller", CalleeID: "callee", Resolution: CallResolutionResolved},
		},
		{
			name: "resolved without callee id",
			edge: CallEdge{CallerID: "caller", Resolution: CallResolutionResolved, Line: 1},
		},
		{
			name: "unresolved with callee id",
			edge: CallEdge{CallerID: "caller", CalleeID: "callee", Resolution: CallResolutionUnresolved, Line: 1},
		},
		{
			name: "unknown resolution",
			edge: CallEdge{CallerID: "caller", Resolution: CallResolution("maybe"), Line: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.edge.Validate()
			if !errors.Is(err, ErrInvalidGraph) {
				t.Fatalf("Validate error = %v, want ErrInvalidGraph", err)
			}
		})
	}
}
