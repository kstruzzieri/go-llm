package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type denialMetadataTool struct {
	contentTool
	result ToolResult
	after  func()
	err    error
}

func (t denialMetadataTool) Invoke(context.Context, json.RawMessage) (ToolResult, error) {
	if t.after != nil {
		t.after()
	}
	return t.result, t.err
}
func (t denialMetadataTool) Origin() Origin { return OriginModel }

func TestChildScopeDenialCarrier(t *testing.T) {
	evidence := []ChildScopeDenial{{Task: 2, Requests: 7}}
	t.Run("JSON exclusion", func(t *testing.T) {
		out := ToolResult{Content: "innocent", ChildScopeDenials: evidence}
		msg := InspectedMessage{Content: "innocent", ChildScopeDenials: evidence}
		for _, value := range []any{out, msg, InputInspection{Messages: []InspectedMessage{msg}}} {
			b, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "ChildScopeDenials") || strings.Contains(string(b), "Requests") {
				t.Fatalf("wire evidence=%s", b)
			}
		}
		encodedInput, _ := json.Marshal(InputInspection{Messages: []InspectedMessage{msg}})
		var roundTrip InputInspection
		if err := json.Unmarshal(encodedInput, &roundTrip); err != nil {
			t.Fatal(err)
		}
		if roundTrip.Messages[0].ChildScopeDenials != nil {
			t.Fatal("inspection round trip retained authority")
		}
		b, _ := json.Marshal(out)
		var decoded ToolResult
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.ChildScopeDenials != nil {
			t.Fatal("round trip retained authority")
		}
		forged := []byte(`{"ChildScopeDenials":[{"Task":0,"Requests":99}]}`)
		if err := json.Unmarshal(forged, &decoded); err != nil {
			t.Fatal(err)
		}
		var inspected InspectedMessage
		if err := json.Unmarshal(forged, &inspected); err != nil {
			t.Fatal(err)
		}
		var input InputInspection
		if err := json.Unmarshal([]byte(`{"Messages":[{"ChildScopeDenials":[{"Task":0,"Requests":99}]}]}`), &input); err != nil {
			t.Fatal(err)
		}
		if decoded.ChildScopeDenials != nil || inspected.ChildScopeDenials != nil || input.Messages[0].ChildScopeDenials != nil {
			t.Fatal("JSON forged authority")
		}
		// json:"-" ignores input; it does not sanitize a populated trusted value.
		decoded.ChildScopeDenials = evidence
		if err := json.Unmarshal(forged, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.ChildScopeDenials, evidence) {
			t.Fatal("trusted value unexpectedly changed")
		}
	})
	t.Run("owned receipt and truncation", func(t *testing.T) {
		source := []ChildScopeDenial{{Task: 2, Requests: 7}}
		tool := denialMetadataTool{contentTool: contentTool{name: "carrier"}, result: ToolResult{Content: strings.Repeat("x", 100), ChildScopeDenials: source}}
		o := newTestOrchestrator(&scriptedCaller{})
		got, err := o.invokeCall(t.Context(), tool, normalizeEffect(Effect{Class: Read, OutputCap: 8}), json.RawMessage("{}"))
		if err != nil {
			t.Fatal(err)
		}
		source[0].Requests = 999
		if !got.Truncated || len(got.Content) != 8 || !reflect.DeepEqual(got.ChildScopeDenials, evidence) {
			t.Fatalf("receipt=%+v", got)
		}
	})
	for _, mixed := range []bool{false, true} {
		t.Run("owned callbacks mixed="+map[bool]string{false: "off", true: "on"}[mixed], func(t *testing.T) {
			source := []ChildScopeDenial{{Task: 2, Requests: 7}}
			tool := denialMetadataTool{contentTool: contentTool{name: "carrier"}, result: ToolResult{Content: "innocent", ChildScopeDenials: source}}
			var captured []ChildScopeDenial
			mutate := &stubInterceptor{name: "mutate", input: func(in InputInspection) []Finding {
				for _, m := range in.Messages {
					if len(m.ChildScopeDenials) > 0 {
						m.ChildScopeDenials[0].Requests = 999
					}
				}
				return nil
			}}
			capture := &stubInterceptor{name: "capture", input: func(in InputInspection) []Finding {
				for _, m := range in.Messages {
					if len(m.ChildScopeDenials) > 0 {
						captured = m.ChildScopeDenials
					}
				}
				return nil
			}}
			obs := &interceptRecorder{onToolResult: func(e *ToolResultEvent) { e.Result.ChildScopeDenials[0].Requests = 888 }}
			o := New(&scriptedCaller{responses: []ModelResult{readCalls("carrier"), finalAnswer("done")}}, ContextManager{Mixed: mixed}, WithInterceptors(mutate, capture))
			res, err := o.Run(t.Context(), Request{Goal: "q", Tools: []Tool{tool}}, obs)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(captured, evidence) || !reflect.DeepEqual(source, evidence) {
				t.Fatalf("aliased source=%+v inspection=%+v", source, captured)
			}
			// The observer's copy is detached from the canonical result as well.
			canonical := ToolResult{Content: "x", ChildScopeDenials: []ChildScopeDenial{{Task: 2, Requests: 7}}}
			state := &State{}
			_, err = o.recordResult(t.Context(), &Result{}, state, obs, &restraintGovernor{}, 0, tc("carrier", "{}"), normalizeEffect(tool.Effect()), ToolCallRecord{Invoked: true}, canonical, false, &batch{}, o.newInterceptorRun())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(canonical.ChildScopeDenials, evidence) {
				t.Fatal("observer mutated canonical result")
			}
			for _, value := range []any{res.Messages, res.ToolCalls, state} {
				b, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), "ChildScopeDenials") {
					t.Fatalf("history retained metadata: %s", b)
				}
			}
		})
	}
}

