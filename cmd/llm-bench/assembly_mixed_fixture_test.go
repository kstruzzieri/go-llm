package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/ollama"
)

// Fixture helpers. Each returns a fresh, fully valid case; reject rows
// mutate a copy. Evidence literals are chosen so each appears in exactly
// its declared domain and nowhere else (not in the other domains, the
// final question, or the system prompt).

func mixedConvCase(id string) mixedCase {
	return mixedCase{
		ID:         id,
		Stratum:    "conversation_only",
		AnswerHome: "conversation",
		System:     "Answer using only the provided context.",
		Events: []mixedEvent{
			{Turn: &mixedTurn{Role: "user", Content: "For the beta rollout we settled on flag-alpha-7 as the gate."}},
			{Turn: &mixedTurn{Role: "assistant", Content: "Understood, I will keep that in mind."}},
			{Turn: &mixedTurn{Role: "user", Content: "Which gate did we settle on for the beta rollout?"}},
		},
		MemoryRecords: []mixedMemoryRecord{
			{ID: "rec-1", Content: "standup moved to 09:30 on wednesdays", Kind: "working", WorkspaceID: "ws-1"},
		},
		RagSources: []assemblySource{
			{Path: "pkg/cfg/cfg.go", Content: "package cfg\n\n// deploy window opens at 04:00 UTC\n", Language: "go"},
		},
		RequiredEvidence: []mixedEvidence{{Domain: "conversation", Literal: "flag-alpha-7"}},
		RequiredDomains:  []string{"conversation", "memory", "rag"},
		// W2 mandatory fields: the answering turn (required on
		// conversation_only) and the registered pressure target (required on
		// every non-control case; recommended authoring is an anchor).
		AnswerTurnIndex: intPtr(0),
		PressureTarget:  &mixedEvidence{Domain: "conversation", Literal: "flag-alpha-7"},
		Golden:          Golden{FinalAnswerCriteria: "States the beta gate is flag-alpha-7."},
	}
}

func mixedMemCase(id string) mixedCase {
	c := mixedConvCase(id)
	c.Stratum = "memory_only"
	c.AnswerHome = "memory"
	c.Events = []mixedEvent{
		{Turn: &mixedTurn{Role: "user", Content: "We were tuning the trainer earlier; details are in my notes."}},
		{Turn: &mixedTurn{Role: "assistant", Content: "Noted, I will check them."}},
		{Turn: &mixedTurn{Role: "user", Content: "What checkpoint size did we record for the trainer?"}},
	}
	c.MemoryRecords = []mixedMemoryRecord{
		{ID: "rec-1", Content: "trainer checkpoint uses batch-size 512", Kind: "semantic"},
	}
	c.RequiredEvidence = []mixedEvidence{{Domain: "memory", Literal: "batch-size 512"}}
	c.PressureTarget = &mixedEvidence{Domain: "memory", Literal: "batch-size 512"}
	c.Golden = Golden{FinalAnswerCriteria: "States the trainer checkpoint batch size is 512."}
	return c
}

func mixedJoinCase(id string) mixedCase {
	c := mixedConvCase(id)
	c.Stratum = "cross_domain_join"
	c.AnswerHome = "join"
	c.Events = []mixedEvent{
		{Turn: &mixedTurn{Role: "user", Content: "We reviewed the gateway settings yesterday."}},
		{Turn: &mixedTurn{Role: "assistant", Content: "Yes, both values are recorded."}},
		{Turn: &mixedTurn{Role: "user", Content: "What port and retry limit does the gateway use?"}},
	}
	c.MemoryRecords = []mixedMemoryRecord{
		{ID: "rec-1", Content: "gateway listens on port 7443", Kind: "semantic"},
	}
	c.RagSources = []assemblySource{
		{Path: "pkg/gw/gw.go", Content: "package gw\n\nconst retryCeiling = 6 // retry ceiling 6\n", Language: "go"},
	}
	c.RequiredEvidence = []mixedEvidence{
		{Domain: "memory", Literal: "port 7443"},
		{Domain: "rag", Literal: "retry ceiling 6"},
	}
	c.PressureTarget = &mixedEvidence{Domain: "memory", Literal: "port 7443"}
	c.Golden = Golden{FinalAnswerCriteria: "Combines the memory port with the rag retry ceiling."}
	return c
}

func mixedStaleCase(id string) mixedCase {
	// stale_vs_fresh permits any valid answer_home; conversation pins that.
	c := mixedConvCase(id)
	c.Stratum = "stale_vs_fresh"
	return c
}

func mixedChainCase(id string) mixedCase {
	c := mixedMemCase(id)
	c.Stratum = "chain_retention"
	return c
}

func mixedControlCase(id string) mixedCase {
	c := mixedConvCase(id)
	c.Control = true
	c.RequiredEvidence = nil
	c.PressureTarget = nil // controls must not carry a pressure target
	return c
}

// withMixedToolCall inserts a tool_call event immediately before the final
// question turn.
func withMixedToolCall(c mixedCase, tc mixedToolCall) mixedCase {
	events := append([]mixedEvent{}, c.Events[:len(c.Events)-1]...)
	events = append(events, mixedEvent{ToolCall: &tc}, c.Events[len(c.Events)-1])
	c.Events = events
	return c
}

