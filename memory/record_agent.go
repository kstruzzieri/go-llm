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

	"github.com/kstruzzieri/go-llm/signing"
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

// MemoryRecordDomain separates canonical record bodies from other signed data.
const MemoryRecordDomain = "go-llm/memory-record/v1"

// RecordTrustClass describes a record's origin, never instruction authority.
type RecordTrustClass string

const (
	// TrustAgentWritten marks records accepted through an ordinary writer surface.
	TrustAgentWritten RecordTrustClass = "agent-written"
	// TrustLegacyUnreviewed marks an imported snapshot with unverified authorship.
	TrustLegacyUnreviewed RecordTrustClass = "legacy-unreviewed"
)

// Provenance records server-stamped origin and descriptive source claims.
// Source fields are opaque claims, never authorization evidence. The range is
// half-open [Start, End); 0 means unset.
type Provenance struct {
	SourceKind      string           `json:"source_kind"`
	SourceID        string           `json:"source_id"`
	Start           int              `json:"source_start"`
	End             int              `json:"source_end"`
	Hash            string           `json:"source_hash"`
	OriginTool      string           `json:"origin_tool"`
	OriginSessionID string           `json:"origin_session_id"`
	TrustClass      RecordTrustClass `json:"trust_class"`
}

// MemoryRecordBody owns every signed field of an agent-memory record.
// Visibility is derived: WorkspaceID=="" is global; WorkspaceID set is
// workspace-private; SessionID set is session-private (and requires WorkspaceID).
type MemoryRecordBody struct {
	ID          string          `json:"id"`
	Kind        MemoryKind      `json:"kind"`
	Content     string          `json:"content"`
	Namespace   string          `json:"namespace"`
	WorkspaceID string          `json:"workspace_id"`
	SessionID   string          `json:"session_id"`
	Provenance  Provenance      `json:"provenance"`
	Metadata    json.RawMessage `json:"metadata"` // opaque valid JSON value; "{}" when empty on write
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	ExpiresAt   time.Time       `json:"expires_at"` // zero = never expires
	DeletedAt   time.Time       `json:"deleted_at"` // zero = live
}

// MemoryRecord is one agent-memory body and its detached signature.
type MemoryRecord struct {
	MemoryRecordBody `json:"body"`
	Signature        signing.Signature `json:"signature"`
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
	ErrRecordIntegrity       = errors.New("memory: record integrity check failed")
	ErrContentTooLong        = errors.New("memory: content exceeds 4096 bytes")
	ErrRecordTooLarge        = errors.New("memory: record exceeds 32 KiB")
	ErrRecordNotFound        = errors.New("memory: record not found")
	ErrEmptyContent          = errors.New("memory: content must not be empty")
	ErrBadKind               = errors.New("memory: invalid kind")
	ErrBadMetadata           = errors.New("memory: metadata must be valid JSON")
	ErrSessionNeedsWorkspace = errors.New("memory: session-scoped record requires workspace_id")
	ErrWorkingNeedsSession   = errors.New("memory: working memory requires session_id")
	ErrBadPromotion          = errors.New("memory: promote target must be semantic or episodic")
	ErrBadProvenanceRange    = errors.New("memory: provenance end must be >= start")
)
