package consult

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const threadLine = `{"type":"thread.started","thread_id":"synthetic-thread"}` + "\n"
const turnLine = `{"type":"turn.started"}` + "\n"
const doneLine = `{"type":"turn.completed","usage":{"input_tokens":7,"cached_input_tokens":2,"cache_write_input_tokens":0,"output_tokens":3,"reasoning_output_tokens":1}}` + "\n"
const answerLine = `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"OK"}}` + "\n"
const reasoningLine = `{"type":"item.completed","item":{"id":"item_0","type":"reasoning","text":"synthetic summary"}}` + "\n"
const success = threadLine + turnLine + reasoningLine + answerLine + doneLine

func message(id, kind, text string) string {
	b, _ := json.Marshal(text)
	return fmt.Sprintf(`{"type":"item.completed","item":{"id":%q,"type":%q,"text":%s}}`+"\n", id, kind, b)
}
func TestCodexAdmission(t *testing.T) {
	got := inspectCodex([]byte(success))
	if got.Verdict != "admitted" || got.Answer != "OK" || got.Usage != (usage{7, 2, 0, 3, 1}) {
		t.Fatalf("literal success: %+v", got)
	}
	for _, tc := range []struct{ name, data, verdict, answer string }{
		{"last", threadLine + turnLine + message("draft", "agent_message", "draft") + message("final", "agent_message", "final") + doneLine, "admitted", "final"},
		{"optional", strings.Replace(success, `"cache_write_input_tokens":0,`, "", 1), "admitted", "OK"},
		{"missing-terminal", threadLine + turnLine + answerLine, "terminal_missing", ""},
		{"no-answer", threadLine + turnLine + doneLine, "no_answer", ""},
		{"unknown-event", threadLine + `{"type":"surprise"}` + "\n", "invalid_protocol", ""},
		{"duplicate", strings.Replace(success, `"thread_id":`, `"type":"thread.started","thread_id":`, 1), "invalid_protocol", ""},
		{"unknown-key", strings.Replace(success, `"thread_id":`, `"extra":0,"thread_id":`, 1), "invalid_protocol", ""},
		{"empty-thread", strings.Replace(success, `"synthetic-thread"`, `""`, 1), "invalid_protocol", ""},
		{"wrong-thread-type", strings.Replace(success, `"synthetic-thread"`, `1`, 1), "invalid_protocol", ""},
		{"order", turnLine + threadLine + answerLine + doneLine, "invalid_protocol", ""},
		{"repeat-thread", threadLine + success, "invalid_protocol", ""},
		{"repeat-turn", threadLine + turnLine + turnLine + answerLine + doneLine, "invalid_protocol", ""},
		{"preturn-answer", threadLine + answerLine + turnLine + doneLine, "invalid_protocol", ""},
		{"post-terminal", success + turnLine, "invalid_protocol", ""},
		{"reuse", threadLine + turnLine + answerLine + answerLine + doneLine, "invalid_protocol", ""},
		{"cross-type", strings.Replace(success, `"id":"item_1"`, `"id":"item_0"`, 1), "invalid_protocol", ""},
		{"empty-id", strings.Replace(success, `"id":"item_1"`, `"id":""`, 1), "invalid_protocol", ""},
		{"text-type", strings.Replace(success, `"text":"OK"`, `"text":3`, 1), "invalid_protocol", ""},
		{"started-agent", strings.Replace(success, `"type":"item.completed","item":{"id":"item_1"`, `"type":"item.started","item":{"id":"item_1"`, 1), "invalid_protocol", ""},
		{"blank-reasoning", strings.Replace(success, "synthetic summary", "  ", 1), "invalid_protocol", ""},
		{"unknown-item", strings.Replace(success, "reasoning", "unknown", 1), "invalid_protocol", ""},
		{"no-newline", strings.TrimSuffix(success, "\n"), "invalid_protocol", ""},
		{"trailing", success + "x", "invalid_protocol", ""},
		{"two-objects", strings.Replace(success, "}\n", "} {}\n", 1), "invalid_protocol", ""},
		{"invalid-utf8", threadLine + turnLine + message("a", "agent_message", "OK") + "\xff\n", "invalid_protocol", ""},
		{"error", threadLine + turnLine + answerLine + `{"type":"error","message":"secret"}` + "\n" + doneLine, "vendor_error", ""},
		{"failed", threadLine + turnLine + `{"type":"turn.failed","error":{"message":"secret"}}` + "\n", "vendor_error", ""},
		{"failure-extra", threadLine + turnLine + `{"type":"turn.failed","error":{"message":"secret","extra":0}}` + "\n", "invalid_protocol", ""},
		{"depth", threadLine + turnLine + `{"type":"error","message":` + strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65) + "}\n", "invalid_protocol", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := inspectCodex([]byte(tc.data))
			if r.Verdict != tc.verdict || r.Answer != tc.answer {
				t.Fatalf("got %+v want %s %q", r, tc.verdict, tc.answer)
			}
		})
	}
}
func TestCodexAnswers(t *testing.T) {
	for _, tc := range []struct{ name, text, verdict, want string }{
		{"empty", "", "no_answer", ""}, {"space", " \t\u2003\n", "no_answer", ""}, {"controls", "\x00\x01\x7f \r\n", "no_answer", ""},
		{"spaces", " OK ", "admitted", " OK "}, {"nul", "\x00OK", "admitted", "�OK"}, {"crlf", "A\r\nB", "admitted", "A\nB"}, {"replacement", "�", "admitted", "�"}, {"zero-width", "\u200b", "admitted", "\u200b"}, {"bom", "\ufeff", "admitted", "\ufeff"},
		{"max", strings.Repeat("A", 65536), "admitted", strings.Repeat("A", 65536)}, {"over", strings.Repeat("A", 65537), "invalid_protocol", ""}, {"expansion", strings.Repeat("A", 65535) + "\x00", "invalid_protocol", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := inspectCodex([]byte(threadLine + turnLine + message("a", "agent_message", tc.text) + doneLine))
			if r.Verdict != tc.verdict || r.Answer != tc.want {
				t.Fatalf("verdict %s bytes %d; want %s bytes %d", r.Verdict, len(r.Answer), tc.verdict, len(tc.want))
			}
		})
	}
}
func TestCodexUsage(t *testing.T) {
	for _, key := range []string{"input_tokens", "cached_input_tokens", "cache_write_input_tokens", "output_tokens", "reasoning_output_tokens"} {
		for _, raw := range []string{"-1", "0.5", "1e0", `"1"`, "9007199254740993", "null", "true", "9223372036854775808", "9007199254740992"} {
			t.Run(key+"/"+raw, func(t *testing.T) {
				m := map[string]any{"input_tokens": json.Number("9007199254740992"), "cached_input_tokens": json.Number("0"), "cache_write_input_tokens": json.Number("0"), "output_tokens": json.Number("9007199254740992"), "reasoning_output_tokens": json.Number("0")}
				m[key] = json.RawMessage(raw)
				b, _ := json.Marshal(m)
				r := inspectCodex([]byte(threadLine + turnLine + answerLine + `{"type":"turn.completed","usage":` + string(b) + "}\n"))
				want := "invalid_protocol"
				if raw == "9007199254740992" {
					want = "admitted"
				}
				if r.Verdict != want {
					t.Fatalf("got %s want %s", r.Verdict, want)
				}
			})
		}
	}
	for _, replacement := range []string{`"input_tokens":1`, `"output_tokens":0`, `"output_tokens":null`, `"extra":3`} {
		data := strings.Replace(success, `"input_tokens":7`, replacement, 1)
		if strings.Contains(replacement, "output_tokens") {
			data = strings.Replace(success, `"output_tokens":3`, replacement, 1)
		}
		if got := inspectCodex([]byte(data)); got.Verdict != "invalid_protocol" {
			t.Fatalf("usage accepted %s", replacement)
		}
	}
}
func TestCodexTodosAndBounds(t *testing.T) {
	start := `{"type":"item.started","item":{"id":"todo","type":"todo_list","items":[{"text":"x","completed":false}]}}` + "\n"
	update := strings.Replace(start, "item.started", "item.updated", 1)
	complete := strings.Replace(start, "item.started", "item.completed", 1)
	for _, tc := range []struct {
		seq string
		ok  bool
	}{{start + update + complete, true}, {start + complete, true}, {update, false}, {complete, false}, {start, false}, {start + start + complete, false}, {start + complete + update, false}, {strings.Replace(start, `"completed":false`, `"completed":0`, 1), false}, {strings.Replace(start, `"items":[{"text":"x","completed":false}]`, `"items":null`, 1), false}} {
		r := inspectCodex([]byte(threadLine + turnLine + tc.seq + answerLine + doneLine))
		if (r.Verdict == "admitted") != tc.ok {
			t.Fatalf("todo verdict %s for %s", r.Verdict, tc.seq)
		}
	}
	var b strings.Builder
	b.WriteString(threadLine + turnLine)
	for i := 0; i < 4092; i++ {
		b.WriteString(message(fmt.Sprint(i), "reasoning", "x"))
	}
	b.WriteString(answerLine + doneLine)
	if r := inspectCodex([]byte(b.String())); r.Verdict != "admitted" {
		t.Fatalf("4096 records %s", r.Verdict)
	}
	tooMany := strings.Replace(b.String(), doneLine, message("extra", "reasoning", "x")+doneLine, 1)
	if r := inspectCodex([]byte(tooMany)); r.Verdict != "invalid_protocol" {
		t.Fatal("4097 records accepted")
	}
	base := threadLine + turnLine + message("big", "reasoning", strings.Repeat("x", 100)) + answerLine + doneLine
	exact := strings.Replace(base, strings.Repeat("x", 100), strings.Repeat("x", 1048576-len(base)+100), 1)
	if r := inspectCodex([]byte(exact)); r.Verdict != "admitted" {
		t.Fatalf("1MiB %s", r.Verdict)
	}
	if r := inspectCodex([]byte(strings.Replace(exact, "xxx", "xxxx", 1))); r.Verdict != "invalid_protocol" {
		t.Fatal("cap+1 accepted")
	}
}
func TestCodexErrorDiagnostics(t *testing.T) {
	pre := `{"type":"item.completed","item":{"id":"pre","type":"error","message":"model rerouted: SECRET"}}` + "\n"
	in := strings.Replace(pre, `"pre"`, `"in"`, 1)
	for _, tc := range []struct {
		data    string
		pre, in int
		reroute bool
	}{{threadLine + pre, 1, 0, true}, {threadLine + turnLine + in, 0, 1, true}, {threadLine + pre + turnLine + in + doneLine, 1, 1, true}, {threadLine + strings.Replace(pre, "model rerouted: ", "Model rerouted: ", 1), 1, 0, false}} {
		r := inspectCodex([]byte(tc.data))
		if r.Verdict != "vendor_error" || r.ErrorItemsPreTurn != tc.pre || r.ErrorItemsInTurn != tc.in || r.ModelReroutedSeen != tc.reroute {
			t.Fatalf("diagnostics %+v", r)
		}
	}
	if r := inspectCodex([]byte(success + pre)); r.Verdict != "invalid_protocol" || r.ErrorItemsInTurn != 0 {
		t.Fatalf("postterminal diagnostic %+v", r)
	}
}

