package rageval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/memory"
	"github.com/kstruzzieri/go-llm/provider"
	"github.com/kstruzzieri/go-llm/rag"
)

// Mixed-assembly eval fixture (#331 slice 3c, Task 7): five hand-built
// agent.States, one per stratum flavor, constructed directly here through the
// PRODUCTION tool producers (agent/tools.Retrieve, agent/tools.
// AgentMemorySearch) over freshly seeded in-memory stores. This is the
// deterministic rageval companion to cmd/llm-bench's human-labeled mixed
// corpus: llm-bench measures answer quality on arms-differ-gated cases;
// rageval measures assembly SHAPE across budget fractions, INCLUDING the
// degenerate no-anchor cases (conversation_only, chain_retention) where both
// arms take the legacy path and the mixed trace is the zero value.

const (
	// mixedEvalFixedEpoch pins every store timestamp. It is llm-bench's
	// assemblyFixedEpoch (cmd/llm-bench/assembly.go) shifted to 12:00 UTC:
	// memory's recordLine/recordCard render CreatedAt dates in LOCAL time
	// (time.UnixMilli), so a midnight-UTC epoch renders as different calendar
	// dates on a UTC CI runner vs a US-Pacific workstation. Noon UTC keeps
	// every zone from UTC-11 through UTC+11 on the same date.
	mixedEvalFixedEpoch = int64(1753574400 + 12*3600)
	// mixedEvalOutputCap restates agent's unexported defaultOutputCap
	// (agent/types.go) — also agenttools.RetrieveOutputCap and llm-bench's
	// mixedDefaultOutputCap (cmd/llm-bench/assembly_mixed_state.go). It is the
	// anchor byte cap mixed assembly bounds alternatives with; zero would
	// reject every alternative.
	mixedEvalOutputCap = 64 << 10
	// mixedEvalEnvelopeTokens and mixedEvalMinViableSlack restate the
	// registered budget-formula constants in
	// cmd/llm-bench/assembly_mixed_fixture.go (mixedEnvelopeTokens 8,
	// mixedMinViableSlack 64). The 0.6-set sweep {0.4, 0.6, 0.8} in
	// mixedSweepFractions brackets that file's registered
	// mixedBudgetFraction = 0.6. Restated, not imported: llm-bench is a
	// package main. Pinned by TestMixedEvalSchemaAndShape.
	mixedEvalEnvelopeTokens = 8
	mixedEvalMinViableSlack = 64

	// Retrieve wiring mirrors golem's live construction and llm-bench's
	// mixed builder (mixedRetrieveK / mixedRetrieveMaxTokens).
	mixedEvalRetrieveK         = 5
	mixedEvalRetrieveMaxTokens = 2048

	mixedEvalSessionID     = "mixed-eval-session"
	mixedEvalWorkspaceID   = "mixed-eval-ws"
	mixedEvalVectorSpaceID = "mixed-eval-space"
)

// mixedEvalEst is the ARM-INDEPENDENT registered token estimator
// est = (len+3)/4, the same basis as llm-bench's mixedEstTokens. Deliberately
// NOT agent's messageCost: the raw-state size feeds the registered budget
// formula and must not move with agent's internal estimator.
func mixedEvalEst(s string) int { return (len(s) + 3) / 4 }

// mixedEvalRawTokens applies the registered raw-size formula over a frozen
// State: est(System) + sum over Messages of est(Content) + envelope.
func mixedEvalRawTokens(st agent.State) int {
	n := mixedEvalEst(st.System)
	for _, m := range st.Messages {
		n += mixedEvalEst(m.Content) + mixedEvalEnvelopeTokens
	}
	return n
}

// mixedEvalCase is one frozen pre-assembly State plus its registered raw size.
type mixedEvalCase struct {
	Name      string
	Stratum   string
	State     agent.State
	RawTokens int
}

