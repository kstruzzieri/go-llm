package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/provider"
)

// All expected values below are literals: the sanitizer never computes its own
// expectation. "\\x1b" in Go source is the four visible characters \x1b.
func TestSanitizePreviewTable(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"ESC CSI", "\x1b[2J", "\\x1b[2J"},
		{"OSC 52", "\x1b]52;c;cHduZWQ=\a", "\\x1b]52;c;cHduZWQ=\\a"},
		{"raw 0x9b", "pre\x9bpost", "pre\uFFFDpost"},
		{"UTF-8 U+009B", "pre\u009bpost", "pre\\u009bpost"},
		{"CR", "a\rb", "a\\rb"},
		{"CRLF keeps LF", "a\r\nb", "a\\r\nb"},
		{"BS", "a\bb", "a\\bb"},
		{"DEL", "a\x7fb", "a\\x7fb"},
		{"tab", "a\tb", "a\\tb"},
		{"bidi override", "a\u202eb", "a\\u202eb"},
		{"line separator", "a\u2028b", "a\\u2028b"},
		{"LF", "a\nb", "a\nb"},
		{"diff prefixes", "--- a/x\n+++ b/x\n-old\n+new\n", "--- a/x\n+++ b/x\n-old\n+new\n"},
		{"ordinary unicode", "héllo 世界", "héllo 世界"},
		{"empty", "", ""},
		{"idempotent: already inert", "\\x1b[2J \\u009b \\r", "\\x1b[2J \\u009b \\r"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizePreview(tc.in)
			if got != tc.want {
				t.Fatalf("SanitizePreview(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if again := SanitizePreview(got); again != got {
				t.Fatalf("not idempotent: second pass %q != first %q", again, got)
			}
		})
	}
}

func TestSanitizePreviewEveryC1(t *testing.T) {
	var runes, raw []byte
	for r := rune(0x80); r <= 0x9f; r++ {
		runes = utf8.AppendRune(runes, r)
		runes = append(runes, ' ')
		raw = append(raw, byte(r), ' ')
	}
	for name, in := range map[string]string{"UTF-8 runes": string(runes), "raw bytes": string(raw)} {
		t.Run(name, func(t *testing.T) {
			out := SanitizePreview(in)
			if !utf8.ValidString(out) {
				t.Fatalf("output is not valid UTF-8, so a raw C1 byte survived: %q", out)
			}
			for _, r := range out {
				if r >= 0x80 && r <= 0x9f {
					t.Fatalf("output holds C1 rune U+%04X: %q", r, out)
				}
			}
		})
	}
	// Spot-check literal renderings at both ends of the range.
	if got := SanitizePreview("\u0080|\u009f"); got != "\\u0080|\\u009f" {
		t.Fatalf("C1 edge runes = %q", got)
	}
	if got := SanitizePreview("\x80|\x9f"); got != "\uFFFD|\uFFFD" {
		t.Fatalf("C1 edge raw bytes = %q", got)
	}
}

// Fixtures whose bytes the sanitizer WOULD change if it touched them. A key or
// an argument string without such a rune cannot show that approve() leaves it
// alone, so each carries ESC, a tab and a raw U+009B (the arguments also a raw
// U+202E and, as JSON escape text, \u001b).
const (
	// previewKey is the plan's ApprovalKey: ESC, CSI text, tab, raw U+009B.
	previewKey = "key-\x1b[2J\t\u009b"
	// previewCallArgs is the model's tool-call arguments, valid JSON: a tab as
	// whitespace, a raw U+009B and U+202E inside the string, and the JSON
	// escape text \u001b (six ASCII bytes, not a control).
	previewCallArgs = "{\"path\":\t\"x\u009b\u202e\",\"seq\":\"\\u001b[2J\"}"
)

// previewTool is a write PlanningTool whose preview the test controls and
// which records exactly what dispatch handed to Plan and Invoke.
type previewTool struct {
	preview     string
	planArgs    string
	invokeArgs  string
	invokeCalls int
}

func (*previewTool) Spec() ToolSpec {
	return ToolSpec{Name: "writer", Parameters: json.RawMessage(`{}`)}
}
func (*previewTool) Effect() Effect { return Effect{Class: Write, Approval: ApprovalOnWrite} }
func (p *previewTool) Plan(_ context.Context, args json.RawMessage) (ToolPlan, error) {
	p.planArgs = string(args)
	return ToolPlan{Effect: p.Effect(), Preview: p.preview, ApprovalKey: previewKey}, nil
}
func (p *previewTool) Invoke(_ context.Context, args json.RawMessage) (ToolResult, error) {
	p.invokeCalls++
	p.invokeArgs = string(args)
	return ToolResult{Content: "wrote"}, nil
}

// seen is what an approver received: which method fired and the preview, key
// and call.Function.Arguments bytes it was handed.
type seen struct {
	via, preview, key, args string
}

func (s *seen) record(via string, call provider.ToolCall, preview, key string) {
	s.via, s.preview, s.key, s.args = via, preview, key, string(call.Function.Arguments)
}

// plainSeen is a plain Approver: no key, no risk.
type plainSeen struct{ seen }

