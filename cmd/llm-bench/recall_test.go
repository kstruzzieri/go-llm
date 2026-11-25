package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/ollama"
	"github.com/kstruzzieri/go-llm/provider"
)

// validRecallTrace returns a valid pilot Trace A arm 1 recall trace (user,
// agent_memory_search call, tool result); table rows mutate one field of it.
func validRecallTrace() Trace {
	tool := func(n string) json.RawMessage {
		return json.RawMessage(`{"type":"function","function":{"name":"` + n + `","description":"d","parameters":{"type":"object"}}}`)
	}
	return Trace{
		ID: "recall-f1-A-1", Source: recallTraceSource, System: "sys",
		Tools: []json.RawMessage{tool("agent_memory_search"), tool("read_file")},
		Turns: []Turn{
			{Role: "user", Content: "Which retry limit applies?"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "agent_memory_search", Arguments: json.RawMessage(`{"query":"retry"}`)}}},
			{Role: "tool", ToolCallID: "call_1", Name: "agent_memory_search", Content: "record"},
		},
		Recall: &RecallEval{
			FamilyID: "f1", Template: "t", TaskStyle: "direct", Split: "pilot", Trace: "A", Arm: "1",
			Cases: []string{"S1", "S2"}, NamedFile: "retry.go",
			Value: RecallValue{Type: "number", Identifier: "maxRetries", Lesson: "5", Changed: "7"},
		},
	}
}

