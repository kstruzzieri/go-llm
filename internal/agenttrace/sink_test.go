package agenttrace

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/contextdepth"
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

func TestTelemetrySecretFindingsCountsOnlyRecognizedRules(t *testing.T) {
	value := "sk-" + strings.Repeat("a8Bc7De9", 3)
	var recognized []agent.Finding
	for _, rule := range []string{
		"sensitive_openai_token", "sensitive_github_token", "sensitive_gitlab_token",
		"sensitive_slack_token", "sensitive_npm_token", "sensitive_bearer_token",
		"sensitive_secret_assignment", "sensitive_private_key", "sensitive_payment_card",
	} {
		recognized = append(recognized, agent.Finding{
			Interceptor: "secrets", Rule: rule, Verdict: agent.VerdictBlock,
			Detail: value, ToolCallID: value,
		})
	}
	// Repeated kinds are separate findings, not a distinct-credential count.
	recognized = append(recognized, recognized[0])
	unrecognized := []agent.Finding{
		{Interceptor: "other", Rule: "sensitive_openai_token", Verdict: agent.VerdictBlock},
		{Interceptor: "secrets", Rule: "sensitive_" + value, Verdict: agent.VerdictBlock},
	}
	for _, tc := range []struct {
		name string
		risk *agent.RiskReport
		want int
	}{
		{name: "nil risk"},
		{name: "empty risk", risk: &agent.RiskReport{}},
		{name: "unrecognized", risk: &agent.RiskReport{Findings: unrecognized}},
		{name: "recognized and repeated", risk: &agent.RiskReport{Findings: append(recognized, unrecognized...)}, want: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "telemetry.jsonl")
			sink, err := NewTelemetrySink(path, "run1", time.Unix(0, 0), func() time.Time { return time.Unix(0, 0) })
			if err != nil {
				t.Fatal("cannot create telemetry sink")
			}
			if err := sink.Finish(agent.Result{Risk: tc.risk}, "completed"); err != nil {
				t.Fatal("cannot finish telemetry")
			}
			if err := sink.Close(); err != nil {
				t.Fatal("cannot close telemetry")
			}
			raw, err := os.ReadFile(path)
			if err != nil || strings.Contains(string(raw), value) {
				t.Fatal("telemetry unreadable or leaked finding content")
			}
			spans := readSpans(t, path)
			if len(spans) != 1 || spans[0]["kind"] != "run" {
				t.Fatal("Finish did not emit exactly one run span")
			}
			count, exists := spans[0]["secret_findings"]
			if tc.want == 0 {
				if exists {
					t.Error("zero secret findings changed the existing JSON shape")
				}
			} else if count != float64(tc.want) {
				t.Errorf("secret_findings = %v, want %d", count, tc.want)
			}
		})
	}
}

