package main

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// guardedCall is one scripted model tool call and what the default guards must
// do with it. For an allowed call, result is the exact raw observation the
// model must receive ("" skips the check for output that varies by host).
type guardedCall struct {
	id, tool, args string
	blocked        bool
	result         string
}

// headlessScript answers by how many tool observations a request carries:
// request n gets call n until the calls run out, then a final answer.
func headlessScript(calls []guardedCall) func(wireRequest) []string {
	return func(req wireRequest) []string {
		if n := len(req.toolMessages()); n < len(calls) {
			c := calls[n]
			return sseToolCall(c.id, c.tool, c.args)
		}
		return sseAnswer("final answer")
	}
}

// TestDefaultGuardsHeadlessContracts (#575): with the default guards on and
// -allow-tool run_command granted, both machine formats keep the
// golem.result.v1 shape, codes and exit statuses; blocked calls emit no tool
// events while invoked calls emit exactly one started and one finished;
// notices stay on stderr; and headless approval renders no risk line even when
// the call carries an egress finding. Every credential path has a real file
// behind it holding the sentinel, so an unguarded run reads them, never hits
// the cap and puts the sentinel on the wire.
func TestDefaultGuardsHeadlessContracts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("command execution requires Unix")
	}
	allow, err := newAllowToolSet([]string{"run_command"})
	if err != nil {
		t.Fatal(err)
	}
	// headlessApprover must not be a RiskApprover: dispatch prefers
	// RiskApprover, and the headless approval path has no risk line (#575).
	if _, ok := any(newHeadlessApprover(allow)).(agent.RiskApprover); ok {
		t.Fatal("headlessApprover implements agent.RiskApprover; headless approval must render no risk line")
	}
	cases := []struct {
		name         string
		calls        []guardedCall
		wantRequests int // chat requests the run makes
		wantStatus   string
		wantStop     string
		wantAnswer   string
		wantCode     string
		wantExit     int
		// wantEvents is the literal stream-json event sequence and wantToolIDs
		// the tool call ID of each tool.started/tool.finished event in order.
		// Blocked calls appear in neither.
		wantEvents  []string
		wantToolIDs []string
		// The stderr text footer's tail. finalFooter prints "done · ... ·
		// risk <score>" on stderr in the machine formats too (the progress
		// renderer is format-independent) and appends "  stopped: <reason>"
		// when the run stopped early. expr is outside the egress quiet set and
		// scores "unknown" 10, so the allowed-exec row has a finding a wrongly
		// wired risk line could show.
		wantFooterTail string
	}{
		{
			name: "allowed exec",
			// The result proves the command ran: exit 0, "2" on stdout, empty stderr.
			calls: []guardedCall{{id: "x1", tool: "run_command", args: `{"argv":["expr","1","+","1"]}`,
				result: "exit code: 0\n--- stdout ---\n2\n\n--- stderr ---\n"}},
			wantRequests: 2,
			wantStatus:   "completed", wantStop: "completed", wantAnswer: "final answer",
			wantEvents:     []string{"run.started", "tool.started", "tool.finished", "message.delta", "run.finished"},
			wantToolIDs:    []string{"x1", "x1"},
			wantFooterTail: " · risk 10",
		},
		{
			name: "block then recover",
			calls: []guardedCall{
				{id: "b1", tool: "read_file", args: `{"path":".env"}`, blocked: true},
				{id: "ok", tool: "read_file", args: `{"path":"hello.txt"}`, result: "hi\n"},
			},
			wantRequests: 3,
			wantStatus:   "completed", wantStop: "completed", wantAnswer: "final answer",
			wantEvents:     []string{"run.started", "tool.started", "tool.finished", "message.delta", "run.finished"},
			wantToolIDs:    []string{"ok", "ok"},
			wantFooterTail: " · risk 30",
		},
		{
			name: "three blocks",
			calls: []guardedCall{
				{id: "b1", tool: "read_file", args: `{"path":".env"}`, blocked: true},
				{id: "b2", tool: "read_file", args: `{"path":".ssh/id_ed25519"}`, blocked: true},
				{id: "b3", tool: "read_file", args: `{"path":".aws/credentials"}`, blocked: true},
			},
			// The cap stops the run after the third observation: no fourth request.
			wantRequests: 3,
			wantStatus:   "error", wantStop: "tool_error_cap_reached", wantAnswer: "", wantCode: "empty_answer", wantExit: 1,
			wantEvents:     []string{"run.started", "run.finished"},
			wantFooterTail: " · risk 90  stopped: tool_error_cap_reached",
		},
	}
	for _, tc := range cases {
		for _, format := range []string{"json", "stream-json"} {
			t.Run(tc.name+"/"+format, func(t *testing.T) {
				configPath, root, requests := fenceWireHarness(t, headlessScript(tc.calls))
				writeCredentialFiles(t, root)
				writeTestFile(t, filepath.Join(root, "hello.txt"), "hi\n", 0o600)
				args := []string{"-config", configPath, "-root", root, "-p", "go", "-output-format", format,
					"-allow-tool", "run_command", "-no-probe", "-no-cap-probe", "-no-rag", "-no-project-context"}
				stdin, stdout, stderr := runTestFiles(t)
				runErr := run(args, stdin, stdout, stderr)
				out, errOut := readRunTestFile(t, stdout), readRunTestFile(t, stderr)
				if got := exitCodeFor(runErr); got != tc.wantExit {
					t.Fatalf("exit = %d (%v), want %d\nstderr:\n%s", got, runErr, tc.wantExit, errOut)
				}

				// Channels: stdout is protocol and result lines only (splitMachineLines
				// fails on any other line); the startup notice is a stderr line.
				if strings.Contains(out, "guards:") {
					t.Errorf("stdout carries the guards notice:\n%s", out)
				}
				errLines := strings.Split(errOut, "\n")
				if !slices.Contains(errLines, guardsNoticeLine) {
					t.Errorf("stderr lines = %q, want the line %q", errLines, guardsNoticeLine)
				}
				// Headless approval has no preview/risk line, even for the expr call
				// whose egress finding (footer risk 10) a risk approver would render.
				if strings.Contains(out+errOut, "interceptor risk") {
					t.Errorf("a headless run printed an interceptor risk line:\nstdout:\n%s\nstderr:\n%s", out, errOut)
				}
				assertFooterTail(t, errOut, tc.wantFooterTail)

				// Wire: the sentinel never leaves the host, the run makes exactly the
				// expected requests, and each blocked call's observation is the framed
				// block, never the file (the cap's final observation is never sent).
				if strings.Contains(out, envSentinelToken) {
					t.Errorf("stdout carries the credential sentinel:\n%s", out)
				}
				reqs := requests()
				if len(reqs) != tc.wantRequests {
					t.Errorf("provider requests = %d, want %d", len(reqs), tc.wantRequests)
				}
				for i, r := range reqs {
					for _, m := range r.Messages {
						if strings.Contains(m.Content, envSentinelToken) {
							t.Errorf("request %d carries the credential sentinel: %q", i, m.Content)
						}
					}
				}
				for i, c := range tc.calls {
					if i+1 >= len(reqs) {
						continue
					}
					tools := reqs[i+1].toolMessages()
					if len(tools) != i+1 || tools[i].ToolCallID != c.id {
						t.Fatalf("request %d tool messages = %+v, want %d ending with call %q", i+1, tools, i+1, c.id)
					}
					if c.blocked && tools[i].Content != framedToolResult(toolFrameKey(t, tools[i].Content), credentialBlocked) {
						t.Errorf("call %q observation = %q, want the framed %q", c.id, tools[i].Content, credentialBlocked)
					}
					if c.result != "" {
						assertFramedTool(t, "call "+c.id, tools[i], c.result)
					}
				}

				// The golem.result.v1 record. splitMachineLines fails on any protocol
				// line after a result, so results[0] is the last stdout line.
				events, results := splitMachineLines(t, out)
				if len(results) != 1 {
					t.Fatalf("result records = %d, want 1", len(results))
				}
				if format == "json" && len(events) != 0 {
					t.Fatalf("json mode emitted %d events", len(events))
				}
				// decodeResult already asserts all seven keys are present, the schema
				// and no protocol key. It does not pin an extra key or any value, and a
				// null would unmarshal into a string without error, so compare each
				// key's raw JSON: a capped run's answer is "" (a string), not null.
				rec := results[0]
				if len(rec) != 7 {
					t.Fatalf("record has %d keys, want exactly 7: %s", len(rec), rec)
				}
				wantRaw := map[string]string{
					"schema": `"golem.result.v1"`, "status": strconv.Quote(tc.wantStatus),
					"answer": strconv.Quote(tc.wantAnswer), "stopReason": strconv.Quote(tc.wantStop),
					"model": `"test/agent-model"`, "grounding": "null", "error": "null",
				}
				if tc.wantCode != "" {
					delete(wantRaw, "error") // the message is diagnostic text; pin the bounded code
					var recErr struct{ Code string }
					if err := json.Unmarshal(rec["error"], &recErr); err != nil || recErr.Code != tc.wantCode {
						t.Errorf("error = %s, want code %q", rec["error"], tc.wantCode)
					}
				}
				for _, key := range slices.Sorted(maps.Keys(wantRaw)) {
					if got := string(rec[key]); got != wantRaw[key] {
						t.Errorf("record %s = %s, want %s", key, got, wantRaw[key])
					}
				}
				if format != "stream-json" {
					return
				}

				// stream-json: the exact event sequence. A blocked call emits no tool
				// event of any kind, an invoked call exactly one started and one
				// finished (not an error), and run.finished is last.
				var types, ids []string
				for _, e := range events {
					types = append(types, e.Type)
					if e.Type != "tool.started" && e.Type != "tool.finished" {
						continue
					}
					var p struct {
						ToolCallID string `json:"toolCallId"`
						IsError    bool   `json:"isError"`
					}
					if err := json.Unmarshal(e.Payload, &p); err != nil {
						t.Fatalf("%s payload %s: %v", e.Type, e.Payload, err)
					}
					if p.IsError {
						t.Errorf("%s for %q reports isError", e.Type, p.ToolCallID)
					}
					ids = append(ids, p.ToolCallID)
				}
				if !slices.Equal(types, tc.wantEvents) {
					t.Errorf("event types = %v, want %v", types, tc.wantEvents)
				}
				if !slices.Equal(ids, tc.wantToolIDs) {
					t.Errorf("tool event call IDs = %v, want %v", ids, tc.wantToolIDs)
				}
			})
		}
	}
}
