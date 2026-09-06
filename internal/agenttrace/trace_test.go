package agenttrace

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/provider"
)

type traceModelCaller struct {
	responses []agent.ModelResult
}

func (c *traceModelCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (agent.ModelResult, error) {
	result := c.responses[0]
	c.responses = c.responses[1:]
	return result, nil
}

func sampleResult() agent.Result {
	return agent.Result{
		Answer:     "the answer",
		Steps:      []agent.StepRecord{{Index: 0, Latency: 10 * time.Millisecond}},
		Usage:      provider.Usage{PromptTokens: 5, CompletionTokens: 7, TotalTokens: 12},
		StopReason: agent.Completed,
	}
}

func TestBuildTrace_StatusAndPartial(t *testing.T) {
	meta := TraceMeta{Goal: "do a thing", System: "sys", MaxSteps: 16}
	res := sampleResult()

	ok := BuildTrace(meta, res, "completed", false, nil)
	if ok.Status != "completed" || ok.Partial {
		t.Fatalf("completed: status=%q partial=%v", ok.Status, ok.Partial)
	}
	if ok.Request.Goal != "do a thing" || ok.Result.Answer != "the answer" {
		t.Fatalf("request/result not embedded: %+v", ok)
	}
	if ok.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", ok.SchemaVersion, SchemaVersion)
	}

	bad := BuildTrace(meta, res, "canceled", true, context.Canceled)
	if bad.Status != "canceled" || !bad.Partial || bad.Error == "" {
		t.Fatalf("canceled: status=%q partial=%v error=%q", bad.Status, bad.Partial, bad.Error)
	}
}

func TestWriteTrace_ExclusiveAndSecured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "20260628-run7.json")
	rec := BuildTrace(TraceMeta{Goal: "g"}, sampleResult(), "completed", false, nil)

	if err := WriteTrace(path, rec); err != nil {
		t.Fatalf("WriteTrace: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != fs.FileMode(0o600) {
		t.Fatalf("perm = %v, want 0600", info.Mode().Perm())
	}
	var got TraceRecord
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Result.Answer != "the answer" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// Exclusive: a second write to the same path must fail, not clobber/append.
	if err := WriteTrace(path, rec); err == nil || !strings.Contains(err.Error(), "exist") {
		t.Fatalf("second WriteTrace err = %v, want exists error", err)
	}
}