var actionItems = []string{
	`{"id":"a","type":"command_execution","command":"secret","aggregated_output":"secret","exit_code":null,"status":"in_progress"}`,
	`{"id":"a","type":"file_change","changes":[{"path":"secret","kind":"add"}],"status":"completed"}`,
	`{"id":"a","type":"mcp_tool_call","server":"secret","tool":"secret","arguments":{},"result":null,"error":null,"status":"failed"}`,
	`{"id":"a","type":"collab_tool_call","tool":"spawn_agent","sender_thread_id":"secret","receiver_thread_ids":[],"prompt":null,"agents_states":{},"status":"in_progress"}`,
	`{"id":"a","type":"web_search","query":"secret","action":{"type":"search","query":"secret"}}`,
}

func TestCodexActions(t *testing.T) {
	for i, item := range actionItems {
		for _, event := range []string{"item.started", "item.updated", "item.completed"} {
			t.Run(fmt.Sprint(i)+event, func(t *testing.T) {
				line := fmt.Sprintf(`{"type":%q,"item":%s}`+"\n", event, item)
				r := inspectCodex([]byte(threadLine + turnLine + answerLine + line + doneLine))
				if r.Verdict != "visible_action" || r.Answer != "" {
					t.Fatalf("visible action %+v", r)
				}
				bad := strings.Replace(line, `"id":"a"`, `"id":"a","extra":0`, 1)
				if r := inspectCodex([]byte(threadLine + turnLine + bad)); r.Verdict != "invalid_protocol" {
					t.Fatal("unknown action key accepted")
				}
			})
		}
	}
	errorLine := `{"type":"item.completed","item":{"id":"warning","type":"error","message":"secret"}}` + "\n"
	actionLine := `{"type":"item.started","item":` + actionItems[0] + "}\n"
	for _, seq := range []string{errorLine + actionLine, actionLine + errorLine} {
		if r := inspectCodex([]byte(threadLine + turnLine + seq)); r.Verdict != "visible_action" {
			t.Fatal("priority action > error")
		}
		if r := inspectCodex([]byte(threadLine + turnLine + seq + "bad\n")); r.Verdict != "invalid_protocol" {
			t.Fatal("priority malformed > action")
		}
	}
}

