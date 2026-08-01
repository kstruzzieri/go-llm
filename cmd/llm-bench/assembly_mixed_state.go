package main

// Mixed-assembly frozen-State builder (#331 slice 3c, Task 4): replays one
// validated mixedCase's event script through PRODUCTION tool producers
// (agent/tools.Retrieve over a seeded rag store, agent/tools.AgentMemorySearch
// over a seeded memory record store) into the frozen agent.State both arms
// will assemble from. This file imports agent packages;
// assembly_mixed_fixture.go (parse/validate) deliberately does not.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/contextdepth"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/provider"
)

// mixedDefaultOutputCap restates agent's unexported defaultOutputCap
// (agent/types.go): the normalized Effect.OutputCap dispatch stamps on a tool
// anchor whose Effect declares none (normalizeEffect, agent/tool.go).
// agent_memory_search's Effect leaves OutputCap zero and fixture_echo has no
// production Effect, so both anchor under this value on the production path.
// A zero OutputCap would be silently fatal downstream: mixed assembly copies
// it into the anchor byte cap and rejects EVERY alternative over it
// (agent/mixed_alloc.go tryAssign). Pinned by TestBuildMixedStateProductionCaps.
// internal/rageval/mixed_fixture.go mirrors this as mixedEvalOutputCap.
const mixedDefaultOutputCap = 64 << 10

// mixedMemoryEpoch pins memory record created_at/updated_at and the search
// tool's Now: assemblyFixedEpoch shifted to 12:00 UTC. The memory tool's
// recordLine/recordCard (agent/tools/agent_memory.go) render CreatedAt as a
// "2006-01-02" date in LOCAL time, so a midnight-UTC epoch renders as
// DIFFERENT calendar dates on a UTC CI runner vs a US workstation — and the
// rendered lines land verbatim in committed mixed traces, breaking
// byte-reproducibility by build machine. Noon UTC keeps every zone from
// UTC-11 through UTC+11 on the same calendar date; rageval's
// mixedEvalFixedEpoch (internal/rageval/mixed_fixture.go) is the same move.
// The RAG indexed_at pin stays at assemblyFixedEpoch: rag renders it as an
// RFC3339 UTC line, which is TZ-immune. Date stability is pinned by
// TestMixedMemoryEpochDateStable.
const mixedMemoryEpoch = assemblyFixedEpoch + 12*3600

// mixedMemorySessionID is the fixed session working-kind fixture records are
// scoped to. The memory store requires a session on working records
// (memory/record_store.go Create), and the search tool resolves the same
// session so those records stay visible — mirroring golem, where one live
// session both writes and searches its own working notes.
const mixedMemorySessionID = "mixed-fixture-session"

// Retrieve wiring values, pinned: they mirror golem's live construction
// (cmd/golem/tools.go: Retrieve{K: 5, MaxTokens: 2048, Progressive: ...}),
// and 2048 also equals agent/tools' unexported defaultRetrieveMaxTokens.
// Pinned by literal in TestBuildMixedStateProductionCaps so a drift here is a
// test failure, not a silently different production shape.
const (
	mixedRetrieveK         = 5
	mixedRetrieveMaxTokens = 2048
)

// mixedBuiltState is one case's frozen pre-assembly State plus the trace
// bookkeeping Task 5 stamps into AssemblyEval: RawStateTokens feeds the
// registered budget formula, StateDigest/CandidateIDs are the cross-arm
// pair-integrity keys, Subjects the ordered "(callID|domain|subjectID)"
// entries from the actually-attached ContextSets.
type mixedBuiltState struct {
	State          agent.State
	RawStateTokens int
	StateDigest    string
	CandidateIDs   []string
	Subjects       []string
}

// buildMixedStates builds every validated case's frozen State in declared
// order. Any single failure aborts the whole build: a fixture whose events
// cannot replay through the real tools is a fixture bug, never a skip.
func buildMixedStates(ctx context.Context, f mixedFixture) ([]mixedBuiltState, error) {
	states := make([]mixedBuiltState, 0, len(f.Cases))
	for _, c := range f.Cases {
		built, err := buildMixedState(ctx, c)
		if err != nil {
			return nil, err
		}
		states = append(states, built)
	}
	return states, nil
}

