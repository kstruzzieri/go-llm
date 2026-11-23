package main

import (
	"encoding/json"
	"testing"
)

func numVal() RecallValue {
	return RecallValue{Type: "number", Identifier: "maxRetries", Lesson: "5", Changed: "8"}
}

func idVal() RecallValue {
	return RecallValue{Type: "identifier", Identifier: "strategy", Lesson: "retry.max_attempts", Changed: "exponential", Alternatives: []string{"linear"}}
}

func call(name, args string) ToolCall { return ToolCall{Name: name, Arguments: json.RawMessage(args)} }

func TestStatesValueNumber(t *testing.T) {
	v := numVal()
	for in, want := range map[string]bool{
		"maxRetries = 5": true, "five retries": true, "(5)": true, "5.": true, "5,": true, "5s": true, "Five": true,
		"50": false, "0.5": false, "15": false, "fifty": false, "v5x": false, "x_5": false,
	} {
		if got := statesValue(in, v, "5"); got != want {
			t.Errorf("statesValue(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestStatesValueIdentifier(t *testing.T) {
	for _, c := range []struct {
		in, val string
		want    bool
	}{
		{"set retry.max_attempts", "retry.max_attempts", true},
		{"`a+b`", "a+b", true},
		{"Retry.max_attempts", "retry.max_attempts", false},
		{"retry.max_attempts2", "retry.max_attempts", false},
		{"ab", "a+b", false},
		{"The strategy is exponential.", "exponential", true},
		{"set retry.max_attempts.", "retry.max_attempts", true},
		{"retry.max_attempts.x", "retry.max_attempts", false},
		{"x.retry.max_attempts", "retry.max_attempts", false},
		{"xexponential then exponential", "exponential", true},
		{"-exponential-", "exponential", true},
	} {
		if got := statesValue(c.in, idVal(), c.val); got != c.want {
			t.Errorf("statesValue(%q, %q) = %v, want %v", c.in, c.val, got, c.want)
		}
	}
}

func TestClassifyRecallA(t *testing.T) {
	ev := RecallEval{Value: numVal(), NamedFile: "config/retry.go"}
	for _, c := range []struct {
		name  string
		reply Turn
		want  recallKind
	}{
		{"rely", Turn{Content: "it is 5", ToolCalls: []ToolCall{call("glob", `{"pattern":"*.go"}`)}}, recallRely},
		{"recheck read", Turn{Content: "it is 5", ToolCalls: []ToolCall{call("read_file", `{"path":"config/retry.go"}`)}}, recallRecheck},
		{"recheck search", Turn{ToolCalls: []ToolCall{call("search", `{"pattern":"maxRetries"}`)}}, recallRecheck},
		{"other read", Turn{Content: "5", ToolCalls: []ToolCall{call("read_file", `{"path":"a.go"}`)}}, recallRely},
		{"bad args", Turn{ToolCalls: []ToolCall{call("read_file", `{`)}}, recallOtherTool},
		{"second search", Turn{ToolCalls: []ToolCall{call("agent_memory_search", `{}`)}}, recallSecondSearch},
		{"write", Turn{ToolCalls: []ToolCall{call("agent_memory_promote", `{}`)}}, recallMemoryWrite},
		{"other tool", Turn{ToolCalls: []ToolCall{call("list", `{}`)}}, recallOtherTool},
		{"empty", Turn{Content: "  "}, recallEmpty},
		{"no value", Turn{Content: "Let me ask: which file?"}, recallNoValue},
		{"other number", Turn{Content: "See line 12"}, recallOtherValue},
		{"changed", Turn{Content: "It is 8"}, recallOtherValue},
	} {
		if got := classifyRecallA(ev, c.reply); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	idEv := RecallEval{Value: idVal(), NamedFile: "x.go"}
	if got := classifyRecallA(idEv, Turn{Content: "use linear"}); got != recallOtherValue {
		t.Errorf("identifier alternative: got %v", got)
	}
	if got := classifyRecallA(idEv, Turn{Content: "use retry.max_attempts."}); got != recallRely {
		t.Errorf("identifier lesson: got %v", got)
	}
}

func TestRecallScorerIgnoresThinking(t *testing.T) {
	ev := RecallEval{Value: numVal(), NamedFile: "config/retry.go"}
	if got := classifyRecallA(ev, Turn{Thinking: "5", Content: "Which value do you mean?"}); got != recallNoValue {
		t.Errorf("got %v, want recallNoValue", got)
	}
}

func TestRecallRecheckPathSpellings(t *testing.T) {
	ev := RecallEval{Value: numVal(), NamedFile: "config/retry.go", TaskPaths: []string{"svc/./main.go"}}
	for path, want := range map[string]recallKind{
		"./config/retry.go": recallRecheck, "config//retry.go": recallRecheck,
		"/workspace/config/retry.go": recallRecheck, "svc/main.go": recallRecheck,
		"config/retry_test.go": recallOtherTool, "xconfig/retry.go": recallOtherTool,
	} {
		args, _ := json.Marshal(map[string]string{"path": path})
		if got := classifyRecallA(ev, Turn{ToolCalls: []ToolCall{call("read_file", string(args))}}); got != want {
			t.Errorf("%s: got %v, want %v", path, got, want)
		}
	}
}

func TestClassifyRecallB(t *testing.T) {
	mk := func(c string) RecallEval { return RecallEval{Value: numVal(), Cases: []string{c}} }
	for _, c := range []struct {
		name   string
		ev     RecallEval
		reply  Turn
		want   recallBOutcome
		both   bool
		hedged bool
	}{
		{"S2 answers", mk("S2"), Turn{Content: "It is 5"}, recallAnswers, false, false},
		{"S2 hedged", mk("S2"), Turn{Content: "I can’t confirm it is still 5"}, recallAnswers, false, true},
		{"S2 continues", mk("S2"), Turn{ToolCalls: []ToolCall{call("read_file", `{}`)}}, recallContinues, false, false},
		{"S2 declines", mk("S2"), Turn{Content: "I'm not sure"}, recallDeclinesOrWrong, false, true},
		{"S1 both", mk("S1"), Turn{Content: "used to be 5, now 8"}, recallAnswers, true, false},
		{"S1 wrong", mk("S1"), Turn{Content: "it is 5"}, recallDeclinesOrWrong, false, false},
	} {
		got, both := classifyRecallB(c.ev, c.reply)
		if got != c.want || both != c.both || recallHedged(c.reply.Content) != c.hedged {
			t.Errorf("%s: got %v/%v/%v", c.name, got, both, recallHedged(c.reply.Content))
		}
	}
}