func mixedCapStressCase(id string) mixedCase {
	c := withMixedToolCall(mixedConvCase(id), mixedToolCall{
		CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"beta gate"}`), OutputCap: 96,
	})
	c.CapStress = true
	return c
}

func mixedFixtureFor(cases ...mixedCase) mixedFixture {
	return mixedFixture{
		Version:   1,
		Kind:      "mixed-assembly",
		Constants: mixedFixtureConstants{Fraction: 0.6, EnvelopeTokens: 8, MinViableSlack: 64},
		Cases:     cases,
	}
}

func intPtr(v int) *int { return &v }

func TestMixedFixtureParseDispatch(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	outDir := filepath.Join(dir, "out")
	mixedOutDir := filepath.Join(dir, "out-mixed")

	t.Run("array routes to the 3a builder", func(t *testing.T) {
		path := write("arr.json", "[]")
		err := assemblyBuildDispatch(context.Background(), path, outDir, mixedOutDir)
		if err == nil || !strings.Contains(err.Error(), "no cases") {
			t.Fatalf("err = %v; want the 3a builder's no-cases error", err)
		}
	})

	t.Run("object routes to the mixed builder", func(t *testing.T) {
		raw, err := json.Marshal(mixedFixtureFor(mixedConvCase("case-1")))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		path := write("mixed.json", string(raw))
		// The tiny case retains everything under the minViable-floored budget,
		// so the mixed builder's own pressure-evidence gate rejects it: proof
		// the object shape routed all the way into the Task 5 arm build.
		err = assemblyBuildDispatch(context.Background(), path, outDir, mixedOutDir)
		if err == nil || !strings.Contains(err.Error(), "pressure evidence") {
			t.Fatalf("err = %v; want the mixed builder's pressure-evidence gate error", err)
		}
	})

	t.Run("garbage errors naming both shapes", func(t *testing.T) {
		path := write("garbage.json", `"nope"`)
		err := assemblyBuildDispatch(context.Background(), path, outDir, mixedOutDir)
		if err == nil || !strings.Contains(err.Error(), "JSON array") || !strings.Contains(err.Error(), "mixed-assembly fixture") {
			t.Fatalf("err = %v; want a shape error naming both accepted shapes", err)
		}
	})

	t.Run("unknown fixture field is rejected", func(t *testing.T) {
		raw, err := json.Marshal(mixedFixtureFor(mixedConvCase("case-1")))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body := strings.Replace(string(raw), `"version"`, `"versionn"`, 1)
		path := write("typo.json", body)
		err = assemblyBuildDispatch(context.Background(), path, outDir, mixedOutDir)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("err = %v; want an unknown-field parse error", err)
		}
	})
}

func TestValidateMixedFixtureCorpus(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*mixedFixture)
		wantErr string
	}{
		{name: "valid corpus accepted", mutate: func(*mixedFixture) {}},
		{
			name:    "wrong version",
			mutate:  func(f *mixedFixture) { f.Version = 2 },
			wantErr: "version",
		},
		{
			name:    "wrong kind",
			mutate:  func(f *mixedFixture) { f.Kind = "assembly" },
			wantErr: "kind",
		},
		{
			name:    "fraction constant mismatch",
			mutate:  func(f *mixedFixture) { f.Constants.Fraction = 0.5 },
			wantErr: "mixedBudgetFraction",
		},
		{
			name:    "envelope constant mismatch",
			mutate:  func(f *mixedFixture) { f.Constants.EnvelopeTokens = 9 },
			wantErr: "mixedEnvelopeTokens",
		},
		{
			name:    "slack constant mismatch",
			mutate:  func(f *mixedFixture) { f.Constants.MinViableSlack = 65 },
			wantErr: "mixedMinViableSlack",
		},
		{
			name:    "empty cases",
			mutate:  func(f *mixedFixture) { f.Cases = nil },
			wantErr: "no cases",
		},
		{
			name: "duplicate case ids",
			mutate: func(f *mixedFixture) {
				f.Cases = []mixedCase{mixedConvCase("case-1"), mixedConvCase("case-1")}
			},
			wantErr: "duplicate case id",
		},
		{
			name: "scenario family crossing strata",
			mutate: func(f *mixedFixture) {
				a := mixedConvCase("case-a")
				a.ScenarioFamily = "fam-x"
				b := mixedMemCase("case-b")
				b.ScenarioFamily = "fam-x"
				f.Cases = []mixedCase{a, b}
			},
			wantErr: "crosses strata",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := mixedFixtureFor(mixedConvCase("case-1"), mixedMemCase("case-2"))
			tt.mutate(&f)
			_, err := validateMixedFixture(f)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMixedFixture = %v; want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateMixedFixture = %v; want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateMixedCase(t *testing.T) {
	mutConv := func(f func(*mixedCase)) mixedCase {
		c := mixedConvCase("case-1")
		f(&c)
		return c
	}
	tests := []struct {
		name    string
		c       mixedCase
		wantErr string
	}{
		// One accept row per stratum, plus control and cap_stress.
		{name: "accept conversation_only", c: mixedConvCase("case-1")},
		{name: "accept memory_only", c: mixedMemCase("case-1")},
		{name: "accept cross_domain_join", c: mixedJoinCase("case-1")},
		{name: "accept stale_vs_fresh any answer_home", c: mixedStaleCase("case-1")},
		{
			name: "accept stale_vs_fresh memory home",
			c: func() mixedCase {
				c := mixedMemCase("case-1")
				c.Stratum = "stale_vs_fresh"
				return c
			}(),
		},
		{name: "accept chain_retention memory home", c: mixedChainCase("case-1")},
		{
			name: "accept chain_retention rag home",
			c: func() mixedCase {
				c := mixedConvCase("case-1")
				c.Stratum = "chain_retention"
				c.AnswerHome = "rag"
				c.RequiredEvidence = []mixedEvidence{{Domain: "rag", Literal: "04:00 UTC"}}
				return c
			}(),
		},
		{name: "accept control without required_evidence", c: mixedControlCase("case-1")},
		{name: "accept cap_stress with output_cap", c: mixedCapStressCase("case-1")},
		{
			name: "accept tool calls without caps on non-cap_stress case",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "agent_memory_search", Args: json.RawMessage(`{"query":"gate"}`),
			}),
		},
		{
			name: "accept absent forbidden literal",
			c:    mutConv(func(c *mixedCase) { c.ForbiddenEvidence = []string{"stale-flag-9"} }),
		},
		{
			name: "accept valid answer_turn_index",
			c:    mutConv(func(c *mixedCase) { c.AnswerTurnIndex = intPtr(0) }),
		},

		// Rule 1: vocabularies.
		{
			name:    "invalid id",
			c:       mutConv(func(c *mixedCase) { c.ID = "Bad_ID" }),
			wantErr: "invalid id",
		},
		{
			name:    "unknown stratum",
			c:       mutConv(func(c *mixedCase) { c.Stratum = "conversationish" }),
			wantErr: "unknown stratum",
		},
		{
			name:    "unknown answer_home",
			c:       mutConv(func(c *mixedCase) { c.AnswerHome = "wiki" }),
			wantErr: "unknown answer_home",
		},

		// Rule 2: stratum/answer_home coherence table.
		{
			name:    "conversation_only cannot home in memory",
			c:       mutConv(func(c *mixedCase) { c.AnswerHome = "memory" }),
			wantErr: "does not permit answer_home",
		},
		{
			name: "memory_only cannot home in conversation",
			c: func() mixedCase {
				c := mixedMemCase("case-1")
				c.AnswerHome = "conversation"
				return c
			}(),
			wantErr: "does not permit answer_home",
		},
		{
			name: "cross_domain_join must home in join",
			c: func() mixedCase {
				c := mixedJoinCase("case-1")
				c.AnswerHome = "rag"
				return c
			}(),
			wantErr: "does not permit answer_home",
		},
		{
			name: "chain_retention cannot home in conversation",
			c: func() mixedCase {
				c := mixedChainCase("case-1")
				c.AnswerHome = "conversation"
				return c
			}(),
			wantErr: "does not permit answer_home",
		},

		// Rule 3: required_domains exactly {conversation, memory, rag}.
		{
			name:    "required_domains subset rejected",
			c:       mutConv(func(c *mixedCase) { c.RequiredDomains = []string{"conversation", "memory"} }),
			wantErr: "must be exactly conversation, memory, rag",
		},
		{
			name:    "required_domains duplicate rejected",
			c:       mutConv(func(c *mixedCase) { c.RequiredDomains = []string{"conversation", "conversation", "memory"} }),
			wantErr: "duplicate domain",
		},
		{
			name:    "required_domains unknown value rejected",
			c:       mutConv(func(c *mixedCase) { c.RequiredDomains = []string{"conversation", "memory", "join"} }),
			wantErr: "unknown domain",
		},

		// System and golden.
		{
			name:    "blank system",
			c:       mutConv(func(c *mixedCase) { c.System = "  " }),
			wantErr: "system is required",
		},
		{
			name:    "blank golden criteria",
			c:       mutConv(func(c *mixedCase) { c.Golden.FinalAnswerCriteria = "" }),
			wantErr: "golden.final_answer_criteria",
		},

		// Rule 4: events.
		{
			name:    "empty events",
			c:       mutConv(func(c *mixedCase) { c.Events = nil }),
			wantErr: "events are required",
		},
		{
			name: "event with both turn and tool_call",
			c: mutConv(func(c *mixedCase) {
				c.Events[1].ToolCall = &mixedToolCall{CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"x"}`)}
			}),
			wantErr: "exactly one of turn or tool_call",
		},
		{
			name:    "event with neither turn nor tool_call",
			c:       mutConv(func(c *mixedCase) { c.Events[1] = mixedEvent{} }),
			wantErr: "exactly one of turn or tool_call",
		},
		{
			name:    "turn role tool rejected",
			c:       mutConv(func(c *mixedCase) { c.Events[1].Turn.Role = "tool" }),
			wantErr: "turn role",
		},
		{
			name:    "blank turn content",
			c:       mutConv(func(c *mixedCase) { c.Events[1].Turn.Content = " \n" }),
			wantErr: "turn content is blank",
		},
		{
			name: "final event must not be a tool call",
			c: mutConv(func(c *mixedCase) {
				c.Events = append(c.Events, mixedEvent{ToolCall: &mixedToolCall{
					CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"x"}`),
				}})
			}),
			wantErr: "final event must be a user turn",
		},
		{
			name: "final event must not be an assistant turn",
			c: mutConv(func(c *mixedCase) {
				c.Events = append(c.Events, mixedEvent{Turn: &mixedTurn{Role: "assistant", Content: "done"}})
			}),
			wantErr: "final event must be a user turn",
		},
		{
			name: "blank tool call_id",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "  ", Tool: "retrieve", Args: json.RawMessage(`{"query":"x"}`),
			}),
			wantErr: "non-empty call_id",
		},
		{
			name: "duplicate tool call_id",
			c: withMixedToolCall(withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"x"}`),
			}), mixedToolCall{
				CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"y"}`),
			}),
			wantErr: "duplicate call_id",
		},
		{
			name: "unknown tool",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "grep", Args: json.RawMessage(`{"query":"x"}`),
			}),
			wantErr: "unknown tool",
		},
		{
			name: "missing args",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "retrieve",
			}),
			wantErr: "args must be a non-empty JSON object",
		},
		{
			name: "invalid args JSON",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query"`),
			}),
			wantErr: "args must be a non-empty JSON object",
		},
		{
			name: "non-object args",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`["x"]`),
			}),
			wantErr: "args must be a non-empty JSON object",
		},
		{
			name: "null args",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`null`),
			}),
			wantErr: "args must be a non-empty JSON object",
		},
		{
			name: "empty object args",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{}`),
			}),
			wantErr: "args must be a non-empty JSON object",
		},
		{
			name:    "cap_stress without tool_call events",
			c:       mutConv(func(c *mixedCase) { c.CapStress = true }),
			wantErr: "at least one tool_call event",
		},
		{
			name: "fixture_echo without content",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "fixture_echo", Args: json.RawMessage(`{"text":"x"}`),
			}),
			wantErr: "fixture_echo args require",
		},
		{
			name: "fixture_echo blank content",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "fixture_echo", Args: json.RawMessage(`{"content":"  "}`),
			}),
			wantErr: "fixture_echo args require",
		},
		{
			name: "fixture_echo non-string content",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "fixture_echo", Args: json.RawMessage(`{"content":7}`),
			}),
			wantErr: "fixture_echo args require",
		},
		{
			name: "output_cap without cap_stress",
			c: withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
				CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"x"}`), OutputCap: 64,
			}),
			wantErr: "only legal on cap_stress",
		},
		{
			name: "cap_stress with zero output_cap",
			c: func() mixedCase {
				c := mixedCapStressCase("case-1")
				c.Events[2].ToolCall.OutputCap = 0
				return c
			}(),
			wantErr: "requires output_cap > 0",
		},
		{
			name: "negative output_cap",
			c: func() mixedCase {
				c := mixedCapStressCase("case-1")
				c.Events[2].ToolCall.OutputCap = -3
				return c
			}(),
			wantErr: "negative output_cap",
		},

		// Rule 5: domain presence.
		{
			name: "no conversation turn beyond the final question",
			c: mutConv(func(c *mixedCase) {
				c.Events = []mixedEvent{c.Events[len(c.Events)-1]}
				c.RequiredEvidence = nil
				c.Control = true
			}),
			wantErr: "at least one conversation turn",
		},
		{
			name:    "no memory records",
			c:       mutConv(func(c *mixedCase) { c.MemoryRecords = nil }),
			wantErr: "at least one memory record",
		},
		{
			name:    "no rag sources",
			c:       mutConv(func(c *mixedCase) { c.RagSources = nil }),
			wantErr: "at least one rag source",
		},

		// Rule 6: memory records.
		{
			name:    "memory record blank id",
			c:       mutConv(func(c *mixedCase) { c.MemoryRecords[0].ID = " " }),
			wantErr: "blank id",
		},
		{
			name: "memory record duplicate id",
			c: mutConv(func(c *mixedCase) {
				c.MemoryRecords = append(c.MemoryRecords, mixedMemoryRecord{ID: "rec-1", Content: "another note"})
			}),
			wantErr: "duplicate id",
		},
		{
			name:    "memory record blank content",
			c:       mutConv(func(c *mixedCase) { c.MemoryRecords[0].Content = "\t" }),
			wantErr: "blank content",
		},
		{
			name:    "memory record unknown kind",
			c:       mutConv(func(c *mixedCase) { c.MemoryRecords[0].Kind = "durable" }),
			wantErr: "unknown kind",
		},
		{
			name:    "working record without workspace",
			c:       mutConv(func(c *mixedCase) { c.MemoryRecords[0].WorkspaceID = "" }),
			wantErr: "working kind requires a workspace_id",
		},

		// Rule 7: rag sources reuse the 3a per-source validation.
		{
			name:    "rag source blank path",
			c:       mutConv(func(c *mixedCase) { c.RagSources[0].Path = " " }),
			wantErr: "path and content are required",
		},
		{
			name: "rag duplicate path",
			c: mutConv(func(c *mixedCase) {
				c.RagSources = append(c.RagSources, c.RagSources[0])
			}),
			wantErr: "duplicate path",
		},
		{
			name:    "rag abstract without overview",
			c:       mutConv(func(c *mixedCase) { c.RagSources[0].Abstract = "an abstract" }),
			wantErr: "abstract and overview",
		},

		// Rule 8: evidence contract.
		{
			name:    "non-control without required_evidence",
			c:       mutConv(func(c *mixedCase) { c.RequiredEvidence = nil }),
			wantErr: "at least one required_evidence",
		},
		{
			name:    "evidence unknown domain",
			c:       mutConv(func(c *mixedCase) { c.RequiredEvidence[0].Domain = "wiki" }),
			wantErr: "unknown domain",
		},
		{
			name:    "evidence blank literal",
			c:       mutConv(func(c *mixedCase) { c.RequiredEvidence[0].Literal = "  " }),
			wantErr: "blank literal",
		},
		{
			name:    "evidence domain must match single answer_home",
			c:       mutConv(func(c *mixedCase) { c.RequiredEvidence[0] = mixedEvidence{Domain: "memory", Literal: "09:30"} }),
			wantErr: "does not match answer_home",
		},
		{
			name: "join evidence must span two domains",
			c: func() mixedCase {
				c := mixedJoinCase("case-1")
				c.RequiredEvidence = []mixedEvidence{
					{Domain: "memory", Literal: "port 7443"},
					{Domain: "memory", Literal: "gateway listens"},
				}
				return c
			}(),
			wantErr: "2 distinct domains",
		},
		{
			name:    "containment: literal absent from declared domain",
			c:       mutConv(func(c *mixedCase) { c.RequiredEvidence[0].Literal = "flag-omega-9" }),
			wantErr: "not found in conversation content",
		},
		{
			name: "containment: literal only in the final question is not contained",
			c: mutConv(func(c *mixedCase) {
				c.Events[len(c.Events)-1].Turn.Content = "Is the gate flag-omega-9 or something else?"
				c.RequiredEvidence[0].Literal = "flag-omega-9"
			}),
			wantErr: "not found in conversation content",
		},
		{
			name: "contamination: literal leaks into another domain",
			c: mutConv(func(c *mixedCase) {
				c.MemoryRecords[0].Content = "reminder: flag-alpha-7 is the gate"
			}),
			wantErr: "leaks into memory content",
		},
		{
			// Also pins two scan properties at once: summaries (abstract and
			// overview) are contamination surfaces, and the contamination side
			// is case-insensitive (a re-cased leak cannot slip past).
			name: "contamination: re-cased literal leaks into rag abstract",
			c: mutConv(func(c *mixedCase) {
				c.RagSources[0].Abstract = "Deploy notes mention FLAG-ALPHA-7 for rollout."
				c.RagSources[0].Overview = "Deployment overview."
			}),
			wantErr: "leaks into rag content",
		},
		{
			name: "self-match: literal appears in the final question",
			c: mutConv(func(c *mixedCase) {
				c.Events[len(c.Events)-1].Turn.Content = "Did we settle on flag-alpha-7 for the beta rollout?"
			}),
			wantErr: "appears in the final question",
		},
		{
			name: "literal appears in the system prompt",
			c: mutConv(func(c *mixedCase) {
				c.System = "Answer using only the provided context; the gate is flag-alpha-7."
			}),
			wantErr: "appears in the system prompt",
		},
		{
			name:    "forbidden_evidence blank literal",
			c:       mutConv(func(c *mixedCase) { c.ForbiddenEvidence = []string{" "} }),
			wantErr: "forbidden_evidence[0]: blank literal",
		},
		{
			name:    "forbidden literal present in a domain",
			c:       mutConv(func(c *mixedCase) { c.ForbiddenEvidence = []string{"deploy window"} }),
			wantErr: "forbidden_evidence[0]",
		},

		// Rule 9: answer_turn_index validity.
		{
			name:    "answer_turn_index out of range",
			c:       mutConv(func(c *mixedCase) { c.AnswerTurnIndex = intPtr(9) }),
			wantErr: "out of range",
		},
		{
			name: "answer_turn_index must point at a turn",
			c: func() mixedCase {
				c := withMixedToolCall(mixedConvCase("case-1"), mixedToolCall{
					CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"x"}`),
				})
				c.AnswerTurnIndex = intPtr(2)
				return c
			}(),
			wantErr: "must point at a turn",
		},
		{
			name:    "answer_turn_index must not be the final question",
			c:       mutConv(func(c *mixedCase) { c.AnswerTurnIndex = intPtr(2) }),
			wantErr: "final question",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMixedCase(tt.c)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMixedCase = %v; want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateMixedCase = %v; want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestMixedFixtureBookkeeping(t *testing.T) {
	early := mixedConvCase("bk-early")
	early.TwinGroup = "tw-pair"

	late := mixedConvCase("bk-late")
	// Three conversation turns; the answer sits on the last one => late third.
	// The answering turn must carry the conversation anchor (W2 binding rule).
	late.Events = []mixedEvent{
		late.Events[0],
		late.Events[1],
		{Turn: &mixedTurn{Role: "assistant", Content: "Confirmed: flag-alpha-7 stays the gate."}},
		late.Events[2],
	}
	late.AnswerTurnIndex = intPtr(2)

	mem := mixedMemCase("bk-mem")
	mem.TwinGroup = "tw-pair"

	control := mixedControlCase("bk-control")

	bk, err := validateMixedFixture(mixedFixtureFor(early, late, mem, control))
	if err != nil {
		t.Fatalf("validateMixedFixture = %v; want nil", err)
	}
	if got := bk.stratumPrimary["conversation_only"]; got != 2 {
		t.Errorf("conversation_only primary = %d; want 2", got)
	}
	if got := bk.stratumControls["conversation_only"]; got != 1 {
		t.Errorf("conversation_only controls = %d; want 1", got)
	}
	if got := bk.stratumPrimary["memory_only"]; got != 1 {
		t.Errorf("memory_only primary = %d; want 1", got)
	}
	if got := bk.stratumControls["memory_only"]; got != 0 {
		t.Errorf("memory_only controls = %d; want 0", got)
	}
	if bk.controlCases != 1 {
		t.Errorf("controlCases = %d; want 1", bk.controlCases)
	}
	if bk.toplineEligible != 3 {
		t.Errorf("toplineEligible = %d; want 3", bk.toplineEligible)
	}
	if bk.answerThirds["early"] != 2 || bk.answerThirds["middle"] != 0 || bk.answerThirds["late"] != 1 {
		t.Errorf("answerThirds = %v; want early=2 middle=0 late=1", bk.answerThirds)
	}

	// The mixed build path prints the bookkeeping summary and the built-state
	// count before the arm build; these tiny cases retain everything under
	// the minViable-floored budget, so the pressure-evidence gate then stops
	// the build loudly.
	raw, err := json.Marshal(mixedFixtureFor(early, late, mem, control))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var buf bytes.Buffer
	err = runMixedFixture(context.Background(), raw, t.TempDir(), &buf)
	if err == nil || !strings.Contains(err.Error(), "pressure evidence") {
		t.Fatalf("runMixedFixture = %v; want the pressure-evidence gate error", err)
	}
	out := buf.String()
	for _, want := range []string{
		"mixed-assembly fixture: 4 case(s)",
		"stratum conversation_only: primary=2 control=1",
		"stratum memory_only: primary=1 control=0",
		"early=2 middle=0 late=1",
		"built 4 frozen state(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q; got:\n%s", want, out)
		}
	}
}

// TestReplayPrefilledFinalTurnBackstopTrimsWhitespace pins the Task 2 review
// fold-in: the replay backstop must treat a whitespace-only final question the
// same way the validator does (TrimSpace), not only the empty string.
func TestReplayPrefilledFinalTurnBackstopTrimsWhitespace(t *testing.T) {
	trace := prefilledTestTrace(AssemblyMixed)
	trace.Turns[len(trace.Turns)-1].Content = " \n\t"
	client := ollamaCandidateClient{client: ollama.NewClient(ollama.WithBaseURL("http://127.0.0.1:0"))}
	_, err := replayPrefilled(context.Background(), client, "m", trace, replayOptions{})
	if err == nil || !strings.Contains(err.Error(), "prefilled final turn must be a non-empty user question") {
		t.Fatalf("err = %v; want the final-turn backstop error", err)
	}
}

// --- Wave 2 (round-2 consult): fixture-side hardening tests. ---

func TestMixedPressureTargetValidation(t *testing.T) {
	tests := []struct {
		name    string
		c       mixedCase
		wantErr string
	}{
		{
			name: "control must not carry pressure_target",
			c: func() mixedCase {
				c := mixedControlCase("pt-ctl")
				c.PressureTarget = &mixedEvidence{Domain: "conversation", Literal: "beta rollout"}
				return c
			}(),
			wantErr: "pressure_target",
		},
		{
			name: "non-control requires pressure_target",
			c: func() mixedCase {
				c := mixedConvCase("pt-req")
				c.PressureTarget = nil
				return c
			}(),
			wantErr: "pressure_target is required",
		},
		{
			name: "unknown domain",
			c: func() mixedCase {
				c := mixedConvCase("pt-dom")
				c.PressureTarget = &mixedEvidence{Domain: "wiki", Literal: "beta rollout"}
				return c
			}(),
			wantErr: "pressure_target: unknown domain",
		},
		{
			name: "blank literal",
			c: func() mixedCase {
				c := mixedConvCase("pt-blank")
				c.PressureTarget = &mixedEvidence{Domain: "conversation", Literal: "  "}
				return c
			}(),
			wantErr: "pressure_target: blank literal",
		},
		{
			name: "literal absent from its domain content",
			c: func() mixedCase {
				c := mixedConvCase("pt-miss")
				c.PressureTarget = &mixedEvidence{Domain: "conversation", Literal: "flag-omega-9"}
				return c
			}(),
			wantErr: "pressure_target",
		},
		{
			// stale_vs_fresh may target the stale carrier: the domain scan covers
			// rag abstract/overview, so a summary-only literal validates.
			name: "stale case may target the stale rag representation",
			c: func() mixedCase {
				c := mixedStaleCase("pt-stale")
				c.RagSources[0].Abstract = "Stale deploy digest 0421 summary."
				c.RagSources[0].Overview = "Deployment overview."
				c.PressureTarget = &mixedEvidence{Domain: "rag", Literal: "digest 0421"}
				return c
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMixedCase(tt.c)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateMixedCase = %v; want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateMixedCase = %v; want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestMixedEvidenceToolArgScan(t *testing.T) {
	// A required anchor hidden — re-cased — in a retrieve query arg is
	// model-visible via the assistant tool-call turn: rejected.
	c := withMixedToolCall(mixedConvCase("arg-req"), mixedToolCall{
		CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"FLAG-ALPHA-7 gate"}`),
	})
	err := validateMixedCase(c)
	if err == nil || !strings.Contains(err.Error(), "tool_call args") || !strings.Contains(err.Error(), "required_evidence") {
		t.Errorf("anchor in retrieve args: err = %v; want a required_evidence tool_call-args rejection", err)
	}

	// Exact-case anchor in agent_memory_search args: rejected too.
	e := withMixedToolCall(mixedConvCase("arg-exact"), mixedToolCall{
		CallID: "c1", Tool: "agent_memory_search", Args: json.RawMessage(`{"query":"flag-alpha-7"}`),
	})
	err = validateMixedCase(e)
	if err == nil || !strings.Contains(err.Error(), "tool_call args") {
		t.Errorf("exact anchor in memory args: err = %v; want a tool_call-args rejection", err)
	}

	// A forbidden literal in fixture_echo args: rejected.
	f := mixedConvCase("arg-forb")
	f.ForbiddenEvidence = []string{"stale-flag-9"}
	f = withMixedToolCall(f, mixedToolCall{
		CallID: "c1", Tool: "fixture_echo", Args: json.RawMessage(`{"content":"note mentions stale-flag-9 here"}`),
	})
	err = validateMixedCase(f)
	if err == nil || !strings.Contains(err.Error(), "tool_call args") || !strings.Contains(err.Error(), "forbidden_evidence") {
		t.Errorf("forbidden literal in echo args: err = %v; want a forbidden_evidence tool_call-args rejection", err)
	}

	// Anchor-free args stay legal.
	ok := withMixedToolCall(mixedConvCase("arg-ok"), mixedToolCall{
		CallID: "c1", Tool: "retrieve", Args: json.RawMessage(`{"query":"beta gate"}`),
	})
	if err := validateMixedCase(ok); err != nil {
		t.Errorf("clean args: err = %v; want nil", err)
	}
}