// buildMixedEvalStates builds the five fixture cases in declared order. Every
// build seeds fresh in-memory stores and pins every timestamp, so two calls
// return deep-equal states; any single failure aborts the build — a fixture
// whose events cannot replay through the real tools is a fixture bug.
func buildMixedEvalStates(ctx context.Context) ([]mixedEvalCase, error) {
	builders := []struct {
		name, stratum string
		build         func(context.Context) (agent.State, error)
	}{
		{"conversation_only", "conversation_only", buildMixedEvalConversationOnly},
		{"memory_only", "memory_only", buildMixedEvalMemoryOnly},
		{"cross_domain", "cross_domain_join", buildMixedEvalCrossDomain},
		{"stale_fresh", "stale_vs_fresh", buildMixedEvalStaleFresh},
		{"chain_retention", "chain_retention", buildMixedEvalChainRetention},
	}
	cases := make([]mixedEvalCase, 0, len(builders))
	for _, b := range builders {
		st, err := b.build(ctx)
		if err != nil {
			return nil, fmt.Errorf("rag eval: mixed fixture %s: %w", b.name, err)
		}
		if len(st.Messages) == 0 {
			return nil, fmt.Errorf("rag eval: mixed fixture %s: empty state", b.name)
		}
		last := st.Messages[len(st.Messages)-1]
		if last.Segment != agent.Pinned || last.Role != "user" {
			return nil, fmt.Errorf("rag eval: mixed fixture %s: final message must be the pinned user goal", b.name)
		}
		cases = append(cases, mixedEvalCase{
			Name: b.name, Stratum: b.stratum, State: st, RawTokens: mixedEvalRawTokens(st),
		})
	}
	return cases, nil
}

// Message constructors mirror the production shape (agent/orchestrator.go
// initState): history turns are Elastic, the final user question is the
// Pinned goal.

func mixedEvalTurn(role, content string) agent.Message {
	return agent.Message{
		ChatMessage: provider.ChatMessage{Role: role, Content: content},
		Segment:     agent.Elastic,
	}
}

func mixedEvalGoal(content string) agent.Message {
	return agent.Message{
		ChatMessage: provider.ChatMessage{Role: "user", Content: content},
		Segment:     agent.Pinned,
	}
}

// mixedEvalChain wraps one real tool result into the production two-message
// chain shape (assistant tool-call + tool observation), mirroring llm-bench's
// buildMixedToolPair (cmd/llm-bench/assembly_mixed_state.go). Fixture output
// is required to sit under the cap outright — a fixture that needs runtime
// truncation is a fixture bug, so this fails instead of restating capOutput.
func mixedEvalChain(callID, toolName string, args json.RawMessage, out agent.ToolResult) ([]agent.Message, error) {
	if out.IsError {
		return nil, fmt.Errorf("tool %s call %s returned error: %s", toolName, callID, out.Content)
	}
	if len(out.Content) > mixedEvalOutputCap {
		return nil, fmt.Errorf("tool %s call %s output %d bytes exceeds cap %d", toolName, callID, len(out.Content), mixedEvalOutputCap)
	}
	assistant := agent.Message{
		ChatMessage: provider.ChatMessage{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID: callID, Type: "function",
				Function: provider.ToolCallFunction{Name: toolName, Arguments: args},
			}},
		},
		Segment: agent.Elastic,
	}
	tool := agent.Message{
		ChatMessage: provider.ChatMessage{
			Role: "tool", Content: out.Content,
			ToolName: toolName, ToolCallID: callID,
		},
		Segment:   agent.Elastic,
		Attrib:    out.Attrib,
		Context:   out.Context,
		OutputCap: mixedEvalOutputCap,
	}
	return []agent.Message{assistant, tool}, nil
}

// --- rag store seeding -----------------------------------------------------

// mixedEvalSource is one seeded rag source: exactly one chunk, rank-ordered
// embedding, optional stored summary. staleSummary stores the summary under a
// fabricated content hash so render time derives stale_content and falls back
// to the metadata ladder — the simplest honest "summary exists but content
// moved on" state (rag/summary_validity.go).
type mixedEvalSource struct {
	path, content      string
	abstract, overview string
	staleSummary       bool
}

