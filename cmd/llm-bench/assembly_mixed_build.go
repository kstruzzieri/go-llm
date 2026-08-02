package main

// Mixed-assembly arm build (#331 slice 3c, Task 5): the registered budget
// formula, the paired legacy/mixed arm assembly over one frozen State, the
// pressure/contrast/survival/reachability gates, and prefilled trace emission
// with the 3a manifest machinery. Fixture parse/validate lives in
// assembly_mixed_fixture.go (agent-import-free); the frozen-State builder in
// assembly_mixed_state.go.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
)

// mixedTraceSource labels every emitted mixed-corpus trace, distinguishing it
// from the 3a "assembly-corpus" flat/progressive traces.
const mixedTraceSource = "assembly-corpus-mixed"

// mixedArm is one assembled arm plus its per-arm evidence: what the assembler
// returned, the pressure it reported, and the shed deltas against the frozen
// pre-assembly State. omittedSubjects is filled on the mixed arm only, from
// ContextAssemblyTrace.OmittedSubjects (the count of omitted subject rows).
type mixedArm struct {
	mode            AssemblyMode
	state           agent.State
	pressure        agent.Pressure
	shedMessages    int
	shedBytes       int
	omittedSubjects int
}

// mixedCaseBudget is the REGISTERED budget formula. Control cases get
// rawStateTokens + slack (generous: both arms fit fully); real cases get
// round(f * raw) floored at the minimum viable prompt — system + final user
// question + slack, all on the registered mixedEstTokens basis — so no legal
// fixture can starve the must-fit segment into ErrContextExhausted.
//
// Precondition: built.State's final message is the user question (the fixture
// validator guarantees a final user turn, and buildMixedState preserves event
// order).
func mixedCaseBudget(built mixedBuiltState, control bool) int {
	if control {
		return built.RawStateTokens + mixedMinViableSlack
	}
	question := built.State.Messages[len(built.State.Messages)-1].Content
	minViable := mixedEstTokens(built.State.System) + mixedEstTokens(question) + mixedMinViableSlack
	budget := int(math.Round(mixedBudgetFraction * float64(built.RawStateTokens)))
	if budget < minViable {
		return minViable
	}
	return budget
}

// buildMixedArms assembles the SAME frozen State under the SAME budget through
// both production assemblers.
//
// TokenBudget (agent/compactor.go) has exactly two fields; only Input is set.
// Thresholds is left zero deliberately: both assemblers normalize a zero value
// to the default pressure bands (PressureThresholds.normalize), so zero is
// safe and keeps the experiment's registered contract to the single budget
// knob. toolSchemaTokens is 0: prefilled replay declares no tool schemas.
//
// Estimate is left nil on both managers: agent's default heuristic
// (1+(len-1)/4, agent/context_manager.go) is the same 4-bytes-per-token family
// as the registered mixedEstTokens basis, but it is agent's OWN estimator. The
// REGISTERED basis governs only the budget and raw_state_tokens inputs; the
// assemblers price their content however production prices it.
//
// An assembly error is a loud build failure naming the arm: the budget floor
// in mixedCaseBudget keeps the must-fit segment viable for every legal
// fixture, so ErrContextExhausted here means the fixture (or the formula) is
// wrong — never something to paper over.
func buildMixedArms(ctx context.Context, built mixedBuiltState, budget int) (mixedArm, mixedArm, error) {
	const exhaustedHint = "the budget floor should keep every legal fixture assemblable; fixture bug"
	tb := agent.TokenBudget{Input: budget}
	legacySt, legacyP, err := agent.ContextManager{}.Assemble(ctx, built.State, 0, tb)
	if err != nil {
		return mixedArm{}, mixedArm{}, fmt.Errorf("legacy arm: assemble at budget %d: %w (%s)", budget, err, exhaustedHint)
	}
	mixedSt, mixedP, tr, err := agent.ContextManager{Mixed: true}.AssembleWithTrace(ctx, built.State, 0, tb)
	if err != nil {
		return mixedArm{}, mixedArm{}, fmt.Errorf("mixed arm: assemble at budget %d: %w (%s)", budget, err, exhaustedHint)
	}
	legacy := mixedArm{mode: AssemblyLegacy, state: legacySt, pressure: legacyP}
	legacy.shedMessages, legacy.shedBytes = mixedShed(built.State, legacySt)
	mixed := mixedArm{mode: AssemblyMixed, state: mixedSt, pressure: mixedP, omittedSubjects: tr.OmittedSubjects}
	mixed.shedMessages, mixed.shedBytes = mixedShed(built.State, mixedSt)
	return legacy, mixed, nil
}

