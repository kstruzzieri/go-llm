package main

// W3 review fold-ins (#331 slice 3c): exact topline quotas, the
// content-tightened subsequence matcher, pressure-target question/system
// scans, NUL-joined twin fingerprints, and decoded-tree tool-arg scans.

import (
	"encoding/json"
	"strings"
	"testing"
)

// (a) Topline quotas are exact at author time: a stratum with ANY eligible
// scenario family must carry at least its registered quota of families.
func TestMixedToplineQuotaFloor(t *testing.T) {
	c := mixedConvCase("qf-1")
	c.ScenarioFamily = "fam-solo"
	c.ToplineFacts = "For the beta rollout the team chose flag-alpha-7 as the gate."
	_, err := validateMixedFixture(mixedFixtureFor(c))
	if err == nil || !strings.Contains(err.Error(), "quota") || !strings.Contains(err.Error(), "conversation_only") {
		t.Errorf("1 family under quota 3 = %v; want a quota-floor error naming the stratum", err)
	}

	// A fixture with no eligible families anywhere never trips the floor
	// (topline selection has nothing to select; the pre-label corpus gate
	// owns global completeness).
	if _, err := validateMixedFixture(mixedFixtureFor(mixedConvCase("qf-2"))); err != nil {
		t.Errorf("family-free fixture = %v; want nil", err)
	}
}

// (b) The subsequence matcher's assistant tool-call branch also requires
// Content equality: mixed never rewrites tool-call turns, so a same-ID
// message with different Content is drift, not a match.
func TestMixedSubsequenceToolCallContentTightened(t *testing.T) {
	asst := mixedTestAsst("c1", `{"query":"x"}`)
	asst.Content = "original prose around the call"
	q := mixedTestUser("q")
	full := mixedTestState(asst, q)

	same := asst
	if err := mixedArmSubsequenceGate("sub-c1", AssemblyMixed, full, mixedTestState(same, q)); err != nil {
		t.Errorf("identical tool-call message rejected: %v", err)
	}

	rewritten := asst
	rewritten.Content = "REWRITTEN prose"
	err := mixedArmSubsequenceGate("sub-c2", AssemblyMixed, full, mixedTestState(rewritten, q))
	if err == nil || !strings.Contains(err.Error(), "sub-c2") {
		t.Errorf("content-rewritten tool-call message accepted = %v; want the subsequence rejection", err)
	}
}

// (c) The pressure target literal must be absent (case-insensitively) from
// the final question turn and the system prompt — the same fold scan the
// evidence contract applies.
func TestMixedPressureTargetAbsentFromQuestionAndSystem(t *testing.T) {
	t.Run("in the final question", func(t *testing.T) {
		c := mixedConvCase("pt-q")
		// "beta rollout" sits in conversation turn 0 (containment holds) AND
		// in the final question — the new scan must reject it.
		c.PressureTarget = &mixedEvidence{Domain: "conversation", Literal: "beta rollout"}
		err := validateMixedPressureTarget(c)
		if err == nil || !strings.Contains(err.Error(), "final question") {
			t.Errorf("question-leaking pressure target = %v; want a final-question rejection", err)
		}
	})
	t.Run("in the system prompt", func(t *testing.T) {
		c := mixedConvCase("pt-s")
		c.Events[1].Turn.Content = "Understood; I will keep the Provided Context in mind."
		c.PressureTarget = &mixedEvidence{Domain: "conversation", Literal: "Provided Context"}
		// System is "Answer using only the provided context." — a re-cased
		// hit, which the fold scan must still catch.
		err := validateMixedPressureTarget(c)
		if err == nil || !strings.Contains(err.Error(), "system") {
			t.Errorf("system-leaking pressure target = %v; want a system rejection", err)
		}
	})
	t.Run("clean target still accepted", func(t *testing.T) {
		if err := validateMixedPressureTarget(mixedConvCase("pt-ok")); err != nil {
			t.Errorf("default case = %v; want nil", err)
		}
	})
}

// (d) Twin fingerprints join with NUL: a single memory ID "rec-1, rec-2"
// must NOT alias the two-record set {rec-1, rec-2} the way the old ", "
// join let it.
func TestMixedTwinFingerprintNulJoin(t *testing.T) {
	a := mixedConvCase("tw-fp-a")
	a.TwinGroup = "tw-fp"
	a.MemoryRecords = []mixedMemoryRecord{
		{ID: "rec-1, rec-2", Content: "standup moved to 09:30 on wednesdays", Kind: "semantic"},
	}
	b := mixedMemCase("tw-fp-b")
	b.TwinGroup = "tw-fp"
	b.MemoryRecords = []mixedMemoryRecord{
		{ID: "rec-1", Content: "trainer checkpoint uses batch-size 512", Kind: "semantic"},
		{ID: "rec-2", Content: "harmless scheduling note", Kind: "semantic"},
	}
	_, err := validateMixedFixture(mixedFixtureFor(a, b))
	if err == nil || !strings.Contains(err.Error(), "memory_records") {
		t.Errorf("comma-aliased memory ID sets accepted as twins = %v; want a memory_records mismatch", err)
	}
}

// (e) mixedToolArgsContainFold also walks the DECODED JSON tree, so a
// \uXXXX-escaped spelling of a literal cannot smuggle past the raw-byte scan.
func TestMixedToolArgsDecodedTreeScan(t *testing.T) {
	// `flag-alpha-7` decodes to "flag-alpha-7" (the required_evidence
	// literal) but never appears verbatim in the raw bytes — the raw-byte
	// scan alone cannot see it.
	smuggled := withMixedToolCall(mixedConvCase("smuggle-1"), mixedToolCall{
		CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"flag\u002dalpha-7 gate"}`),
	})
	err := validateMixedCase(smuggled)
	if err == nil || !strings.Contains(err.Error(), "tool_call args") {
		t.Errorf("unicode-smuggled literal in args = %v; want the args contamination rejection", err)
	}

	// Smuggling through an object KEY is caught too.
	keySmuggled := withMixedToolCall(mixedConvCase("smuggle-2"), mixedToolCall{
		CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"flag\u002dalpha-7": "q"}`),
	})
	err = validateMixedCase(keySmuggled)
	if err == nil || !strings.Contains(err.Error(), "tool_call args") {
		t.Errorf("unicode-smuggled literal in an args KEY = %v; want the args contamination rejection", err)
	}

	// Clean args stay accepted.
	ok := withMixedToolCall(mixedConvCase("smuggle-ok"), mixedToolCall{
		CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"beta gate"}`),
	})
	if err := validateMixedCase(ok); err != nil {
		t.Errorf("clean args = %v; want nil", err)
	}
}
