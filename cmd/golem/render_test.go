package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestRenderer_StreamToolStepFinal(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock }, false)

	ctx := context.Background()
	_ = r.OnToken(ctx, agent.TokenEvent{Step: 0, Content: "hello "})
	_ = r.OnToken(ctx, agent.TokenEvent{Step: 0, Content: "world"})
	_ = r.OnToolCall(ctx, agent.ToolCallEvent{
		Step: 0,
		Call: provider.ToolCall{Function: provider.ToolCallFunction{
			Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`),
		}},
	})
	clock = clock.Add(1500 * time.Millisecond)
	_ = r.OnStep(ctx, agent.StepEvent{
		Index:        0,
		RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "ollama", Model: "gemma4:31b"}},
		Pressure:     agent.Pressure{UsedPct: 0.45},
	})

	got := buf.String()
	want := "hello world" +
		"\n> read_file {\"path\":\"a.txt\"}\n" +
		"ollama/gemma4:31b · 1.5s · ctx 45% · step 1/16\n"
	if got != want {
		t.Errorf("output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderer_FinalFooter_Stopped(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock }, false)
	clock = clock.Add(3 * time.Second)
	r.finalFooter(agent.Result{
		Steps:      make([]agent.StepRecord, 2),
		Usage:      provider.Usage{TotalTokens: 128},
		StopReason: agent.StepCapReached,
	})
	want := "done · 2 steps · 3.0s · 128 tok  stopped: step_cap_reached\n"
	if buf.String() != want {
		t.Errorf("final footer = %q, want %q", buf.String(), want)
	}
}

func TestRenderer_OnStepStartsNewLineAfterUnterminatedToken(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock }, false)

	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "answer"}); err != nil {
		t.Fatalf("OnToken: %v", err)
	}
	if err := r.OnStep(context.Background(), agent.StepEvent{Index: 0, Pressure: agent.Pressure{}}); err != nil {
		t.Fatalf("OnStep: %v", err)
	}

	want := "answer\n? · 0.0s · ctx 0% · step 1/16\n"
	if buf.String() != want {
		t.Errorf("step footer after unterminated token = %q, want %q", buf.String(), want)
	}
}

func TestRenderer_FinalFooter_Completed_NoStoppedSuffix(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock }, false)
	clock = clock.Add(2 * time.Second)
	r.finalFooter(agent.Result{
		Steps:      make([]agent.StepRecord, 1),
		Usage:      provider.Usage{TotalTokens: 42},
		StopReason: agent.Completed,
	})
	want := "done · 1 step · 2.0s · 42 tok\n"
	if buf.String() != want {
		t.Errorf("completed footer = %q, want %q (no stopped suffix)", buf.String(), want)
	}
}

func TestRenderer_OnStep_NilRouteOutcome_RendersQuestionMark(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, false, 16, func() time.Time { return clock }, false)
	if err := r.OnStep(context.Background(), agent.StepEvent{Index: 0, Pressure: agent.Pressure{}}); err != nil {
		t.Fatalf("OnStep: %v", err)
	}
	want := "? · 0.0s · ctx 0% · step 1/16\n"
	if buf.String() != want {
		t.Errorf("nil-RouteOutcome footer = %q, want %q", buf.String(), want)
	}
}

func TestRenderer_OnToolCall_NoArgs_OmitsTrailingSpace(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, false, 16, func() time.Time { return time.Unix(0, 0) }, false)
	if err := r.OnToolCall(context.Background(), agent.ToolCallEvent{
		Call: provider.ToolCall{Function: provider.ToolCallFunction{Name: "list"}},
	}); err != nil {
		t.Fatalf("OnToolCall: %v", err)
	}
	if buf.String() != "\n> list\n" {
		t.Errorf("no-args tool line = %q, want %q", buf.String(), "\n> list\n")
	}
}

func TestRenderer_Color_WrapsFooter(t *testing.T) {
	var buf bytes.Buffer
	clock := time.Unix(0, 0)
	r := newRenderer(&buf, true, 16, func() time.Time { return clock }, false)
	_ = r.OnStep(context.Background(), agent.StepEvent{Index: 0, Pressure: agent.Pressure{}})
	got := buf.String()
	if got[:4] != "\x1b[2m" || got[len(got)-5:] != "\x1b[0m\n" {
		t.Errorf("color footer not dim-wrapped: %q", got)
	}
}

func TestRendererMarkdownStreamsAndResetsBeforeStepFooter(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, true, 16, func() time.Time { return time.Unix(0, 0) }, false)
	ctx := context.Background()
	for _, chunk := range []string{"#", " Heading"} {
		if err := r.OnToken(ctx, agent.TokenEvent{Content: chunk}); err != nil {
			t.Fatalf("OnToken(%q): %v", chunk, err)
		}
	}
	if err := r.OnStep(ctx, agent.StepEvent{Index: 0, Pressure: agent.Pressure{}}); err != nil {
		t.Fatalf("OnStep: %v", err)
	}

	want := "\x1b[1m# Heading\x1b[0m\n\x1b[2m? · 0.0s · ctx 0% · step 1/16\x1b[0m\n"
	if got := buf.String(); got != want {
		t.Fatalf("renderer output mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRendererMarkdownFenceContinuesAcrossStepChrome(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, true, 16, func() time.Time { return time.Unix(0, 0) }, false)
	ctx := context.Background()
	if err := r.OnToken(ctx, agent.TokenEvent{Content: "```go\npart"}); err != nil {
		t.Fatalf("OnToken opener: %v", err)
	}
	if err := r.OnStep(ctx, agent.StepEvent{Index: 0}); err != nil {
		t.Fatalf("OnStep: %v", err)
	}
	if err := r.OnToken(ctx, agent.TokenEvent{Content: "rest\n```\n"}); err != nil {
		t.Fatalf("OnToken closer: %v", err)
	}
	if err := r.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	want := markdownAccent + "rest" + markdownReset + "\n" + markdownFence + "```" + markdownReset + "\n"
	if got := buf.String(); !strings.Contains(got, want) {
		t.Fatalf("step chrome discarded the open fence:\n got: %q\nwant substring: %q", got, want)
	}
}

func TestRendererMarkdownFenceClosesAtStepBoundary(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, true, 16, func() time.Time { return time.Unix(0, 0) }, false)
	ctx := context.Background()
	if err := r.OnToken(ctx, agent.TokenEvent{Content: "```go\ncode\n```"}); err != nil {
		t.Fatalf("OnToken fence: %v", err)
	}
	if err := r.OnStep(ctx, agent.StepEvent{Index: 0}); err != nil {
		t.Fatalf("OnStep: %v", err)
	}
	if err := r.OnToken(ctx, agent.TokenEvent{Content: "plain\n"}); err != nil {
		t.Fatalf("OnToken prose: %v", err)
	}
	if err := r.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	want := markdownFence + "```" + markdownReset + "\n" +
		"\x1b[2m? · 0.0s · ctx 0% · step 1/16\x1b[0m\nplain\n"
	if got := buf.String(); !strings.Contains(got, want) {
		t.Fatalf("step boundary did not close the fence:\n got: %q\nwant substring: %q", got, want)
	}
}

func TestRendererMarkdownFenceOpensAtStepBoundary(t *testing.T) {
	for _, opener := range []string{"```", "```go"} {
		t.Run(opener, func(t *testing.T) {
			var buf bytes.Buffer
			r := newRenderer(&buf, true, 16, func() time.Time { return time.Unix(0, 0) }, false)
			ctx := context.Background()
			if err := r.OnToken(ctx, agent.TokenEvent{Content: opener}); err != nil {
				t.Fatalf("OnToken opener: %v", err)
			}
			if err := r.OnStep(ctx, agent.StepEvent{Index: 0}); err != nil {
				t.Fatalf("OnStep: %v", err)
			}
			if err := r.OnToken(ctx, agent.TokenEvent{Content: "code\n```\n"}); err != nil {
				t.Fatalf("OnToken body: %v", err)
			}
			if err := r.finish(); err != nil {
				t.Fatalf("finish: %v", err)
			}

			want := markdownFence + opener + markdownReset + "\n" +
				"\x1b[2m? · 0.0s · ctx 0% · step 1/16\x1b[0m\n" +
				markdownAccent + "code" + markdownReset + "\n" + markdownFence + "```" + markdownReset + "\n"
			if got := buf.String(); !strings.Contains(got, want) {
				t.Fatalf("step boundary did not open the fence:\n got: %q\nwant substring: %q", got, want)
			}
		})
	}
}

func renderResult(t *testing.T, name string, res agent.ToolResult) string {
	t.Helper()
	var buf bytes.Buffer
	r := newRenderer(&buf, false, 16, func() time.Time { return time.Unix(0, 0) }, false)
	ev := agent.ToolResultEvent{
		Call:   provider.ToolCall{Function: provider.ToolCallFunction{Name: name}},
		Result: res,
	}
	if err := r.OnToolResult(context.Background(), ev); err != nil {
		t.Fatalf("OnToolResult: %v", err)
	}
	return buf.String()
}

func TestResultSummary_ByTool(t *testing.T) {
	tests := []struct {
		name string
		tool string
		res  agent.ToolResult
		want string
	}{
		{"read_file lines", "read_file", agent.ToolResult{Content: strings.Repeat("x\n", 86)}, "< 86 lines\n"},
		{"read_file truncated", "read_file", agent.ToolResult{Content: "a\nb", Truncated: true}, "< 2 lines (truncated)\n"},
		{"search matches", "search", agent.ToolResult{Content: "a.go:1: foo\na.go:5: foo\nb.go:2: foo"}, "< 3 matches in 2 files\n"},
		{"search none", "search", agent.ToolResult{Content: "no matches"}, "< no matches\n"},
		{"glob entries", "glob", agent.ToolResult{Content: "x\ny/\nz"}, "< 3 entries\n"},
		{"list none", "list", agent.ToolResult{Content: "no entries"}, "< no entries\n"},
		{"retrieve sources", "retrieve", agent.ToolResult{Content: "ctx", Attrib: &agent.RetrievalAttribution{
			Sources: make([]agent.RetrievedSource, 4)}}, "< 4 sources\n"},
		{"retrieve no attrib", "retrieve", agent.ToolResult{Content: "a\nb"}, "< 2 lines\n"},
		{"error", "read_file", agent.ToolResult{IsError: true, Content: "binary file (NUL byte detected); refusing to read"},
			"< error: binary file (NUL byte detected); refusing to read\n"},
		{"preview override", "search", agent.ToolResult{Preview: "custom"}, "< custom\n"},
		{"unknown tool defaults to lines", "whatever", agent.ToolResult{Content: "one\ntwo"}, "< 2 lines\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderResult(t, tc.tool, tc.res); got != tc.want {
				t.Errorf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOnToolResult_NewlineAfterUnterminatedToken(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, false, 16, func() time.Time { return time.Unix(0, 0) }, false)
	ctx := context.Background()
	if err := r.OnToken(ctx, agent.TokenEvent{Content: "partial"}); err != nil {
		t.Fatalf("OnToken: %v", err)
	}
	if err := r.OnToolResult(ctx, agent.ToolResultEvent{
		Call:   provider.ToolCall{Function: provider.ToolCallFunction{Name: "read_file"}},
		Result: agent.ToolResult{Content: "a"},
	}); err != nil {
		t.Fatalf("OnToolResult: %v", err)
	}
	want := "partial\n< 1 line\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

func TestRendererOnPressureGatedOncePerRun(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, false, 16, func() time.Time { return time.Unix(0, 0) }, false)
	r.warnPressure = true
	warn := agent.PressureEvent{Pressure: agent.Pressure{Level: agent.LevelWarn, UsedPct: 0.80, Cause: agent.CauseHistory}}
	below := agent.PressureEvent{Pressure: agent.Pressure{Level: agent.LevelWatch, UsedPct: 0.65}}
	if err := r.OnPressure(context.Background(), below); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("watch level must not print: %q", buf.String())
	}
	if err := r.OnPressure(context.Background(), warn); err != nil {
		t.Fatal(err)
	}
	first := buf.String()
	if !strings.Contains(first, "pressure") {
		t.Fatalf("warn level should print a pressure line, got %q", first)
	}
	if err := r.OnPressure(context.Background(), warn); err != nil {
		t.Fatal(err)
	}
	if buf.String() != first {
		t.Fatalf("second warn must not print again (one per run), got %q", buf.String())
	}
}

func TestRendererOnPressureDisabled(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, false, 16, func() time.Time { return time.Unix(0, 0) }, false)
	r.warnPressure = false
	warn := agent.PressureEvent{Pressure: agent.Pressure{Level: agent.LevelCritical, UsedPct: 0.95}}
	if err := r.OnPressure(context.Background(), warn); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("disabled renderer must not print: %q", buf.String())
	}
}

