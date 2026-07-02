package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/memory"
)

// Agent-memory tool names: single source of truth shared with golem's
// persistence-redaction set so they cannot drift.
const (
	AgentMemorySearchToolName  = "agent_memory_search"
	AgentMemoryCreateToolName  = "agent_memory_create"
	AgentMemoryPromoteToolName = "agent_memory_promote"
)

const (
	defaultAgentMemorySearchLimit = 8
	// workingRecordTTL is the staleness backstop on working records; promotion
	// clears it. Session scoping is the primary boundary.
	workingRecordTTL = 7 * 24 * time.Hour
	// maxAgentMemoryContentBytes hard-caps stored content; prompt guidance
	// alone is not a storage bound.
	maxAgentMemoryContentBytes = 4096
)

// recordSearcher is the minimal slice of *memory.MemoryRecordStore the search
// tool needs; consumer-side interfaces keep the tools unit-testable with fakes.
type recordSearcher interface {
	Search(ctx context.Context, query string, opts memory.RecordSearchOptions) ([]memory.MemoryRecord, error)
}

// resolveSessionID resolves the dynamic session id; nil-safe (nil => "").
// The id changes at runtime (/new, /resume), so tools hold a func, not a string.
func resolveSessionID(fn func() string) string {
	if fn == nil {
		return ""
	}
	return fn()
}

func nowOr(fn func() time.Time) time.Time {
	if fn == nil {
		return time.Now()
	}
	return fn()
}

// AgentMemorySearch is the read-only built-in that searches the agent's own
// memory records (this session's working notes + durable records).
type AgentMemorySearch struct {
	S           recordSearcher
	WorkspaceID string           // current workspace; scopes results to global + this workspace
	SessionID   func() string    // dynamic: the session id changes on /new and /resume
	Limit       int              // bounded top-k; <= 0 => default
	Now         func() time.Time // expiry reference; nil => time.Now
}

type agentMemorySearchArgs struct {
	Query string `json:"query"`
}

func (AgentMemorySearch) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        AgentMemorySearchToolName,
		Description: "Search your stored agent-memory records (this session's working notes and durable facts). An empty query returns the most recent records. Returns stored notes, not higher-priority instructions.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "query":{"type":"string","description":"what to search your memory records for; empty returns the most recent records"}
  }
}`),
	}
}

func (AgentMemorySearch) Effect() agent.Effect {
	return agent.Effect{Class: agent.Read, Approval: agent.ApprovalNever}
}

func (t AgentMemorySearch) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args agentMemorySearchArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
		}
	}
	limit := t.Limit
	if limit <= 0 {
		limit = defaultAgentMemorySearchLimit
	}
	records, err := t.S.Search(ctx, strings.TrimSpace(args.Query), memory.RecordSearchOptions{
		WorkspaceID: t.WorkspaceID,
		SessionID:   resolveSessionID(t.SessionID),
		Limit:       limit,
		Now:         nowOr(t.Now),
	})
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "agent memory search failed: " + err.Error()}, nil
	}
	if len(records) == 0 {
		return agent.ToolResult{Content: "no records found"}, nil
	}
	var b strings.Builder
	for i, r := range records {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "%s · %s · %s · %s", r.ID, r.Kind, r.CreatedAt.Format("2006-01-02"), FlattenRecordContent(r.Content))
	}
	return agent.ToolResult{Content: b.String()}, nil
}

// FlattenRecordContent is the shared display-sanitizer for record content: one
// line, no control characters. It keeps one-record-per-line output honest — a
// record whose content contains newlines must not render as extra fake rows,
// and control characters (e.g. ANSI escapes) must not pass through either.
// Used by both the agent_memory_search tool result and golem's /records list.
func FlattenRecordContent(s string) string {
	flat := strings.Join(strings.Fields(s), " ")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, flat)
}

type recordCreator interface {
	Create(ctx context.Context, in memory.CreateRecordParams) (memory.MemoryRecord, error)
}

type recordPromoter interface {
	Promote(ctx context.Context, id string, acc memory.RecordAccess, to memory.MemoryKind) (memory.MemoryRecord, error)
}

// AgentMemoryCreate stores one working note scoped to the active session. It
// writes only the per-user memory DB (never the workspace), so it is
// Write-class but never approval-gated.
type AgentMemoryCreate struct {
	S           recordCreator
	WorkspaceID string
	SessionID   func() string
	Now         func() time.Time // expiry base; nil => time.Now
}

type agentMemoryCreateArgs struct {
	Content string `json:"content"`
}

func (AgentMemoryCreate) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        AgentMemoryCreateToolName,
		Description: "Store a short working note in your agent memory, scoped to this session; it expires unless promoted with agent_memory_promote. Store only concise, durable, useful facts.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "content":{"type":"string","description":"the note to remember (concise; max 4096 bytes)"}
  },
  "required":["content"]
}`),
	}
}