func TestChildScopeDenialLifecycle(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		for _, mode := range []string{"allow", "block", "abort", "interceptor error", "interception observer error", "result observer error", "governor discard", "cancel before inspection", "cancel during drain", "invoke error", "invalid context"} {
			t.Run(fmt.Sprintf("%s/mixed=%v", mode, mixed), func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				reporter := &stubInterceptor{name: "native", input: func(in InputInspection) []Finding {
					var out []Finding
					for _, m := range in.Messages {
						for _, d := range m.ChildScopeDenials {
							out = append(out, Finding{Rule: "denial", Risk: int(d.Requests) * 10, Verdict: VerdictAllow, Target: TargetMessage, StateIndex: m.StateIndex})
						}
					}
					return out
				}}
				chain := []Interceptor{reporter}
				var obs Observer
				want := 1
				wantErr := false
				n := 1
				switch mode {
				case "block", "abort":
					verdict := VerdictBlock
					if mode == "abort" {
						verdict = VerdictAbort
						wantErr = true
					}
					chain = append(chain, &stubInterceptor{name: "reject", input: func(in InputInspection) []Finding {
						if isObservation(in) {
							return []Finding{{Rule: "reject", Verdict: verdict}}
						}
						return nil
					}})
				case "interceptor error":
					chain = append(chain, &errOnInput{name: "error", when: isObservation})
					wantErr = true
				case "interception observer error":
					obs = &interceptRecorder{err: errors.New("boom")}
					wantErr = true
				case "result observer error":
					obs = &resultRec{failAt: 1}
					wantErr = true
				case "governor discard":
					n = 5
					want = 3
				case "cancel before inspection":
					want = 0
					wantErr = true
				case "cancel during drain":
					n = 2
					want = 2
					wantErr = true
					obs = &interceptRecorder{onToolResult: func(e *ToolResultEvent) {
						if e.Call.Function.Name == "carrier0" {
							cancel()
						}
					}}
				case "invoke error":
					want = 0
				case "invalid context":
					if mixed {
						want = 0
						wantErr = true
					}
				}
				var tools []Tool
				var names []string
				for i := 0; i < n; i++ {
					name := fmt.Sprintf("carrier%d", i)
					names = append(names, name)
					tool := denialMetadataTool{contentTool: contentTool{name: name}, result: ToolResult{Content: "innocent", ChildScopeDenials: []ChildScopeDenial{{Task: i, Requests: 1}}}}
					if mode == "governor discard" {
						tool.result.IsError = true
					}
					if mode == "cancel before inspection" {
						tool.after = cancel
					}
					if mode == "invoke error" {
						tool.err = errors.New("hard tool failure")
					}
					if mode == "invalid context" {
						tool.result.Context = groupsSet(maxContextGroups + 1)
					}
					tools = append(tools, tool)
				}
				o := New(&scriptedCaller{responses: []ModelResult{readCalls(names...), finalAnswer("done")}}, ContextManager{Mixed: mixed}, WithInterceptors(chain...))
				res, err := o.Run(ctx, Request{Goal: "q", Tools: tools}, obs)
				if (err != nil) != wantErr {
					t.Fatalf("err=%v want error=%v", err, wantErr)
				}
				got, score := 0, 0
				if res.Risk != nil {
					score = res.Risk.Score
					for _, f := range res.Risk.Findings {
						if f.Rule == "denial" {
							got++
						}
					}
				}
				if got != want || score != want*10 {
					t.Fatalf("risk=%+v want %d native findings", res.Risk, want)
				}
				if mode == "governor discard" && (res.StopReason != ToolErrorCapReached || len(res.ToolCalls) != 5) {
					t.Fatalf("discard audit=%+v", res)
				}
				if mode == "cancel before inspection" && (len(res.ToolCalls) != 1 || !res.ToolCalls[0].Invoked) {
					t.Fatal("lost invocation audit")
				}
			})
		}
	}
}