func TestOnToolResult_RecordsDelegatedModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	s, err := NewTelemetrySink(path, "run1", time.Unix(0, 0), func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatalf("NewTelemetrySink: %v", err)
	}
	ro := &provider.RouteOutcome{
		ActualModel:   provider.ModelKey{Provider: "local", Model: "coder"},
		PlannedModel:  provider.ModelKey{Provider: "local", Model: "coder"},
		FallbacksUsed: 0,
	}
	err = s.OnToolResult(context.Background(), agent.ToolResultEvent{
		Step:    0,
		Call:    provider.ToolCall{Function: provider.ToolCallFunction{Name: "delegate_code"}},
		Effect:  agent.Effect{Class: agent.Read | agent.Network},
		Invoked: true,
		Result:  agent.ToolResult{Content: "code", RouteOutcome: ro},
	})
	if err != nil {
		t.Fatalf("OnToolResult: %v", err)
	}
	_ = s.Close()

	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"delegated_model":"local/coder"`) {
		t.Fatalf("span missing delegated_model: %s", data)
	}
}

func TestOnToolResult_OmitsDelegatedModelWhenNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	s, _ := NewTelemetrySink(path, "run1", time.Unix(0, 0), func() time.Time { return time.Unix(0, 0) })
	_ = s.OnToolResult(context.Background(), agent.ToolResultEvent{
		Step:    0,
		Call:    provider.ToolCall{Function: provider.ToolCallFunction{Name: "read_file"}},
		Effect:  agent.Effect{Class: agent.Read},
		Invoked: true,
		Result:  agent.ToolResult{Content: "x"},
	})
	_ = s.Close()
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "delegated_model") {
		t.Fatalf("nil RouteOutcome must omit delegated_model: %s", data)
	}
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

func TestTelemetrySink_OmitsStopReasonForNonCompletedStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.jsonl")
	sink, err := NewTelemetrySink(path, "run-error", time.Unix(0, 0), time.Now)
	if err != nil {
		t.Fatalf("NewTelemetrySink: %v", err)
	}

	if err := sink.Finish(agent.Result{}, "error"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	spans := readSpans(t, path)
	if len(spans) != 1 || spans[0]["kind"] != "run" {
		t.Fatalf("spans = %+v, want one run span", spans)
	}
	if _, ok := spans[0]["stop_reason"]; ok {
		t.Fatalf("error span has stop_reason = %v", spans[0]["stop_reason"])
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

func TestTelemetrySinkOnPressureSpan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemetry.jsonl")
	started := time.Unix(0, 0)
	sink, err := NewTelemetrySink(path, "run1", started, func() time.Time { return started })
	if err != nil {
		t.Fatalf("NewTelemetrySink: %v", err)
	}
	p1 := agent.Pressure{UsedPct: 0.50, InputTokens: 50, InputBudget: 100, Level: agent.LevelOK, Cause: agent.CauseHistory, Mitigation: agent.MitigationNone}
	p2 := agent.Pressure{UsedPct: 0.80, InputTokens: 80, InputBudget: 100, Level: agent.LevelWarn, Cause: agent.CauseHistory, Mitigation: agent.MitigationWarn}
	if err := sink.OnPressure(context.Background(), agent.PressureEvent{Step: 0, Pressure: p1}); err != nil {
		t.Fatal(err)
	}
	if err := sink.OnPressure(context.Background(), agent.PressureEvent{Step: 1, Pressure: p2}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Finish(agent.Result{}, "completed"); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	_ = sink.Close()

	spans := readSpans(t, path)
	var stage0, stage1, run map[string]any
	for _, s := range spans {
		switch s["kind"] {
		case "runtime_stage":
			if s["span_id"] == "run1-stage-assemble-0" {
				stage0 = s
			}
			if s["span_id"] == "run1-stage-assemble-1" {
				stage1 = s
			}
		case "run":
			run = s
		}
	}
	if stage0 == nil || stage1 == nil || run == nil {
		t.Fatalf("missing spans: stage0=%v stage1=%v run=%v", stage0 != nil, stage1 != nil, run != nil)
	}
	if stage0["stage"] != "assemble" || stage0["parent_id"] != "run1-run" {
		t.Fatalf("stage0 fields wrong: %+v", stage0)
	}
	if stage0["used_pct_delta"].(float64) != 0 {
		t.Fatalf("first delta should be 0, got %v", stage0["used_pct_delta"])
	}
	if d := stage1["used_pct_delta"].(float64); d < 0.29 || d > 0.31 {
		t.Fatalf("second delta should be ~0.30, got %v", d)
	}
	if stage1["level"] != "warn" || stage1["mitigation"] != "warn" || stage1["cause"] != "history" {
		t.Fatalf("stage1 labels wrong: %+v", stage1)
	}
	if mp := run["max_used_pct"].(float64); mp < 0.79 || mp > 0.81 {
		t.Fatalf("run max_used_pct should be ~0.80, got %v", mp)
	}
	if run["max_pressure_level"] != "warn" {
		t.Fatalf("run max_pressure_level should be warn, got %v", run["max_pressure_level"])
	}
	if int(stage0["schema_version"].(float64)) != SchemaVersion || SchemaVersion != 2 {
		t.Fatalf("schema version: span=%v const=%d want 2", stage0["schema_version"], SchemaVersion)
	}
}

func TestTelemetrySinkExhaustedOutcome(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	started := time.Unix(0, 0)
	sink, _ := NewTelemetrySink(path, "runX", started, func() time.Time { return started })
	p := agent.Pressure{UsedPct: 1.2, InputTokens: 120, InputBudget: 100, Level: agent.LevelCritical, Cause: agent.CausePinned, Mitigation: agent.MitigationHalt}
	_ = sink.OnPressure(context.Background(), agent.PressureEvent{Step: 0, Pressure: p})
	_ = sink.Close()
	for _, s := range readSpans(t, path) {
		if s["kind"] == "runtime_stage" {
			if s["outcome"] != "exhausted" {
				t.Fatalf("halt mitigation should yield outcome=exhausted, got %v", s["outcome"])
			}
			return
		}
	}
	t.Fatal("no runtime_stage span written")
}

// TestTelemetrySinkStepSpanEnrichedPressure guards the model_step pressureLite
// enrichment (level/cause/mitigation/input_budget) added in schema v2, which the
// other tests exercise only via the runtime_stage span.
func TestTelemetrySinkStepSpanEnrichedPressure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	started := time.Unix(0, 0)
	sink, _ := NewTelemetrySink(path, "runE", started, func() time.Time { return started })
	_ = sink.OnStep(context.Background(), agent.StepEvent{
		Index:    0,
		Pressure: agent.Pressure{UsedPct: 0.8, InputTokens: 80, InputBudget: 100, Level: agent.LevelWarn, Cause: agent.CauseHistory, Mitigation: agent.MitigationWarn},
	})
	_ = sink.Close()
	for _, s := range readSpans(t, path) {
		if s["kind"] != "model_step" {
			continue
		}
		pr, ok := s["pressure"].(map[string]any)
		if !ok {
			t.Fatalf("model_step missing pressure object: %+v", s)
		}
		if pr["level"] != "warn" || pr["cause"] != "history" || pr["mitigation"] != "warn" {
			t.Fatalf("model_step pressure not enriched: %+v", pr)
		}
		if int(pr["input_budget"].(float64)) != 100 || int(pr["input_tokens"].(float64)) != 80 {
			t.Fatalf("model_step pressure tokens missing: %+v", pr)
		}
		return
	}
	t.Fatal("no model_step span written")
}

// TestTelemetrySinkAnchorOmissions guards the field that carries #331's
// within-anchor omissions to operators. Both span kinds are checked because the
// sink projects Pressure twice, and a copy dropped from either one loses the
// signal on that span. The zero case pins omitempty: a legacy or lossless turn
// emits the same bytes schema v2 always did.
func TestTelemetrySinkAnchorOmissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	started := time.Unix(0, 0)
	sink, _ := NewTelemetrySink(path, "runA", started, func() time.Time { return started })
	shed := agent.Pressure{UsedPct: 0.08, AnchorOmissions: 5, Compactions: 1, InputTokens: 8, InputBudget: 100,
		Level: agent.LevelOK, Cause: agent.CauseToolOutput, Mitigation: agent.MitigationEvict}
	clean := agent.Pressure{UsedPct: 0.08, InputTokens: 8, InputBudget: 100,
		Level: agent.LevelOK, Cause: agent.CauseToolOutput, Mitigation: agent.MitigationNone}
	_ = sink.OnPressure(context.Background(), agent.PressureEvent{Step: 0, Pressure: shed})
	_ = sink.OnPressure(context.Background(), agent.PressureEvent{Step: 1, Pressure: clean})
	_ = sink.OnStep(context.Background(), agent.StepEvent{Index: 0, Pressure: shed})
	_ = sink.OnStep(context.Background(), agent.StepEvent{Index: 1, Pressure: clean})
	_ = sink.Close()

	seen := 0
	for _, s := range readSpans(t, path) {
		field := s
		switch s["kind"] {
		case "runtime_stage":
		case "model_step":
			pr, ok := s["pressure"].(map[string]any)
			if !ok {
				t.Fatalf("model_step missing pressure object: %+v", s)
			}
			field = pr
		default:
			continue
		}
		seen++
		v, present := field["anchor_omissions"]
		if s["step"].(float64) == 0 {
			if !present || int(v.(float64)) != 5 {
				t.Errorf("%v span: anchor_omissions = %v (present %v), want 5", s["kind"], v, present)
			}
			continue
		}
		if present {
			t.Errorf("%v span: anchor_omissions = %v on a lossless turn, want the key omitted", s["kind"], v)
		}
	}
	if seen != 4 {
		t.Fatalf("%d pressure-bearing spans, want 4", seen)
	}
}

// TestTelemetrySinkContextAssemblySpan pins the #331 span: the aggregate
// counters, the decision/omission breakdowns, and — the part that is a policy
// choice rather than a mapping — that NO subject identifier reaches the file.
// The fixture deliberately uses a real-looking source path, a memory record ID
// and a tool call ID, so a future field that forwarded any of them turns this
// red instead of quietly widening what telemetry retains.
func TestTelemetrySinkContextAssemblySpan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	started := time.Unix(0, 0)
	sink, err := NewTelemetrySink(path, "runA", started, func() time.Time { return started })
	if err != nil {
		t.Fatalf("NewTelemetrySink: %v", err)
	}
	tr := agent.ContextAssemblyTrace{
		MaxTokens: 4096, EstimatedTokensUsed: 1200, EstimatedTokensFree: 2896,
		SelectedSubjects: 5, RenderedSubjects: 3, OmittedSubjects: 2, VerbatimShortfalls: 1,
		Subjects: []agent.ContextSubjectTrace{
			{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "internal/secret/plan.go"},
				ToolCallID: "call-abc123", Decision: agent.DecisionBase, Bytes: 100},
			{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "internal/secret/other.go"},
				ToolCallID: "call-abc123", Decision: agent.DecisionUpgrade, Bytes: 250},
			{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainMemory, ID: "mem-9f3c"},
				ToolCallID: "call-def456", Decision: agent.DecisionFloor, Bytes: 40},
			{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainRAG, ID: "internal/secret/big.go"},
				ToolCallID: "call-abc123", Omitted: true, Decision: agent.DecisionOmitted, OmissionReason: agent.OmitByteCap},
			{Subject: contextdepth.SubjectRef{Domain: contextdepth.DomainConversation, ID: "3"},
				Omitted: true, Decision: agent.DecisionOmitted, OmissionReason: agent.OmitTokenBudget},
		},
	}
	if err := sink.OnContextAssembly(context.Background(), agent.ContextAssemblyEvent{Step: 2, Trace: tr}); err != nil {
		t.Fatalf("OnContextAssembly: %v", err)
	}
	_ = sink.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Content pin, not a shape pin: greping the RAW bytes is what catches an
	// identifier smuggled in under any field name, including one added later.
	for _, leak := range []string{"internal/secret", "plan.go", "mem-9f3c", "call-abc123", "call-def456"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("telemetry leaked %q:\n%s", leak, raw)
		}
	}

	spans := readSpans(t, path)
	if len(spans) != 1 {
		t.Fatalf("%d spans, want 1: %+v", len(spans), spans)
	}
	s := spans[0]
	for field, want := range map[string]float64{
		"schema_version": SchemaVersion, "step": 2, "max_tokens": 4096,
		"used_tokens": 1200, "free_tokens": 2896, "subjects": 5, "rendered": 3,
		"omitted": 2, "verbatim_shortfalls": 1, "rendered_bytes": 390,
	} {
		if got, ok := s[field].(float64); !ok || got != want {
			t.Errorf("%s = %v, want %v", field, s[field], want)
		}
	}
	if s["kind"] != "context_assembly" {
		t.Errorf("kind = %v, want context_assembly", s["kind"])
	}
	if s["span_id"] != "runA-stage-context-2" || s["parent_id"] != "runA-run" {
		t.Errorf("span_id = %v parent_id = %v", s["span_id"], s["parent_id"])
	}
	wantDec := map[string]any{agent.DecisionBase: 1.0, agent.DecisionUpgrade: 1.0, agent.DecisionFloor: 1.0}
	if got := s["by_decision"]; !reflect.DeepEqual(got, wantDec) {
		t.Errorf("by_decision = %v, want %v", got, wantDec)
	}
	wantOmit := map[string]any{agent.OmitByteCap: 1.0, agent.OmitTokenBudget: 1.0}
	if got := s["by_omission_reason"]; !reflect.DeepEqual(got, wantOmit) {
		t.Errorf("by_omission_reason = %v, want %v", got, wantOmit)
	}
}

// A legacy or no-anchor turn never fires OnContextAssembly, so the maps must not
// appear at all on the spans such a run does emit — the same
// byte-identical-when-unused rule AnchorOmissions follows.
func TestTelemetrySinkContextAssemblyAbsentWithoutMixed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	started := time.Unix(0, 0)
	sink, _ := NewTelemetrySink(path, "runA", started, func() time.Time { return started })
	_ = sink.OnStep(context.Background(), agent.StepEvent{Index: 0})
	_ = sink.Close()
	for _, s := range readSpans(t, path) {
		if s["kind"] == "context_assembly" {
			t.Errorf("context_assembly span emitted without a mixed assembly: %+v", s)
		}
	}
}

func TestOnToolResult_RecordsAutoApproved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	s, err := NewTelemetrySink(path, "run1", time.Unix(0, 0), func() time.Time { return time.Unix(0, 0) })
	if err != nil {
		t.Fatalf("NewTelemetrySink: %v", err)
	}
	if err := s.OnToolResult(context.Background(), agent.ToolResultEvent{
		Step:         0,
		Call:         provider.ToolCall{Function: provider.ToolCallFunction{Name: "run_command"}},
		Effect:       agent.Effect{Class: agent.Exec},
		Invoked:      true,
		AutoApproved: true,
		Result:       agent.ToolResult{Content: "ok"},
	}); err != nil {
		t.Fatalf("OnToolResult: %v", err)
	}
	_ = s.Close()
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"auto_approved":true`) {
		t.Fatalf("span missing auto_approved: %s", data)
	}
}

func TestOnToolResult_OmitsAutoApprovedWhenFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trace.jsonl")
	s, _ := NewTelemetrySink(path, "run1", time.Unix(0, 0), func() time.Time { return time.Unix(0, 0) })
	_ = s.OnToolResult(context.Background(), agent.ToolResultEvent{
		Step:    0,
		Call:    provider.ToolCall{Function: provider.ToolCallFunction{Name: "run_command"}},
		Effect:  agent.Effect{Class: agent.Exec},
		Invoked: true,
		Result:  agent.ToolResult{Content: "ok"},
	})
	_ = s.Close()
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "auto_approved") {
		t.Fatalf("false AutoApproved must be omitted (SchemaVersion 2 byte-identity): %s", data)
	}
}