// buildMixedState replays one case's event script into its frozen State. The
// build is a pure function of the case: stores are seeded fresh in memory,
// every timestamp is pinned (assemblyFixedEpoch for rag, mixedMemoryEpoch for
// memory — see that const for the timezone split), and iteration only ever
// walks slices (the one set — candidate IDs — is sorted), so two builds of
// the same case are deep-equal with equal digests.
func buildMixedState(ctx context.Context, c mixedCase) (mixedBuiltState, error) {
	rig, err := seedAssemblyRig(ctx, c.RagSources)
	if err != nil {
		return mixedBuiltState{}, fmt.Errorf("case %q: seed rag store: %w", c.ID, err)
	}
	defer func() { _ = rig.Close() }()
	memStore, memDB, workspaceID, err := seedMixedMemoryStore(ctx, c.MemoryRecords)
	if err != nil {
		return mixedBuiltState{}, fmt.Errorf("case %q: seed memory store: %w", c.ID, err)
	}
	defer func() { _ = memDB.Close() }()

	// Production tool construction. Values mirror golem's live wiring —
	// cmd/golem/tools.go (Retrieve) and cmd/golem/memory_records.go
	// (AgentMemorySearch) — copied rather than imported: golem is a consumer
	// binary, and the contract here is the VALUES, not its package graph.
	// Now is pinned for build purity; it only feeds expiry filtering, and
	// fixture records carry no expiry.
	retrieveTool := agenttools.Retrieve{R: rig.retr, K: mixedRetrieveK, MaxTokens: mixedRetrieveMaxTokens, Progressive: true}
	memoryTool := agenttools.AgentMemorySearch{
		S: memStore, WorkspaceID: workspaceID,
		SessionID: func() string { return mixedMemorySessionID },
		Now:       func() time.Time { return time.Unix(mixedMemoryEpoch, 0).UTC() },
	}

	msgs := make([]agent.Message, 0, len(c.Events)+len(c.Events)/2)
	for i, e := range c.Events {
		if e.Turn != nil {
			// Production shape (agent/orchestrator.go initState): history turns
			// are Elastic; the final user question is the Pinned goal.
			seg := agent.Elastic
			if i == len(c.Events)-1 {
				seg = agent.Pinned
			}
			msgs = append(msgs, agent.Message{
				ChatMessage: provider.ChatMessage{Role: e.Turn.Role, Content: e.Turn.Content},
				Segment:     seg,
			})
			continue
		}
		pair, err := buildMixedToolPair(ctx, c, e.ToolCall, retrieveTool, memoryTool)
		if err != nil {
			return mixedBuiltState{}, err
		}
		msgs = append(msgs, pair...)
	}

	st := agent.State{System: c.System, Messages: msgs}
	subjects, candidateIDs, err := mixedSubjectsAndCandidates(c, st, rig.chunkIDs)
	if err != nil {
		return mixedBuiltState{}, err
	}
	return mixedBuiltState{
		State:          st,
		RawStateTokens: mixedRawStateTokens(st),
		StateDigest:    mixedStateDigest(st),
		CandidateIDs:   candidateIDs,
		Subjects:       subjects,
	}, nil
}

// buildMixedToolPair replays one tool_call event through the real tool and
// returns the two production-shaped messages: the assistant tool-call message
// and the tool observation. Message construction mirrors agent dispatch
// (assistantMessage in agent/orchestrator.go; toolObservation + the OutputCap
// stamp in agent/dispatch.go recordResult): Content is the capped canonical
// fallback, Context is the tool's structured payload attached as-is, and
// OutputCap is the normalized effect cap — overridden by the fixture's
// output_cap on cap_stress cases only (validation enforces the coupling).
func buildMixedToolPair(ctx context.Context, c mixedCase, tc *mixedToolCall,
	retrieveTool agenttools.Retrieve, memoryTool agenttools.AgentMemorySearch) ([]agent.Message, error) {

	var (
		out       agent.ToolResult
		err       error
		outputCap = mixedDefaultOutputCap
	)
	switch tc.Tool {
	case "retrieve":
		out, err = retrieveTool.Invoke(ctx, tc.Args)
		outputCap = agenttools.RetrieveOutputCap
	case "agent_memory_search":
		out, err = memoryTool.Invoke(ctx, tc.Args)
	case "fixture_echo":
		var args struct {
			Content string `json:"content"`
		}
		if uerr := json.Unmarshal(tc.Args, &args); uerr != nil {
			return nil, fmt.Errorf("case %q: tool_call %q: fixture_echo args: %w", c.ID, tc.CallID, uerr)
		}
		out = agent.ToolResult{Content: args.Content}
	default:
		return nil, fmt.Errorf("case %q: tool_call %q: unknown tool %q", c.ID, tc.CallID, tc.Tool)
	}
	if err != nil {
		return nil, fmt.Errorf("case %q: tool_call %q (%s): invoke: %w", c.ID, tc.CallID, tc.Tool, err)
	}
	if out.IsError {
		return nil, fmt.Errorf("case %q: tool_call %q (%s): tool error (fixture bug): %s", c.ID, tc.CallID, tc.Tool, out.Content)
	}
	if tc.OutputCap > 0 {
		outputCap = tc.OutputCap
	}
	assistant := agent.Message{
		ChatMessage: provider.ChatMessage{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID: tc.CallID, Type: "function",
				Function: provider.ToolCallFunction{Name: tc.Tool, Arguments: tc.Args},
			}},
		},
		Segment: agent.Elastic,
	}
	tool := agent.Message{
		ChatMessage: provider.ChatMessage{
			Role: "tool", Content: mixedCapContent(out.Content, outputCap),
			ToolName: tc.Tool, ToolCallID: tc.CallID,
		},
		Segment:   agent.Elastic,
		Attrib:    out.Attrib,
		Context:   out.Context,
		OutputCap: outputCap,
	}
	return []agent.Message{assistant, tool}, nil
}