func TestRejectedOversizedFinalAnswer(t *testing.T) {
	for _, record := range []string{`{"type":"item.completed","item":{"id":"warn","type":"error","message":"warning"}}` + "\n", `{"type":"item.started","item":` + actionItems[0] + "}\n"} {
		r := inspectCodex([]byte(threadLine + turnLine + record + message("large", "agent_message", strings.Repeat("x", 65537)) + doneLine))
		if r.Verdict != "invalid_protocol" {
			t.Fatalf("answer limit lost priority: %s", r.Verdict)
		}
	}
}
func TestQuotedInvalidUTF8(t *testing.T) {
	data := strings.Replace(success, "OK", "bad\xff", 1)
	if inspectCodex([]byte(data)).Verdict != "invalid_protocol" {
		t.Fatal("UTF8 repaired and admitted")
	}
}
func TestRerouteExactPrefix(t *testing.T) {
	line := `{"type":"item.completed","item":{"id":"warn","type":"error","message":"model rerouted:WITHOUT SPACE"}}` + "\n"
	if inspectCodex([]byte(threadLine + line)).ModelReroutedSeen {
		t.Fatal("nonliteral prefix matched")
	}
}

func TestActionFieldTypes(t *testing.T) {
	for i, fixture := range actionItems {
		var original map[string]any
		_ = json.Unmarshal([]byte(fixture), &original)
		for key := range original {
			if key == "id" || key == "type" || key == "arguments" {
				continue
			}
			for _, bad := range []any{nil, 17.5, true, []any{17}} {
				m := map[string]any{}
				for k, v := range original {
					m[k] = v
				}
				m[key] = bad
				if bad == nil && (key == "exit_code" || key == "result" || key == "error" || key == "prompt") {
					continue
				}
				b, _ := json.Marshal(m)
				r := inspectCodex([]byte(threadLine + turnLine + `{"type":"item.completed","item":` + string(b) + "}\n"))
				if r.Verdict != "invalid_protocol" {
					t.Fatalf("action %d field %s value %v: %s", i, key, bad, r.Verdict)
				}
			}
		}
	}
}

