package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/kstruzzieri/go-llm/provider"
)

// deniedCaller plays responses, then fails the next request with err, as a
// guarded openai-compat client refuses a body carrying a configured
// credential (#424 D4): a wrapped provider.ErrContentDenied and no response.
type deniedCaller struct {
	responses []ModelResult
	err       error
	calls     int
}

func (d *deniedCaller) Chat(context.Context, provider.ChatRequest, func(provider.ChatResponse) error) (ModelResult, error) {
	d.calls++
	if d.calls <= len(d.responses) {
		return d.responses[d.calls-1], nil
	}
	return ModelResult{}, d.err
}

// The marker and the wrapped denial, spelled out so the tests never read the
// production values they pin.
const suppressedMarker = "[...]"

func contentDenial() error {
	return fmt.Errorf("router: chat: %w", provider.ErrContentDenied)
}

// TestOutboundDenialSuppressesCopiedInput (#424 D4): a goal carrying the
// fixture is refused by the outbound guard. Result.Messages copies the goal,
// so the copy shows the marker; the caller's history is not rewritten. The
// controls show the goal is copied verbatim with no guard installed, and on
// an unrelated failure.
func TestOutboundDenialSuppressesCopiedInput(t *testing.T) {
	fx := newSensitiveFixture(t)
	goal := "deploy with " + fx.value
	exact := exactFake{name: "exact", fixture: fx.value}
	for _, tc := range []struct {
		name     string
		chain    []Interceptor
		err      error
		wantGoal string
	}{
		{"guarded denial", []Interceptor{exact}, contentDenial(), suppressedMarker},
		{"control: no guard installed", nil, contentDenial(), goal},
		{"control: unrelated failure", []Interceptor{exact}, errors.New("provider unavailable"), goal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := []provider.ChatMessage{{Role: "user", Content: "earlier " + fx.value}, {Role: "assistant", Content: "noted"}}
			want := slices.Clone(history)
			o := newTestOrchestrator(&deniedCaller{err: tc.err}, WithInterceptors(tc.chain...))
			res, err := o.Run(context.Background(), Request{Goal: goal, History: history}, nil)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if len(res.Messages) != 1 || res.Messages[0].Role != "user" {
				t.Fatalf("Result.Messages = %d messages, want the goal alone", len(res.Messages))
			}
			if got := res.Messages[0].Content; got != tc.wantGoal {
				t.Errorf("goal copy = %q, want %q", got, tc.wantGoal)
			}
			if tc.wantGoal == suppressedMarker {
				fx.assertJSONClean(t, "Result.Messages", res.Messages)
			}
			if !reflect.DeepEqual(history, want) {
				t.Error("the caller's history was rewritten")
			}
		})
	}
}

// TestOutboundDenialLaterTurnProjection (#424 D4, plan-consult 424-B2): turn
// 1 reads a file whose content carries the fixture; turn 2's request, which
// holds it, is refused. The call's arguments carry the fixture too, as \u
// escapes in malformed JSON wrapped in a string literal, the form
// openaicompat gives arguments Go's decoder rejects and the outbound guard
// decodes leniently. The tool message's content and the call's arguments are
// replaced wholesale in Result.Messages, and no JSON string there decodes to
// the fixture. The control shows both copies arrive verbatim with no guard.
func TestOutboundDenialLaterTurnProjection(t *testing.T) {
	fx := newSensitiveFixture(t)
	wrapped, err := json.Marshal(`{"path":"notes.txt","note":"` + fx.escaped + `"`)
	if err != nil {
		t.Fatal(err)
	}
	args := string(wrapped)
	for _, tc := range []struct {
		name    string
		guarded bool
	}{{"guarded denial", true}, {"control: no guard installed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var chain []Interceptor
			if tc.guarded {
				chain = []Interceptor{exactFake{name: "exact", fixture: fx.value}}
			}
			turn1 := oneCall("c1", "read_file", args)
			mc := &deniedCaller{responses: []ModelResult{turn1}, err: contentDenial()}
			tool := contentTool{name: "read_file", content: "notes: " + fx.value}
			history := []provider.ChatMessage{{Role: "user", Content: "earlier"}, {Role: "assistant", Content: "noted"}}
			wantHistory := slices.Clone(history)
			o := newTestOrchestrator(mc, WithInterceptors(chain...))
			res, err := o.Run(context.Background(), Request{Goal: "summarize notes.txt", History: history, Tools: []Tool{tool}}, nil)
			if !errors.Is(err, provider.ErrContentDenied) || mc.calls != 2 {
				t.Fatalf("err = %v after %d model calls, want the turn 2 denial", err, mc.calls)
			}
			if len(res.Messages) != 3 || res.Messages[1].Role != "assistant" || len(res.Messages[1].ToolCalls) != 1 || res.Messages[2].Role != "tool" {
				t.Fatalf("Result.Messages = %d messages, want goal, call and observation", len(res.Messages))
			}
			call, obs := res.Messages[1].ToolCalls[0], res.Messages[2]
			if res.Messages[0].Content != "summarize notes.txt" || call.ID != "c1" || call.Function.Name != "read_file" ||
				obs.ToolName != "read_file" || obs.ToolCallID != "c1" {
				t.Error("a clean field was rewritten")
			}
			wantContent, wantArgs := "notes: "+fx.value, args
			if tc.guarded {
				wantContent, wantArgs = suppressedMarker, `"`+suppressedMarker+`"`
				fx.assertJSONClean(t, "Result.Messages", res.Messages)
			}
			if obs.Content != wantContent {
				t.Errorf("observation = %q, want %q", obs.Content, wantContent)
			}
			if got := string(call.Function.Arguments); got != wantArgs {
				t.Errorf("call arguments = %s, want %s", got, wantArgs)
			}
			if got := string(turn1.Response.ToolCalls[0].Function.Arguments); got != args {
				t.Error("the model's response was rewritten")
			}
			if !reflect.DeepEqual(history, wantHistory) {
				t.Error("the caller's history was rewritten")
			}
		})
	}
}
