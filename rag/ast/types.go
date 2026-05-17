// Package ast models a structural symbol graph that lives alongside the
// rag chunk store. Where rag's chunks carry only text + position, an [Extractor]
// produces a [SymbolGraph] of named program entities (functions, methods,
// types, top-level bindings) with edges (call relationships) between them.
// Together a chunk store and a symbol graph give the retriever both lexical
// recall and structural reach.
//
// # Drift signature
//
// Every [SymbolGraph] is stamped with a VectorSpaceID — an opaque string
// identifying the vector space the parallel chunk store was written for.
// When the chunk store re-embeds under a new vector space, the symbol graph
// must be re-extracted and re-stamped: nodes and edges from the old vsid
// become unreachable in one atomic step. This invariant exists at the graph
// level only; individual nodes do not carry their own vsid.
//
// # Storage shape
//
// [SymbolGraph] is a write-only transport structure: an extractor produces
// one, a [SymbolStore] consumes one. Consumers MUST NOT traverse the graph
// by linear scan of its slices — the store builds the indexes needed for
// O(1) lookup and exposes them via its read methods. The graph itself is
// permitted to be unindexed and unsorted.
//
// # Trust boundary
//
// [SymbolNode.Signature] and [SymbolNode.Doc] carry raw source content from
// arbitrary files an extractor was pointed at. Extractors MUST NOT
// pre-sanitize them — the data is structural truth. Any consumer that
// surfaces these fields to an LLM (MCP tool outputs, chat prompts) owns
// the sanitization required to mitigate prompt injection from hostile
// source comments. This package guarantees fidelity, not safety.
package ast

// SymbolKind is the structural category of a [SymbolNode]. Typed string
// (not iota) so the value survives JSON round-trips without a custom
// marshaler.
type SymbolKind string

const (
	SymbolKindUnknown   SymbolKind = "unknown"
	SymbolKindFunction  SymbolKind = "function"
	SymbolKindMethod    SymbolKind = "method"
	SymbolKindStruct    SymbolKind = "struct"
	SymbolKindInterface SymbolKind = "interface"
	SymbolKindVar       SymbolKind = "var"
	SymbolKindConst     SymbolKind = "const"
	SymbolKindType      SymbolKind = "type"
)

// SymbolNode is a single resolved program entity. ID is the stable identity
// used as the endpoint of every edge in the graph; see [SymbolID]. The
// node does NOT carry its VectorSpaceID — the enclosing [SymbolGraph] owns
// that for the whole batch.
//
// Signature and Doc contain raw, unsanitized source content; see the
// package-level "Trust boundary" note.
type SymbolNode struct {
	ID        string
	Kind      SymbolKind
	Name      string // bare identifier (e.g. "ValidateToken")
	Receiver  string // method receiver type, empty for non-methods
	PkgPath   string // canonical Go module path (per go.mod major-version rules)
	File      string // canonicalized per [SymbolGraph.Root]
	StartLine int    // 1-indexed
	EndLine   int    // 1-indexed, inclusive
	Signature string // formatted func signature or type definition (raw)
	Doc       string // associated godoc comment if any (raw)
}

// CallEdge represents a call relationship between two symbols. Three states
// are representable via (CalleeID, Resolved):
//
//	Resolved=true,  CalleeID!="" — extractor identified the target SymbolID
//	Resolved=false, CalleeID==""  — extractor attempted resolution and failed
//	Resolved=false, CalleeID==""  — extractor did not attempt (heuristic mode)
//
// (The latter two states are presently indistinguishable; M2's go/types
// integration will introduce a third boolean if the distinction becomes
// load-bearing.)
type CallEdge struct {
	CallerID  string
	CalleeID  string // resolved SymbolID; empty when Resolved=false
	CalleeRaw string // unresolved identifier as it appeared in source
	Resolved  bool   // true iff the extractor produced a valid CalleeID
	File      string // canonicalized per [SymbolGraph.Root]
	Line      int    // 1-indexed
}

// SymbolGraph is the result of a single extraction pass — a transport
// structure between an [Extractor] and a [SymbolStore]. Callers consume
// graphs by handing them to a store, not by traversal.
//
// Root anchors all File fields in Nodes and Calls. Paths inside the graph
// are filepath.Clean'd with forward slashes, relative to Root.
type SymbolGraph struct {
	VectorSpaceID string // drift signature stamped on every persisted row
	Root          string // anchor for SymbolNode.File and CallEdge.File
	Nodes         []SymbolNode
	Calls         []CallEdge
}

// symbolIDSep separates the three components of a SymbolID. '#' is
// forbidden in Go module paths (per golang.org/ref/mod) and in Go
// identifiers (per the language spec). Using the same separator at both
// boundaries makes the encoding a bijection for every input that does
// not itself contain '#'.
const symbolIDSep = "#"

// SymbolID derives the stable identity used as the endpoint of every edge.
// The encoding is always three components joined by '#':
//
//	free function: "<pkgPath>##<name>"   (empty receiver slot)
//	method:        "<pkgPath>#<receiver>#<name>"
//
// Because '#' cannot appear in a well-formed Go module path or Go
// identifier, the encoding is a bijection: distinct inputs produce
// distinct outputs, and the parts can be recovered with strings.Split.
//
// Inputs are NOT validated. Callers that pass strings containing '#'
// get malformed IDs back; the function does not panic.
//
// TODO(keith): revisit in the milestone that introduces a real Go
// extractor — generics (type parameters), pointer vs. value receivers,
// anonymous functions, and methods promoted via embedding all need
// explicit policies. The current form is the minimum needed to make
// non-pathological symbols round-trip through tests.
func SymbolID(pkgPath, receiver, name string) string {
	return pkgPath + symbolIDSep + receiver + symbolIDSep + name
}
