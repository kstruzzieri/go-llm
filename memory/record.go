// Package memory provides explicit, user-controlled agent memory backed by
// SQLite. Memories are short user-authored notes (preferences, project
// conventions) scoped either globally (per user) or to a single workspace, and
// are searched by keyword (FTS5/bm25). It is intentionally separate from
// document RAG and conversation storage.
package memory

import (
	"errors"
	"time"
)

// Scope controls a memory's visibility.
type Scope string

const (
	// ScopeGlobal memories are visible in every workspace.
	ScopeGlobal Scope = "global"
	// ScopeWorkspace memories are visible only in their originating workspace.
	ScopeWorkspace Scope = "workspace"
)

// Memory is the single record shape shared by storage, the search tool, and
// golem's /memories display.
type Memory struct {
	ID              string
	Text            string
	Scope           Scope
	WorkspaceID     string // "" when Scope == ScopeGlobal
	SourceSessionID string // provenance; "" if unknown / --no-session
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       time.Time // zero == live
}

// AddParams are the inputs to Add.
type AddParams struct {
	Text            string
	Scope           Scope
	WorkspaceID     string // required iff Scope == ScopeWorkspace; forced "" for global
	SourceSessionID string
}

// ListOptions scopes List to global + the given workspace.
type ListOptions struct {
	WorkspaceID string
}

// SearchOptions scopes and bounds Search.
type SearchOptions struct {
	WorkspaceID string
	Limit       int // <= 0 => default 8
}

// Sentinel errors.
var (
	ErrNotFound  = errors.New("memory: not found")
	ErrAmbiguous = errors.New("memory: id prefix matches multiple memories")
	ErrEmptyText = errors.New("memory: text must not be empty")
	ErrBadScope  = errors.New("memory: invalid scope")
)