func TestWriteTraceRetainsOrderedDelegateProposalEvidence(t *testing.T) {
	const (
		promptA = " first prompt "
		promptB = "second prompt"
	)
	delegate := agenttools.NewDelegateCode(&traceModelCaller{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{Content: "proposal A"}, RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "coder"}}},
		{Response: provider.ChatResponse{Content: "proposal B"}, RouteOutcome: &provider.RouteOutcome{ActualModel: provider.ModelKey{Provider: "local", Model: "coder"}}},
	}})
	parent := &traceModelCaller{responses: []agent.ModelResult{
		{Response: provider.ChatResponse{ToolCalls: []provider.ToolCall{
			{ID: "unknown", Type: "function", Function: provider.ToolCallFunction{Name: "missing", Arguments: json.RawMessage(`{"ignored":true}`)}},
			{ID: "delegate-a", Type: "function", Function: provider.ToolCallFunction{Name: "delegate_code", Arguments: json.RawMessage(`{"prompt":" first prompt "}`)}},
			{ID: "delegate-b", Type: "function", Function: provider.ToolCallFunction{Name: "delegate_code", Arguments: json.RawMessage(`{"prompt":"second prompt"}`)}},
		}}},
		{Response: provider.ChatResponse{Content: "done", Done: true}},
	}}
	result, err := agent.New(parent, agent.ContextManager{}).Run(context.Background(), agent.Request{
		Goal: "audit delegates", Tools: []agent.Tool{delegate},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.ToolCalls) != 3 || result.ToolCalls[0].Name != "missing" || result.ToolCalls[0].Provenance != nil {
		t.Fatalf("ordered runtime records = %+v", result.ToolCalls)
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("Marshal(Result): %v", err)
	}
	var resultRoundTrip agent.Result
	if err := json.Unmarshal(resultJSON, &resultRoundTrip); err != nil {
		t.Fatalf("Unmarshal(Result): %v", err)
	}
	if len(resultRoundTrip.ToolCalls) != 3 || resultRoundTrip.ToolCalls[1].Provenance == nil || resultRoundTrip.ToolCalls[2].Provenance == nil {
		t.Fatalf("Result round trip lost provenance: %+v", resultRoundTrip.ToolCalls)
	}

	path := filepath.Join(t.TempDir(), "delegate-trace.json")
	if err := WriteTrace(path, BuildTrace(TraceMeta{Goal: "audit delegates"}, result, "completed", false, nil)); err != nil {
		t.Fatalf("WriteTrace: %v", err)
	}
	rawTrace, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(trace): %v", err)
	}
	var trace TraceRecord
	if err := json.Unmarshal(rawTrace, &trace); err != nil {
		t.Fatalf("Unmarshal(trace): %v", err)
	}

	var proposals []agenttools.DelegateProposal
	var prompts []string
	for recordIndex, record := range trace.Result.ToolCalls {
		if record.Name != "delegate_code" {
			continue
		}
		var step *agent.StepRecord
		for i := range trace.Result.Steps {
			if trace.Result.Steps[i].Index == record.Step {
				step = &trace.Result.Steps[i]
				break
			}
		}
		if step == nil {
			t.Fatalf("missing StepRecord for tool record %d step %d", recordIndex, record.Step)
		}
		position := 0
		for i := 0; i < recordIndex; i++ {
			if trace.Result.ToolCalls[i].Step == record.Step {
				position++
			}
		}
		if position >= len(step.Response.ToolCalls) {
			t.Fatalf("record %d position %d exceeds %d original calls", recordIndex, position, len(step.Response.ToolCalls))
		}
		call := step.Response.ToolCalls[position]
		if call.Function.Name != "delegate_code" {
			t.Fatalf("record %d selected original call %q at position %d", recordIndex, call.Function.Name, position)
		}
		var args struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(call.Function.Arguments, &args); err != nil {
			t.Fatalf("decode call %d arguments: %v", position, err)
		}
		var proposal agenttools.DelegateProposal
		if err := json.Unmarshal(record.Provenance, &proposal); err != nil {
			t.Fatalf("decode proposal %d: %v", position, err)
		}
		if err := agenttools.VerifyDelegateProposal(context.Background(), delegate.ProposalVerifier(), &proposal, args.Prompt); err != nil {
			t.Fatalf("verify proposal %d with recovered prompt %q: %v", position, args.Prompt, err)
		}
		proposals = append(proposals, proposal)
		prompts = append(prompts, args.Prompt)
	}
	if len(proposals) != 2 || prompts[0] != promptA || prompts[1] != promptB {
		t.Fatalf("recovered prompts = %q, want [%q %q]", prompts, promptA, promptB)
	}
	if err := agenttools.VerifyDelegateProposal(context.Background(), delegate.ProposalVerifier(), &proposals[0], promptB); err == nil {
		t.Fatal("first proposal verified against the second call's prompt")
	}
	if err := agenttools.VerifyDelegateProposal(context.Background(), delegate.ProposalVerifier(), &proposals[1], promptA); err == nil {
		t.Fatal("second proposal verified against the first call's prompt")
	}

	step := trace.Result.Steps[0]
	wantCalls := []struct{ name, arguments string }{
		{"missing", `{"ignored":true}`},
		{"delegate_code", `{"prompt":" first prompt "}`},
		{"delegate_code", `{"prompt":"second prompt"}`},
	}
	if len(step.Response.ToolCalls) != len(wantCalls) {
		t.Fatalf("trace calls = %d, want %d", len(step.Response.ToolCalls), len(wantCalls))
	}
	for i, want := range wantCalls {
		var compact bytes.Buffer
		if err := json.Compact(&compact, step.Response.ToolCalls[i].Function.Arguments); err != nil {
			t.Fatalf("compact call %d arguments: %v", i, err)
		}
		if step.Response.ToolCalls[i].Function.Name != want.name || compact.String() != want.arguments {
			t.Fatalf("trace call %d = %s %s, want %s %s", i, step.Response.ToolCalls[i].Function.Name, compact.String(), want.name, want.arguments)
		}
	}
}

// TestTraceRecordGroundingIsAdditive pins the compatibility contract for the
// #348 grounding payload: a trace without one must serialize to the exact bytes
// it did before the field existed, and one with a payload must embed it as a
// JSON object rather than a quoted string. json.RawMessage gets both wrong if
// the field is declared as a plain string or loses omitempty.
func TestTraceRecordGroundingIsAdditive(t *testing.T) {
	rec := BuildTrace(TraceMeta{Goal: "g", System: "s", MaxSteps: 4}, sampleResult(), "completed", false, nil)

	without, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "grounding") {
		t.Fatalf("a trace with no grounding must omit the key entirely:\n%s", without)
	}

	rec.Grounding = json.RawMessage(`{"status":"partial","reason":"","tokens":7}`)
	with, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(with), `"grounding":{"status":"partial","reason":"","tokens":7}`) {
		t.Fatalf("grounding not embedded verbatim as an object:\n%s", with)
	}
	if rec.SchemaVersion != 2 {
		t.Fatalf("an additive omitempty field must not bump the schema: %d", rec.SchemaVersion)
	}

	// The only difference between the two encodings is the appended key: the
	// rest of the record must be byte-identical, so a pre-#348 consumer reading
	// a grounding-free trace sees exactly what it always did.
	var a, b map[string]json.RawMessage
	if err := json.Unmarshal(without, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(with, &b); err != nil {
		t.Fatal(err)
	}
	delete(b, "grounding")
	if len(a) != len(b) {
		t.Fatalf("grounding changed the record shape: %d keys vs %d", len(a), len(b))
	}
	for k, v := range a {
		if string(b[k]) != string(v) {
			t.Fatalf("field %q changed: %s vs %s", k, v, b[k])
		}
	}
}