// mixedShed is the per-arm pressure evidence: message-count and content-byte
// deltas of the assembled copy against the frozen State, clamped at zero (the
// mixed arm can legally REWRITE anchor Content, so a raw delta may go
// negative on either axis).
func mixedShed(before, after agent.State) (msgs, bytes int) {
	msgs = len(before.Messages) - len(after.Messages)
	if msgs < 0 {
		msgs = 0
	}
	bytes = mixedStateContentBytes(before) - mixedStateContentBytes(after)
	if bytes < 0 {
		bytes = 0
	}
	return msgs, bytes
}

func mixedStateContentBytes(st agent.State) int {
	n := 0
	for _, m := range st.Messages {
		n += len(m.Content)
	}
	return n
}

// mixedPressureGate (gate 1). Non-control: each arm must have shed something —
// a case the budget never pressures measures nothing. Control: the INVERSE
// assertion — the generous budget must retain everything, so ANY shed means
// agent's envelope pricing (tool-call args/IDs, which the registered raw
// basis does not count) exceeded raw+slack and the control traces would emit
// silently truncated.
func mixedPressureGate(caseID string, control bool, legacy, mixed mixedArm) error {
	for _, arm := range []mixedArm{legacy, mixed} {
		if control {
			if arm.shedMessages != 0 || arm.shedBytes != 0 {
				return fmt.Errorf("case %q: control %s arm shed %dmsg/%dB under the generous budget: agent's envelope (tool-call args/ids) exceeded raw+slack; shrink the case's tool-call payloads",
					caseID, arm.mode, arm.shedMessages, arm.shedBytes)
			}
			continue
		}
		if arm.shedMessages == 0 && arm.shedBytes == 0 {
			return fmt.Errorf("case %q: %s arm shows no pressure evidence (shed_messages=0, shed_bytes=0): the registered budget retained the full state (fixture bug: grow the state until f=%v binds)",
				caseID, arm.mode, mixedBudgetFraction)
		}
	}
	return nil
}

// mixedMsgFieldDiff names the first differing model-visible field between two
// messages, or "" when they are equal on every compared field: role, content,
// tool_call_id, tool_name, and the tool_calls list (ID, Name, argument
// bytes). Deliberately DEEP: two arms identical in role+content but differing
// in a tool-call ID or argument bytes present different prompts to the model,
// so a role+content compare would call them identical and mask real contrast
// (or miss real control drift).
func mixedMsgFieldDiff(a, b agent.Message) string {
	switch {
	case a.Role != b.Role:
		return "role"
	case a.Content != b.Content:
		return "content"
	case a.ToolCallID != b.ToolCallID:
		return "tool_call_id"
	case a.ToolName != b.ToolName:
		return "tool_name"
	}
	if len(a.ToolCalls) != len(b.ToolCalls) {
		return "tool_calls"
	}
	for i := range a.ToolCalls {
		ta, tb := a.ToolCalls[i], b.ToolCalls[i]
		if ta.ID != tb.ID || ta.Function.Name != tb.Function.Name ||
			!bytes.Equal(ta.Function.Arguments, tb.Function.Arguments) {
			return "tool_calls"
		}
	}
	return ""
}

// mixedArmsFirstDiff returns the first index at which the two assembled
// message sequences differ under mixedMsgFieldDiff plus the differing field
// name, or (-1, "") when identical. When one sequence is a strict prefix of
// the other, the diff index is the shorter length with field "presence" (the
// first message present on only one side).
func mixedArmsFirstDiff(a, b agent.State) (int, string) {
	n := len(a.Messages)
	if len(b.Messages) < n {
		n = len(b.Messages)
	}
	for i := 0; i < n; i++ {
		if field := mixedMsgFieldDiff(a.Messages[i], b.Messages[i]); field != "" {
			return i, field
		}
	}
	if len(a.Messages) != len(b.Messages) {
		return n, "presence"
	}
	return -1, ""
}

func mixedArmMessagesEqual(a, b agent.State) bool {
	idx, _ := mixedArmsFirstDiff(a, b)
	return idx < 0
}