func TestMixedAnswerTurnIndexBinding(t *testing.T) {
	// Required on conversation_only.
	c := mixedConvCase("ati-req")
	c.AnswerTurnIndex = nil
	if err := validateMixedCase(c); err == nil || !strings.Contains(err.Error(), "answer_turn_index is required") {
		t.Errorf("missing index on conversation_only: err = %v; want the required error", err)
	}

	// Control conversation_only cases carry the index too (no anchors to bind).
	ctl := mixedControlCase("ati-ctl")
	ctl.AnswerTurnIndex = nil
	if err := validateMixedCase(ctl); err == nil || !strings.Contains(err.Error(), "answer_turn_index is required") {
		t.Errorf("missing index on control conversation_only: err = %v; want the required error", err)
	}

	// The indexed turn must contain every conversation-domain anchor.
	b := mixedConvCase("ati-bind")
	b.AnswerTurnIndex = intPtr(1) // assistant ack turn; no flag-alpha-7
	if err := validateMixedCase(b); err == nil ||
		!strings.Contains(err.Error(), "answer_turn_index does not contain the conversation anchors") {
		t.Errorf("unbound index: err = %v; want the conversation-anchors binding error", err)
	}

	// Other strata: still optional.
	m := mixedMemCase("ati-opt")
	m.AnswerTurnIndex = nil
	if err := validateMixedCase(m); err != nil {
		t.Errorf("memory_only without index: err = %v; want nil (optional outside conversation_only)", err)
	}
}

