package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/internal/promptfence"
	"github.com/kstruzzieri/go-llm/provider"
)

// TestToolTrustContractDescribesTheWireMarkers pins the contract's marker
// description to promptfence's real formatting with the random id replaced
// by the placeholder, so prose and mechanism cannot drift.
func TestToolTrustContractDescribesTheWireMarkers(t *testing.T) {
	f := promptfence.New()
	open := strings.Replace(f.Open(toolResultRegion), f.ID(), "<key>", 1)
	closing := strings.Replace(f.Close(toolResultRegion), f.ID(), "<key>", 1)
	for _, want := range []string{
		"begins with " + open + " ",
		"ends with " + closing + ".",
	} {
		if !strings.Contains(ToolTrustContract, want) {
			t.Errorf("contract does not describe marker %q:\n%s", want, ToolTrustContract)
		}
	}
	if strings.ContainsAny(ToolTrustContract, "\r\n") {
		t.Errorf("contract must be one paragraph")
	}
}

// TestRunAddsToolTrustContractOnce: every request of a run carries the
// contract exactly once, after the caller's text, whether or not the caller
// supplied an application prompt.
func TestRunAddsToolTrustContractOnce(t *testing.T) {
	for _, tc := range []struct{ name, system, want string }{
		{"custom application prompt", "sys", "sys\n\n" + ToolTrustContract},
		{"empty application prompt", "", ToolTrustContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := &wireCaller{responses: []ModelResult{
				toolCallResponse(call("1", "echo", `{}`)),
				{Response: provider.ChatResponse{Content: "done", Done: true}},
			}}
			o := newTestOrchestrator(mc)
			if _, err := o.Run(context.Background(), Request{Goal: "q", System: tc.system, Tools: []Tool{echoTool{name: "echo"}}}, nil); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(mc.requests) != 2 {
				t.Fatalf("requests = %d, want 2", len(mc.requests))
			}
			for i, req := range mc.requests {
				sys := req.Messages[0]
				if sys.Role != "system" {
					t.Fatalf("request %d: base contract missing: first message is %+v", i, sys)
				}
				if sys.Content != tc.want {
					t.Errorf("request %d: system = %q, want %q", i, sys.Content, tc.want)
				}
				switch n := strings.Count(sys.Content, ToolTrustContract); {
				case n == 0:
					t.Errorf("request %d: base contract missing", i)
				case n > 1:
					t.Errorf("request %d: base contract repeated %d times", i, n)
				}
			}
		})
	}
}

type addendumStub struct {
	*stubInterceptor
	text string
}

func (s addendumStub) ForRun(context.Context, RunScope) (Interceptor, string, error) {
	return s.stubInterceptor, s.text, nil
}

func TestRunSeparatesInterceptorAddenda(t *testing.T) {
	for _, tc := range []struct {
		name    string
		addenda []string
		want    string
	}{
		{"plain text", []string{"Use compact output.", "Preserve units."}, "\n\nUse compact output.\n\nPreserve units."},
		{"empty addenda", []string{"", ""}, ""},
		{"verbatim whitespace", []string{"", "  Use compact output.\n", "", "\tPreserve units.  ", ""}, "\n\n  Use compact output.\n\n\n\tPreserve units.  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, system := range []string{"", "Caller instructions.\n\n"} {
				chain := make([]Interceptor, len(tc.addenda))
				for i, text := range tc.addenda {
					chain[i] = addendumStub{&stubInterceptor{name: strings.Repeat("x", i+1)}, text}
				}
				mc := &wireCaller{responses: []ModelResult{finalAnswer("done")}}
				o := newTestOrchestrator(mc, WithInterceptors(chain...))
				if _, err := o.Run(context.Background(), Request{Goal: "q", System: system}, nil); err != nil {
					t.Fatal(err)
				}
				want := ToolTrustContract + tc.want
				if system != "" {
					want = "Caller instructions.\n\n\n\n" + want
				}
				if got := mc.requests[0].Messages[0].Content; got != want {
					t.Fatalf("caller=%q: system = %q, want %q", system, got, want)
				}
			}
		})
	}
}

// TestToolTrustContractPrecedesInspectionAndFitting: the step-0 initial
// inspection sees the contract, scoped interceptors still see only the
// caller's text, and the contract's cost is pinned, so a budget that fit
// the bare application prompt exhausts before any model call.
func TestToolTrustContractPrecedesInspectionAndFitting(t *testing.T) {
	ic := &stubInterceptor{name: "rec"}
	sc := &scopedStub{name: "scoped"}
	mc := &wireCaller{responses: []ModelResult{{Response: provider.ChatResponse{Content: "done", Done: true}}}}
	o := newTestOrchestrator(mc, WithInterceptors(ic, sc))
	if _, err := o.Run(context.Background(), Request{Goal: "q", System: "sys"}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(ic.inputs) == 0 || !strings.Contains(ic.inputs[0].System, ToolTrustContract) {
		t.Errorf("inspection missed base contract: step-0 System = %q", ic.inputs)
	}
	if want := "sys\n\n" + ToolTrustContract + "\n\n [canary:x]"; len(ic.inputs) == 0 || ic.inputs[0].System != want {
		t.Errorf("inspected System = %q, want %q (caller text, contract, addenda)", ic.inputs[0].System, want)
	}
	if len(sc.scopes) != 1 || sc.scopes[0] != (RunScope{System: "sys"}) {
		t.Errorf("scoped interceptor saw %+v, want the caller's text alone", sc.scopes)
	}

	// Pinned cost: "sys\n\n" (5) + contract + goal "q" (1), no tools. One
	// below that ceiling must exhaust before Chat.
	pinned := 5 + len([]rune(ToolTrustContract)) + 1
	mc = &wireCaller{}
	o = newTestOrchestrator(mc)
	_, err := o.Run(context.Background(), Request{Goal: "q", System: "sys", Budget: Budget{InputCeiling: pinned - 1}}, nil)
	if !errors.Is(err, ErrContextExhausted) || len(mc.requests) != 0 {
		t.Fatalf("expected context exhaustion before Chat, got err=%v requests=%d", err, len(mc.requests))
	}
	mc = &wireCaller{responses: []ModelResult{{Response: provider.ChatResponse{Content: "done", Done: true}}}}
	o = newTestOrchestrator(mc)
	if _, err := o.Run(context.Background(), Request{Goal: "q", System: "sys", Budget: Budget{InputCeiling: pinned}}, nil); err != nil {
		t.Fatalf("exact pinned ceiling must fit: %v", err)
	}
}