// mixedRoleAt names the role at index i, or "(absent)" past the end — the
// prefix-diff case, where one arm simply has fewer messages.
func mixedRoleAt(st agent.State, i int) string {
	if i >= len(st.Messages) {
		return "(absent)"
	}
	return st.Messages[i].Role
}

// mixedArmsDifferGate (gate 2): non-control arms must differ (identical arms
// carry no assembly contrast); control arms must be identical (the generous
// budget retains everything on both paths, so a difference is a fixture bug —
// the anchor-free control rule in validateMixedCase should make this
// unreachable, and the diff index below is the belt for a future tool that
// slips past it).
func mixedArmsDifferGate(caseID string, control bool, legacy, mixed agent.State) error {
	idx, field := mixedArmsFirstDiff(legacy, mixed)
	if control {
		if idx >= 0 {
			return fmt.Errorf("case %q: control arms differ first at message %d in field %s (legacy role %s, mixed role %s): control cases must be anchor-free and fully retained; a control case with differing arms is a fixture bug",
				caseID, idx, field, mixedRoleAt(legacy, idx), mixedRoleAt(mixed, idx))
		}
		return nil
	}
	if idx < 0 {
		return fmt.Errorf("case %q: legacy and mixed arms are identical (deep compare: role, content, tool ids/names, tool calls): no assembly contrast to measure (fixture bug)", caseID)
	}
	return nil
}

// mixedQuestionSurvivalGate (gate 3, both arms, control included): the FINAL
// assembled message must be the original final user question EXACTLY — an
// identity check, never a substring one, so a question folded into some other
// message can never satisfy it.
func mixedQuestionSurvivalGate(caseID string, mode AssemblyMode, st agent.State, question string) error {
	if len(st.Messages) == 0 {
		return fmt.Errorf("case %q: %s arm assembled no messages at all", caseID, mode)
	}
	final := st.Messages[len(st.Messages)-1]
	if final.Role != "user" || final.Content != question {
		return fmt.Errorf("case %q: %s arm final message (role %q, content %q) is not identically the original user question (question survival is an identity check, not a substring check)",
			caseID, mode, final.Role, mixedPreview(final.Content))
	}
	return nil
}

// mixedPreview truncates diagnostic content to ~80 bytes on a rune boundary.
func mixedPreview(s string) string {
	if len(s) <= 80 {
		return s
	}
	return mixedCapContent(s, 80) + "..."
}

// mixedArmContains reports whether lit appears verbatim (case-SENSITIVE —
// anchors are verbatim) in any assembled message EXCLUDING the final question
// message. Scanned content per message is Content PLUS every tool call's
// argument bytes: args ride the assistant tool-call turn, so they are exactly
// as model-visible as content and an anchor reachable only through them still
// reaches. Per-message scan, no concatenation artifacts (mirrors
// mixedDomainScan); excluding the question mirrors 3a's assemblyAnswerReach —
// a question that leaks its own literal must not count as evidence.
func mixedArmContains(st agent.State, lit string) bool {
	if len(st.Messages) == 0 {
		return false
	}
	for _, m := range st.Messages[:len(st.Messages)-1] {
		if strings.Contains(m.Content, lit) {
			return true
		}
		for _, tc := range m.ToolCalls {
			if strings.Contains(string(tc.Function.Arguments), lit) {
				return true
			}
		}
	}
	return false
}

// mixedPressureTargetGate (runs after evidence reachability, non-control
// only) kills "pressure theater": both arms shedding only answer-irrelevant
// filler while every message carrying the registered pressure target survives
// byte-identical. Carriers are ALL full-State messages whose Content contains
// the target literal (>= 1, else the fixture never put the target on a
// model-visible message — loud fixture bug). The gate passes iff in AT LEAST
// ONE arm at least one carrier is ABSENT or PRESENT-with-different-Content
// (dropped, truncated, or re-rendered). "Unchanged" is the exact triple
// (Role, ToolCallID, Content) — an anchor rewrite keeps Role and ToolCallID
// but changes Content, so it counts as pressure on the carrier. Deliberately
// direction-neutral: no requirement on WHICH arm moved.
func mixedPressureTargetGate(caseID string, target mixedEvidence, full, legacy, mixed agent.State) error {
	var carriers []agent.Message
	for _, m := range full.Messages {
		if strings.Contains(m.Content, target.Literal) {
			carriers = append(carriers, m)
		}
	}
	if len(carriers) == 0 {
		return fmt.Errorf("case %q: pressure target %q (%s) appears in no built-State message content (fixture bug: the validated fixture containment never reached a model-visible message)",
			caseID, target.Literal, target.Domain)
	}
	for _, arm := range []agent.State{legacy, mixed} {
		for _, carrier := range carriers {
			if !mixedCarrierUnchanged(arm, carrier) {
				return nil
			}
		}
	}
	return fmt.Errorf("case %q: pressure target %q (%s) survived unchanged in both arms: pressure theater (the budget shed only answer-irrelevant filler; move the target where assembly actually bites)",
		caseID, target.Literal, target.Domain)
}