func TestMixedTwinContract(t *testing.T) {
	pair := func(tg, convID, memID string) (mixedCase, mixedCase) {
		a := mixedConvCase(convID)
		a.TwinGroup = tg
		b := mixedMemCase(memID)
		b.TwinGroup = tg
		return a, b
	}

	// Valid: two twin pairs across conversation_only and memory_only, sharing
	// the same rag paths and memory record ids.
	a1, b1 := pair("tw-1", "twc-1", "twm-1")
	a2, b2 := pair("tw-2", "twc-2", "twm-2")
	if _, err := validateMixedFixture(mixedFixtureFor(a1, b1, a2, b2)); err != nil {
		t.Fatalf("valid twin pairs: err = %v; want nil", err)
	}

	t.Run("fifth twin group rejected", func(t *testing.T) {
		var cases []mixedCase
		for i := 1; i <= 5; i++ {
			a, b := pair(fmt.Sprintf("tw-%d", i), fmt.Sprintf("twc-%d", i), fmt.Sprintf("twm-%d", i))
			cases = append(cases, a, b)
		}
		_, err := validateMixedFixture(mixedFixtureFor(cases...))
		if err == nil || !strings.Contains(err.Error(), "at most 4") {
			t.Errorf("5 twin groups: err = %v; want the at-most-4 rejection", err)
		}
	})

	t.Run("single-member twin group rejected", func(t *testing.T) {
		solo := mixedConvCase("tw-solo")
		solo.TwinGroup = "tw-s"
		_, err := validateMixedFixture(mixedFixtureFor(solo))
		if err == nil || !strings.Contains(err.Error(), "2-3") {
			t.Errorf("single-member twin: err = %v; want the 2-3 members rejection", err)
		}
	})

	t.Run("same-stratum members rejected", func(t *testing.T) {
		x := mixedConvCase("tw-x1")
		x.TwinGroup = "tw-ss"
		y := mixedConvCase("tw-x2")
		y.TwinGroup = "tw-ss"
		_, err := validateMixedFixture(mixedFixtureFor(x, y))
		if err == nil || !strings.Contains(err.Error(), "stratum") {
			t.Errorf("same-stratum twins: err = %v; want a different-strata rejection", err)
		}
	})

	t.Run("stale_vs_fresh member rejected", func(t *testing.T) {
		s := mixedStaleCase("tw-st")
		s.TwinGroup = "tw-sf"
		c := mixedConvCase("tw-cv")
		c.TwinGroup = "tw-sf"
		_, err := validateMixedFixture(mixedFixtureFor(s, c))
		if err == nil || !strings.Contains(err.Error(), "stale_vs_fresh") {
			t.Errorf("stale twin member: err = %v; want a rejection naming the illegal stratum", err)
		}
	})

	t.Run("differing rag source paths rejected", func(t *testing.T) {
		a := mixedConvCase("tw-p1")
		a.TwinGroup = "tw-dp"
		j := mixedJoinCase("tw-p2") // pkg/gw/gw.go, not pkg/cfg/cfg.go
		j.TwinGroup = "tw-dp"
		_, err := validateMixedFixture(mixedFixtureFor(a, j))
		if err == nil || !strings.Contains(err.Error(), "rag_sources") {
			t.Errorf("differing paths: err = %v; want a rag_sources mismatch rejection", err)
		}
	})

	t.Run("differing memory record ids rejected", func(t *testing.T) {
		a, b := pair("tw-mi", "tw-i1", "tw-i2")
		b.MemoryRecords[0].ID = "rec-9"
		_, err := validateMixedFixture(mixedFixtureFor(a, b))
		if err == nil || !strings.Contains(err.Error(), "memory_records") {
			t.Errorf("differing record ids: err = %v; want a memory_records mismatch rejection", err)
		}
	})
}