// TestResultSummaryTruncatedUnderMixed pins the three cells that matter.
// ToolResult.Truncated describes the FLAT Content; under mixed assembly a
// structured result's flat content is replaced wholesale, so the marker would
// name a rendering nobody read. The two negative cells are what stop the fix
// from degenerating into "never print (truncated)".
func TestResultSummaryTruncatedUnderMixed(t *testing.T) {
	structured := &agent.ContextSet{Groups: []agent.ContextGroup{{}}}
	for _, tc := range []struct {
		name   string
		mixed  bool
		ctxSet *agent.ContextSet
		want   bool
	}{
		{"mixed off, structured result: flat content is what the model read", false, structured, true},
		{"mixed on, plain result: flat content is kept verbatim", true, nil, true},
		{"mixed on, structured result: flat content was discarded", true, structured, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := newRenderer(&buf, false, 16, func() time.Time { return time.Unix(0, 0) }, tc.mixed)
			err := r.OnToolResult(context.Background(), agent.ToolResultEvent{
				Call:   provider.ToolCall{Function: provider.ToolCallFunction{Name: "read_file"}},
				Result: agent.ToolResult{Content: "a\nb\n", Truncated: true, Context: tc.ctxSet},
			})
			if err != nil {
				t.Fatalf("OnToolResult: %v", err)
			}
			// Assert on the rendered LINE, not on a recomputed suffix: the bug was
			// in what reached the terminal.
			if got := strings.Contains(buf.String(), "(truncated)"); got != tc.want {
				t.Errorf("output %q contains (truncated) = %v, want %v", buf.String(), got, tc.want)
			}
			if !strings.Contains(buf.String(), "2 lines") {
				t.Errorf("summary body lost: %q", buf.String())
			}
		})
	}
}
