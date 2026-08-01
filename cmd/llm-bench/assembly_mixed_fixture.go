package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode"
)

// Mixed-assembly fixture schema v2 (#331 slice 3c, Task 3). This file owns
// the corpus fixture PARSE + VALIDATION only; the Task 4 builder that replays
// events through real agent tools into frozen States lives in
// assembly_mixed_state.go, so this file must not import agent packages.

// Registered mixed-assembly constants: the single source of truth for the
// budget formula inputs. The fixture restates them under "constants" so the
// committed corpus is self-describing, but the Go constants govern — a
// fixture that disagrees is rejected at validation time.
const (
	mixedBudgetFraction = 0.6
	mixedEnvelopeTokens = 8
	mixedMinViableSlack = 64
)

const mixedFixtureKind = "mixed-assembly"

// Closed per-case vocabularies.
var (
	mixedStrata = map[string]struct{}{
		"conversation_only": {}, "memory_only": {}, "cross_domain_join": {},
		"stale_vs_fresh": {}, "chain_retention": {},
	}
	mixedAnswerHomes = map[string]struct{}{
		"conversation": {}, "memory": {}, "rag": {}, "join": {},
	}
	// mixedEvidenceDomains is ordered so error messages and the
	// required_domains rule read deterministically.
	mixedEvidenceDomains = [3]string{"conversation", "memory", "rag"}
	mixedTools           = map[string]struct{}{
		"retrieve": {}, "agent_memory_search": {}, "fixture_echo": {},
	}
)

// mixedFixture is the top-level mixed-assembly corpus fixture (a JSON
// object, distinguishing it from the 3a case ARRAY at -assembly-build time).
type mixedFixture struct {
	Version   int                   `json:"version"`
	Kind      string                `json:"kind"`
	Constants mixedFixtureConstants `json:"constants"`
	Cases     []mixedCase           `json:"cases"`
}

// mixedFixtureConstants restates the registered Go constants above so the
// committed corpus documents the formula it was built under.
type mixedFixtureConstants struct {
	Fraction       float64 `json:"fraction"`
	EnvelopeTokens int     `json:"envelope_tokens"`
	MinViableSlack int     `json:"min_viable_slack"`
}

// mixedCase is one mixed-assembly QA case: an event script (turns + tool
// calls) plus the memory records and rag sources the Task 4 builder seeds
// its stores with, and the evidence contract the validator enforces.
type mixedCase struct {
	ID             string `json:"id"`
	Stratum        string `json:"stratum"`
	AnswerHome     string `json:"answer_home"`
	ScenarioFamily string `json:"scenario_family,omitempty"` // bootstrap cluster unit; must stay within one stratum
	TwinGroup      string `json:"twin_group,omitempty"`      // descriptive lane-bias label; may span strata, never a clustering unit
	Control        bool   `json:"control,omitempty"`
	CapStress      bool   `json:"cap_stress,omitempty"`
	System         string `json:"system"`

	Events        []mixedEvent        `json:"events"`
	MemoryRecords []mixedMemoryRecord `json:"memory_records,omitempty"`
	RagSources    []assemblySource    `json:"rag_sources,omitempty"`

	RequiredEvidence  []mixedEvidence `json:"required_evidence,omitempty"`
	ForbiddenEvidence []string        `json:"forbidden_evidence,omitempty"`
	// RequiredDomains is self-description, not choice: every case carries all
	// three domains as competing distractors by design, so the field must be
	// exactly {conversation, memory, rag}.
	RequiredDomains []string `json:"required_domains"`

	// AnswerTurnIndex optionally points at the answering conversation turn's
	// index within Events (bookkeeping: answer-position thirds for
	// conversation_only balance). Pointer so index 0 is distinguishable from
	// absent.
	AnswerTurnIndex *int `json:"answer_turn_index,omitempty"`

	// ToplineFacts, when set, feeds the unpaired topline ceiling arm: a single
	// "Facts + Question" prompt with the answer-bearing facts handed over
	// directly. Only legal on non-control cases (controls are excluded from the
	// verdict and feed no ceiling), and must contain every required_evidence
	// literal case-sensitively — facts that cannot support the answer would
	// make the ceiling meaningless.
	ToplineFacts string `json:"topline_facts,omitempty"`

	Golden Golden `json:"golden"`
}