// mixedEvalQueryEmbedder and mixedEvalRankEmbedding copy llm-bench's
// rank-embedding idea (cmd/llm-bench/assembly.go): the query embeds to (1,0)
// and chunk i to a unit vector whose cosine similarity strictly decreases
// with i, so fixture order IS retrieval order.
func mixedEvalQueryEmbedder() rag.Embedder {
	return rag.EmbedderFunc(func(_ context.Context, _ string, inputs []string) (rag.EmbedResult, error) {
		if len(inputs) != 1 {
			return rag.EmbedResult{}, fmt.Errorf("rag eval: mixed embedder: got %d inputs, want 1", len(inputs))
		}
		return rag.EmbedResult{
			Embeddings: [][]float64{{1, 0}},
			Model:      "mixed-eval-fixture", Provider: "fixture",
			VectorSpaceID: mixedEvalVectorSpaceID,
		}, nil
	})
}

func mixedEvalRankEmbedding(rank int) []float64 {
	angle := float64(rank+1) * 0.01
	return []float64{math.Cos(angle), math.Sin(angle)}
}

// seedMixedEvalRetriever seeds an in-memory rag store (one provenance-complete
// chunk per source, pinned indexed_at, atomic abstract/overview summaries) and
// returns the production retriever over it. Small sibling of llm-bench's
// seedAssemblyRig, restated because that lives in package main.
func seedMixedEvalRetriever(ctx context.Context, sources []mixedEvalSource) (*rag.Retriever, *rag.SQLiteStore, error) {
	store, err := rag.NewSQLiteStore(":memory:")
	if err != nil {
		return nil, nil, fmt.Errorf("create rag store: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = store.Close()
		}
	}()
	paths := make([]string, len(sources))
	for i, s := range sources {
		paths[i] = s.path
		chunk := rag.Chunk{
			ID:        fmt.Sprintf("mixed-eval-chunk-%02d", i),
			Source:    s.path,
			StartLine: 1,
			EndLine:   strings.Count(s.content, "\n") + 1,
			Language:  "go",
			Content:   s.content,
		}
		if err := store.ReplaceSourceWithHashAndVectorSpaceID(ctx, s.path,
			[]rag.Chunk{chunk}, [][]float64{mixedEvalRankEmbedding(i)},
			fixtureSourceSignature([]rag.Chunk{chunk}), mixedEvalVectorSpaceID); err != nil {
			return nil, nil, fmt.Errorf("seed source %q: %w", s.path, err)
		}
	}
	// The store stamps indexed_at with time.Now() and the progressive renderer
	// emits it as an "indexed:" RFC3339 line — pin it (same move as
	// seedOutlineStore) or byte-identity depends on the wall clock.
	if _, err := store.DB().ExecContext(ctx, "UPDATE chunks SET indexed_at = ?", mixedEvalFixedEpoch); err != nil {
		return nil, nil, fmt.Errorf("pin indexed_at: %w", err)
	}
	prov, err := store.SourceProvenanceBatch(ctx, paths)
	if err != nil {
		return nil, nil, err
	}
	for _, s := range sources {
		if s.abstract == "" {
			continue
		}
		if s.overview == "" {
			return nil, nil, fmt.Errorf("source %q: abstract without overview (atomic pair)", s.path)
		}
		p := prov[s.path]
		if p.ContentHash == "" || p.VectorSpaceID == "" {
			return nil, nil, fmt.Errorf("source %q lacks provenance for summary", s.path)
		}
		contentHash := p.ContentHash
		if s.staleSummary {
			// Write validation only rejects blanks; a non-matching hash stores
			// fine and every render derives stale_content (documented on
			// rag.UpsertSourceSummary), which is exactly the ladder this
			// stratum exercises.
			contentHash = "mixed-eval-stale-content-hash"
		}
		if err := store.UpsertSourceSummary(ctx, rag.SourceSummary{
			Source: s.path, ContentHash: contentHash, VectorSpaceID: p.VectorSpaceID,
			Abstract: s.abstract, Overview: s.overview,
			SummaryModel: "mixed-eval-fixture", FormatVersion: rag.SourceSummaryFormatVersion,
			SummarizedAt: mixedEvalFixedEpoch,
		}); err != nil {
			return nil, nil, err
		}
	}
	retr, err := rag.NewRetrieverWithEmbedder(mixedEvalQueryEmbedder(), store,
		rag.WithRetrieverModel("mixed-eval-fixture"), rag.WithVectorOnly())
	if err != nil {
		return nil, nil, err
	}
	ok = true
	return retr, store, nil
}