// mixedCarrierUnchanged reports whether the arm holds a byte-identical copy
// of the carrier: same Role, ToolCallID, and Content.
func mixedCarrierUnchanged(arm agent.State, carrier agent.Message) bool {
	for _, m := range arm.Messages {
		if m.Role == carrier.Role && m.ToolCallID == carrier.ToolCallID && m.Content == carrier.Content {
			return true
		}
	}
	return false
}

// mixedArmSubsequenceGate (both arms, every case, controls included) is
// always-on drift protection for the frozen runtime: the assembled messages
// must map to DISTINCT full-State indices in INCREASING order — no
// reordering, no duplication, no synthesized messages. Matching identity per
// message: Role must be equal; assistant tool-call messages match on their
// ToolCalls' ID sequence (mixed may re-render around them); other messages
// match on Content, except a tool message may instead match on ToolCallID
// (mixed legally REWRITES anchor Content in place).
func mixedArmSubsequenceGate(caseID string, mode AssemblyMode, full, assembled agent.State) error {
	j := 0
	for i, m := range assembled.Messages {
		for j < len(full.Messages) && !mixedSubsequenceMatch(full.Messages[j], m) {
			j++
		}
		if j >= len(full.Messages) {
			return fmt.Errorf("case %q: %s arm: assembler reordered or duplicated messages: assembled message %d (role %s) maps to no remaining full-State message in order",
				caseID, mode, i, m.Role)
		}
		j++
	}
	return nil
}

// mixedSubsequenceMatch is the per-message identity mixedArmSubsequenceGate
// walks with (see there for the rule).
func mixedSubsequenceMatch(full, asm agent.Message) bool {
	if full.Role != asm.Role {
		return false
	}
	if len(asm.ToolCalls) > 0 || len(full.ToolCalls) > 0 {
		if len(asm.ToolCalls) != len(full.ToolCalls) {
			return false
		}
		for i := range asm.ToolCalls {
			if asm.ToolCalls[i].ID != full.ToolCalls[i].ID {
				return false
			}
		}
		return true
	}
	if full.Content == asm.Content {
		return true
	}
	return asm.Role == "tool" && asm.ToolCallID != "" && full.ToolCallID == asm.ToolCallID
}

// mixedEvidenceGate (gate 4, non-control): every required-evidence anchor must
// reach at least one assembled arm, and ALL anchors must co-occur in at least
// one arm (join semantics; a single-anchor case degenerates to plain reach).
// An anchor reaching exactly one arm is legal and noted on w, mirroring 3a's
// single-arm answer_literal note.
func mixedEvidenceGate(caseID string, evidence []mixedEvidence, legacy, mixed agent.State, w io.Writer) error {
	allLegacy, allMixed := true, true
	placements := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		inLegacy := mixedArmContains(legacy, ev.Literal)
		inMixed := mixedArmContains(mixed, ev.Literal)
		switch {
		case !inLegacy && !inMixed:
			return fmt.Errorf("case %q: required evidence %q (%s) reaches neither assembled arm", caseID, ev.Literal, ev.Domain)
		case inLegacy != inMixed:
			arm := AssemblyLegacy
			if inMixed {
				arm = AssemblyMixed
			}
			_, _ = fmt.Fprintf(w, "mixed-assembly build: %s: required evidence %q reaches the %s arm only\n", caseID, ev.Literal, arm)
		}
		allLegacy = allLegacy && inLegacy
		allMixed = allMixed && inMixed
		placements = append(placements, fmt.Sprintf("%q->legacy=%t,mixed=%t", ev.Literal, inLegacy, inMixed))
	}
	if !allLegacy && !allMixed {
		return fmt.Errorf("case %q: required evidence does not co-occur in any single arm (join semantics need all anchors reachable together): %s",
			caseID, strings.Join(placements, "; "))
	}
	return nil
}