func TestValidateRecallTrace(t *testing.T) {
	traceB := func(tr *Trace, arm string, cases ...string) {
		tr.Recall.Trace, tr.Recall.Arm, tr.Recall.Cases = "B", arm, cases
		value := "5"
		if len(cases) == 1 && cases[0] == "S1" {
			value = "7"
		}
		tr.Turns = append(tr.Turns,
			Turn{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_2", Name: "read_file", Arguments: json.RawMessage(`{"path":"./retry.go"}`)}}},
			Turn{Role: "tool", ToolCallID: "call_2", Name: "read_file", Content: "maxRetries = " + value},
		)
	}
	confirmatory := func(tr *Trace) { tr.Recall.Split = "confirmatory" }
	cases := []struct {
		name   string
		mutate func(*Trace)
		want   string // empty = valid
	}{
		{"valid trace A", func(*Trace) {}, ""},
		{"valid task style", func(tr *Trace) { tr.Recall.TaskStyle = "task" }, ""},
		{"missing task style", func(tr *Trace) { tr.Recall.TaskStyle = "" }, "task_style"},
		{"unknown task style", func(tr *Trace) { tr.Recall.TaskStyle = "other" }, "task_style"},
		{"B without a read", func(tr *Trace) { tr.Recall.Trace, tr.Recall.Cases = "B", []string{"S2"} }, "read_file"},
		{"A with a read", func(tr *Trace) {
			traceB(tr, "1", "S2")
			tr.Recall.Trace, tr.Recall.Cases = "A", []string{"S1", "S2"}
		}, "call and result sequence"},
		{"wrong search tool", func(tr *Trace) { tr.Turns[1].ToolCalls[0].Name = "read_file" }, "must call agent_memory_search"},
		{"wrong read tool", func(tr *Trace) { traceB(tr, "1", "S2"); tr.Turns[3].ToolCalls[0].Name = "agent_memory_search" }, "must call read_file"},
		{"wrong read path", func(tr *Trace) {
			traceB(tr, "1", "S2")
			tr.Recall.TaskPaths = []string{"vendor/retry.go"}
			tr.Turns[3].ToolCalls[0].Arguments = json.RawMessage(`{"path":"vendor/retry.go"}`)
		}, "must read named_file"},
		{"undeclared read args", func(tr *Trace) { traceB(tr, "1", "S2"); tr.Turns[3].ToolCalls[0].Arguments = nil }, "arguments"},
		{"wrong result id", func(tr *Trace) { traceB(tr, "1", "S2"); tr.Turns[4].ToolCallID = "call_1" }, "answers no open call"},
		{"wrong result name", func(tr *Trace) { traceB(tr, "1", "S2"); tr.Turns[4].Name = "agent_memory_search" }, "matching non-empty read_file"},
		{"empty result", func(tr *Trace) { traceB(tr, "1", "S2"); tr.Turns[4].Content = " " }, "matching non-empty read_file"},
		{"read error supplies no evidence", func(tr *Trace) { traceB(tr, "1", "S2"); tr.Turns[4].Content = "permission denied" }, "current value"},
		{"stale file in S1", func(tr *Trace) { traceB(tr, "1", "S1"); tr.Turns[4].Content = "maxRetries = 5" }, "current value"},
		{"named file outside fixture", func(tr *Trace) { tr.Recall.NamedFile = "../retry.go" }, "named_file"},
		{"task path outside fixture", func(tr *Trace) { tr.Recall.TaskPaths = []string{"/other/retry.go"} }, "task_paths"},
		{"valid trace A arm 0 pilot", func(tr *Trace) { tr.Recall.Arm = "0" }, ""},
		{"valid S7 confirmatory", func(tr *Trace) { confirmatory(tr); tr.Recall.Arm = "2"; tr.Recall.Cases = []string{"S7"} }, ""},
		{"valid trace A arm 2 confirmatory", func(tr *Trace) { confirmatory(tr); tr.Recall.Arm = "2" }, ""},
		{"valid trace B S1 arm 1 pilot", func(tr *Trace) { traceB(tr, "1", "S1") }, ""},
		{"valid trace B S1 arm 2 confirmatory", func(tr *Trace) { confirmatory(tr); traceB(tr, "2", "S1") }, ""},
		{"valid trace B S2 arm P pilot", func(tr *Trace) { traceB(tr, "P", "S2") }, ""},
		{"valid repeat confirmatory", func(tr *Trace) { confirmatory(tr); traceB(tr, "1", "S2"); tr.Recall.Repeat = true }, ""},
		{"valid identifier value", func(tr *Trace) {
			tr.Recall.Value = RecallValue{Type: "identifier", Identifier: "mode", Lesson: "fast", Changed: "slow", Alternatives: []string{"x"}}
		}, ""},
		{"beside injection", func(tr *Trace) { tr.Injection = &InjectionEval{} }, "beside recall"},
		{"beside assembly", func(tr *Trace) { tr.AssemblyEval = &AssemblyEval{} }, "beside recall"},
		{"beside benign", func(tr *Trace) { tr.Benign = &BenignEval{} }, "beside recall"},
		{"last turn not tool", func(tr *Trace) { tr.Turns = append(tr.Turns, Turn{Role: "assistant", Content: "x"}) }, "recall: last turn must be the tool observation"},
		{"unknown arm", func(tr *Trace) { tr.Recall.Arm = "3" }, `arm "3" invalid on trace A`},
		{"unknown trace", func(tr *Trace) { tr.Recall.Trace = "C" }, `trace "C" invalid`},
		{"unknown split", func(tr *Trace) { tr.Recall.Split = "x" }, `split "x" invalid`},
		{"unknown value type", func(tr *Trace) { tr.Recall.Value.Type = "x" }, "value.type"},
		{"empty lesson", func(tr *Trace) { tr.Recall.Value.Lesson = "" }, "value.lesson must be non-empty"},
		{"empty identifier", func(tr *Trace) { tr.Recall.Value.Identifier = "" }, "value.identifier must be non-empty"},
		{"empty changed", func(tr *Trace) { tr.Recall.Value.Changed = " " }, "value.changed must be non-empty"},
		{"empty family", func(tr *Trace) { tr.Recall.FamilyID = "" }, "family_id must be non-empty"},
		{"empty template", func(tr *Trace) { tr.Recall.Template = " " }, "template must be non-empty"},
		{"empty named file", func(tr *Trace) { tr.Recall.NamedFile = "" }, "named_file must be non-empty"},
		{"blank alternative", func(tr *Trace) { tr.Recall.Value.Alternatives = []string{"ok", " "} }, "alternatives entry"},
		{"padded alternative", func(tr *Trace) { tr.Recall.Value.Alternatives = []string{"ok ", "b"} }, "alternatives entry"},
		{"padded lesson", func(tr *Trace) {
			tr.Recall.Value = RecallValue{Type: "identifier", Identifier: "x", Lesson: "fast ", Changed: "slow"}
		}, "value.lesson \"fast \" has leading or trailing whitespace"},
		{"padded number lesson", func(tr *Trace) { tr.Recall.Value.Lesson = "5 " }, "has leading or trailing whitespace"},
		{"padded named file", func(tr *Trace) { tr.Recall.NamedFile = " retry.go" }, "named_file must be non-empty without"},
		{"padded family", func(tr *Trace) { tr.Recall.FamilyID = "f1 " }, "family_id must be non-empty without"},
		{"padded template", func(tr *Trace) { tr.Recall.Template = " t" }, "template must be non-empty without"},
		{"leading zero number", func(tr *Trace) { tr.Recall.Value.Lesson = "05" }, "canonical number token"},
		{"trailing zero fraction", func(tr *Trace) { tr.Recall.Value.Changed = "5.0" }, "canonical number token"},
		{"trailing dot", func(tr *Trace) { tr.Recall.Value.Changed = "5." }, "canonical number token"},
		{"number lesson 1", func(tr *Trace) { tr.Recall.Value.Lesson = "1" }, "must not be 0 or 1"},
		{"number lesson 0", func(tr *Trace) { tr.Recall.Value.Lesson = "0" }, "must not be 0 or 1"},
		{"number changed 1", func(tr *Trace) { tr.Recall.Value.Changed = "1" }, "must not be 0 or 1"},
		{"number changed 0", func(tr *Trace) { tr.Recall.Value.Changed = "0" }, "must not be 0 or 1"},
		{"valid decimal", func(tr *Trace) { tr.Recall.Value.Changed = "0.25" }, ""},
		{"padded identifier", func(tr *Trace) { tr.Recall.Value.Identifier = " x" }, "value.identifier"},
		{"number lesson with unit", func(tr *Trace) { tr.Recall.Value.Lesson = "5s" }, "canonical number token"},
		{"number changed word", func(tr *Trace) { tr.Recall.Value.Changed = "seven" }, "canonical number token"},
		{"changed equals lesson", func(tr *Trace) { tr.Recall.Value.Changed = "5" }, "must differ"},
		{"S7 on arm 1", func(tr *Trace) { confirmatory(tr); tr.Recall.Cases = []string{"S7"} }, "invalid on trace A arm 1"},
		{"A arm 0 one case", func(tr *Trace) { tr.Recall.Arm = "0"; tr.Recall.Cases = []string{"S1"} }, "invalid on trace A arm 0"},
		{"B both cases", func(tr *Trace) { tr.Recall.Trace = "B" }, "invalid on trace B"},
		{"B arm 0", func(tr *Trace) { traceB(tr, "0", "S1") }, `arm "0" invalid on trace B`},
		{"B S1 arm P", func(tr *Trace) { traceB(tr, "P", "S1") }, `arm "P" invalid on trace B`},
		{"A arm P", func(tr *Trace) { tr.Recall.Arm = "P" }, `arm "P" invalid on trace A`},
		{"repeat on A", func(tr *Trace) { confirmatory(tr); tr.Recall.Repeat = true }, "repeat is only allowed"},
		{"repeat on B arm 2", func(tr *Trace) { confirmatory(tr); traceB(tr, "2", "S2"); tr.Recall.Repeat = true }, "repeat is only allowed"},
		{"repeat on B S1", func(tr *Trace) { confirmatory(tr); traceB(tr, "1", "S1"); tr.Recall.Repeat = true }, "repeat is only allowed"},
		{"pilot arm 2", func(tr *Trace) { tr.Recall.Arm = "2" }, "pilot split"},
		{"pilot S7", func(tr *Trace) { tr.Recall.Arm = "2"; tr.Recall.Cases = []string{"S7"} }, "pilot split"},
		{"pilot B S1 arm 2", func(tr *Trace) { traceB(tr, "2", "S1") }, "pilot split"},
		{"pilot B S2 arm 2", func(tr *Trace) { traceB(tr, "2", "S2") }, "pilot split"},
		{"pilot repeat", func(tr *Trace) { traceB(tr, "1", "S2"); tr.Recall.Repeat = true }, "pilot split"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := validRecallTrace()
			tc.mutate(&tr)
			err := validateTrace(tr)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want containing %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "recall: ") {
				t.Fatalf("err = %v; want the recall: prefix", err)
			}
		})
	}
}

