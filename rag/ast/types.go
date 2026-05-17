// Package ast models a structural symbol graph that lives alongside the rag
// chunk store. Where rag's chunks carry only text + position, an [Extractor]
// produces a [SymbolGraph] of named program entities (functions, methods,
// types, top-level bindings) with edges (call relationships) between them.
// Together a chunk store and a symbol graph give the retriever both lexical
// recall and structural reach.
//
// # Scope and drift signature
//
// Every [SymbolGraph] is stamped with both Scope and VectorSpaceID. Scope is
// the caller-defined corpus/root/index namespace; VectorSpaceID is the opaque
// embedding-space identity used by the parallel chunk store. Stores key graph
// rows by (scope, vectorSpaceID), never by vectorSpaceID alone, so two corpora
// indexed with the same embedder cannot delete or read each other's graph.
//
// When the chunk store re-embeds under a new vector space, the symbol graph
// must be re-extracted and re-stamped. Nodes and edges from the old vsid become
// unreachable within the same scope in one atomic step. This invariant exists
// at the graph level only; individual nodes do not carry their own scope/vsid.
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
// [SymbolNode.Declaration] and [SymbolNode.Doc] carry raw source content from
// arbitrary files an extractor was pointed at. Extractors MUST NOT
// pre-sanitize them — the data is structural truth. Any consumer that
// surfaces these fields to an LLM (MCP tool outputs, chat prompts) owns
// the sanitization required to mitigate prompt injection from hostile
// source comments. This package guarantees fidelity, not safety.
package ast

import (
	"encoding/base64"
	"fmt"
	"strings"
)

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

// CallResolution records how much work an extractor did to identify a call's
// target. The zero value is invalid; use one of the constants below.
type CallResolution string

const (
	CallResolutionResolved     CallResolution = "resolved"
	CallResolutionUnresolved   CallResolution = "unresolved"
	CallResolutionNotAttempted CallResolution = "not_attempted"
)

// SymbolKey is the language-neutral identity material encoded by [SymbolID].
// Namespace is the language's package/module/scope path. Disambiguator is an
// optional, language-specific stable suffix for overloads, local functions,
// anonymous functions, or any construct that cannot be uniquely named by the
// other fields alone. Go extractors normally leave Disambiguator empty.
type SymbolKey struct {
	Language      string
	Kind          SymbolKind
	Namespace     string
	Receiver      string
	Name          string
	Disambiguator string
}

// SymbolNode is a single resolved program entity. ID is the stable identity
// used as the endpoint of every edge in the graph; see [SymbolID]. The
// node does NOT carry its Scope or VectorSpaceID — the enclosing [SymbolGraph]
// owns those for the whole batch.
//
// Declaration and Doc contain raw, unsanitized source content; see the
// package-level "Trust boundary" note.
type SymbolNode struct {
	ID            string
	Language      string     // rag.Chunk.Language token (e.g. "go", "python")
	Kind          SymbolKind // structural category
	Namespace     string     // package/module/scope path in the source language
	Name          string     // bare identifier (e.g. "ValidateToken")
	Receiver      string     // method receiver/class/type, empty for non-methods
	Disambiguator string     // optional stable suffix for overloads/local symbols
	File          string     // canonicalized per [SymbolGraph.Root]
	StartLine     int        // 1-indexed
	EndLine       int        // 1-indexed, inclusive
	Declaration   string     // formatted declaration/signature/type definition (raw)
	Doc           string     // associated source doc/comment if any (raw)
}

// CallEdge represents a call relationship between two symbols. Resolution
// makes the extraction state explicit:
//
//	CallResolutionResolved     — extractor identified CalleeID
//	CallResolutionUnresolved   — extractor attempted resolution and failed
//	CallResolutionNotAttempted — extractor recorded a heuristic/raw call only
type CallEdge struct {
	CallerID   string
	CalleeID   string         // resolved SymbolID; required only when resolved
	CalleeRaw  string         // identifier/expression as it appeared in source
	Resolution CallResolution // explicit resolution state
	File       string         // canonicalized per [SymbolGraph.Root]
	Line       int            // 1-indexed
}

// Validate checks the defensive invariants a store must enforce before
// persisting an edge.
func (e CallEdge) Validate() error {
	if e.CallerID == "" {
		return fmt.Errorf("%w: call edge missing caller id", ErrInvalidGraph)
	}
	if e.Line < 1 {
		return fmt.Errorf("%w: call edge line must be >= 1", ErrInvalidGraph)
	}
	switch e.Resolution {
	case CallResolutionResolved:
		if e.CalleeID == "" {
			return fmt.Errorf("%w: resolved call edge missing callee id", ErrInvalidGraph)
		}
	case CallResolutionUnresolved, CallResolutionNotAttempted:
		if e.CalleeID != "" {
			return fmt.Errorf("%w: unresolved call edge must not carry callee id", ErrInvalidGraph)
		}
	default:
		return fmt.Errorf("%w: unknown call resolution %q", ErrInvalidGraph, e.Resolution)
	}
	return nil
}

// SymbolGraph is the result of a single extraction pass — a transport
// structure between an [Extractor] and a [SymbolStore]. Callers consume
// graphs by handing them to a store, not by traversal.
//
// Root anchors all File fields in Nodes and Calls. Paths inside the graph
// are filepath.Clean'd with forward slashes, relative to Root.
type SymbolGraph struct {
	Scope               string // caller-defined corpus/root/index namespace
	VectorSpaceID       string // embedding-space identity stamped on persisted rows
	ExtractionSignature string // opaque token persisted for Extractor.Stale
	Root                string // anchor for SymbolNode.File and CallEdge.File
	Nodes               []SymbolNode
	Calls               []CallEdge
}

// symbolIDSep separates base64url-encoded SymbolID components. '#' is not in
// the raw URL-safe base64 alphabet, so arbitrary component text remains a
// bijection without relying on any one language's identifier grammar.
const symbolIDSep = "#"

const symbolIDPrefix = "symbol-v1"

// SymbolID derives the stable identity used as the endpoint of every edge.
// It is language-neutral: each key component is base64url encoded before
// joining, so names containing punctuation, separators, or non-Go syntax do
// not collide. Extractors are responsible for filling Disambiguator whenever
// their language can have multiple distinct symbols with the same language,
// kind, namespace, receiver, and name.
func SymbolID(key SymbolKey) string {
	parts := []string{
		symbolIDPrefix,
		encodeSymbolIDPart(key.Language),
		encodeSymbolIDPart(string(key.Kind)),
		encodeSymbolIDPart(key.Namespace),
		encodeSymbolIDPart(key.Receiver),
		encodeSymbolIDPart(key.Name),
		encodeSymbolIDPart(key.Disambiguator),
	}
	return strings.Join(parts, symbolIDSep)
}

func encodeSymbolIDPart(part string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(part))
}

func decodeSymbolID(id string) (SymbolKey, bool) {
	parts := strings.Split(id, symbolIDSep)
	if len(parts) != 7 || parts[0] != symbolIDPrefix {
		return SymbolKey{}, false
	}
	decoded := make([]string, 0, 6)
	for _, part := range parts[1:] {
		raw, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			return SymbolKey{}, false
		}
		decoded = append(decoded, string(raw))
	}
	return SymbolKey{
		Language:      decoded[0],
		Kind:          SymbolKind(decoded[1]),
		Namespace:     decoded[2],
		Receiver:      decoded[3],
		Name:          decoded[4],
		Disambiguator: decoded[5],
	}, true
}
