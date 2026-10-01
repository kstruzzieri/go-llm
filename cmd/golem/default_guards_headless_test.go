package main

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
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
		// The stderr text footer's tail. finalFooter prints "done · ... ·
		// risk <score>" on stderr in the machine formats too (the progress
		// renderer is format-independent) and appends "  stopped: <reason>"
		// when the run stopped early. uname scores egress "unknown" 10, so the
		// allowed-exec row has a finding a wrongly wired risk line could show.
		wantFooterTail string
	}{
		{
			name:         "allowed exec",
			calls:        []guardedCall{{id: "x1", tool: "run_command", args: `{"argv":["uname"]}`}},
			wantRequests: 2,
			wantStatus:   "completed", wantStop: "completed", wantAnswer: "final answer",
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
				// Headless approval has no preview/risk line, even for the uname call
				// whose egress finding (footer risk 10) a risk approver would render.
				if strings.Contains(out+errOut, "interceptor risk") {
					t.Errorf("a headless run printed an interceptor risk line:\nstdout:\n%s\nstderr:\n%s", out, errOut)
				}
				footer := false
				for _, line := range errLines {
					if strings.HasPrefix(line, "done · ") && strings.HasSuffix(line, tc.wantFooterTail) {
						footer = true
					}
				}
				if !footer {
					t.Errorf("stderr has no footer ending %q:\n%s", tc.wantFooterTail, errOut)
				}

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

				// The golem.result.v1 record: the last stdout line, exactly seven keys.
				events, results := splitMachineLines(t, out)
				if len(results) != 1 {
					t.Fatalf("result records = %d, want 1", len(results))
				}
				if format == "json" && len(events) != 0 {
					t.Fatalf("json mode emitted %d events", len(events))
				}
				lines := strings.Split(strings.TrimSpace(out), "\n")
				if len(lines) == 0 || lines[0] == "" {
					t.Fatalf("stdout has no lines")
				}
				// decodeResult checks key presence only; pin the exact key count and
				// every field's value and nullability. A capped run's answer is "" (a
				// string), not null (buildResult).
				rec := decodeResult(t, lines[len(lines)-1])
				if len(rec) != 7 {
					t.Fatalf("record has %d keys, want exactly 7: %s", len(rec), lines[len(lines)-1])
				}
				var status, stop, answer, model string
				for key, dst := range map[string]*string{"status": &status, "stopReason": &stop, "answer": &answer, "model": &model} {
					if err := json.Unmarshal(rec[key], dst); err != nil {
						t.Fatalf("record %s = %s, want a string: %v", key, rec[key], err)
					}
				}
				if status != tc.wantStatus || stop != tc.wantStop || answer != tc.wantAnswer || model != "test/agent-model" {
					t.Fatalf("record = status %q stop %q answer %q model %q, want %q %q %q test/agent-model",
						status, stop, answer, model, tc.wantStatus, tc.wantStop, tc.wantAnswer)
				}
				if string(rec["grounding"]) != "null" {
					t.Fatalf("grounding = %s, want null", rec["grounding"])
				}
				if tc.wantCode == "" {
					if string(rec["error"]) != "null" {
						t.Fatalf("error = %s, want null", rec["error"])
					}
				} else {
					var recErr struct{ Code string }
					if err := json.Unmarshal(rec["error"], &recErr); err != nil || recErr.Code != tc.wantCode {
						t.Fatalf("error = %s, want code %q", rec["error"], tc.wantCode)
					}
				}
				if format != "stream-json" {
					return
				}

				// stream-json: run.finished is the last event, and tool events exist
				// exactly once per invoked call and never for a blocked one.
				if len(events) == 0 {
					t.Fatal("stream-json emitted no events")
				}
				if last := events[len(events)-1]; last.Type != "run.finished" {
					t.Fatalf("terminal event = %q, want run.finished", last.Type)
				}
				started, finished := map[string]int{}, map[string]int{}
				for _, e := range events {
					var p struct {
						ToolCallID string `json:"toolCallId"`
					}
					switch e.Type {
					case "tool.started":
						if err := json.Unmarshal(e.Payload, &p); err != nil {
							t.Fatalf("tool.started payload: %v", err)
						}
						started[p.ToolCallID]++
					case "tool.finished":
						if err := json.Unmarshal(e.Payload, &p); err != nil {
							t.Fatalf("tool.finished payload: %v", err)
						}
						finished[p.ToolCallID]++
					}
				}
				invoked := 0
				for _, c := range tc.calls {
					want := 1
					if c.blocked {
						want = 0
					} else {
						invoked++
					}
					if started[c.id] != want || finished[c.id] != want {
						t.Fatalf("call %s (blocked=%v): started %d finished %d, want %d and %d", c.id, c.blocked, started[c.id], finished[c.id], want, want)
					}
				}
				if len(started) != invoked || len(finished) != invoked {
					t.Fatalf("tool events cover started=%v finished=%v, want only the %d invoked calls", started, finished, invoked)
				}
			})
		}
	}
}
