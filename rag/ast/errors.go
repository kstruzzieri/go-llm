package ast

import "errors"

// Sentinel errors. Distinct from rag's own sentinels because the recovery
// action diverges: rag/ vsid mismatches mean "re-embed"; rag/ast vsid
// mismatches mean "re-extract the AST graph". Both can happen independently
// when callers mix-and-match stores.
//
// Error strings are namespaced "rag/ast: ..." to match the full import path
// and disambiguate from go/ast in log aggregators.
var (
	// ErrSymbolNotFound indicates GetSymbol or SymbolEnclosing could not
	// resolve the requested symbol or location to a node in the store.
	ErrSymbolNotFound = errors.New("rag/ast: symbol not found")

	// ErrVectorSpaceMismatch indicates a read attempted to use a vsid not
	// present in the store. Distinct from rag.ErrVectorSpaceMismatch
	// because the symbol graph may drift independently of the chunk store
	// during partial migrations.
	ErrVectorSpaceMismatch = errors.New("rag/ast: vector-space mismatch")

	// ErrInvalidGraph indicates a graph or edge violates the SymbolStore
	// persistence contract.
	ErrInvalidGraph = errors.New("rag/ast: invalid graph")

	// ErrInvalidArgument indicates a caller supplied an invalid read argument
	// such as a negative limit.
	ErrInvalidArgument = errors.New("rag/ast: invalid argument")
)