func (a *plainSeen) Approve(_ context.Context, call provider.ToolCall, preview string) (bool, error) {
	a.record("Approve", call, preview, "")
	return true, nil
}

// keyedSeen is a KeyedApprover.
type keyedSeen struct{ seen }

func (a *keyedSeen) Approve(_ context.Context, call provider.ToolCall, preview string) (bool, error) {
	a.record("Approve", call, preview, "")
	return true, nil
}

func (a *keyedSeen) ApproveKeyed(_ context.Context, call provider.ToolCall, preview, key string) (ApprovalDecision, error) {
	a.record("ApproveKeyed", call, preview, key)
	return ApprovalDecision{Approved: true}, nil
}

// riskSeen is a RiskApprover; dispatch prefers it over the keyed and plain tiers.
type riskSeen struct{ keyedSeen }

func (a *riskSeen) ApproveWithRisk(_ context.Context, call provider.ToolCall, preview, key string, _ RiskReport) (ApprovalDecision, error) {
	a.record("ApproveWithRisk", call, preview, key)
	return ApprovalDecision{Approved: true}, nil
}

func TestApproveSanitizesForEmbeddedApprovers(t *testing.T) {
	const (
		dirty     = "+x\x1b[31mred\x1b[0m\n\x1b]52;c;cHduZWQ=\a\u009b\r\b\x7f"
		wantClean = "+x\\x1b[31mred\\x1b[0m\n\\x1b]52;c;cHduZWQ=\\a\\u009b\\r\\b\\x7f"
		control   = "+plain\n"
	)
	// The fixtures must be blind-proof: a key or arguments the sanitizer would
	// leave alone could not tell "untouched" from "sanitized".
	if !strings.ContainsAny(previewKey, "\x1b\t\u009b") {
		t.Fatalf("previewKey %q carries no control the sanitizer changes", previewKey)
	}
	if !strings.ContainsAny(previewCallArgs, "\t\u009b\u202e") || !json.Valid([]byte(previewCallArgs)) {
		t.Fatalf("previewCallArgs %q must be valid JSON that carries controls the sanitizer changes", previewCallArgs)
	}
	tiers := []struct {
		name  string
		via   string // the method dispatch must reach for this tier
		keyed bool   // false: the tier cannot observe the approval key
		new   func() (Approver, func() seen)
	}{
		{"plain Approver", "Approve", false, func() (Approver, func() seen) {
			a := &plainSeen{}
			return a, func() seen { return a.seen }
		}},
		{"KeyedApprover", "ApproveKeyed", true, func() (Approver, func() seen) {
			a := &keyedSeen{}
			return a, func() seen { return a.seen }
		}},
		{"RiskApprover", "ApproveWithRisk", true, func() (Approver, func() seen) {
			a := &riskSeen{}
			return a, func() seen { return a.seen }
		}},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			run := func(preview string) (*previewTool, seen) {
				tool := &previewTool{preview: preview}
				ap, got := tier.new()
				runPreviewTool(t, tool, ap)
				return tool, got()
			}
			ctl, ctlSeen := run(control)
			atk, atkSeen := run(dirty)

			for name, got := range map[string]seen{"attack": atkSeen, "control": ctlSeen} {
				if got.via != tier.via {
					t.Fatalf("%s run reached %s, want %s", name, got.via, tier.via)
				}
				// The call's arguments are not display data: byte-identical
				// to the model's literal, controls and all.
				if got.args != previewCallArgs {
					t.Fatalf("%s run: approver call arguments = %q, want %q", name, got.args, previewCallArgs)
				}
				if tier.keyed && got.key != previewKey {
					t.Fatalf("%s run: approver key = %q, want %q", name, got.key, previewKey)
				}
			}
			// containment: PREVIEW-PROFILE
			if atkSeen.preview != wantClean {
				t.Fatalf("approver preview = %q, want %q", atkSeen.preview, wantClean)
			}
			for i := 0; i < len(atkSeen.preview); i++ {
				if atkSeen.preview[i] == 0x1b || atkSeen.preview[i] == 0x9b {
					t.Fatalf("approver preview carries raw control byte 0x%02x: %q", atkSeen.preview[i], atkSeen.preview)
				}
			}
			// The control run proves the sanitizer is the only difference: an
			// already-inert preview passes through byte-identical.
			if ctlSeen.preview != control {
				t.Fatalf("control preview = %q, want %q", ctlSeen.preview, control)
			}
			// What Plan and Invoke received is untouched too.
			for name, tool := range map[string]*previewTool{"attack": atk, "control": ctl} {
				if tool.planArgs != previewCallArgs || tool.invokeArgs != previewCallArgs || tool.invokeCalls != 1 {
					t.Fatalf("%s run: plan args %q invoke args %q calls %d, want %q once", name, tool.planArgs, tool.invokeArgs, tool.invokeCalls, previewCallArgs)
				}
			}
		})
	}
}

func runPreviewTool(t *testing.T, tool *previewTool, ap Approver) {
	t.Helper()
	mc := &scriptedCaller{responses: stepsOf([]provider.ToolCall{writerCallWith("1", previewCallArgs)})}
	o := newTestOrchestrator(mc)
	if _, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{tool}, Approver: ap}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
