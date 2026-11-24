package main

import (
	"encoding/json"
	"slices"
	"testing"
)

// recallNumVal is a number-type recall value (lesson 5, changed 8).
func recallNumVal() RecallValue {
	return RecallValue{Type: "number", Identifier: "maxRetries", Lesson: "5", Changed: "8"}
}

// recallIDVal is an identifier-type recall value.
func recallIDVal() RecallValue {
	return RecallValue{Type: "identifier", Identifier: "strategy", Lesson: "retry.max_attempts", Changed: "exponential", Alternatives: []string{"linear"}}
}

// recallCall builds a tool call with raw JSON arguments.
func recallCall(name, args string) ToolCall {
	return ToolCall{Name: name, Arguments: json.RawMessage(args)}
}

func TestStatesValueNumber(t *testing.T) {
	v := recallNumVal()
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
		if got := statesValue(c.in, recallIDVal(), c.val); got != c.want {
			t.Errorf("statesValue(%q, %q) = %v, want %v", c.in, c.val, got, c.want)
		}
	}
}

func TestClassifyRecallA(t *testing.T) {
	ev := RecallEval{Value: recallNumVal(), NamedFile: "config/retry.go"}
	for _, c := range []struct {
		name  string
		reply Turn
		want  recallKind
	}{
		{"rely", Turn{Content: "it is 5", ToolCalls: []ToolCall{recallCall("glob", `{"pattern":"*.go"}`)}}, recallRely},
		{"recheck read", Turn{Content: "it is 5", ToolCalls: []ToolCall{recallCall("read_file", `{"path":"config/retry.go"}`)}}, recallRecheck},
		{"recheck search", Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"maxRetries"}`)}}, recallRecheck},
		{"other read", Turn{Content: "5", ToolCalls: []ToolCall{recallCall("read_file", `{"path":"a.go"}`)}}, recallRely},
		{"bad args", Turn{ToolCalls: []ToolCall{recallCall("read_file", `{`)}}, recallEmpty},
		{"second search", Turn{ToolCalls: []ToolCall{recallCall("agent_memory_search", `{}`)}}, recallSecondSearch},
		{"write", Turn{ToolCalls: []ToolCall{recallCall("agent_memory_promote", `{}`)}}, recallMemoryWrite},
		{"other tool", Turn{ToolCalls: []ToolCall{recallCall("list", `{}`)}}, recallOtherTool},
		{"empty", Turn{Content: "  "}, recallEmpty},
		{"no value", Turn{Content: "Let me ask: which file?"}, recallNoValue},
		{"other number", Turn{Content: "See line 12"}, recallOtherValue},
		{"changed", Turn{Content: "It is 8"}, recallOtherValue},
		{"version string", Turn{Content: "upgrade to version 5.0.1"}, recallNoValue},
	} {
		if got := classifyRecallA(ev, c.reply); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	idEv := RecallEval{Value: recallIDVal(), NamedFile: "x.go"}
	if got := classifyRecallA(idEv, Turn{Content: "use linear"}); got != recallOtherValue {
		t.Errorf("identifier alternative: got %v", got)
	}
	if got := classifyRecallA(idEv, Turn{Content: "use retry.max_attempts."}); got != recallRely {
		t.Errorf("identifier lesson: got %v", got)
	}
}

func TestRecallScorerIgnoresThinking(t *testing.T) {
	ev := RecallEval{Value: recallNumVal(), NamedFile: "config/retry.go"}
	if got := classifyRecallA(ev, Turn{Thinking: "5", Content: "Which value do you mean?"}); got != recallNoValue {
		t.Errorf("got %v, want recallNoValue", got)
	}
}

func TestRecallRecheckPathSpellings(t *testing.T) {
	ev := RecallEval{Value: recallNumVal(), NamedFile: "config/retry.go", TaskPaths: []string{"svc/./main.go"}}
	for path, want := range map[string]recallKind{
		"./config/retry.go": recallRecheck, "config//retry.go": recallRecheck,
		"/workspace/config/retry.go": recallRecheck, "svc/main.go": recallRecheck,
		"config/retry_test.go": recallOtherTool, "xconfig/retry.go": recallOtherTool,
		"vendor/config/retry.go": recallOtherTool, "../config/retry.go": recallOtherTool,
		"/other/config/retry.go": recallOtherTool, "/workspace/vendor/config/retry.go": recallOtherTool,
		"/workspace/../other/config/retry.go": recallOtherTool, "/workspace/svc/main.go": recallRecheck,
	} {
		args, _ := json.Marshal(map[string]string{"path": path})
		if got := classifyRecallA(ev, Turn{ToolCalls: []ToolCall{recallCall("read_file", string(args))}}); got != want {
			t.Errorf("%s: got %v, want %v", path, got, want)
		}
	}
}

func TestClassifyRecallB(t *testing.T) {
	mk := func(c string) RecallEval { return RecallEval{Value: recallNumVal(), Cases: []string{c}} }
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
		{"S2 continues", mk("S2"), Turn{ToolCalls: []ToolCall{recallCall("read_file", `{}`)}}, recallContinues, false, false},
		{"S2 declines", mk("S2"), Turn{Content: "I'm not sure"}, recallDeclinesOrWrong, false, true},
		{"S1 both", mk("S1"), Turn{Content: "used to be 5, now 8"}, recallAnswers, true, false},
		{"S2 answers with call", mk("S2"), Turn{Content: "It is 5", ToolCalls: []ToolCall{recallCall("read_file", `{"path":"a.go"}`)}}, recallAnswers, false, false},
		{"S2 bad call declines", mk("S2"), Turn{ToolCalls: []ToolCall{recallCall("read_file", `{`)}}, recallDeclinesOrWrong, false, false},
		{"id S2 answers", RecallEval{Value: recallIDVal(), Cases: []string{"S2"}}, Turn{Content: "retry.max_attempts"}, recallAnswers, false, false},
		{"id S2 declines", RecallEval{Value: recallIDVal(), Cases: []string{"S2"}}, Turn{Content: "exponential"}, recallDeclinesOrWrong, false, false},
		{"S1 wrong", mk("S1"), Turn{Content: "it is 5"}, recallDeclinesOrWrong, false, false},
	} {
		got, both := classifyRecallB(c.ev, c.reply)
		if got != c.want || both != c.both || recallHedged(c.reply.Content) != c.hedged {
			t.Errorf("%s: got %v/%v/%v, want %v/%v/%v", c.name, got, both, recallHedged(c.reply.Content), c.want, c.both, c.hedged)
		}
	}
}

func TestRecallHedged(t *testing.T) {
	frozen := []string{"can't confirm", "cannot confirm", "not sure", "may have changed",
		"might have changed", "unable to verify", "I would need to check"}
	if !slices.Equal(recallHedgePhrases, frozen) {
		t.Errorf("recallHedgePhrases = %q, want %q", recallHedgePhrases, frozen)
	}
	for _, p := range frozen {
		if !recallHedged("so " + p + " here") {
			t.Errorf("phrase %q not hedged", p)
		}
	}
	for in, want := range map[string]bool{"I CAN\u2019T CONFIRM": true, "can\u2019t confirm": true, "I would NEED to check": true, "sure": false} {
		if got := recallHedged(in); got != want {
			t.Errorf("recallHedged(%q) = %v", in, got)
		}
	}
}

func TestRecallSearchNumberTokenizer(t *testing.T) {
	ev := RecallEval{Value: recallNumVal(), NamedFile: "config/retry.go"}
	empty := RecallEval{Value: RecallValue{Type: "number", Identifier: "x", Lesson: "", Changed: "8"}}
	for _, c := range []struct {
		ev      RecallEval
		pattern string
		want    recallKind
	}{
		{ev, "md5", recallOtherTool}, {ev, "k8s", recallOtherTool}, {ev, "e2e", recallOtherTool},
		{ev, "http2", recallOtherTool}, {ev, "timeout = 15", recallOtherTool}, {ev, "sha256", recallOtherTool},
		{ev, "5", recallRecheck}, {ev, "=5", recallRecheck}, {ev, `\b5\b`, recallRecheck},
		{ev, `maxRetries\s*=\s*5`, recallRecheck}, {ev, "5.0", recallRecheck},
		{ev, "version 5.0.1", recallOtherTool}, {ev, "5.0.1", recallOtherTool},
		{empty, "anything", recallOtherTool},
	} {
		args, _ := json.Marshal(map[string]string{"pattern": c.pattern})
		if got := classifyRecallA(c.ev, Turn{ToolCalls: []ToolCall{recallCall("search", string(args))}}); got != c.want {
			t.Errorf("pattern %q: got %v, want %v", c.pattern, got, c.want)
		}
	}
}

func TestStatesValueVersionAndWords(t *testing.T) {
	v := recallNumVal()
	for in, want := range map[string]bool{
		"upgrade to version 5.0.1": false, "5.0.1": false, "5.0. Then": true,
		"five_x": false, "five5": false, "five x": true,
	} {
		if got := statesValue(in, v, "5"); got != want {
			t.Errorf("statesValue(%q) = %v, want %v", in, got, want)
		}
	}
	if statesValue("bump to 1.2.0", v, "1.2") {
		t.Error("version string stated a decimal")
	}
	if statesValue("version 5.0.1", v, "5.0") || statesValue("version 5.0.1", v, "5") {
		t.Error("version string stated a number")
	}
}

func TestClassifyRecallAOrderAndSpec(t *testing.T) {
	ev := RecallEval{Value: recallNumVal(), NamedFile: "config/retry.go"}
	idEv := RecallEval{Value: recallIDVal(), NamedFile: "x.go"}
	read := recallCall("read_file", `{"path":"config/retry.go"}`)
	for _, c := range []struct {
		name  string
		ev    RecallEval
		reply Turn
		want  recallKind
	}{
		{"recheck beats search", ev, Turn{ToolCalls: []ToolCall{read, recallCall("agent_memory_search", `{}`)}}, recallRecheck},
		{"search beats create", ev, Turn{ToolCalls: []ToolCall{recallCall("agent_memory_search", `{}`), recallCall("agent_memory_create", `{}`)}}, recallSecondSearch},
		{"write beats other", ev, Turn{ToolCalls: []ToolCall{recallCall("agent_memory_promote", `{}`), recallCall("glob", `{}`)}}, recallMemoryWrite},
		{"rely with two other calls", ev, Turn{Content: "5", ToolCalls: []ToolCall{recallCall("glob", `{}`), recallCall("list", `{}`)}}, recallRely},
		{"bad args is no call", ev, Turn{Content: "5", ToolCalls: []ToolCall{recallCall("read_file", `{`)}}, recallRely},
		{"glob alone", ev, Turn{ToolCalls: []ToolCall{recallCall("glob", `{}`)}}, recallOtherTool},
		{"bad args alone", ev, Turn{ToolCalls: []ToolCall{recallCall("read_file", `{`)}}, recallEmpty},
		{"bad memory search is no call", ev, Turn{ToolCalls: []ToolCall{recallCall("agent_memory_search", `{`)}}, recallEmpty},
		{"create alone", ev, Turn{ToolCalls: []ToolCall{recallCall("agent_memory_create", `{}`)}}, recallMemoryWrite},
		{"literal empty", ev, Turn{}, recallEmpty},
		{"search 5", ev, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"5"}`)}}, recallRecheck},
		{"search timeout 15", ev, Turn{Content: "It is 5", ToolCalls: []ToolCall{recallCall("search", `{"pattern":"timeout = 15"}`)}}, recallRely},
		{"search word boundary", ev, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"\\b5\\b"}`)}}, recallRecheck},
		{"search maxRetries.*5", ev, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"maxRetries.*5"}`)}}, recallRecheck},
		{"search =5", ev, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"=5"}`)}}, recallRecheck},
		{"search sha256", ev, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"sha256"}`)}}, recallOtherTool},
		{"search http 503", ev, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"http 503"}`)}}, recallOtherTool},
		{"search decimal", ev, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"0.5 and 5.5"}`)}}, recallOtherTool},
		{"search upper identifier", ev, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"MAXRETRIES"}`)}}, recallRecheck},
		{"search (?i) identifier", ev, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"(?i)MAXRETRIES"}`)}}, recallRecheck},
		{"search id lesson", idEv, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"RETRY.max_attempts"}`)}}, recallRecheck},
		{"string envelope", ev, Turn{Content: "It is 5", ToolCalls: []ToolCall{recallCall("read_file", `"{\"path\":\"config/retry.go\"}"`)}}, recallRecheck},
		{"search empty identifier", RecallEval{Value: RecallValue{Type: "number", Identifier: "", Lesson: "5", Changed: "7"}}, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"zzz"}`)}}, recallOtherTool},
		{"search empty id lesson", RecallEval{Value: RecallValue{Type: "identifier", Identifier: "mode", Lesson: "", Changed: "slow"}}, Turn{ToolCalls: []ToolCall{recallCall("search", `{"pattern":"zzz"}`)}}, recallOtherTool},
		{"one pronoun", ev, Turn{Content: "Which one do you mean?"}, recallNoValue},
		{"one pronoun lesson 2", RecallEval{Value: RecallValue{Type: "number", Identifier: "n", Lesson: "2", Changed: "1"}}, Turn{Content: "Which one?"}, recallNoValue},
	} {
		if got := classifyRecallA(c.ev, c.reply); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestStatesValueNumberEdges(t *testing.T) {
	v := recallNumVal()
	for in, want := range map[string]bool{"x_five": false, "9five": false, "no_one": false, ".5": false, "5.0": true, "5.00": true, "05": false, "five.": true, "(five)": true} {
		if got := statesValue(in, v, "5"); got != want {
			t.Errorf("statesValue(%q) = %v, want %v", in, got, want)
		}
	}
	if !statesValue("2.5s", v, "2.5") || !statesValue("2.50", v, "2.5") {
		t.Error("2.5 not stated")
	}
	if !statesValue("0", v, "0") || !statesValue("0.0", v, "0") {
		t.Error("0 not stated")
	}
	if statesValue("anything", recallIDVal(), "") {
		t.Error("empty identifier stated")
	}
}

func TestRecallSearchNumberTokenizerPerLesson(t *testing.T) {
	mk := func(lesson, changed string) RecallEval {
		return RecallEval{Value: RecallValue{Type: "number", Identifier: "zzz", Lesson: lesson, Changed: changed}, NamedFile: "f.go"}
	}
	l5, l8, l2 := mk("5", "9"), mk("8", "9"), mk("2", "9")
	for _, c := range []struct {
		ev      RecallEval
		pattern string
		want    recallKind
	}{
		{l8, "k8s", recallOtherTool}, {l8, "utf8", recallOtherTool}, {l8, "=8", recallRecheck},
		{l2, "e2e", recallOtherTool}, {l2, "http2", recallOtherTool}, {l2, "oauth2", recallOtherTool},
		{l2, `\b2\b`, recallRecheck},
		{l5, `retries\s5`, recallRecheck}, {l5, `\d5`, recallRecheck},
		{l5, `5\.5`, recallOtherTool}, {l5, `5\.0\.1`, recallOtherTool}, {l2, `2\.5`, recallOtherTool},
		{l5, `5\.0`, recallRecheck}, {l5, `\.5`, recallOtherTool},
	} {
		args, _ := json.Marshal(map[string]string{"pattern": c.pattern})
		if got := classifyRecallA(c.ev, Turn{ToolCalls: []ToolCall{recallCall("search", string(args))}}); got != c.want {
			t.Errorf("lesson %s pattern %q: got %v, want %v", c.ev.Value.Lesson, c.pattern, got, c.want)
		}
	}
}