// mixedTurnsFromState maps an assembled State to llm-bench prefilled Turns.
// Tool-call args map into ToolCall.Arguments (the Turn's ToolCall struct has
// the slot); tool observations carry Content, ToolCallID, and Name.
func mixedTurnsFromState(st agent.State) []Turn {
	turns := make([]Turn, 0, len(st.Messages))
	for _, m := range st.Messages {
		turn := Turn{Role: m.Role, Content: m.Content}
		if m.Role == "tool" {
			turn.ToolCallID = m.ToolCallID
			turn.Name = m.ToolName
		}
		for _, tc := range m.ToolCalls {
			turn.ToolCalls = append(turn.ToolCalls, ToolCall{
				ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		turns = append(turns, turn)
	}
	return turns
}

// mixedTraceEstTokens is the registered-basis prompt estimate for one emitted
// trace: est(System) + sum est(turn Content). Deliberately the registered
// mixedEstTokens, not agent's estimator — this is trace metadata for the
// report's token-reduction column.
func mixedTraceEstTokens(system string, turns []Turn) int {
	n := mixedEstTokens(system)
	for _, t := range turns {
		n += mixedEstTokens(t.Content)
	}
	return n
}

// mixedArmTrace renders one arm's prefilled trace with the full AssemblyEval
// v2 metadata. OmittedSubjects is stamped on the mixed arm only (it is always
// zero on legacy, which has no subjects).
func mixedArmTrace(c mixedCase, built mixedBuiltState, budget int, arm mixedArm) Trace {
	turns := mixedTurnsFromState(arm.state)
	ae := &AssemblyEval{
		PairID:                c.ID,
		Mode:                  arm.mode,
		CandidateIDs:          built.CandidateIDs,
		EstimatedPromptTokens: mixedTraceEstTokens(arm.state.System, turns),
		StateDigest:           built.StateDigest,
		Subjects:              built.Subjects,
		Stratum:               c.Stratum,
		AnswerHome:            c.AnswerHome,
		ScenarioFamily:        c.ScenarioFamily,
		TwinGroup:             c.TwinGroup,
		Control:               c.Control,
		Budget:                budget,
		RawStateTokens:        built.RawStateTokens,
		PressureLevel:         arm.pressure.Level.String(),
		ShedMessages:          arm.shedMessages,
		ShedBytes:             arm.shedBytes,
	}
	if arm.mode == AssemblyMixed {
		ae.OmittedSubjects = arm.omittedSubjects
	}
	return Trace{
		ID:           c.ID + "-" + string(arm.mode),
		Source:       mixedTraceSource,
		System:       arm.state.System,
		Turns:        turns,
		Golden:       c.Golden,
		AssemblyEval: ae,
	}
}

// mixedToplineTrace renders the unpaired full-facts ceiling arm for a case
// that declares topline_facts (validated non-control with every required
// literal contained). CandidateIDs/StateDigest/Budget stay empty: topline
// validation requires only PairID, and the arm never enters pairing.
//
// topline_facts is trusted fixture prose: a "Question:" embedded inside the
// facts is not rejected — it could only confuse the ceiling arm itself, which
// is descriptive and never enters the paired verdict.
func mixedToplineTrace(c mixedCase) Trace {
	question := c.Events[len(c.Events)-1].Turn.Content
	content := "Facts:\n" + c.ToplineFacts + "\n\nQuestion: " + question
	return Trace{
		ID:     c.ID + "-" + string(AssemblyTopline),
		Source: mixedTraceSource,
		System: c.System,
		Turns:  []Turn{{Role: "user", Content: content}},
		Golden: c.Golden,
		AssemblyEval: &AssemblyEval{
			PairID:                c.ID,
			Mode:                  AssemblyTopline,
			EstimatedPromptTokens: mixedEstTokens(c.System) + mixedEstTokens(content),
			Stratum:               c.Stratum,
			AnswerHome:            c.AnswerHome,
			ScenarioFamily:        c.ScenarioFamily,
			TwinGroup:             c.TwinGroup,
		},
	}
}

// buildMixedCaseTraces runs one case end to end: budget, both arms, the
// always-on subsequence invariant, the four gates in registered order, the
// pressure-target carrier-change gate, then the two (or three, with topline)
// validated traces. A validateTrace failure on an assembled arm is a REAL
// finding — the prefilled contract says assembled chains stay atomic — so it
// is a loud stop, never a reason to loosen validation.
//
// Precondition: built is c's own frozen State (emitMixedCorpus pairs them by
// index), whose final message is the case's user question.
func buildMixedCaseTraces(ctx context.Context, c mixedCase, built mixedBuiltState, w io.Writer) ([]Trace, error) {
	budget := mixedCaseBudget(built, c.Control)
	legacy, mixed, err := buildMixedArms(ctx, built, budget)
	if err != nil {
		return nil, fmt.Errorf("case %q: %w", c.ID, err)
	}
	for _, arm := range []mixedArm{legacy, mixed} {
		if err := mixedArmSubsequenceGate(c.ID, arm.mode, built.State, arm.state); err != nil {
			return nil, err
		}
	}
	if err := mixedPressureGate(c.ID, c.Control, legacy, mixed); err != nil {
		return nil, err
	}
	if err := mixedArmsDifferGate(c.ID, c.Control, legacy.state, mixed.state); err != nil {
		return nil, err
	}
	question := built.State.Messages[len(built.State.Messages)-1].Content
	for _, arm := range []mixedArm{legacy, mixed} {
		if err := mixedQuestionSurvivalGate(c.ID, arm.mode, arm.state, question); err != nil {
			return nil, err
		}
	}
	if !c.Control {
		if err := mixedEvidenceGate(c.ID, c.RequiredEvidence, legacy.state, mixed.state, w); err != nil {
			return nil, err
		}
		if c.PressureTarget == nil {
			return nil, fmt.Errorf("case %q: non-control case reached the arm build without a pressure_target (validation bug)", c.ID)
		}
		if err := mixedPressureTargetGate(c.ID, *c.PressureTarget, built.State, legacy.state, mixed.state); err != nil {
			return nil, err
		}
	}
	traces := []Trace{
		mixedArmTrace(c, built, budget, legacy),
		mixedArmTrace(c, built, budget, mixed),
	}
	if c.ToplineFacts != "" {
		traces = append(traces, mixedToplineTrace(c))
	}
	for _, tr := range traces {
		if err := validateTrace(tr); err != nil {
			return nil, fmt.Errorf("case %q: built trace %q fails prefilled validation: %w (an assembled arm violating the prefilled contract is a real finding: report it, do not loosen validation)",
				c.ID, tr.ID, err)
		}
	}
	_, _ = fmt.Fprintf(w, "  case %s gated: budget=%d legacy shed=%dmsg/%dB mixed shed=%dmsg/%dB omitted_subjects=%d\n",
		c.ID, budget, legacy.shedMessages, legacy.shedBytes, mixed.shedMessages, mixed.shedBytes, mixed.omittedSubjects)
	return traces, nil
}

// emitMixedCorpus builds every case's arms, gates them, and publishes the
// traces to outDir under the 3a manifest machinery: the mixed corpus keeps its
// OWN manifest in its own directory, preflight refuses unowned output, and
// stale owned traces are reconciled away. All gates for all cases run before
// the first byte is written.
func emitMixedCorpus(ctx context.Context, f mixedFixture, states []mixedBuiltState, outDir string, w io.Writer) error {
	if len(states) != len(f.Cases) {
		return fmt.Errorf("built %d state(s) for %d case(s): buildMixedStates must produce exactly one State per case, in order", len(states), len(f.Cases))
	}
	traces := make([]Trace, 0, len(f.Cases)*3)
	for i, c := range f.Cases {
		caseTraces, err := buildMixedCaseTraces(ctx, c, states[i], w)
		if err != nil {
			return err
		}
		traces = append(traces, caseTraces...)
	}
	expected := make(map[string]struct{}, len(traces))
	for _, tr := range traces {
		expected[tr.ID+".json"] = struct{}{}
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	previous, err := readAssemblyManifest(outDir)
	if err != nil {
		return fmt.Errorf("read output manifest: %w", err)
	}
	if err := preflightAssemblyOutput(outDir, previous.Files, expected); err != nil {
		return fmt.Errorf("preflight output: %w", err)
	}
	for _, tr := range traces {
		if err := writeTraceJSON(filepath.Join(outDir, tr.ID+".json"), tr); err != nil {
			return err
		}
	}
	if err := removeStaleAssemblyTraces(outDir, previous.Files, expected); err != nil {
		return fmt.Errorf("reconcile output: %w", err)
	}
	if err := writeAssemblyManifest(outDir, expected); err != nil {
		return fmt.Errorf("write output manifest: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  wrote %d trace(s)\n", len(traces))
	return nil
}