// mixedEvalRetrieve replays one retrieve call through the production tool over
// a store seeded from sources, and returns the production chain messages.
func mixedEvalRetrieve(ctx context.Context, callID, query string, sources []mixedEvalSource) ([]agent.Message, error) {
	retr, store, err := seedMixedEvalRetriever(ctx, sources)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	tool := agenttools.Retrieve{R: retr, K: mixedEvalRetrieveK, MaxTokens: mixedEvalRetrieveMaxTokens, Progressive: true}
	args, err := json.Marshal(struct {
		Query string `json:"query"`
	}{query})
	if err != nil {
		return nil, err
	}
	out, err := tool.Invoke(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("retrieve %s: %w", callID, err)
	}
	return mixedEvalChain(callID, "retrieve", args, out)
}

// --- memory store seeding --------------------------------------------------

// mixedEvalMemoryRecord is one seeded semantic agent-memory record.
type mixedEvalMemoryRecord struct {
	id, content string
}

// mixedEvalMemorySearch seeds an in-memory record store through the
// PRODUCTION Create path, pins each row's identity and timestamps (direct
// UPDATE on the caller-owned *sql.DB — NewMemoryRecordStore's documented
// seam, the same move as llm-bench's seedMixedMemoryStore), replays one
// agent_memory_search call, and returns the production chain messages.
func mixedEvalMemorySearch(ctx context.Context, callID, query string, records []mixedEvalMemoryRecord) ([]agent.Message, error) {
	// Driver registered by the memory package's hardened-open file
	// (modernc.org/sqlite blank import).
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open memory db: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	store, err := memory.NewMemoryRecordStore(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("new record store: %w", err)
	}
	epochMs := mixedEvalFixedEpoch * 1000
	for i, r := range records {
		rec, err := store.Create(ctx, memory.CreateRecordParams{
			Kind: memory.KindSemantic, Content: r.content, WorkspaceID: mixedEvalWorkspaceID,
		})
		if err != nil {
			return nil, fmt.Errorf("record %d (%s): %w", i, r.id, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE memory_records SET id = ?, created_at = ?, updated_at = ? WHERE id = ?`,
			r.id, epochMs, epochMs, rec.ID); err != nil {
			return nil, fmt.Errorf("record %d (%s): pin row: %w", i, r.id, err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE memory_records_fts SET id = ? WHERE id = ?`, r.id, rec.ID); err != nil {
			return nil, fmt.Errorf("record %d (%s): pin fts row: %w", i, r.id, err)
		}
	}
	tool := agenttools.AgentMemorySearch{
		S: store, WorkspaceID: mixedEvalWorkspaceID,
		SessionID: func() string { return mixedEvalSessionID },
		Now:       func() time.Time { return time.Unix(mixedEvalFixedEpoch, 0).UTC() },
	}
	args, err := json.Marshal(struct {
		Query string `json:"query"`
	}{query})
	if err != nil {
		return nil, err
	}
	out, err := tool.Invoke(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("memory search %s: %w", callID, err)
	}
	return mixedEvalChain(callID, agenttools.AgentMemorySearchToolName, args, out)
}

// --- the five cases --------------------------------------------------------

// buildMixedEvalConversationOnly: plain turns only, the answer-bearing fact in
// the FIRST exchange so budget pressure sheds it first. No tools => no
// structured anchors => the mixed arm takes the legacy path and the trace is
// the zero value; this case exists to keep the degenerate no-anchor shape in
// the sweep.
func buildMixedEvalConversationOnly(context.Context) (agent.State, error) {
	exchanges := [][2]string{
		{
			"Before we start on the migration plan, note this down: our credential rotation interval is exactly 42 days, and the compliance team audits the rotation logs on the first business day of every quarter without exception.",
			"Noted. The credential rotation interval is 42 days and rotation logs are audited quarterly. I will keep that constraint in mind while we lay out the migration plan and its cutover checkpoints in the next steps.",
		},
		{
			"The first migration phase moves the ingestion workers to the new queue. They currently drain about eleven thousand messages an hour at peak, and we cannot drop below eight thousand during the switchover window.",
			"Understood. Phase one migrates the ingestion workers with a floor of eight thousand messages an hour during cutover. A staged drain with dual-write to both queues keeps throughput above that floor for the whole window.",
		},
		{
			"Phase two is the metadata service. It holds the schema registry, so we need the compatibility checker running on both sides until every producer has re-registered its schemas against the new endpoint.",
			"Right — phase two keeps the compatibility checker dual-homed until producer re-registration completes. I would sequence the registry export, the checker mirror, and then the producer flip, verifying checksums at each step.",
		},
		{
			"Phase three retires the old brokers. Finance wants the hardware decommissioned within the same budget quarter, which gives us roughly five weeks of overlap for rollback capacity before the racks are pulled.",
			"Five weeks of rollback overlap is workable. I would hold the old brokers in read-only standby for the first three weeks, then snapshot and archive their logs before the racks are decommissioned in week five.",
		},
		{
			"Last logistics item: the change advisory board meets Tuesdays, so every phase gate needs its runbook submitted the Friday before, including the rollback rehearsal results from staging and the on-call roster.",
			"Got it — runbooks land the Friday before each Tuesday board review, with staging rollback rehearsal results and the on-call roster attached. I will fold those deadlines into the phase schedule for all three phases.",
		},
	}
	st := agent.State{System: "You are a deterministic planning assistant for a queue-migration project. Answer strictly from the visible transcript; never invent numbers that were not stated."}
	for _, e := range exchanges {
		st.Messages = append(st.Messages, mixedEvalTurn("user", e[0]), mixedEvalTurn("assistant", e[1]))
	}
	st.Messages = append(st.Messages, mixedEvalGoal("What credential rotation interval did I state at the very start of this conversation? Answer with the exact number of days and nothing else."))
	return st, nil
}

// buildMixedEvalMemoryOnly: a deliberately SMALL state with a LARGE pinned
// goal, sized so the minViable floor beats round(f*raw) at the low fractions —
// the floor-wins row TestMixedEvalSchemaAndShape hand-computes.
func buildMixedEvalMemoryOnly(ctx context.Context) (agent.State, error) {
	chain, err := mixedEvalMemorySearch(ctx, "mem-call-01", "release conventions", []mixedEvalMemoryRecord{
		{"rec-alpha", "Release conventions: every release branch is cut on Wednesday and tagged from green CI only."},
		{"rec-beta", "Release conventions: hotfixes cherry-pick to the newest two release branches, never further back."},
		{"rec-gamma", "Release conventions: the changelog entry is written before the tag, reviewed by the release captain."},
	})
	if err != nil {
		return agent.State{}, err
	}
	st := agent.State{System: "You are a deterministic release assistant. Prefer facts retrieved from agent memory over guesses; cite the record wording."}
	st.Messages = append(st.Messages,
		mixedEvalTurn("user", "I need a refresher on our release conventions before Wednesday."),
		mixedEvalTurn("assistant", "Let me check the stored conventions in agent memory first."),
	)
	st.Messages = append(st.Messages, chain...)
	st.Messages = append(st.Messages, mixedEvalGoal(
		"Given the conventions you just retrieved, walk me through exactly what happens if CI goes red on Wednesday morning before the cut: state which day the release branch is cut, what the tagging precondition is, how far back a hotfix may be cherry-picked, and who reviews the changelog entry before the tag is created. Quote the record wording where you can, keep the answer to four bullet points in that order, and do not introduce any convention that is not present in the retrieved records."))
	return st, nil
}

// buildMixedEvalCrossDomain: turns + a progressive retrieve chain + a memory
// chain, so the mixed arm allocates subjects from BOTH producer domains and
// the f=0.6 histogram carries rendered and omitted buckets side by side.
func buildMixedEvalCrossDomain(ctx context.Context) (agent.State, error) {
	sources := []mixedEvalSource{
		{
			path:     "internal/billing/invoice.go",
			content:  "package billing\n\n// Invoice totals are computed in minor units to avoid float drift.\nfunc TotalMinorUnits(lines []Line) int64 {\n\tvar total int64\n\tfor _, l := range lines {\n\t\ttotal += l.UnitPriceMinor * int64(l.Quantity)\n\t}\n\treturn total\n}\n",
			abstract: "Invoice totaling in integer minor units.",
			overview: "TotalMinorUnits sums line items in minor currency units; float arithmetic is banned in billing paths.",
		},
		{
			path:     "internal/billing/tax.go",
			content:  "package billing\n\n// ApplyTax adds jurisdiction tax in minor units, rounding half-up once per invoice.\nfunc ApplyTax(totalMinor int64, rate Rate) int64 {\n\treturn totalMinor + rate.RoundHalfUp(totalMinor)\n}\n",
			abstract: "Tax application with single half-up rounding.",
			overview: "ApplyTax rounds exactly once per invoice at the total, never per line, using Rate.RoundHalfUp.",
		},
		{
			path:     "internal/billing/ledger.go",
			content:  "package billing\n\n// PostToLedger writes one balanced double-entry pair per invoice.\nfunc PostToLedger(inv Invoice, l *Ledger) error {\n\tif inv.TotalMinor == 0 {\n\t\treturn ErrZeroInvoice\n\t}\n\treturn l.PostPair(inv.ID, inv.TotalMinor)\n}\n",
			abstract: "Balanced double-entry ledger posting.",
			overview: "PostToLedger rejects zero invoices and posts one balanced debit/credit pair keyed by invoice ID.",
		},
		{
			path:     "internal/billing/refund.go",
			content:  "package billing\n\n// Refunds reverse the original posting; partial refunds carry the original invoice ID.\nfunc Refund(inv Invoice, amountMinor int64, l *Ledger) error {\n\treturn l.PostPair(inv.ID, -amountMinor)\n}\n",
			abstract: "Refunds as reversed ledger postings.",
			overview: "Refund posts a negative pair against the original invoice ID so partial refunds stay traceable.",
		},
	}
	retrieveChain, err := mixedEvalRetrieve(ctx, "rag-call-01", "how are invoice totals, tax and refunds posted", sources)
	if err != nil {
		return agent.State{}, err
	}
	// Query is the single term "billing": memory search is FTS5 MATCH with
	// AND semantics, so a two-term query would silently narrow the join to
	// the one record containing both terms.
	memoryChain, err := mixedEvalMemorySearch(ctx, "mem-call-02", "billing", []mixedEvalMemoryRecord{
		{"rec-billing-1", "Billing incident 2031: per-line tax rounding double-charged 0.01 on three-line invoices; fixed by rounding once at the total."},
		{"rec-billing-2", "Billing convention: refunds must reference the original invoice ID or reconciliation flags them as orphans."},
		{"rec-billing-3", "Billing convention: ledger postings are immutable; corrections are new reversing pairs, never edits."},
	})
	if err != nil {
		return agent.State{}, err
	}
	// Turn contents are deliberately heavy: the history spans are the lowest
	// retention lane, so their size is the lever that keeps the f=0.6 cell
	// under real pressure — an omitted span next to rendered anchors is the
	// side-by-side decision histogram this stratum exists to show.
	st := agent.State{System: "You are a deterministic billing-domain assistant. Join code retrieval with stored incident memory before answering; prefer cited evidence to recollection."}
	st.Messages = append(st.Messages,
		mixedEvalTurn("user", "I am reviewing the billing pipeline before the external audit next month. First question: where exactly does tax rounding happen relative to invoice totaling in our code, and has rounding ever caused a customer-visible incident for us? The auditors flagged rounding as a standard focus area, and I want our written answer to match both the current code and whatever incident history we have on record, because a mismatch between those two is the kind of finding that spawns a remediation project."),
		mixedEvalTurn("assistant", "I will pull the billing sources for the totaling, tax and ledger paths, and check our stored incident memory in parallel, so the written answer joins current code with recorded history instead of relying on recollection. Once both are in front of us I can point at the exact function where rounding happens, the invariant it maintains, and the incident that motivated it, each with a citation the auditors can follow back to a file or a record."),
	)
	st.Messages = append(st.Messages, retrieveChain...)
	st.Messages = append(st.Messages,
		mixedEvalTurn("user", "Also confirm how refunds are represented in the ledger, because the auditors will certainly ask about traceability of partial refunds across reporting periods. Finance told me reconciliation once flagged a batch of refund postings as orphans years ago, before my time, and I would like to know whether the current representation makes that class of problem structurally impossible or merely unlikely, because the answer changes how much manual review we commit to in the audit response."),
		mixedEvalTurn("assistant", "Refund representation is covered by the retrieved ledger and refund sources — refunds post as reversing pairs against the original invoice ID, which is what makes partial refunds traceable across periods. Let me also check the stored conventions in memory for the orphan-flagging rule you mentioned, so the audit response can state both the mechanism and the recorded convention that reconciliation relies on, each with its own citation."),
	)
	st.Messages = append(st.Messages, memoryChain...)
	st.Messages = append(st.Messages, mixedEvalGoal(
		"Using the retrieved billing sources AND the stored incident memory together: state where tax rounding happens, which past incident that rule prevents, and how a partial refund stays traceable in the ledger. Cite the source file or record for each of the three claims."))
	return st, nil
}

// buildMixedEvalStaleFresh: one retrieve chain over a store where one source's
// summary is STALE (stored under a mismatched content hash, so render time
// derives stale_content and falls back to the metadata ladder) while its full
// content stays fresh, and a sibling source carries a fresh summary.
func buildMixedEvalStaleFresh(ctx context.Context) (agent.State, error) {
	sources := []mixedEvalSource{
		{
			path:         "internal/sync/replicator.go",
			content:      "package sync\n\n// Replicate streams change batches to every follower with a per-follower ack window.\nfunc Replicate(batch Batch, followers []Follower) error {\n\tfor _, f := range followers {\n\t\tif err := f.Send(batch); err != nil {\n\t\t\treturn fmt.Errorf(\"follower %s: %w\", f.ID, err)\n\t\t}\n\t}\n\treturn nil\n}\n",
			abstract:     "Change-batch replication to followers.",
			overview:     "Replicate fans each batch out to every follower and fails fast on the first send error.",
			staleSummary: true,
		},
		{
			path:     "internal/sync/checkpoint.go",
			content:  "package sync\n\n// Checkpoint persists the last fully-acked batch index per follower.\nfunc Checkpoint(f Follower, idx int64, s *Store) error {\n\treturn s.PutCheckpoint(f.ID, idx)\n}\n",
			abstract: "Per-follower replication checkpoints.",
			overview: "Checkpoint records the last fully-acked batch index for one follower in the checkpoint store.",
		},
		{
			path:    "internal/sync/backfill.go",
			content: "package sync\n\n// Backfill replays batches from a follower's checkpoint to the head.\nfunc Backfill(f Follower, s *Store) error {\n\tidx, err := s.GetCheckpoint(f.ID)\n\tif err != nil {\n\t\treturn err\n\t}\n\treturn replayFrom(f, idx)\n}\n",
		},
	}
	chain, err := mixedEvalRetrieve(ctx, "rag-call-02", "how replication checkpoints and backfill interact", sources)
	if err != nil {
		return agent.State{}, err
	}
	st := agent.State{System: "You are a deterministic replication-domain assistant. Distinguish orientation text from verbatim evidence when you cite retrieved sources."}
	st.Messages = append(st.Messages,
		mixedEvalTurn("user", "A follower fell behind overnight and I want to understand our recovery story before I page anyone about replication lag on the primary."),
		mixedEvalTurn("assistant", "Let me retrieve the replication, checkpoint and backfill sources so the recovery story is grounded in the current code."),
	)
	st.Messages = append(st.Messages, chain...)
	st.Messages = append(st.Messages, mixedEvalGoal(
		"From the retrieved sources: explain how a lagging follower catches up, naming the function that records progress and the function that replays from it, and note which retrieved source you would trust least if its summary looked out of date."))
	return st, nil
}

// buildMixedEvalChainRetention: an UNSTRUCTURED echo-style chain (plain
// Content, nil Context — constructed directly, no real tool) positioned
// early so it is shed-eligible under pressure. No structured anchors => the
// mixed arm takes the legacy path; the sweep records how both arms treat a
// plain completed chain against later plain turns.
func buildMixedEvalChainRetention(context.Context) (agent.State, error) {
	echoContent := "diagnostics snapshot: queue depth 4812 messages; oldest message age 96 seconds; consumer group lag by partition p0=311 p1=502 p2=1288 p3=44; broker heap 61 percent; last rebalance 14 minutes ago triggered by consumer c-17 restart; no under-replicated partitions; produce p99 latency 38 milliseconds; fetch p99 latency 21 milliseconds."
	assistant := agent.Message{
		ChatMessage: provider.ChatMessage{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID: "echo-call-01", Type: "function",
				Function: provider.ToolCallFunction{Name: "fixture_echo", Arguments: json.RawMessage(`{"content":"diagnostics snapshot"}`)},
			}},
		},
		Segment: agent.Elastic,
	}
	toolMsg := agent.Message{
		ChatMessage: provider.ChatMessage{
			Role: "tool", Content: echoContent,
			ToolName: "fixture_echo", ToolCallID: "echo-call-01",
		},
		Segment:   agent.Elastic,
		OutputCap: mixedEvalOutputCap,
	}
	st := agent.State{System: "You are a deterministic operations assistant. Tool observations in the transcript are the only telemetry you have; say so when one has been dropped."}
	st.Messages = append(st.Messages,
		mixedEvalTurn("user", "Grab a diagnostics snapshot of the ingest queue before we discuss the consumer incident from this morning's alert storm."),
		assistant, toolMsg,
		mixedEvalTurn("user", "While that snapshot is fresh: the alerts fired between 06:40 and 06:55, mostly consumer-lag warnings on partition p2, with one broker heap warning that cleared on its own."),
		mixedEvalTurn("assistant", "Understood — a fifteen-minute alert window dominated by p2 consumer lag plus one transient broker heap warning. The snapshot shows p2 lag well above its siblings, which is consistent with those alerts."),
		mixedEvalTurn("user", "The consumer team suspects the c-17 restart caused the rebalance and the lag spike, but they want evidence from telemetry rather than a hunch before they roll anything back."),
		mixedEvalTurn("assistant", "The snapshot records the last rebalance fourteen minutes ago triggered by the c-17 restart, and p2 lag at 1288 messages against double digits elsewhere — that is direct telemetry linking the restart to the spike."),
	)
	st.Messages = append(st.Messages, mixedEvalGoal(
		"From the diagnostics snapshot earlier in this conversation: what was the exact consumer lag on partition p2, and what triggered the last rebalance? If the snapshot is no longer visible to you, say exactly that instead of guessing."))
	return st, nil
}
