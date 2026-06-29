package agenttrace

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

func readSpans(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	var spans []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad span line %q: %v", sc.Text(), err)
		}
		spans = append(spans, m)
	}
	return spans
}

func TestTelemetrySink_SpansAndContentLight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.jsonl")
	clk := time.Unix(0, 0)
	sink, err := NewTelemetrySink(path, "run7", clk, func() time.Time { return clk.Add(3 * time.Second) })
	if err != nil {
		t.Fatalf("NewTelemetrySink: %v", err)
	}

	ctx := context.Background()
	// A model step that carries a SECRET in assistant content + usage/route.
	_ = sink.OnStep(ctx, agent.StepEvent{
		Index:    0,
		Response: provider.ChatResponse{Content: "SECRET-assistant", Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 1, TotalTokens: 10}},
		RouteOutcome: &provider.RouteOutcome{
			ActualModel: provider.ModelKey{Provider: "llamacpp", Model: "qwen3:8b"},
			WasSticky:   true,
		},
		Pressure: agent.Pressure{UsedPct: 0.5},
		Latency:  2 * time.Second,
	})
	// A tool result that carries SECRET args + output.
	_ = sink.OnToolResult(ctx, agent.ToolResultEvent{
		Step:    0,
		Call:    provider.ToolCall{Function: provider.ToolCallFunction{Name: "read_file", Arguments: json.RawMessage(`{"path":"SECRET-arg"}`)}},
		Effect:  agent.Effect{Class: agent.Read},
		Result:  agent.ToolResult{Content: "SECRET-output", Truncated: true},
		Invoked: true,
		Latency: 4 * time.Millisecond,
	})
	res := agent.Result{Steps: []agent.StepRecord{{Index: 0}}, StopReason: agent.Completed}
	if err := sink.Finish(res, "completed"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := readSpans(t, path)
	kinds := map[string]int{}
	for _, s := range spans {
		kinds[s["kind"].(string)]++
	}
	if kinds["model_step"] != 1 || kinds["tool_call"] != 1 || kinds["run"] != 1 {
		t.Fatalf("span kinds = %v, want one each", kinds)
	}

	raw, _ := os.ReadFile(path)
	for _, secret := range []string{"SECRET-assistant", "SECRET-arg", "SECRET-output"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("telemetry leaked %q:\n%s", secret, raw)
		}
	}
	// Sanity: content-light fields ARE present.
	if !strings.Contains(string(raw), "qwen3:8b") || !strings.Contains(string(raw), `"content_bytes":13`) {
		t.Fatalf("missing content-light fields:\n%s", raw)
	}
}

// TestTelemetrySink_SwallowsWriteErrors proves the sink never aborts a run.
func TestTelemetrySink_SwallowsWriteErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.jsonl")
	sink, err := NewTelemetrySink(path, "run1", time.Unix(0, 0), time.Now)
	if err != nil {
		t.Fatalf("NewTelemetrySink: %v", err)
	}
	// Close the underlying file early so subsequent writes fail.
	_ = sink.Close()
	if err := sink.OnStep(context.Background(), agent.StepEvent{Index: 0}); err != nil {
		t.Fatalf("OnStep returned %v, want nil (best-effort)", err)
	}
	if err := sink.OnToolResult(context.Background(), agent.ToolResultEvent{Step: 0}); err != nil {
		t.Fatalf("OnToolResult returned %v, want nil", err)
	}
}