// mixedCapContent mirrors agent's unexported capOutput (agent/dispatch.go):
// tool Content is truncated at the effect cap on a UTF-8 rune boundary before
// the observation is recorded. Restated because the builder constructs tool
// Messages directly rather than through dispatch.
func mixedCapContent(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	end := limit
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// mixedSubjectsAndCandidates walks the built State's tool messages in order
// and derives the two AssemblyEval identity fields from the ContextSets the
// tools ACTUALLY attached:
//
//   - subjects: ordered, non-deduplicated "(callID|domain|subjectID)" per
//     group — the same triple mixed assembly keys anchors on.
//   - candidateIDs: sorted deduplicated union of rag chunk IDs + memory
//     record IDs. Memory group subjects ARE record IDs. Rag group subjects are
//     SOURCE PATHS (rag/progressive_groups.go), and the agent/tools bridge
//     drops ChunkID from attribution (agent.RetrievedSource carries none), so
//     the builder restates each returned subject in 3a's chunk-ID vocabulary
//     via the seed-time path->chunkID map — total by construction, since
//     3a-style seeding writes exactly one chunk per source. Vocabulary
//     assumption: fixture record IDs are short human-readable strings, never
//     64-hex chunk digests, so the union cannot collapse a record onto a chunk.
func mixedSubjectsAndCandidates(c mixedCase, st agent.State, chunkIDs map[string]string) ([]string, []string, error) {
	var subjects []string
	candidateSet := map[string]struct{}{}
	for _, m := range st.Messages {
		if m.Context == nil {
			continue
		}
		for _, g := range m.Context.Groups {
			ref := g.Desc.Subject
			subjects = append(subjects, fmt.Sprintf("(%s|%s|%s)", m.ToolCallID, ref.Domain, ref.ID))
			switch ref.Domain {
			case contextdepth.DomainRAG:
				id, ok := chunkIDs[ref.ID]
				if !ok {
					return nil, nil, fmt.Errorf("case %q: call %q: rag subject %q has no seeded chunk", c.ID, m.ToolCallID, ref.ID)
				}
				candidateSet[id] = struct{}{}
			case contextdepth.DomainMemory:
				candidateSet[ref.ID] = struct{}{}
			default:
				return nil, nil, fmt.Errorf("case %q: call %q: unexpected subject domain %q", c.ID, m.ToolCallID, ref.Domain)
			}
		}
	}
	candidateIDs := make([]string, 0, len(candidateSet))
	for id := range candidateSet {
		candidateIDs = append(candidateIDs, id)
	}
	sort.Strings(candidateIDs)
	return subjects, candidateIDs, nil
}

// mixedEstTokens is the ARM-INDEPENDENT registered cost basis for
// raw_state_tokens: est = (len+3)/4. Deliberately NOT agent's messageCost and
// not imported/replicated from agent internals — the raw-state size feeds the
// registered budget formula and must not move when agent's internal estimator
// changes.
func mixedEstTokens(s string) int { return (len(s) + 3) / 4 }

// mixedRawStateTokens applies the registered formula over the frozen State:
// est(System) + sum over Messages of est(Content) + mixedEnvelopeTokens.
func mixedRawStateTokens(st agent.State) int {
	n := mixedEstTokens(st.System)
	for _, m := range st.Messages {
		n += mixedEstTokens(m.Content) + mixedEnvelopeTokens
	}
	return n
}

// Digest encoding: an unexported struct tree marshaled with json.Marshal.
// encoding/json emits struct fields in DECLARATION order, so declaration
// order here IS the canonical field order — append new fields at the end,
// never reorder, or every stored digest changes meaning.
type mixedDigestState struct {
	System   string           `json:"system"`
	Messages []mixedDigestMsg `json:"messages"`
}

type mixedDigestMsg struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	ToolCalls  []mixedDigestCall  `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	OutputCap  int                `json:"output_cap"`
	Context    []mixedDigestGroup `json:"context,omitempty"`
}

type mixedDigestCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"`
}

// mixedDigestGroup keeps the digest content-sensitive without serializing
// alternative payloads wholesale: per alternative, the sha256 of its Content,
// so any deep content change flips the state digest.
type mixedDigestGroup struct {
	Domain       string   `json:"domain"`
	ID           string   `json:"id"`
	Alternatives []string `json:"alternatives"` // sha256 hex per alternative Content
}

