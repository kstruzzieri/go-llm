package ast

import (
	"strings"
	"testing"
)

func TestSymbolID(t *testing.T) {
	tests := []struct {
		name     string
		pkgPath  string
		receiver string
		symbol   string
		want     string
	}{
		{
			name:    "free function",
			pkgPath: "github.com/kstruzzieri/go-llm/rag",
			symbol:  "NewIndexer",
			want:    "github.com/kstruzzieri/go-llm/rag##NewIndexer",
		},
		{
			name:     "method",
			pkgPath:  "github.com/kstruzzieri/go-llm/rag",
			receiver: "Indexer",
			symbol:   "IndexFile",
			want:     "github.com/kstruzzieri/go-llm/rag#Indexer#IndexFile",
		},
		{
			name:    "main package free function",
			pkgPath: "main",
			symbol:  "main",
			want:    "main##main",
		},
		{
			// Pathological input from the criticize-review collision
			// argument: pkgPath has a dot, receiver and name look like
			// a method on a dotted-receiver type. The '#' boundaries
			// keep this distinct from any free-function form with the
			// same characters.
			name:     "dotted pkgPath method",
			pkgPath:  "github.com/foo.bar/baz",
			receiver: "Type",
			symbol:   "Method",
			want:     "github.com/foo.bar/baz#Type#Method",
		},
		{
			name:   "empty pkgPath",
			symbol: "Foo",
			want:   "##Foo",
		},
		{
			name:    "empty name",
			pkgPath: "pkg",
			want:    "pkg##",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SymbolID(tt.pkgPath, tt.receiver, tt.symbol)
			if got != tt.want {
				t.Errorf("SymbolID(%q, %q, %q) = %q, want %q", tt.pkgPath, tt.receiver, tt.symbol, got, tt.want)
			}
		})
	}
}

// TestSymbolIDNoCollisionAcrossDottedPkgPaths pins the collision-resistance
// property that motivated the '#' separator choice. Includes the exact
// cases the criticize-review identified as collision risks under the old
// dot-separator encoding.
func TestSymbolIDNoCollisionAcrossDottedPkgPaths(t *testing.T) {
	cases := [][3]string{
		{"foo.bar", "", "baz"},
		{"foo", "bar", "baz"},
		{"foo", "", "bar.baz"},
		{"foo.bar.baz", "", "x"},
		{"foo.bar", "baz", "qux"},
		{"foo", "bar.baz", "qux"}, // even pathological dots-in-receiver are safe
	}
	seen := make(map[string][3]string, len(cases))
	for _, c := range cases {
		id := SymbolID(c[0], c[1], c[2])
		if prev, ok := seen[id]; ok {
			t.Errorf("SymbolID collision: %v and %v both produced %q", prev, c, id)
		}
		seen[id] = c
	}
}

// FuzzSymbolID asserts the bijection property: for any (pkg, recv, name)
// where no component contains '#', the resulting ID splits back into the
// original three components on '#'.
func FuzzSymbolID(f *testing.F) {
	f.Add("pkg", "", "Name")
	f.Add("github.com/x/y", "Recv", "Method")
	f.Add("pkg.with.dots", "", "Name")
	f.Add("", "", "")
	f.Add("foo.bar", "Type", "Method")
	f.Fuzz(func(t *testing.T, pkg, recv, name string) {
		// Contract: components MUST NOT contain '#' (the separator).
		// '#' is illegal in Go module paths and Go identifiers, so
		// well-formed extractor output always satisfies this.
		if strings.ContainsRune(pkg, '#') ||
			strings.ContainsRune(recv, '#') ||
			strings.ContainsRune(name, '#') {
			t.Skip()
		}
		id := SymbolID(pkg, recv, name)
		parts := strings.Split(id, "#")
		if len(parts) != 3 {
			t.Errorf("SymbolID(%q, %q, %q) = %q split to %d parts, want 3", pkg, recv, name, id, len(parts))
			return
		}
		if parts[0] != pkg || parts[1] != recv || parts[2] != name {
			t.Errorf("SymbolID(%q, %q, %q) = %q failed round-trip: parts=%v", pkg, recv, name, id, parts)
		}
	})
}

func BenchmarkSymbolID(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = SymbolID("github.com/kstruzzieri/go-llm/rag", "Indexer", "IndexFile")
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
}

func TestSymbolGraphZeroValue(t *testing.T) {
	var g SymbolGraph
	if len(g.Nodes) != 0 || len(g.Calls) != 0 {
		t.Errorf("zero SymbolGraph should have empty slices, got nodes=%d calls=%d", len(g.Nodes), len(g.Calls))
	}
	if g.VectorSpaceID != "" || g.Root != "" {
		t.Errorf("zero SymbolGraph should have empty vsid+root, got vsid=%q root=%q", g.VectorSpaceID, g.Root)
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
