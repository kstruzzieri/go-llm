package main

import (
	"bytes"
	"context"
	"encoding/json"
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
		Golden:           Golden{FinalAnswerCriteria: "States the beta gate is flag-alpha-7."},
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
	early.AnswerTurnIndex = intPtr(0)
	early.TwinGroup = "tw-pair"

	late := mixedConvCase("bk-late")
	// Three conversation turns; the answer sits on the last one => late third.
	late.Events = []mixedEvent{
		late.Events[0],
		late.Events[1],
		{Turn: &mixedTurn{Role: "assistant", Content: "Confirmed once more."}},
		late.Events[2],
	}
	late.AnswerTurnIndex = intPtr(2)

	mem := mixedMemCase("bk-mem")
	mem.TwinGroup = "tw-pair"

	control := mixedControlCase("bk-control")
	control.TwinGroup = "tw-solo"

	bk, err := validateMixedFixture(mixedFixtureFor(early, late, mem, control))
	if err != nil {
		t.Fatalf("validateMixedFixture = %v; want nil", err)
	}
	if got := bk.stratumCounts["conversation_only"]; got != 3 {
		t.Errorf("conversation_only count = %d; want 3", got)
	}
	if got := bk.stratumCounts["memory_only"]; got != 1 {
		t.Errorf("memory_only count = %d; want 1", got)
	}
	if bk.controlCases != 1 {
		t.Errorf("controlCases = %d; want 1", bk.controlCases)
	}
	if bk.toplineEligible != 3 {
		t.Errorf("toplineEligible = %d; want 3", bk.toplineEligible)
	}
	if bk.answerThirds["early"] != 1 || bk.answerThirds["middle"] != 0 || bk.answerThirds["late"] != 1 {
		t.Errorf("answerThirds = %v; want early=1 middle=0 late=1", bk.answerThirds)
	}
	if len(bk.twinWarnings) != 1 || !strings.Contains(bk.twinWarnings[0], "tw-solo") {
		t.Errorf("twinWarnings = %v; want exactly one naming tw-solo", bk.twinWarnings)
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
		"stratum conversation_only: 3",
		"stratum memory_only: 1",
		"early=1 middle=0 late=1",
		"tw-solo",
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