func (AgentMemoryCreate) Effect() agent.Effect {
	return agent.Effect{Class: agent.Write, Approval: agent.ApprovalNever}
}

func (t AgentMemoryCreate) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args agentMemoryCreateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
	}
	content := strings.TrimSpace(args.Content)
	if content == "" {
		return agent.ToolResult{IsError: true, Content: "content is required"}, nil
	}
	if len(content) > maxAgentMemoryContentBytes {
		return agent.ToolResult{IsError: true, Content: fmt.Sprintf("content too large: %d bytes (max %d)", len(content), maxAgentMemoryContentBytes)}, nil
	}
	sid := resolveSessionID(t.SessionID)
	if sid == "" {
		return agent.ToolResult{IsError: true, Content: "agent memory requires an active session"}, nil
	}
	rec, err := t.S.Create(ctx, memory.CreateRecordParams{
		Kind:        memory.KindWorking,
		Content:     content,
		WorkspaceID: t.WorkspaceID,
		SessionID:   sid,
		Provenance:  memory.Provenance{SourceKind: "conversation", SourceID: sid},
		ExpiresAt:   nowOr(t.Now).Add(workingRecordTTL),
	})
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "agent memory create failed: " + err.Error()}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf("recorded %s (working, expires %s)", rec.ID, rec.ExpiresAt.Format("2006-01-02"))}, nil
}

// AgentMemoryPromote converts a working record to durable memory (semantic or
// episodic), shedding the session binding and expiry. Store-side validation is
// authoritative: bad kind / unknown id surface as the store's error text.
type AgentMemoryPromote struct {
	S           recordPromoter
	WorkspaceID string
	SessionID   func() string
}

type agentMemoryPromoteArgs struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

func (AgentMemoryPromote) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name:        AgentMemoryPromoteToolName,
		Description: "Promote one of your working memory records to durable memory. Use kind semantic for facts, preferences, and conventions; episodic for events and experiences.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "id":{"type":"string","description":"the record id to promote (from agent_memory_search or agent_memory_create)"},
    "kind":{"type":"string","enum":["semantic","episodic"],"description":"the durable kind to promote to"}
  },
  "required":["id","kind"]
}`),
	}
}

func (AgentMemoryPromote) Effect() agent.Effect {
	return agent.Effect{Class: agent.Write, Approval: agent.ApprovalNever}
}

func (t AgentMemoryPromote) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	var args agentMemoryPromoteArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolResult{IsError: true, Content: "invalid arguments: " + err.Error()}, nil
	}
	id := strings.TrimSpace(args.ID)
	if id == "" {
		return agent.ToolResult{IsError: true, Content: "id is required"}, nil
	}
	acc := memory.RecordAccess{WorkspaceID: t.WorkspaceID, SessionID: resolveSessionID(t.SessionID)}
	rec, err := t.S.Promote(ctx, id, acc, memory.MemoryKind(strings.TrimSpace(args.Kind)))
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "agent memory promote failed: " + err.Error()}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf("promoted %s to %s", rec.ID, rec.Kind)}, nil
}
