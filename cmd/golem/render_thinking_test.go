package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/internal/agenttrace"
)

func thinkingRendererEmit(t *testing.T, r *renderer, deltas ...string) {
	t.Helper()
	for _, d := range deltas {
		if err := r.OnThinking(context.Background(), agent.ThinkingEvent{Content: d}); err != nil {
			t.Fatalf("OnThinking(%q): %v", d, err)
		}
	}
}

func TestRendererThinkingDistinctFromAnswer(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, true, 4, nil)
	thinkingRendererEmit(t, r, "I should", " check")
	if err := r.OnToken(context.Background(), agent.TokenEvent{Content: "answer"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if !strings.Contains(out, "[thinking]") {
		t.Fatalf("missing [thinking] header in %q", out)
	}
	if !strings.Contains(out, "\x1b[2mI should\x1b[0m") || !strings.Contains(out, "\x1b[2m check\x1b[0m") {
		t.Fatalf("thinking deltas not dim-wrapped in %q", out)
	}
	// Newline separates the thinking block from the answer; the answer starts
	// on its own line (lastNL bookkeeping intact).
	i := strings.Index(out, "answer")
	if i <= 0 || out[i-1] != '\n' {
		t.Fatalf("answer not on its own line start in %q", out)
	}
	if strings.Contains(out[i:], "\x1b[") {
		t.Fatalf("answer must not be dimmed: %q", out[i:])
	}
}

func TestRendererThinkingNoColor(t *testing.T) {
	var buf bytes.Buffer
	r := newRenderer(&buf, false, 4, nil)
	thinkingRendererEmit(t, r, "pondering")
	out := buf.String()
	if !strings.Contains(out, "[thinking]") {
		t.Fatalf("missing [thinking] header in %q", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("color=false output contains ANSI escapes: %q", out)
	}
}

func TestRendererThinkingResetsPerStep(t *testing.T) {
	var buf bytes.Buffer
	now := func() time.Time { return time.Unix(1719600000, 0) }
	r := newRenderer(&buf, false, 4, now)
	if err := r.OnThinking(context.Background(), agent.ThinkingEvent{Step: 0, Content: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := r.OnStep(context.Background(), agent.StepEvent{Index: 0}); err != nil {
		t.Fatal(err)
	}
	if err := r.OnThinking(context.Background(), agent.ThinkingEvent{Step: 1, Content: "second"}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), "[thinking]"); n != 2 {
		t.Fatalf("want 2 [thinking] headers across steps, got %d in %q", n, buf.String())
	}
}

func TestComposedObserverForwardsThinking(t *testing.T) {
	var buf bytes.Buffer
	rend := newRenderer(&buf, false, 4, nil)

	now := func() time.Time { return time.Unix(1719600000, 0) }
	sink, err := agenttrace.NewTelemetrySink(t.TempDir()+"/telemetry.jsonl", "run-1", now(), now)
	if err != nil {
		t.Fatalf("sink: %v", err)
	}
	defer sink.Close()

	obs := composeObserver(rend, sink)
	if obs == agent.Observer(rend) {
		t.Fatal("composeObserver should have wrapped the renderer")
	}
	to, ok := obs.(agent.ThinkingObserver)
	if !ok {
		t.Fatal("composed observer does not implement agent.ThinkingObserver")
	}
	if err := to.OnThinking(context.Background(), agent.ThinkingEvent{Content: "hmm"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "[thinking]") || !strings.Contains(out, "hmm") {
		t.Fatalf("thinking not forwarded to renderer: %q", out)
	}
}