func TestNoNewItemAfterTerminal(t *testing.T) {
	if r := inspectCodex([]byte(success + message("late", "agent_message", "later"))); r.Verdict != "invalid_protocol" {
		t.Fatalf("postterminal item %s", r.Verdict)
	}
}

func TestCodexDepthBoundary(t *testing.T) {
	for _, tc := range []struct {
		arrays int
		want   string
	}{{62, "visible_action"}, {63, "invalid_protocol"}} {
		item := strings.Replace(actionItems[2], `"arguments":{}`, `"arguments":`+strings.Repeat("[", tc.arrays)+"0"+strings.Repeat("]", tc.arrays), 1)
		data := threadLine + turnLine + `{"type":"item.completed","item":` + item + "}\n"
		if got := inspectCodex([]byte(data)); got.Verdict != tc.want {
			t.Fatalf("depth %d = %s, want %s", tc.arrays+2, got.Verdict, tc.want)
		}
	}
}

func TestCodexRequiredUsage(t *testing.T) {
	for _, field := range []string{"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_output_tokens"} {
		t.Run(field, func(t *testing.T) {
			counters := map[string]int{"input_tokens": 7, "cached_input_tokens": 2, "cache_write_input_tokens": 0, "output_tokens": 3, "reasoning_output_tokens": 1}
			delete(counters, field)
			b, err := json.Marshal(counters)
			if err != nil {
				t.Fatal(err)
			}
			data := threadLine + turnLine + answerLine + `{"type":"turn.completed","usage":` + string(b) + "}\n"
			if got := inspectCodex([]byte(data)); got.Verdict != "invalid_protocol" || got.Answer != "" {
				t.Fatalf("missing %s = %+v, want invalid_protocol", field, got)
			}
		})
	}
}