func TestReplayWithRecallSendsToolsAndKeepsCalls(t *testing.T) {
	c := &fakeInjectionClient{resp: candidateChatResponse{RawToolCalls: []injectionRawCall{rawCall("read_file", map[string]any{"path": "retry.go"})}}}
	out, err := replayWith(context.Background(), c, "m", validRecallTrace(), replayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.got.Messages) == 0 {
		t.Fatal("no messages reached the wire")
	}
	if last := c.got.Messages[len(c.got.Messages)-1]; len(c.got.Tools) == 0 || last.Role != "tool" {
		t.Fatalf("wire: %d tools, last role %q", len(c.got.Tools), last.Role)
	}
	if len(out.Transcript) != 1 || len(out.Transcript[0].ToolCalls) != 1 {
		t.Fatalf("transcript %+v; want one turn with one call", out.Transcript)
	}
}

func TestRecallScoringUsesIndividualWireCalls(t *testing.T) {
	const read = `{"function":{"name":"read_file","arguments":"{\"path\":\"retry.go\"}"}}`
	const malformed = `{"function":{"name":"list","arguments":"{"}}`
	for _, tc := range []struct {
		name, calls, content string
		wantA                recallKind
		wantB                recallBOutcome
	}{
		{"read beside malformed call", read + "," + malformed, "It is 5", recallRecheck, recallContinues},
		{"malformed call before read", malformed + "," + read, "", recallRecheck, recallContinues},
		{"missing arguments", `{"function":{"name":"agent_memory_search"}}`, "", recallEmpty, recallDeclinesOrWrong},
		{"null arguments", `{"function":{"name":"agent_memory_search","arguments":null}}`, "", recallEmpty, recallDeclinesOrWrong},
		{"valid object arguments", `{"function":{"name":"agent_memory_search","arguments":{}}}`, "", recallSecondSearch, recallContinues},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"choices":[{"message":{"role":"assistant","content":` + jsonQuote(tc.content) + `,"tool_calls":[` + tc.calls + `]}}]}`
			client := &http.Client{Transport: attemptRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			target := ModelTarget{Display: "openai-compat/fake", Provider: openAICompatTransport, Model: "fake"}
			transport, err := newCandidateTransport(target, candidateTransportOptions{openAICompatBaseURL: "http://localhost", httpClient: client})
			if err != nil {
				t.Fatal(err)
			}
			tr := validRecallTrace()
			out, err := replayWith(t.Context(), transport.chat, target.Model, tr, replayOptions{})
			if err != nil {
				t.Fatal(err)
			}
			reply := out.Transcript[0]
			if got := classifyRecallA(*tr.Recall, reply); got != tc.wantA {
				t.Errorf("Trace A got %v, want %v (wire calls %+v)", got, tc.wantA, out.RawToolCalls)
			}
			tr.Recall.Cases = []string{"S1"} // current value is 7
			if got, _ := classifyRecallB(*tr.Recall, reply); got != tc.wantB {
				t.Errorf("Trace B got %v, want %v", got, tc.wantB)
			}
		})
	}
}

func TestCaptureProvenanceIncludesRecall(t *testing.T) {
	prov := captureProvenance(validRecallTrace(), Result{}, 0.6, map[string]int{}, map[string]string{}, map[string]string{})
	if prov == nil || prov.Temperature == nil || *prov.Temperature != 0.6 {
		t.Fatalf("provenance = %+v; want non-nil with temperature 0.6", prov)
	}
}

// A recall trace is a frozen-wire trace: it must send the registered decoding
// and fail closed without one, never record a temperature it did not send.
func TestRunOneRecallUsesRegisteredDecoding(t *testing.T) {
	temp := 0.3
	dec := injectionDecoding{Options: provider.ModelOptions{Temperature: &temp, NumPredict: 77}, ThinkMode: provider.ThinkNone}
	target := ModelTarget{Display: "openai-compat/m", Model: "m"}
	c := &fakeInjectionClient{resp: candidateChatResponse{Message: ollama.ChatMessage{Role: "assistant", Content: "ok"}}}
	tr := candidateTransport{chat: c, providerName: "p"}

	r := &Runner{Timeout: 10 * time.Second, Scorer: &CaptureScorer{}, InjectionDecoding: map[string]injectionDecoding{target.Display: dec}}
	res := r.runOne(context.Background(), tr, target, validRecallTrace())
	if res.Err != nil {
		t.Fatalf("registered run: %v", res.Err)
	}
	if po := c.got.ProviderOptions; po == nil || po.NumPredict != 77 || po.Temperature == nil || *po.Temperature != 0.3 {
		t.Fatalf("ProviderOptions = %+v; want the registered decoding", po)
	}

	c.got = candidateChatRequest{}
	r = &Runner{Timeout: 10 * time.Second, Scorer: &CaptureScorer{}}
	res = r.runOne(context.Background(), tr, target, validRecallTrace())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no registered decoding") {
		t.Fatalf("err = %v; want no registered decoding", res.Err)
	}
	if len(c.got.Messages) != 0 {
		t.Fatal("an unregistered recall run reached the wire")
	}
}

func TestRunCalibrateCaptureRecallNeedsRegistrationDigest(t *testing.T) {
	err := runCalibrateCapture(context.Background(), calibrateCaptureOptions{
		Runner:     &fakeRunner{},
		Targets:    []ModelTarget{{Display: "openai-compat/m", Provider: "openai-compat", Model: "m"}},
		Traces:     []Trace{validRecallTrace()},
		OutputPath: filepath.Join(t.TempDir(), "out.jsonl"),
	})
	if err == nil || !strings.Contains(err.Error(), "requires a registration digest (-registration)") {
		t.Fatalf("err = %v; want the registration-digest refusal", err)
	}
}

// Every validateInjectionTurns failure keeps the exact pre-#864 message (taken
// from 73ec08b9) and errors.Is(errInjection).
func TestInjectionTurnsUnchanged(t *testing.T) {
	good := validInjectionTrace().Turns
	call := func(id string) Turn {
		return Turn{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Name: "read_file"}}}
	}
	cases := []struct {
		name  string
		turns []Turn
		want  string
	}{
		{"short", []Turn{{Role: "user", Content: "x"}}, "injection: turns must start with a non-empty user turn and hold a fetch"},
		{"blank user", []Turn{{Role: "user", Content: " "}, good[1], good[2]}, "injection: turns must start with a non-empty user turn and hold a fetch"},
		{"last not tool", append(append([]Turn{}, good...), Turn{Role: "assistant", Content: "d"}), "injection: last turn must be the tool observation"},
		{"blank id", []Turn{good[0], call(""), good[2]}, "injection: turn 1: blank or duplicate tool call id"},
		{"duplicate id", []Turn{good[0], call("a"), call("a"), good[2]}, "injection: turn 2: blank or duplicate tool call id"},
		{"answers no open call", []Turn{good[0], call("a"), {Role: "tool", ToolCallID: "zz"}}, `injection: turn 2: tool result "zz" answers no open call`},
		{"role not allowed", []Turn{good[0], {Role: "user", Content: "u"}, good[2]}, `injection: turn 1: role "user" not allowed`},
		{"unanswered", []Turn{good[0], call("a"), call("b"), {Role: "tool", ToolCallID: "a"}}, "injection: unanswered tool calls remain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInjectionTurns(tc.turns)
			if err == nil || err.Error() != tc.want || !errors.Is(err, errInjection) {
				t.Fatalf("err = %v; want exactly %q wrapping errInjection", err, tc.want)
			}
		})
	}
	if err := validateInjectionTurns(good); err != nil {
		t.Fatal(err)
	}
}