func TestMixedControlStratumCap(t *testing.T) {
	ctl := func(id string) mixedCase { return mixedControlCase(id) }
	// Two controls in one stratum: legal.
	if _, err := validateMixedFixture(mixedFixtureFor(mixedConvCase("cc-p"), ctl("cc-1"), ctl("cc-2"))); err != nil {
		t.Fatalf("two controls: err = %v; want nil", err)
	}
	// A third control in the same stratum: rejected.
	_, err := validateMixedFixture(mixedFixtureFor(mixedConvCase("cc-p"), ctl("cc-1"), ctl("cc-2"), ctl("cc-3")))
	if err == nil || !strings.Contains(err.Error(), "control") || !strings.Contains(err.Error(), "at most 2") {
		t.Errorf("three controls in one stratum: err = %v; want the at-most-2 rejection", err)
	}
}

func TestMixedToplineRegisteredSelection(t *testing.T) {
	// The registered quotas, pinned by literal; they must sum to 12.
	if mixedToplineConversationOnly != 3 || mixedToplineMemoryOnly != 2 ||
		mixedToplineCrossDomainJoin != 3 || mixedToplineStaleVsFresh != 2 ||
		mixedToplineChainRetention != 2 {
		t.Fatalf("registered topline quotas changed; re-registration required")
	}
	if total := mixedToplineConversationOnly + mixedToplineMemoryOnly + mixedToplineCrossDomainJoin +
		mixedToplineStaleVsFresh + mixedToplineChainRetention; total != 12 {
		t.Fatalf("topline quota total = %d; want 12", total)
	}

	conv := func(id, fam, facts string) mixedCase {
		c := mixedConvCase(id)
		c.ScenarioFamily = fam
		c.ToplineFacts = facts
		return c
	}
	mem := func(id, fam, facts string) mixedCase {
		c := mixedMemCase(id)
		c.ScenarioFamily = fam
		c.ToplineFacts = facts
		return c
	}
	const convFacts = "For the beta rollout the team chose flag-alpha-7 as the gate."
	const memFacts = "The trainer checkpoint recorded batch-size 512."

	// FNV-1a-64 ascending (hand-computed):
	//   conversation families: fam-d < fam-b < fam-c < fam-a, quota 3 selects
	//   fam-d, fam-b, fam-c; within fam-d the lexicographically smallest id
	//   (top-d1) carries.
	//   memory families: fam-m1 < fam-m3 < fam-m2, quota 2 selects fam-m1 and
	//   fam-m3.
	corpus := func() []mixedCase {
		return []mixedCase{
			conv("top-d2", "fam-d", ""),
			conv("top-d1", "fam-d", convFacts),
			conv("top-b1", "fam-b", convFacts),
			conv("top-c1", "fam-c", convFacts),
			conv("top-a1", "fam-a", ""),
			mem("top-m1", "fam-m1", memFacts),
			mem("top-m2", "fam-m2", ""),
			mem("top-m3", "fam-m3", memFacts),
		}
	}
	if _, err := validateMixedFixture(mixedFixtureFor(corpus()...)); err != nil {
		t.Fatalf("registered selection corpus: err = %v; want nil", err)
	}

	// Missing on a selected case: error naming the case.
	miss := corpus()
	miss[2].ToplineFacts = "" // top-b1, selected
	_, err := validateMixedFixture(mixedFixtureFor(miss...))
	if err == nil || !strings.Contains(err.Error(), "top-b1") || !strings.Contains(err.Error(), "topline_facts missing") {
		t.Errorf("missing on selected: err = %v; want a topline_facts-missing error naming top-b1", err)
	}

	// Present on an unselected case: error naming the case.
	extra := corpus()
	extra[4].ToplineFacts = convFacts // top-a1, fam-a not selected
	_, err = validateMixedFixture(mixedFixtureFor(extra...))
	if err == nil || !strings.Contains(err.Error(), "top-a1") || !strings.Contains(err.Error(), "outside the registered topline selection") {
		t.Errorf("present on unselected: err = %v; want an outside-selection error naming top-a1", err)
	}

	// Within a family the smallest id carries: facts on top-d2 instead of
	// top-d1 must fail in BOTH directions (missing on d1 reported first, in
	// declaration order).
	swap := corpus()
	swap[0].ToplineFacts = convFacts // top-d2
	swap[1].ToplineFacts = ""        // top-d1
	_, err = validateMixedFixture(mixedFixtureFor(swap...))
	if err == nil || !strings.Contains(err.Error(), "top-d") {
		t.Errorf("family carrier swap: err = %v; want a selection error naming a fam-d case", err)
	}
}

// TestResolveCandidateDigestsEmptyDigestSkipped covers the successful-ShowModel
// branch that returns no digest: the target degrades to a missing digest (with
// a stderr note) exactly like the error branch.
func TestResolveCandidateDigestsEmptyDigestSkipped(t *testing.T) {
	targets := []ModelTarget{{Display: "m-empty", Provider: "ollama", Model: "m-empty"}}
	got := resolveCandidateDigests(context.Background(), &fakeShowModeler{digests: map[string]string{"m-empty": ""}}, targets)
	if d, ok := got["m-empty"]; ok {
		t.Fatalf("digest for m-empty = %q; want absent (empty digest degrades to missing)", d)
	}
}
