package agent

import (
	"context"
	"encoding/json"
	"testing"
	"unicode/utf8"
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
	return ToolPlan{Effect: p.Effect(), Preview: p.preview, ApprovalKey: "key-1"}, nil
}
func (p *previewTool) Invoke(_ context.Context, args json.RawMessage) (ToolResult, error) {
	p.invokeCalls++
	p.invokeArgs = string(args)
	return ToolResult{Content: "wrote"}, nil
}

func TestApproveSanitizesForEmbeddedApprovers(t *testing.T) {
	const (
		dirty     = "+x\x1b[31mred\x1b[0m\n\x1b]52;c;cHduZWQ=\a\u009b\r\b\x7f"
		wantClean = "+x\\x1b[31mred\\x1b[0m\n\\x1b]52;c;cHduZWQ=\\a\\u009b\\r\\b\\x7f"
		control   = "+plain\n"
		wantArgs  = `{"path":"x"}`
		wantKey   = "key-1"
	)
	// run drives a real orchestrator over the tool and returns the preview and
	// key the approver actually received.
	tiers := []struct {
		name  string
		keyed bool // false: the tier cannot observe the approval key
		run   func(t *testing.T, tool *previewTool) (preview, key string)
	}{
		{"plain Approver", false, func(t *testing.T, tool *previewTool) (string, string) {
			ap := &capturingApprover{allow: true}
			runPreviewTool(t, tool, ap)
			return ap.gotPreview, ""
		}},
		{"KeyedApprover", true, func(t *testing.T, tool *previewTool) (string, string) {
			ap := &keyedCapturingApprover{decision: ApprovalDecision{Approved: true}}
			runPreviewTool(t, tool, ap)
			if ap.keyedCalls != 1 || ap.plainCalls != 0 {
				t.Fatalf("keyed=%d plain=%d, want 1/0", ap.keyedCalls, ap.plainCalls)
			}
			return ap.gotPreview, ap.gotKey
		}},
		{"RiskApprover", true, func(t *testing.T, tool *previewTool) (string, string) {
			ap := &riskCapturingApprover{keyedCapturingApprover: keyedCapturingApprover{decision: ApprovalDecision{Approved: true}}}
			runPreviewTool(t, tool, ap)
			if ap.riskCalls != 1 {
				t.Fatalf("riskCalls = %d, want 1", ap.riskCalls)
			}
			return ap.gotPreview, ap.gotKey
		}},
	}
	for _, tier := range tiers {
		t.Run(tier.name, func(t *testing.T) {
			ctl := &previewTool{preview: control}
			ctlPreview, ctlKey := tier.run(t, ctl)
			atk := &previewTool{preview: dirty}
			gotPreview, gotKey := tier.run(t, atk)

			if gotPreview != wantClean {
				t.Fatalf("approver preview = %q, want %q", gotPreview, wantClean)
			}
			for i := 0; i < len(gotPreview); i++ {
				if gotPreview[i] == 0x1b || gotPreview[i] == 0x9b {
					t.Fatalf("approver preview carries raw control byte 0x%02x: %q", gotPreview[i], gotPreview)
				}
			}
			// The control run proves the sanitizer is the only difference: an
			// already-inert preview passes through byte-identical.
			if ctlPreview != control {
				t.Fatalf("control preview = %q, want %q", ctlPreview, control)
			}
			// The key and the planned/invoked args are not display data.
			if tier.keyed && (gotKey != wantKey || ctlKey != wantKey) {
				t.Fatalf("approval key = %q (control %q), want %q", gotKey, ctlKey, wantKey)
			}
			for name, tool := range map[string]*previewTool{"attack": atk, "control": ctl} {
				if tool.planArgs != wantArgs || tool.invokeArgs != wantArgs || tool.invokeCalls != 1 {
					t.Fatalf("%s run: plan args %q invoke args %q calls %d, want %q once", name, tool.planArgs, tool.invokeArgs, tool.invokeCalls, wantArgs)
				}
			}
		})
	}
}

func runPreviewTool(t *testing.T, tool *previewTool, ap Approver) {
	t.Helper()
	mc := &scriptedCaller{responses: []ModelResult{writerCall("1"), finalAnswer("done")}}
	o := newTestOrchestrator(mc)
	if _, err := o.Run(context.Background(), Request{Goal: "q", Tools: []Tool{tool}, Approver: ap}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