// mixedEvent is one scripted event: exactly one of Turn / ToolCall set.
type mixedEvent struct {
	Turn     *mixedTurn     `json:"turn,omitempty"`
	ToolCall *mixedToolCall `json:"tool_call,omitempty"`
}

// mixedTurn is a scripted conversation turn (roles user|assistant only; tool
// results come from replaying ToolCall events in Task 4, never from turns).
type mixedTurn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// mixedToolCall scripts one tool invocation the Task 4 builder replays
// through the real tool. OutputCap truncates the tool's output and is only
// legal on cap_stress cases; the uniform-cap design is intended — every tool
// call on a cap_stress case carries a cap, and mixed capped/uncapped
// scenarios are authored as separate cases.
type mixedToolCall struct {
	CallID    string          `json:"call_id"`
	Tool      string          `json:"tool"` // retrieve | agent_memory_search | fixture_echo
	Args      json.RawMessage `json:"args"`
	OutputCap int             `json:"output_cap,omitempty"`
}

// mixedMemoryRecord seeds one agent-memory record. Fields mirror what the 3b
// agent_memory_search L0 card renders (agent/tools/agent_memory.go), kept
// minimal: ID and Content drive the flat line, Kind and WorkspaceID the
// card's kind and scope class. Timestamps are deliberately absent — Task 4
// pins them to the fixed epoch for byte-reproducible builds.
type mixedMemoryRecord struct {
	ID          string `json:"id"`
	Content     string `json:"content"`
	Kind        string `json:"kind,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// mixedEvidence is one required-evidence entry: a literal that must appear
// verbatim (case-sensitive) in its declared domain's fixture content and be
// absent — case-INSENSITIVELY — everywhere else (the other two domains, the
// final question, and the system prompt), so a re-cased leak cannot slip
// past the contamination scan. Anchor literals per the 3a AnswerLiteral rule
// ("claimBatch = 25", not "25"); there is no hard length check, but short
// unanchored literals are rejected by the contamination scan in practice.
type mixedEvidence struct {
	Domain  string `json:"domain"` // conversation | memory | rag
	Literal string `json:"literal"`
}

// mixedBookkeeping is the corpus balance summary computed alongside
// validation — reporting only, never rejection (the Task 12 pre-label gate
// hardens the balance checks).
type mixedBookkeeping struct {
	stratumCounts map[string]int
	controlCases  int
	// toplineEligible counts non-control cases: control pairs are excluded
	// from the verdict, so only real cases feed the topline ceiling arm.
	toplineEligible int
	// answerThirds buckets conversation_only cases that declare
	// answer_turn_index into early/middle/late position thirds.
	answerThirds map[string]int
	// twinWarnings flags twin groups covering fewer than two distinct
	// answer_home values (warning-level; the hard check is Task 12's).
	twinWarnings []string
}

// parseMixedFixture strictly decodes a mixed-assembly fixture object.
// Unknown fields are rejected: the corpus is hand-authored, and a typoed
// key silently dropped is exactly the authoring failure this schema exists
// to catch.
func parseMixedFixture(raw []byte) (mixedFixture, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f mixedFixture
	if err := dec.Decode(&f); err != nil {
		return mixedFixture{}, err
	}
	if dec.More() {
		return mixedFixture{}, fmt.Errorf("trailing data after the fixture object")
	}
	return f, nil
}

// validateMixedFixture applies the corpus-level rules and per-case
// validation, returning the balance bookkeeping on success.
func validateMixedFixture(f mixedFixture) (mixedBookkeeping, error) {
	bk := mixedBookkeeping{stratumCounts: map[string]int{}, answerThirds: map[string]int{}}
	if f.Version != 1 {
		return bk, fmt.Errorf("unsupported fixture version %d (want 1)", f.Version)
	}
	if f.Kind != mixedFixtureKind {
		return bk, fmt.Errorf("invalid fixture kind %q (want %q)", f.Kind, mixedFixtureKind)
	}
	// The fixture restates the registered constants; the Go constants govern.
	if f.Constants.Fraction != mixedBudgetFraction {
		return bk, fmt.Errorf("constants.fraction %v does not equal registered mixedBudgetFraction %v (the Go constant governs)",
			f.Constants.Fraction, mixedBudgetFraction)
	}
	if f.Constants.EnvelopeTokens != mixedEnvelopeTokens {
		return bk, fmt.Errorf("constants.envelope_tokens %d does not equal registered mixedEnvelopeTokens %d (the Go constant governs)",
			f.Constants.EnvelopeTokens, mixedEnvelopeTokens)
	}
	if f.Constants.MinViableSlack != mixedMinViableSlack {
		return bk, fmt.Errorf("constants.min_viable_slack %d does not equal registered mixedMinViableSlack %d (the Go constant governs)",
			f.Constants.MinViableSlack, mixedMinViableSlack)
	}
	if len(f.Cases) == 0 {
		return bk, fmt.Errorf("no cases")
	}
	seen := make(map[string]struct{}, len(f.Cases))
	famStratum := map[string]string{} // scenario family -> the one stratum it may inhabit
	twinHomes := map[string]map[string]struct{}{}
	for _, c := range f.Cases {
		if _, dup := seen[c.ID]; dup {
			return bk, fmt.Errorf("duplicate case id %q", c.ID)
		}
		seen[c.ID] = struct{}{}
		if err := validateMixedCase(c); err != nil {
			return bk, fmt.Errorf("case %q: %w", c.ID, err)
		}
		// A scenario family is the bootstrap cluster unit and must stay within
		// one stratum. Rejected here at AUTHOR time; the report-side exclusion
		// (computeAssemblyMixedSection) stays as defense in depth.
		if c.ScenarioFamily != "" {
			if s, ok := famStratum[c.ScenarioFamily]; ok && s != c.Stratum {
				return bk, fmt.Errorf("scenario_family %q crosses strata %q and %q (a family must stay within one stratum)",
					c.ScenarioFamily, s, c.Stratum)
			}
			famStratum[c.ScenarioFamily] = c.Stratum
		}

		bk.stratumCounts[c.Stratum]++
		if c.Control {
			bk.controlCases++
		} else {
			bk.toplineEligible++
		}
		if c.Stratum == "conversation_only" && c.AnswerTurnIndex != nil {
			bk.answerThirds[mixedAnswerThird(c)]++
		}
		if c.TwinGroup != "" {
			if twinHomes[c.TwinGroup] == nil {
				twinHomes[c.TwinGroup] = map[string]struct{}{}
			}
			twinHomes[c.TwinGroup][c.AnswerHome] = struct{}{}
		}
	}
	twins := make([]string, 0, len(twinHomes))
	for tg := range twinHomes {
		twins = append(twins, tg)
	}
	sort.Strings(twins)
	for _, tg := range twins {
		if n := len(twinHomes[tg]); n < 2 {
			bk.twinWarnings = append(bk.twinWarnings, fmt.Sprintf(
				"twin_group %q covers %d answer_home value(s); twins should span >= 2 (hard check lands in Task 12)", tg, n))
		}
	}
	return bk, nil
}

// validateMixedCase applies the per-case rules: vocabularies, the pinned
// stratum/answer_home coherence table, the required_domains identity, event
// shape, domain presence, memory/rag content rules, the evidence contract,
// and answer_turn_index validity.
func validateMixedCase(c mixedCase) error {
	if !validAssemblyCaseID(c.ID) {
		return fmt.Errorf("invalid id %q: use lowercase ASCII letters, digits, and non-leading hyphens", c.ID)
	}
	if _, ok := mixedStrata[c.Stratum]; !ok {
		return fmt.Errorf("unknown stratum %q", c.Stratum)
	}
	if _, ok := mixedAnswerHomes[c.AnswerHome]; !ok {
		return fmt.Errorf("unknown answer_home %q", c.AnswerHome)
	}
	if !mixedStratumHomeOK(c.Stratum, c.AnswerHome) {
		return fmt.Errorf("stratum %q does not permit answer_home %q", c.Stratum, c.AnswerHome)
	}
	if err := validateMixedRequiredDomains(c.RequiredDomains); err != nil {
		return err
	}
	if strings.TrimSpace(c.System) == "" {
		return fmt.Errorf("system is required")
	}
	if strings.TrimSpace(c.Golden.FinalAnswerCriteria) == "" {
		return fmt.Errorf("golden.final_answer_criteria is required")
	}
	if err := validateMixedEvents(c); err != nil {
		return err
	}

	// Domain presence: every case carries all three domains as competing
	// distractors (the required_domains identity above is the declaration;
	// this is the enforcement).
	if mixedConversationTurnCount(c) == 0 {
		return fmt.Errorf("at least one conversation turn beyond the final question is required")
	}
	if len(c.MemoryRecords) == 0 {
		return fmt.Errorf("at least one memory record is required")
	}
	if len(c.RagSources) == 0 {
		return fmt.Errorf("at least one rag source is required")
	}

	seenRecords := make(map[string]struct{}, len(c.MemoryRecords))
	for i, r := range c.MemoryRecords {
		// A blank record ID is a 3b producer hard-error (agent_memory_search
		// rejects it at run time); fail at authoring instead.
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("memory_records[%d]: blank id", i)
		}
		if _, dup := seenRecords[r.ID]; dup {
			return fmt.Errorf("memory_records[%d]: duplicate id %q", i, r.ID)
		}
		seenRecords[r.ID] = struct{}{}
		if strings.TrimSpace(r.Content) == "" {
			return fmt.Errorf("memory_records[%d]: blank content", i)
		}
		// Restates memory's MemoryKind vocabulary (memory/record_agent.go:
		// working|semantic|episodic; blank = builder default). This file stays
		// memory-import-free, so the strings are pinned here; the Task 4 builder
		// seeds through the real store, which re-validates them.
		switch r.Kind {
		case "", "working", "semantic", "episodic":
		default:
			return fmt.Errorf("memory_records[%d]: unknown kind %q (want working, semantic, or episodic)", i, r.Kind)
		}
		// The store binds working records to a session, and a session requires a
		// workspace (memory/record_store.go Create); reject at author time
		// instead of failing the build.
		if r.Kind == "working" && strings.TrimSpace(r.WorkspaceID) == "" {
			return fmt.Errorf("memory_records[%d]: working kind requires a workspace_id", i)
		}
	}
	if err := validateAssemblySources(c.RagSources); err != nil {
		return fmt.Errorf("rag_sources: %w", err)
	}

	if !c.Control {
		if err := validateMixedEvidenceContract(c); err != nil {
			return err
		}
	}

	if c.ToplineFacts != "" {
		if c.Control {
			return fmt.Errorf("topline_facts is only legal on non-control cases (controls are excluded from the verdict and feed no ceiling arm)")
		}
		if strings.TrimSpace(c.ToplineFacts) == "" {
			return fmt.Errorf("topline_facts is whitespace-only; omit it or state the facts")
		}
		// Case-SENSITIVE containment: the facts must support the answer with
		// the same verbatim anchors the evidence contract pinned.
		for i, ev := range c.RequiredEvidence {
			if !strings.Contains(c.ToplineFacts, ev.Literal) {
				return fmt.Errorf("topline_facts does not contain required_evidence[%d] literal %q (the facts must support the answer)", i, ev.Literal)
			}
		}
	}

	if c.AnswerTurnIndex != nil {
		idx := *c.AnswerTurnIndex
		if idx < 0 || idx >= len(c.Events) {
			return fmt.Errorf("answer_turn_index %d out of range (events: %d)", idx, len(c.Events))
		}
		if c.Events[idx].Turn == nil {
			return fmt.Errorf("answer_turn_index %d must point at a turn event", idx)
		}
		if idx == len(c.Events)-1 {
			return fmt.Errorf("answer_turn_index %d must not be the final question turn", idx)
		}
	}
	return nil
}

// mixedStratumHomeOK is the PINNED stratum/answer_home coherence table.
func mixedStratumHomeOK(stratum, home string) bool {
	switch stratum {
	case "conversation_only":
		return home == "conversation"
	case "memory_only":
		return home == "memory"
	case "cross_domain_join":
		return home == "join"
	case "stale_vs_fresh":
		return true // any valid answer_home
	case "chain_retention":
		return home == "rag" || home == "memory" || home == "join"
	}
	return false
}

// validateMixedRequiredDomains enforces the identity: exactly
// {conversation, memory, rag}, any order, no duplicates.
func validateMixedRequiredDomains(domains []string) error {
	seen := make(map[string]struct{}, len(domains))
	for i, d := range domains {
		if !mixedValidEvidenceDomain(d) {
			return fmt.Errorf("required_domains[%d]: unknown domain %q", i, d)
		}
		if _, dup := seen[d]; dup {
			return fmt.Errorf("required_domains[%d]: duplicate domain %q", i, d)
		}
		seen[d] = struct{}{}
	}
	if len(seen) != len(mixedEvidenceDomains) {
		return fmt.Errorf("required_domains must be exactly conversation, memory, rag (got %v)", domains)
	}
	return nil
}

func mixedValidEvidenceDomain(d string) bool {
	for _, want := range mixedEvidenceDomains {
		if d == want {
			return true
		}
	}
	return false
}

// validateMixedEvents enforces the event-script shape: non-empty, each event
// exactly one of turn/tool_call, turn roles user|assistant with non-blank
// content, tool calls with unique call IDs / known tools / object args, the
// output_cap-cap_stress coupling, and a final user-question turn.
func validateMixedEvents(c mixedCase) error {
	if len(c.Events) == 0 {
		return fmt.Errorf("events are required")
	}
	callIDs := map[string]struct{}{}
	for i, e := range c.Events {
		switch {
		case e.Turn != nil && e.ToolCall != nil, e.Turn == nil && e.ToolCall == nil:
			return fmt.Errorf("event %d: exactly one of turn or tool_call must be set", i)
		case e.Turn != nil:
			if e.Turn.Role != "user" && e.Turn.Role != "assistant" {
				return fmt.Errorf("event %d: turn role %q invalid (want user or assistant)", i, e.Turn.Role)
			}
			if strings.TrimSpace(e.Turn.Content) == "" {
				return fmt.Errorf("event %d: turn content is blank", i)
			}
		default:
			if err := validateMixedToolCall(i, e.ToolCall, c.CapStress, callIDs); err != nil {
				return err
			}
		}
	}
	final := c.Events[len(c.Events)-1]
	if final.Turn == nil || final.Turn.Role != "user" {
		return fmt.Errorf("final event must be a user turn (the question) with non-empty content")
	}
	// A cap_stress case with nothing to cap tests nothing: the flag exists to
	// stress tool-output truncation.
	if c.CapStress && len(callIDs) == 0 {
		return fmt.Errorf("cap_stress case requires at least one tool_call event")
	}
	return nil
}

func validateMixedToolCall(i int, tc *mixedToolCall, capStress bool, callIDs map[string]struct{}) error {
	if strings.TrimSpace(tc.CallID) == "" {
		return fmt.Errorf("event %d: tool_call requires a non-empty call_id", i)
	}
	if _, dup := callIDs[tc.CallID]; dup {
		return fmt.Errorf("event %d: duplicate call_id %q", i, tc.CallID)
	}
	callIDs[tc.CallID] = struct{}{}
	if _, ok := mixedTools[tc.Tool]; !ok {
		return fmt.Errorf("event %d: unknown tool %q (want retrieve, agent_memory_search, or fixture_echo)", i, tc.Tool)
	}
	var args map[string]json.RawMessage
	if len(tc.Args) == 0 || json.Unmarshal(tc.Args, &args) != nil || len(args) == 0 {
		return fmt.Errorf("event %d: tool_call args must be a non-empty JSON object", i)
	}
	if tc.Tool == "fixture_echo" {
		var content string
		raw, ok := args["content"]
		if !ok || json.Unmarshal(raw, &content) != nil || strings.TrimSpace(content) == "" {
			return fmt.Errorf(`event %d: fixture_echo args require a non-empty string "content" field`, i)
		}
	}
	switch {
	case tc.OutputCap < 0:
		return fmt.Errorf("event %d: negative output_cap %d", i, tc.OutputCap)
	case tc.OutputCap > 0 && !capStress:
		return fmt.Errorf("event %d: output_cap is only legal on cap_stress cases", i)
	case tc.OutputCap == 0 && capStress:
		return fmt.Errorf("event %d: cap_stress tool_call requires output_cap > 0", i)
	}
	return nil
}

// mixedConversationTurnCount counts turn events excluding the final question
// turn — the conversation-domain content the evidence contract scans.
func mixedConversationTurnCount(c mixedCase) int {
	n := 0
	for i, e := range c.Events {
		if e.Turn != nil && i != len(c.Events)-1 {
			n++
		}
	}
	return n
}

// mixedDomainContains reports whether lit appears verbatim (case-sensitive)
// in one domain's fixture content — the CONTAINMENT side of the evidence
// contract.
func mixedDomainContains(c mixedCase, domain, lit string) bool {
	return mixedDomainScan(c, domain, func(s string) bool { return strings.Contains(s, lit) })
}

// mixedDomainContainsFold is the CONTAMINATION-side variant: case-insensitive
// on both sides, so a re-cased leak ("Flag-Alpha-7") cannot slip past.
// Containment stays case-sensitive (verbatim) by design — the asymmetry is a
// pure tightening.
func mixedDomainContainsFold(c mixedCase, domain, lit string) bool {
	folded := strings.ToLower(lit)
	return mixedDomainScan(c, domain, func(s string) bool {
		return strings.Contains(strings.ToLower(s), folded)
	})
}

// mixedDomainScan applies match to every item of one domain's fixture
// content, per item (no concatenation artifacts). Conversation content
// deliberately EXCLUDES the final question turn: a question that leaks its
// own literal must not satisfy containment — that would mask a case no
// domain can actually answer (same move as 3a's assemblyAnswerReach).
func mixedDomainScan(c mixedCase, domain string, match func(string) bool) bool {
	switch domain {
	case "conversation":
		for i, e := range c.Events {
			if e.Turn == nil || i == len(c.Events)-1 {
				continue
			}
			if match(e.Turn.Content) {
				return true
			}
		}
	case "memory":
		for _, r := range c.MemoryRecords {
			if match(r.Content) {
				return true
			}
		}
	case "rag":
		for _, s := range c.RagSources {
			if match(s.Content) || match(s.Abstract) || match(s.Overview) {
				return true
			}
		}
	}
	return false
}

// validateMixedEvidenceContract enforces the required/forbidden evidence
// rules for non-control cases: containment in the declared domain
// (case-sensitive), contamination-freedom everywhere else — the other two
// domains, the final question, and the system prompt, all case-insensitive —
// answer_home coherence, and join coverage. Callers skip it entirely for
// control cases, and it requires validateMixedEvents to have passed (it
// reads the final event as the guaranteed user-question turn).
func validateMixedEvidenceContract(c mixedCase) error {
	if len(c.RequiredEvidence) == 0 {
		return fmt.Errorf("non-control case requires at least one required_evidence entry")
	}
	lowerQuestion := strings.ToLower(c.Events[len(c.Events)-1].Turn.Content)
	lowerSystem := strings.ToLower(c.System)
	homes := map[string]struct{}{}
	for i, ev := range c.RequiredEvidence {
		if !mixedValidEvidenceDomain(ev.Domain) {
			return fmt.Errorf("required_evidence[%d]: unknown domain %q", i, ev.Domain)
		}
		if strings.TrimSpace(ev.Literal) == "" {
			return fmt.Errorf("required_evidence[%d]: blank literal", i)
		}
		if c.AnswerHome != "join" && ev.Domain != c.AnswerHome {
			return fmt.Errorf("required_evidence[%d]: domain %q does not match answer_home %q", i, ev.Domain, c.AnswerHome)
		}
		if !mixedDomainContains(c, ev.Domain, ev.Literal) {
			return fmt.Errorf("required_evidence[%d]: literal %q not found in %s content", i, ev.Literal, ev.Domain)
		}
		for _, d := range mixedEvidenceDomains {
			if d == ev.Domain {
				continue
			}
			if mixedDomainContainsFold(c, d, ev.Literal) {
				return fmt.Errorf("required_evidence[%d]: literal %q leaks into %s content", i, ev.Literal, d)
			}
		}
		folded := strings.ToLower(ev.Literal)
		if strings.Contains(lowerQuestion, folded) {
			return fmt.Errorf("required_evidence[%d]: literal %q appears in the final question", i, ev.Literal)
		}
		if strings.Contains(lowerSystem, folded) {
			return fmt.Errorf("required_evidence[%d]: literal %q appears in the system prompt", i, ev.Literal)
		}
		homes[ev.Domain] = struct{}{}
	}
	if c.AnswerHome == "join" && len(homes) < 2 {
		return fmt.Errorf("join case requires required_evidence spanning >= 2 distinct domains (got %d)", len(homes))
	}
	for i, lit := range c.ForbiddenEvidence {
		if strings.TrimSpace(lit) == "" {
			return fmt.Errorf("forbidden_evidence[%d]: blank literal", i)
		}
		for _, d := range mixedEvidenceDomains {
			if mixedDomainContainsFold(c, d, lit) {
				return fmt.Errorf("forbidden_evidence[%d]: literal %q present in %s content", i, lit, d)
			}
		}
	}
	return nil
}

// mixedAnswerThird buckets the answering conversation turn's position among
// the conversation turns (final question excluded) into thirds. Callers
// validate AnswerTurnIndex first, which also guarantees at least one
// conversation turn exists.
func mixedAnswerThird(c mixedCase) string {
	idx := *c.AnswerTurnIndex
	pos, count := 0, 0
	for i, e := range c.Events {
		if e.Turn == nil || i == len(c.Events)-1 {
			continue
		}
		if i == idx {
			pos = count
		}
		count++
	}
	switch pos * 3 / count {
	case 0:
		return "early"
	case 1:
		return "middle"
	default:
		return "late"
	}
}

// runMixedFixture parses + validates a mixed-assembly fixture, prints the
// bookkeeping summary to w, builds every case's frozen State through the
// production producers (buildMixedStates, assembly_mixed_state.go), then
// hands off to the Task 5 arm build: paired legacy/mixed assembly under the
// registered budget, the gates, and manifest-backed trace emission into
// outDir (emitMixedCorpus, assembly_mixed_build.go).
func runMixedFixture(ctx context.Context, raw []byte, outDir string, w io.Writer) error {
	f, err := parseMixedFixture(raw)
	if err != nil {
		return fmt.Errorf("assembly build: parse mixed fixture: %w", err)
	}
	bk, err := validateMixedFixture(f)
	if err != nil {
		return fmt.Errorf("assembly build: %w", err)
	}
	_, _ = fmt.Fprintf(w, "mixed-assembly fixture: %d case(s): %d control, %d topline-eligible\n",
		len(f.Cases), bk.controlCases, bk.toplineEligible)
	strata := make([]string, 0, len(bk.stratumCounts))
	for s := range bk.stratumCounts {
		strata = append(strata, s)
	}
	sort.Strings(strata)
	for _, s := range strata {
		_, _ = fmt.Fprintf(w, "  stratum %s: %d\n", s, bk.stratumCounts[s])
	}
	if len(bk.answerThirds) > 0 {
		_, _ = fmt.Fprintf(w, "  conversation_only answer thirds: early=%d middle=%d late=%d\n",
			bk.answerThirds["early"], bk.answerThirds["middle"], bk.answerThirds["late"])
	}
	for _, warn := range bk.twinWarnings {
		_, _ = fmt.Fprintf(w, "  warning: %s\n", warn)
	}
	states, err := buildMixedStates(ctx, f)
	if err != nil {
		return fmt.Errorf("assembly build: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  built %d frozen state(s)\n", len(states))
	if err := emitMixedCorpus(ctx, f, states, outDir, w); err != nil {
		return fmt.Errorf("assembly build: %w", err)
	}
	return nil
}

// assemblyBuildDispatch routes -assembly-build by fixture shape: a JSON
// array is the 3a flat/progressive case corpus (existing path, unchanged,
// emitted into outDir); a JSON object is the 3c mixed-assembly fixture,
// emitted into mixedOutDir (the mixed corpus keeps its own directory and its
// own manifest).
func assemblyBuildDispatch(ctx context.Context, fixturePath, outDir, mixedOutDir string) error {
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		return fmt.Errorf("assembly build: read fixture: %w", err)
	}
	switch trimmed := bytes.TrimLeftFunc(raw, unicode.IsSpace); {
	case len(trimmed) > 0 && trimmed[0] == '[':
		return assemblyBuild(ctx, fixturePath, outDir)
	case len(trimmed) > 0 && trimmed[0] == '{':
		return runMixedFixture(ctx, raw, mixedOutDir, os.Stderr)
	default:
		return fmt.Errorf("assembly build: fixture %q must be a JSON array of 3a assembly cases or a mixed-assembly fixture object with version/kind", fixturePath)
	}
}