// mixedStateDigest is the canonical pre-assembly State digest: sha256 hex
// over the JSON encoding above. It covers System; per message role, content,
// tool-call IDs/names/args, ToolCallID, OutputCap; and per attached
// ContextSet the ordered (domain, id) subjects with per-alternative content
// hashes.
func mixedStateDigest(st agent.State) string {
	d := mixedDigestState{System: st.System, Messages: make([]mixedDigestMsg, 0, len(st.Messages))}
	for _, m := range st.Messages {
		dm := mixedDigestMsg{
			Role: m.Role, Content: m.Content,
			ToolCallID: m.ToolCallID, OutputCap: m.OutputCap,
		}
		for _, call := range m.ToolCalls {
			dm.ToolCalls = append(dm.ToolCalls, mixedDigestCall{
				ID: call.ID, Name: call.Function.Name, Args: string(call.Function.Arguments),
			})
		}
		if m.Context != nil {
			for _, g := range m.Context.Groups {
				dg := mixedDigestGroup{
					Domain: g.Desc.Subject.Domain, ID: g.Desc.Subject.ID,
					Alternatives: make([]string, 0, len(g.Alternatives)),
				}
				for _, a := range g.Alternatives {
					sum := sha256.Sum256([]byte(a.Content))
					dg.Alternatives = append(dg.Alternatives, hex.EncodeToString(sum[:]))
				}
				dm.Context = append(dm.Context, dg)
			}
		}
		d.Messages = append(d.Messages, dm)
	}
	raw, _ := json.Marshal(d) // strings/ints/slices only; cannot fail
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// seedMixedMemoryStore opens an in-memory agent-memory record store, seeds it
// from the fixture records through the PRODUCTION Create path, and pins each
// row's identity and timestamps to the fixture values. The store stamps
// newID()/time.Now() with no override, but NewMemoryRecordStore's documented
// seam is a caller-owned *sql.DB — so the pin is a direct UPDATE on that
// handle, the same move 3a uses for chunks.indexed_at.
//
// The returned workspace ID is the ONE distinct non-empty workspace the
// records declare (a single search tool has a single active workspace, as in
// golem); two distinct workspaces in one case is a fixture bug. Working-kind
// records are seeded under mixedMemorySessionID because the store requires a
// session on working records; the search tool resolves the same session.
func seedMixedMemoryStore(ctx context.Context, records []mixedMemoryRecord) (*memory.MemoryRecordStore, *sql.DB, string, error) {
	workspaceID := ""
	for i, r := range records {
		if r.WorkspaceID == "" || r.WorkspaceID == workspaceID {
			continue
		}
		if workspaceID != "" {
			return nil, nil, "", fmt.Errorf("memory_records[%d] (%s): workspace %q conflicts with %q (one workspace per case)",
				i, r.ID, r.WorkspaceID, workspaceID)
		}
		workspaceID = r.WorkspaceID
	}

	// The driver is registered by the memory package's own hardened-open file
	// (modernc.org/sqlite blank import). OpenHardenedDB is not used here: it
	// prepares an on-disk file, and ":memory:" must stay in memory.
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, nil, "", fmt.Errorf("open memory db: %w", err)
	}
	db.SetMaxOpenConns(1)
	ok := false
	defer func() {
		if !ok {
			_ = db.Close()
		}
	}()
	store, err := memory.NewMemoryRecordStore(ctx, db)
	if err != nil {
		return nil, nil, "", fmt.Errorf("new record store: %w", err)
	}

	epochMs := mixedMemoryEpoch * 1000
	for i, r := range records {
		kind := memory.MemoryKind(r.Kind)
		if r.Kind == "" {
			kind = memory.KindSemantic // durable default: no session/expiry coupling
		}
		params := memory.CreateRecordParams{Kind: kind, Content: r.Content, WorkspaceID: r.WorkspaceID}
		if kind == memory.KindWorking {
			params.SessionID = mixedMemorySessionID
		}
		rec, err := store.Create(ctx, params)
		if err != nil {
			return nil, nil, "", fmt.Errorf("memory_records[%d] (%s): %w", i, r.ID, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE memory_records SET id = ?, created_at = ?, updated_at = ? WHERE id = ?`,
			r.ID, epochMs, epochMs, rec.ID); err != nil {
			return nil, nil, "", fmt.Errorf("memory_records[%d] (%s): pin row: %w", i, r.ID, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE memory_records_fts SET id = ? WHERE id = ?`, r.ID, rec.ID); err != nil {
			return nil, nil, "", fmt.Errorf("memory_records[%d] (%s): pin fts row: %w", i, r.ID, err)
		}
	}
	ok = true
	return store, db, workspaceID, nil
}
