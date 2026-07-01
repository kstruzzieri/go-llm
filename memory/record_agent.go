// This file adds the agent-memory record surface (MemoryRecord /
// MemoryRecordStore): typed working/semantic/episodic records with provenance,
// expiration, scoped FTS search, and opt-in promotion. It is a sibling of the
// user-authored Memory surface in this package and shares the same SQLite
// migration chain, but owns separate tables (memory_records/_fts). It imports
// neither conversation/ nor rag/: agent memory, conversation persistence, and
// document RAG stay separate storage concepts.
package memory

import (
	"encoding/json"
	"errors"
	"time"
)

// MemoryKind is the record's memory semantics, orthogonal to visibility.
type MemoryKind string

const (
	// KindWorking is session-scoped, typically expiring memory; the promotion source.
	KindWorking MemoryKind = "working"
	// KindSemantic is a durable fact, preference, or convention.
	KindSemantic MemoryKind = "semantic"
	// KindEpisodic is a durable event or experience.
	KindEpisodic MemoryKind = "episodic"
)

// Provenance links a record back to what produced it. All fields are opaque to
// this package. The range is half-open [Start, End); 0 means unset.
type Provenance struct {
	SourceKind string // e.g. "conversation" | "tool" | "user"
	SourceID   string // conversation/session id, tool name, ...
	Start      int
	End        int
	Hash       string // optional content fingerprint
}

// MemoryRecord is one agent-memory row. Visibility is derived: WorkspaceID=="" is
// global; WorkspaceID set is workspace-private; SessionID set is session-private
// (and requires WorkspaceID).
type MemoryRecord struct {
	ID          string
	Kind        MemoryKind
	Content     string
	Namespace   string
	WorkspaceID string
	SessionID   string
	Provenance  Provenance
	Metadata    json.RawMessage // opaque valid JSON value; "{}" when empty
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time // zero = never expires
	DeletedAt   time.Time // zero = live
}

// CreateRecordParams are the inputs to Create.
type CreateRecordParams struct {
	Kind        MemoryKind
	Content     string
	Namespace   string
	WorkspaceID string
	SessionID   string
	Provenance  Provenance
	Metadata    json.RawMessage // nil/empty normalized to {}
	ExpiresAt   time.Time       // zero = never
}

// UpdateRecordParams mutates only safe fields. Kind, WorkspaceID, and SessionID
// are immutable here (Promote is the only kind-changer; re-scoping is deferred),
// so Update can never break a create-time invariant. A nil field is left as-is.
type UpdateRecordParams struct {
	Content   *string
	Namespace *string
	Metadata  json.RawMessage // nil = leave; non-nil empty normalized to {}
	ExpiresAt *time.Time      // nil = leave; point at zero-time to clear expiry
}

// RecordAccess is the visibility scope a caller presents to Get/Update/SoftDelete/
// Promote. A record not visible under this scope is ErrRecordNotFound (existence
// is never leaked across a workspace/session boundary).
type RecordAccess struct {
	WorkspaceID string
	SessionID   string
}

// RecordSearchOptions scopes, filters, and bounds Search.
type RecordSearchOptions struct {
	WorkspaceID    string
	SessionID      string
	Kind           MemoryKind // "" = any
	Namespace      string     // "" = any
	Limit          int        // <= 0 => default 8
	Now            time.Time  // expiry reference; zero => time.Now()
	IncludeExpired bool
}

// Sentinel errors for the agent-memory record surface.
var (
	ErrRecordNotFound        = errors.New("memory: record not found")
	ErrEmptyContent          = errors.New("memory: content must not be empty")
	ErrBadKind               = errors.New("memory: invalid kind")
	ErrBadMetadata           = errors.New("memory: metadata must be valid JSON")
	ErrSessionNeedsWorkspace = errors.New("memory: session-scoped record requires workspace_id")
	ErrWorkingNeedsSession   = errors.New("memory: working memory requires session_id")
	ErrBadPromotion          = errors.New("memory: promote target must be semantic or episodic")
	ErrBadProvenanceRange    = errors.New("memory: provenance end must be >= start")
)
