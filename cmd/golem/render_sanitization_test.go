package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func TestRendererTerminalTokenQuotesOSC52WithoutColor(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, false, 4, nil, false)
	r.terminal = true
	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "safe\x1b]52;c;Y2xpcA==\a text"}); err != nil {
		t.Fatal(err)
	}
	if err := r.finish(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), `safe\x1b]52;c;Y2xpcA==\a text`; got != want {
		t.Fatalf("terminal output = %q, want %q", got, want)
	}
}

func TestRendererDetectsTTYWithColorDisabled(t *testing.T) {
	terminal := openTestTerminal(t)
	r := newRenderer(terminal, false, 4, nil, false)
	if !r.terminal || r.color {
		t.Fatalf("TTY detection = %v, color = %v; want true, false", r.terminal, r.color)
	}
}

func TestRendererTerminalToolCallQuotesControls(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, false, 4, nil, false)
	r.terminal = true
	err := r.OnToolCall(context.Background(), agent.ToolCallEvent{Call: provider.ToolCall{
		Function: provider.ToolCallFunction{
			Name:      "read\x1b[1D_file",
			Arguments: json.RawMessage("{\"path\":\"x\x1b]8;;https://spoof\a\"}"),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "\n> read\\x1b[1D_file {\"path\":\"x\\x1b]8;;https://spoof\\a\"}\n"
	if got := out.String(); got != want {
		t.Fatalf("terminal tool call = %q, want %q", got, want)
	}
}

func TestRendererTerminalThinkingQuotesControlsKeepsStyle(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, true, 4, nil, false)
	r.terminal = true
	if err := r.OnThinking(context.Background(), agent.ThinkingEvent{Content: "think\x1b[2J\r\b\t\u009b"}); err != nil {
		t.Fatal(err)
	}
	if err := r.finish(); err != nil {
		t.Fatal(err)
	}
	want := "\x1b[2m[thinking]\x1b[0m\n\x1b[2mthink\\x1b[2J\\r\\b\x1b[0m\x1b[2m\\t\\u009b\x1b[0m"
	if got := out.String(); got != want {
		t.Fatalf("terminal thinking = %q, want %q", got, want)
	}
}

func TestRendererTerminalTokenPreservesSplitUTF8AndQuotesControls(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, false, 4, nil, false)
	r.terminal = true
	for _, delta := range []string{"A\xf0", "\x9f", "\x99\x82\x1b", "[2J\nB"} {
		if err := r.OnToken(context.Background(), agent.TokenEvent{Content: delta}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.finish(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "A🙂\\x1b[2J\nB"; got != want {
		t.Fatalf("split terminal token = %q, want %q", got, want)
	}
}

func TestRendererTerminalTokenKeepsCompleteRuneBeforeNextPartialRune(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, false, 4, nil, false)
	r.terminal = true
	for _, delta := range []string{"\xf0\x9f\x99", "\x82\xe2\x82", "\xac"} {
		if err := r.OnToken(context.Background(), agent.TokenEvent{Content: delta}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.finish(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "🙂€"; got != want {
		t.Fatalf("adjacent split runes = %q, want %q", got, want)
	}
}

func TestRendererTerminalThinkingPreservesSplitUTF8BeforeAnswer(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, true, 4, nil, false)
	r.terminal = true
	for _, delta := range []string{"\xf0", "\x9f\x99", "\x82"} {
		if err := r.OnThinking(context.Background(), agent.ThinkingEvent{Content: delta}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "answer"}); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !utf8.ValidString(got) || !strings.Contains(got, "\x1b[2m🙂\x1b[0m\nanswer") {
		t.Fatalf("split thinking rune lost or styling crossed transition: %q", got)
	}
}

func TestRendererTerminalFlushesIncompleteUTF8BeforeStepAndFinish(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, false, 4, nil, false)
	r.terminal = true
	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "before\xf0"}); err != nil {
		t.Fatal(err)
	}
	if err := r.OnStep(context.Background(), agent.StepEvent{}); err != nil {
		t.Fatal(err)
	}
	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "after\xf0"}); err != nil {
		t.Fatal(err)
	}
	if err := r.finish(); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "before�\n? · 0.0s · ctx 0% · step 1/4\nafter�"; got != want {
		t.Fatalf("incomplete terminal token = %q, want %q", got, want)
	}
}

func TestRendererTerminalFlushesIncompleteThinkingBeforeAnswer(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, true, 4, nil, false)
	r.terminal = true
	if err := r.OnThinking(context.Background(), agent.ThinkingEvent{Content: "thought\xf0"}); err != nil {
		t.Fatal(err)
	}
	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "answer"}); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "\x1b[2m[thinking]\x1b[0m\n\x1b[2mthought\x1b[0m\x1b[2m�\x1b[0m\nanswer"; got != want {
		t.Fatalf("incomplete thinking transition = %q, want %q", got, want)
	}
}

func TestRendererTerminalReportsPendingFlushError(t *testing.T) {
	r := newRenderer(shortWriter{}, false, 4, nil, false)
	r.terminal = true
	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "\xf0"}); err != nil {
		t.Fatal(err)
	}
	if err := r.finish(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("finish error = %v, want io.ErrShortWrite", err)
	}
}

func TestRendererNonTerminalPreservesForeignBytes(t *testing.T) {
	var out bytes.Buffer
	r := newRenderer(&out, false, 4, nil, false)
	if r.terminal {
		t.Fatal("buffer must not be detected as a terminal")
	}
	ctx := context.Background()
	for _, delta := range []string{"A\xf0", "\x9f\x99\x82\x1b]52;c;YQ==\a\xf0"} {
		if err := r.OnToken(ctx, agent.TokenEvent{Content: delta}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.OnThinking(ctx, agent.ThinkingEvent{Content: "\r\b\u009b"}); err != nil {
		t.Fatal(err)
	}
	if err := r.OnToolCall(ctx, agent.ToolCallEvent{Call: provider.ToolCall{Function: provider.ToolCallFunction{
		Name: "read", Arguments: json.RawMessage("\x1b[2J"),
	}}}); err != nil {
		t.Fatal(err)
	}
	want := "A🙂\x1b]52;c;YQ==\a\xf0\n[thinking]\n\r\b\u009b\n> read \x1b[2J\n"
	if got := out.String(); got != want {
		t.Fatalf("non-terminal output = %q, want %q", got, want)
	}
}
