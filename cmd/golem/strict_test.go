package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"unicode"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/mcpclient"
	"github.com/kstruzzieri/go-llm/provider"
	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// strict_test.go pins -strict-approval (#584) in Golem: replApprover's
// one-time prompt, the /grants exposure line, the auto-edits display, the
// flag, and the gate's effect on real REPL turns.

const (
	foreignNotice   = "reusable approvals suspended: outside content entered this conversation (mcp__docs__fetch); /new or /clear starts fresh"
	unknownNotice   = "reusable approvals suspended: this conversation's provenance is unknown (history); /new or /clear starts fresh"
	suspendedEdits  = "auto-edits: on (suspended: this conversation is exposed)"
	networkLine     = "network is not confined"
	writeQuestion   = "Apply this change? [y/N] "
	mcpQuestion     = "Run this MCP tool? [y/N] "
	grantedApproval = "auto-approved (session grant)"
)

func freshCall(name string) provider.ToolCall {
	return provider.ToolCall{ID: "c1", Type: "function", Function: provider.ToolCallFunction{Name: name}}
}

// askFresh runs ApproveFresh on a scanner reading answers and returns the
// decision, the error and everything the approver printed.
func askFresh(t *testing.T, ap func(*replApprover), call provider.ToolCall, keyed bool, exposure agent.Exposure, answers string) (string, bool, error) {
	t.Helper()
	var out strings.Builder
	a := newReplApprover(newScannerSource(strings.NewReader(answers), &out), &out, false)
	if ap != nil {
		ap(a)
	}
	ok, err := a.ApproveFresh(context.Background(), call, "PREVIEW", keyed, agent.RiskReport{}, exposure)
	return out.String(), ok, err
}

func TestApproveFreshPromptOffersYesNoOnly(t *testing.T) {
	for _, tc := range []struct {
		name, question string
	}{
		{"write_file", writeQuestion},
		{"edit_file", writeQuestion},
		{"run_command", "Run this command? [y/N] "},
		{"start_command", "Start this background command? [y/N] "},
		{verifyToolName, "Run this verification command? [y/N] "},
		{"mcp__docs__fetch", mcpQuestion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, ans := range []struct {
				in   string
				want bool
			}{{"y\n", true}, {"yes\n", true}, {"a\n", false}, {"always\n", false}, {"n\n", false}} {
				grants := newApprovalGrants()
				out, ok, err := askFresh(t, func(a *replApprover) {
					a.grants = grants
					a.beforeWrite = func() error { _, werr := io.WriteString(a.out, "FLUSHED\n"); return werr }
				}, freshCall(tc.name), true, foreignExposure, ans.in)
				if err != nil || ok != ans.want {
					t.Fatalf("answer %q: ok=%v err=%v, want %v", ans.in, ok, err, ans.want)
				}
				if !strings.HasPrefix(out, "FLUSHED\n"+foreignNotice+"\n") || strings.Count(out, "FLUSHED") != 1 {
					t.Fatalf("answer %q: want beforeWrite once, then the notice:\n%s", ans.in, out)
				}
				if n, p, q := strings.Index(out, foreignNotice), strings.Index(out, "PREVIEW"), strings.Index(out, tc.question); n < 0 || p < n || q < p {
					t.Fatalf("answer %q: want notice, preview, then %q:\n%s", ans.in, tc.question, out)
				}
				if strings.Contains(out, "a=") || strings.Contains(out, grantedApproval) {
					t.Fatalf("answer %q: fresh prompt offered or used a grant:\n%s", ans.in, out)
				}
				if grants.count() != 0 {
					t.Fatalf("answer %q stored %d grants, want none", ans.in, grants.count())
				}
			}
		})
	}
}

// TestApproveFreshNeverConsultsGrants is A22: a held grant of the right scope
// and key is not consulted, so the call still asks and "n" denies it.
func TestApproveFreshNeverConsultsGrants(t *testing.T) {
	grants := newApprovalGrants()
	grants.grant(grantScopeFiles, agenttools.WriteClassApprovalKey)
	grants.grant(grantScopeExec, "exec:abc")
	grants.grant(grantScopeVerify, "verify:abc")
	for _, name := range []string{"write_file", "edit_file", "run_command", "start_command", verifyToolName} {
		out, ok, err := askFresh(t, func(a *replApprover) { a.grants = grants }, freshCall(name), true, foreignExposure, "n\n")
		if err != nil || ok {
			t.Fatalf("%s: ok=%v err=%v, want a prompted denial", name, ok, err)
		}
		if !strings.Contains(out, "? [y/N] ") || strings.Contains(out, grantedApproval) {
			t.Fatalf("%s: a held grant answered for the person:\n%s", name, out)
		}
	}
	if grants.count() != 3 {
		t.Fatalf("grants = %d, want the three held ones untouched", grants.count())
	}
}

func TestApproveFreshNoticeWording(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exposure agent.Exposure
		want     string
	}{
		{"foreign", foreignExposure, foreignNotice},
		{"foreign without a source", agent.Exposure{Recorded: true, Foreign: true}, "reusable approvals suspended: outside content entered this conversation; /new or /clear starts fresh"},
		{"foreign consult", agent.Exposure{Recorded: true, Foreign: true, First: "consult"}, "reusable approvals suspended: outside content entered this conversation (consult); /new or /clear starts fresh"},
		{"unknown history", agent.Exposure{Recorded: true, Unknown: true, First: "history"}, unknownNotice},
		{"unknown without a source", agent.Exposure{Recorded: true, Unknown: true}, unknownNotice},
		{"unknown tool", agent.Exposure{Recorded: true, Unknown: true, First: "tool:mystery"}, "reusable approvals suspended: this conversation's provenance is unknown (mystery); /new or /clear starts fresh"},
		{"not recorded", agent.Exposure{}, unknownNotice},
		// The agent reads any unrecorded value as unknown "history".
		{"not recorded with stray fields", agent.Exposure{Foreign: true, First: "tool:x"}, unknownNotice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, _ := askFresh(t, nil, freshCall("write_file"), true, tc.exposure, "n\n")
			if first, _, _ := strings.Cut(out, "\n"); first != tc.want {
				t.Fatalf("notice = %q, want %q", first, tc.want)
			}
		})
	}
}

// hostile10KB is a label an MCP server could choose: a newline, terminal
// controls, a bidi override, NUL, NEL and 10 KB of padding.
func hostile10KB() string {
	return "tool:mcp__x\n\x1b[2J\rforged\u202e line\x00\u0085" + strings.Repeat("A", 10<<10)
}

// requireOneInertLine fails unless line holds no control or format rune.
func requireOneInertLine(t *testing.T, line string) {
	t.Helper()
	for _, r := range line {
		if r != ' ' && !unicode.IsGraphic(r) {
			t.Fatalf("line keeps rune %U: %q", r, line)
		}
	}
	if len(line) > 1024 {
		t.Fatalf("line is %d bytes, want a bounded label: %q", len(line), line)
	}
}

func TestApproveFreshNoticeIsOneSanitizedLine(t *testing.T) {
	for _, exp := range []agent.Exposure{
		{Recorded: true, Foreign: true, First: hostile10KB()},
		{Recorded: true, Unknown: true, First: hostile10KB()},
	} {
		out, _, _ := askFresh(t, nil, freshCall("mcp__x__y"), false, exp, "n\n")
		first, rest, _ := strings.Cut(out, "\n")
		if !strings.HasPrefix(first, "reusable approvals suspended: ") || !strings.HasSuffix(first, "); /new or /clear starts fresh") {
			t.Fatalf("notice is not one line:\n%q", out)
		}
		if !strings.Contains(first, "(mcp__x ") || !strings.Contains(first, `\x1b[2J`) || !strings.Contains(first, "forged") {
			t.Fatalf("notice dropped the label instead of neutralizing it: %q", first)
		}
		requireOneInertLine(t, first)
		if strings.Contains(rest, "forged") || strings.Contains(rest, "AAAA") {
			t.Fatalf("label leaked past the notice line:\n%q", rest)
		}
	}
}

func TestApproveFreshCtrlCAndEOF(t *testing.T) {
	for _, tc := range []struct {
		name    string
		src     lineSource
		wantErr error
	}{
		{"interrupted", &stubAnswerSource{err: errInterrupted}, context.Canceled},
		{"eof", &stubAnswerSource{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			grants := newApprovalGrants()
			grants.grant(grantScopeFiles, agenttools.WriteClassApprovalKey)
			ap := newReplApprover(tc.src, &out, false)
			ap.grants = grants
			ok, err := ap.ApproveFresh(context.Background(), freshCall("write_file"), "PREVIEW", true, agent.RiskReport{}, foreignExposure)
			if ok || !errors.Is(err, tc.wantErr) {
				t.Fatalf("ok=%v err=%v, want denied with %v", ok, err, tc.wantErr)
			}
			if errors.Is(err, errInterrupted) {
				t.Fatalf("err = %v: the editor sentinel leaked", err)
			}
			if grants.count() != 1 {
				t.Fatalf("grants = %d, want the held one", grants.count())
			}
		})
	}

	// Through a real turn: neither answer changes the stored record or the
	// grants, and the write does not happen.
	for _, tc := range []struct {
		name string
		src  func(*strings.Builder) lineSource
	}{
		{"interrupted turn", func(out *strings.Builder) lineSource {
			return interruptingAnswers{newScannerSource(strings.NewReader("go\n"), out)}
		}},
		{"eof turn", func(out *strings.Builder) lineSource { return newScannerSource(strings.NewReader("go\n"), out) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := newStrictSession(t, &scriptCaller{responses: []agent.ModelResult{writeOut("out.txt"), answerStep("done")}})
			id := seedActive(t, sess)
			sess.grants.grant(grantScopeFiles, agenttools.WriteClassApprovalKey)
			var out strings.Builder
			if err := runREPL(t.Context(), tc.src(&out), &out, nil, sess); err != nil {
				t.Fatalf("runREPL: %v", err)
			}
			// The source prints the question, so the notice marks the prompt.
			if strings.Count(out.String(), foreignNotice) != 1 {
				t.Fatalf("want one fresh prompt:\n%s", out.String())
			}
			if exists(t, filepath.Join(sess.root, "out.txt")) {
				t.Fatal("out.txt written without a yes")
			}
			if sess.grants.count() != 1 {
				t.Fatalf("grants = %d, want the held one", sess.grants.count())
			}
			wantExposure(t, sess, id, foreignExposure)
		})
	}
}

// interruptingAnswers reads goals from its source and answers every
// approval with the editor's Ctrl-C.
type interruptingAnswers struct{ lineSource }

func (interruptingAnswers) ReadAnswer(context.Context, string) (string, bool, error) {
	return "", false, errInterrupted
}

func TestApproveFreshHostNetworkLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		host bool
		want bool
	}{
		{"run_command", true, true},
		{"start_command", true, true},
		{verifyToolName, true, true},
		{"write_file", true, false},
		{"edit_file", true, false},
		{"stop_command", true, false},
		{"mcp__x__y", true, false},
		{"run_command", false, false},
		{"start_command", false, false},
		{verifyToolName, false, false},
	} {
		out, _, _ := askFresh(t, func(a *replApprover) { a.hostCommands = tc.host }, freshCall(tc.name), true, foreignExposure, "n\n")
		if got := strings.Contains(out, networkLine); got != tc.want {
			t.Fatalf("%s host=%v: network line present=%v, want %v:\n%s", tc.name, tc.host, got, tc.want, out)
		}
		if tc.want && !strings.HasPrefix(out, foreignNotice+"\n"+networkLine+"\nPREVIEW\n") {
			t.Fatalf("%s: want the notice, the network line, then the preview:\n%s", tc.name, out)
		}
	}
	// The ordinary path never prints it.
	var out strings.Builder
	ap := newReplApprover(newScannerSource(strings.NewReader("n\n"), &out), &out, false)
	ap.hostCommands = true
	if _, err := ap.ApproveWithRisk(context.Background(), freshCall("run_command"), "PREVIEW", "exec:abc", agent.RiskReport{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), networkLine) || strings.Contains(out.String(), "suspended") {
		t.Fatalf("the ordinary prompt changed:\n%s", out.String())
	}
}

// TestStrictHostCommandSaysNetworkIsNotConfined: runOnce marks a host session's
// approver, so a command in an exposed conversation says so before it runs. A
// session on a native sandbox confines the network itself and omits the line.
// The tools are built on the host manager either way and the answer is N, so
// nothing executes and no native backend is needed.
func TestStrictHostCommandSaysNetworkIsNotConfined(t *testing.T) {
	for _, tt := range []struct {
		name     string
		sandbox  agenttools.SandboxConfig
		wantLine bool
	}{
		{"host", agenttools.SandboxConfig{}, true},
		{"seatbelt", agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeSeatbelt}, false},
		{"bwrap", agenttools.SandboxConfig{Runtime: agenttools.SandboxRuntimeBwrap}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			mgr, execTools, err := buildExecMount(root, agenttools.SandboxConfig{}, agenttools.ExecToolsOptions{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(mgr.Shutdown)
			sess := newStrictSession(t, &scriptCaller{responses: []agent.ModelResult{
				call("run_command", map[string]any{"argv": []string{"true"}}), answerStep("done"),
			}}, execTools...)
			sess.allowExec = true
			sess.sandbox = tt.sandbox
			seedActive(t, sess)
			out := strictTurn(t, sess, "N\n")
			if strings.Count(out, "Run this command? [y/N] ") != 1 || !strings.Contains(out, foreignNotice+"\n") {
				t.Fatalf("want the notice on one fresh prompt:\n%s", out)
			}
			if got := strings.Contains(out, networkLine); got != tt.wantLine {
				t.Fatalf("network line present = %v, want %v:\n%s", got, tt.wantLine, out)
			}
			if tt.wantLine && !strings.Contains(out, foreignNotice+"\n"+networkLine+"\n") {
				t.Fatalf("want the network line right after the notice:\n%s", out)
			}
		})
	}
}

func TestApproveFreshHarnessNotice(t *testing.T) {
	t.Run("keyed decides", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			keyed bool
			want  bool
		}{
			{"write_file", true, false},
			{"write_file", false, true},
			{"edit_file", true, false},
			{"edit_file", false, true},
			{"run_command", false, false},
			{"stop_command", false, false},
		} {
			out, _, _ := askFresh(t, nil, freshCall(tc.name), tc.keyed, foreignExposure, "n\n")
			if got := strings.Contains(out, harnessNoticeLine); got != tc.want {
				t.Fatalf("%s keyed=%v: harness notice=%v, want %v:\n%s", tc.name, tc.keyed, got, tc.want, out)
			}
		}
	})

	// Real plans: an ordinary target is keyed; a direct harness target, a
	// referenced target and an unverifiable spelling plan with the empty key.
	// One run per tool: three denials in a row end a run.
	for _, tool := range []string{"write_file", "edit_file"} {
		t.Run("planned targets "+tool, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{autoEdits: true})
			targets := []string{"notes.txt", "AGENTS.md", "docs/a.md"}
			for _, p := range targets {
				writeFixtureFile(t, f.Root, p, "old\n")
			}
			writeFixtureFile(t, f.Root, "CLAUDE.md", "see @docs/a.md\n")
			var calls []agent.ModelResult
			for _, p := range targets {
				calls = append(calls, fileCall(tool, p))
			}
			requireFreshHarness(t, f, calls, []bool{false, true, true})
		})
	}

	t.Run("unverifiable spelling", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("unix permission fixture")
		}
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory read permission")
		}
		tmp := t.TempDir()
		mkdirFixture(t, tmp, "repo/.git")
		root := filepath.Join(tmp, "repo", "a", "b")
		writeFixtureFile(t, root, "notes.txt", "old\n")
		// The root's spelling is fixed when the workspace is built, so the
		// folder below the repository boundary is unlistable before that.
		unlistable := filepath.Join(tmp, "repo", "a")
		mkdirFixture(t, tmp, "repo/a/outside")
		if err := os.Chmod(unlistable, 0o311); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(unlistable, 0o755) })
		f := newFixtureAt(t, root, fixtureOpts{autoEdits: true})
		requireFreshHarness(t, f, []agent.ModelResult{fileCall("write_file", "notes.txt"), fileCall("edit_file", "notes.txt")}, []bool{true, true})
	})
}

// fileCall scripts write_file of "new\n" or edit_file of "old" to "new".
func fileCall(tool, path string) agent.ModelResult {
	if tool == "edit_file" {
		return call(tool, map[string]string{"path": path, "old_string": "old", "new_string": "new"})
	}
	return call(tool, map[string]string{"path": path, "content": "new\n"})
}

// requireFreshHarness runs calls in a strict run of an exposed conversation,
// answering N to each, and checks which prompts carry the harness notice.
func requireFreshHarness(t *testing.T, f *fixture, calls []agent.ModelResult, want []bool) {
	t.Helper()
	answers := strings.Repeat("N\n", len(calls))
	caller := &scriptCaller{responses: append(calls, answerStep("done"))}
	prior := foreignExposure
	res, err := f.runRequest(t, caller, agent.Request{
		Goal: "containment", Tools: f.tools, Approver: f.approver(answers), MaxSteps: len(calls) + 1,
		StrictApproval: true, PriorExposure: &prior,
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	requireRecords(t, res, len(calls))
	prompts := strings.Split(f.Out.String(), writeQuestion)
	if len(prompts) != len(want)+1 {
		t.Fatalf("prompts = %d, want %d fresh ones:\n%s", len(prompts)-1, len(want), f.Out.String())
	}
	for i, w := range want {
		requireDenied(t, res, i)
		if !strings.Contains(prompts[i], foreignNotice) {
			t.Fatalf("prompt %d is not fresh:\n%s", i, prompts[i])
		}
		if got := strings.Contains(prompts[i], harnessNoticeLine); got != w {
			t.Fatalf("prompt %d harness notice=%v, want %v:\n%s", i, got, w, prompts[i])
		}
	}
	if f.Grants.count() != 1 {
		t.Fatalf("grants = %d, want only the held /auto-edits one", f.Grants.count())
	}
}

func TestHeadlessApproverIsNotFresh(t *testing.T) {
	// containment: STRICT-5
	if _, ok := any((*headlessApprover)(nil)).(agent.FreshApprover); ok {
		t.Fatal("headlessApprover implements agent.FreshApprover; an unattended run must be denied once exposed")
	}
	if _, ok := any((*replApprover)(nil)).(agent.FreshApprover); !ok {
		t.Fatal("replApprover does not implement agent.FreshApprover")
	}
}

// slashOutput runs one slash command and returns what it printed.
func slashOutput(t *testing.T, sess *replSession, line string) string {
	t.Helper()
	var out strings.Builder
	dispatchSlash(t.Context(), &out, sess, line)
	return out.String()
}

func TestGrantsPrintsExposureOnlyUnderStrict(t *testing.T) {
	t.Run("strict off is unchanged and never reads the record", func(t *testing.T) {
		sess := newExposureSession(t)
		seedActive(t, sess)
		sess.runtime = nil // a ThreadExposure call would panic
		if got := slashOutput(t, sess, "/grants"); got != "session grants: 0\n" {
			t.Fatalf("/grants = %q", got)
		}
		sess.allowWrite = true
		sess.grants.grant(grantScopeFiles, agenttools.WriteClassApprovalKey)
		if got := slashOutput(t, sess, "/grants"); got != "session grants: 1\nauto-edits: on\n" {
			t.Fatalf("/grants = %q", got)
		}
	})
	for _, tc := range []struct {
		name   string
		record *agent.Exposure // nil leaves the active conversation unsaved
		want   string
	}{
		{"clean", nil, "exposure: clean"},
		{"clean with history", &agent.Exposure{Recorded: true}, "exposure: clean"},
		{"foreign", &foreignExposure, "exposure: foreign (mcp__docs__fetch)"},
		{"foreign without a source", &agent.Exposure{Recorded: true, Foreign: true}, "exposure: foreign"},
		{"consult", &agent.Exposure{Recorded: true, Foreign: true, First: "consult"}, "exposure: foreign (consult)"},
		{"unknown", &agent.Exposure{Recorded: true, Unknown: true, First: "history"}, "exposure: unknown (history)"},
		{"unknown without a source", &agent.Exposure{Recorded: true, Unknown: true}, "exposure: unknown (history)"},
		{"not recorded", &agent.Exposure{}, "exposure: unknown (history)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := newExposureSession(t)
			sess.strictApproval = true
			if tc.record != nil {
				seedRecord(t, sess, *tc.record)
			}
			if got := slashOutput(t, sess, "/grants"); got != "session grants: 0\n"+tc.want+"\n" {
				t.Fatalf("/grants = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("no session reads nothing", func(t *testing.T) {
		sess := newExposureSession(t)
		sess.strictApproval = true
		sess.session, sess.runtime = nil, nil
		if got := slashOutput(t, sess, "/grants"); got != "session grants: 0\nexposure: clean (--no-session: each turn starts fresh)\n" {
			t.Fatalf("/grants = %q", got)
		}
	})
	t.Run("unreadable record", func(t *testing.T) {
		sess := newExposureSession(t)
		sess.strictApproval = true
		if err := sess.runtime.Close(); err != nil {
			t.Fatal(err)
		}
		if got := slashOutput(t, sess, "/grants"); got != "session grants: 0\nexposure: unknown (could not read the record)\n" {
			t.Fatalf("/grants = %q", got)
		}
	})
	t.Run("label uses the notice's sanitizer", func(t *testing.T) {
		sess := newExposureSession(t)
		sess.strictApproval = true
		seedRecord(t, sess, agent.Exposure{Recorded: true, Foreign: true, First: hostile10KB()})
		stored, err := sess.runtime.ThreadExposure(t.Context(), sess.session.id)
		if err != nil {
			t.Fatal(err)
		}
		got := slashOutput(t, sess, "/grants")
		line := strings.TrimSuffix(strings.TrimPrefix(got, "session grants: 0\n"), "\n")
		requireOneInertLine(t, line)
		prompt, _, _ := askFresh(t, nil, freshCall("write_file"), true, stored, "n\n")
		notice, _, _ := strings.Cut(prompt, "\n")
		label := strings.TrimSuffix(strings.TrimPrefix(notice, "reusable approvals suspended: outside content entered this conversation ("), "); /new or /clear starts fresh")
		if line != "exposure: foreign ("+label+")" || !strings.Contains(label, `\x1b`) {
			t.Fatalf("/grants line %q does not carry the notice's label %q", line, label)
		}
	})
}

// seedRecord stores the active conversation with history and record, and
// reloads the CLI's cache from it.
func seedRecord(t *testing.T, sess *replSession, record agent.Exposure) string {
	t.Helper()
	id := sess.session.id
	seedExposed(t, sess, id, record)
	if _, err := sess.session.switchTo(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAutoEditsShowsSuspensionUnderStrict is A7: all four surfaces that print
// an "on" auto-edits state say it is suspended in an exposed or unreadable
// conversation under strict approval, and nowhere else.
func TestAutoEditsShowsSuspensionUnderStrict(t *testing.T) {
	surfaces := []string{"/tools", "/auto-edits", "/auto-edits on", "/grants"}
	for _, tc := range []struct {
		name   string
		strict bool
		setup  func(t *testing.T, sess *replSession)
		want   string
	}{
		{"strict exposed", true, func(t *testing.T, sess *replSession) { seedActive(t, sess) }, suspendedEdits},
		{"strict unknown", true, func(t *testing.T, sess *replSession) { seedRecord(t, sess, agent.Exposure{}) }, suspendedEdits},
		{"strict unreadable", true, func(t *testing.T, sess *replSession) {
			if err := sess.runtime.Close(); err != nil {
				t.Fatal(err)
			}
		}, suspendedEdits},
		{"strict clean", true, func(*testing.T, *replSession) {}, "auto-edits: on"},
		{"strict no session", true, func(_ *testing.T, sess *replSession) { sess.session, sess.runtime = nil, nil }, "auto-edits: on"},
		{"strict off exposed", false, func(t *testing.T, sess *replSession) { seedActive(t, sess); sess.runtime = nil }, "auto-edits: on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, surface := range surfaces {
				sess := newExposureSession(t)
				sess.allowWrite, sess.strictApproval = true, tc.strict
				tc.setup(t, sess)
				sess.grants.grant(grantScopeFiles, agenttools.WriteClassApprovalKey)
				got := slashOutput(t, sess, surface)
				if !strings.Contains(got, tc.want+"\n") || strings.Count(got, "auto-edits:") != 1 {
					t.Fatalf("%s: output %q, want the line %q", surface, got, tc.want)
				}
			}
			// Off is never decorated.
			sess := newExposureSession(t)
			sess.allowWrite, sess.strictApproval = true, tc.strict
			tc.setup(t, sess)
			if got := slashOutput(t, sess, "/auto-edits off"); got != "auto-edits: off\n" {
				t.Fatalf("/auto-edits off = %q", got)
			}
		})
	}
}

func TestStrictApprovalFlagAcceptedInAllModes(t *testing.T) {
	errStop := errors.New("stop at session ready")
	start := func(t *testing.T, extra ...string) (string, bool, bool) {
		t.Helper()
		isolateRunEnv(t)
		configPath, root := writeRunLifecycleConfig(t)
		stdin, stdout, stderr := runTestFiles(t)
		args := append([]string{"-config", configPath, "-root", root, "-no-probe", "-no-cap-probe",
			"-no-session", "-no-memory", "-no-project-context"}, extra...)
		var sessStrict, runtimeStrict bool
		err := run(args, stdin, stdout, stderr, runHooks{afterSessionReady: func(s *replSession) error {
			sessStrict, runtimeStrict = s.strictApproval, s.runtime.EffectivePermissions().StrictApproval
			return errStop
		}})
		var perm string
		for _, line := range stderrLines(t, stderr) {
			if strings.HasPrefix(line, "permissions:") {
				perm = line
			}
		}
		if !errors.Is(err, errStop) {
			t.Fatalf("run(%q) = %v\nstderr:\n%s", extra, err, readRunTestFile(t, stderr))
		}
		return perm, sessStrict, runtimeStrict
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"repl", nil},
		{"repl trusted", []string{"-profile", "trusted"}},
		{"one-shot", []string{"-p", "hi"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off, sessOff, rtOff := start(t, tc.args...)
			on, sessOn, rtOn := start(t, append(slices.Clone(tc.args), "-strict-approval")...)
			if off == "" || strings.Contains(off, "strict-approval") || sessOff || rtOff {
				t.Fatalf("without the flag: %q session=%v runtime=%v", off, sessOff, rtOff)
			}
			if !sessOn || !rtOn {
				t.Fatalf("with the flag: session=%v runtime=%v, want both on", sessOn, rtOn)
			}
			if strings.Count(on, " strict-approval=on") != 1 || strings.Replace(on, " strict-approval=on", "", 1) != off {
				t.Fatalf("permissions line with the flag = %q, want %q plus strict-approval=on", on, off)
			}
		})
	}

	// AgentFlow modes accept it; nothing else is asserted, because they print
	// no permissions line.
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"plan", []string{"-plan", plan, "-approve-plan-edits", "-approve-plan-gates"}},
		{"goal", []string{"-goal", "plan it"}},
		{"resume", []string{"-agentflow-resume", "-plan", plan, "-approve-plan-edits", "-approve-plan-gates"}},
	} {
		t.Run("agentflow "+tc.name, func(t *testing.T) {
			isolateRunEnv(t)
			configPath, root := writeRunLifecycleConfig(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serveCompat(w, r, "agent-model", nil)
			}))
			t.Cleanup(server.Close)
			stdin, stdout, stderr := runTestFiles(t)
			args := append([]string{"-config", configPath, "-root", root, "-base-url", server.URL,
				"-no-probe", "-no-cap-probe", "-no-project-context", "-strict-approval"}, tc.args...)
			if err := run(args, stdin, stdout, stderr, runHooks{afterSessionReady: func(*replSession) error { return errStop }}); !errors.Is(err, errStop) {
				t.Fatalf("run = %v\nstderr:\n%s", err, readRunTestFile(t, stderr))
			}
		})
	}
	t.Run("agentflow status", func(t *testing.T) {
		f, err := parseFlags([]string{"-agentflow-status", "-strict-approval"})
		if err != nil || !f.strictApproval {
			t.Fatalf("parseFlags = %+v, %v", f.strictApproval, err)
		}
		if err := validateFlags(f); err != nil {
			t.Fatalf("validateFlags: %v", err)
		}
	})
}

// newStrictSession is newExposureSession with the journaled write tools and
// any extra tools mounted, and -strict-approval on in the CLI and the runtime.
func newStrictSession(t *testing.T, caller agent.ModelCaller, extra ...agent.Tool) *replSession {
	t.Helper()
	sess, _ := newStrictSessionOpts(t, caller, golemruntime.Options{}, extra...)
	return sess
}

// newStrictSessionOpts is newStrictSession over base runtime options (a
// summarizer, for C4). It returns the options the runtime was built from, so
// a test can build a second runtime over the same database (C6).
func newStrictSessionOpts(t *testing.T, caller agent.ModelCaller, base golemruntime.Options, extra ...agent.Tool) (*replSession, golemruntime.Options) {
	t.Helper()
	root := t.TempDir()
	sess := newSessionedTestSession(t, caller, root, "user:active")
	sess.root = root
	sess.grants = newApprovalGrants()
	store := openTestStore(t, root)
	writeTools, journal, _, err := buildWriteTools(root, store, testStoreGetenv(store))
	if err != nil {
		t.Fatalf("buildWriteTools: %v", err)
	}
	mounted := append(writeTools, extra...)
	sess.tools = append(sess.tools, mounted...)
	sess.journal, sess.allowWrite, sess.strictApproval = journal, true, true
	base.Profile, base.Tools, base.StrictApproval = golemruntime.ProfileTrusted, mounted, true
	installCompactRuntime(t, sess, base)
	return sess, base
}

func writeOut(path string) agent.ModelResult {
	return call("write_file", map[string]string{"path": path, "content": "x\n"})
}

// strictTurn runs one REPL goal and answers its prompts from answers.
func strictTurn(t *testing.T, sess *replSession, answers string) string {
	t.Helper()
	var out strings.Builder
	if err := runREPL(t.Context(), newScannerSource(strings.NewReader("go\n"+answers), &out), &out, nil, sess); err != nil {
		t.Fatalf("runREPL: %v", err)
	}
	return out.String()
}

// TestStrictGateEffect is A11's gate-effect half: after each lifecycle step
// that leaves an exposed conversation active, a grant re-established (or kept)
// by /auto-edits no longer writes; the same steps in a clean conversation
// write on the grant with no prompt.
func TestStrictGateEffect(t *testing.T) {
	clean := agent.Exposure{Recorded: true}
	for _, tc := range []struct {
		name   string
		record agent.Exposure // the exposed run; the control uses clean
		notice string
		steps  func(t *testing.T, sess *replSession, record agent.Exposure)
	}{
		{"failed clear", foreignExposure, foreignNotice, func(t *testing.T, sess *replSession, record agent.Exposure) {
			seedRecord(t, sess, record)
			sess.session.store = canaryDeleteFailureStore{sess.session.store}
			if got := slashOutput(t, sess, "/clear"); got != "clear failed: delete failed\n" {
				t.Fatalf("/clear = %q", got)
			}
			slashOutput(t, sess, "/auto-edits on")
		}},
		{"successful resume", foreignExposure, foreignNotice, func(t *testing.T, sess *replSession, record agent.Exposure) {
			seedExposed(t, sess, "user:other", record)
			slashOutput(t, sess, "/auto-edits on")
			if got := slashOutput(t, sess, "/resume user:other"); sess.session.id != "user:other" || sess.grants.count() != 0 {
				t.Fatalf("/resume = %q", got)
			}
			slashOutput(t, sess, "/auto-edits on")
		}},
		{"failed resume", foreignExposure, foreignNotice, func(t *testing.T, sess *replSession, record agent.Exposure) {
			id := seedRecord(t, sess, record)
			slashOutput(t, sess, "/auto-edits on")
			if got := slashOutput(t, sess, "/resume does-not-exist"); sess.session.id != id || !strings.Contains(got, "resume failed") {
				t.Fatalf("/resume = %q", got)
			}
		}},
		{"unknown conversation", agent.Exposure{}, unknownNotice, func(t *testing.T, sess *replSession, record agent.Exposure) {
			seedRecord(t, sess, record)
			slashOutput(t, sess, "/auto-edits on")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := newStrictSession(t, &scriptCaller{responses: []agent.ModelResult{writeOut("out.txt"), answerStep("done")}})
			tc.steps(t, sess, tc.record)
			if !sess.grants.granted(grantScopeFiles, agenttools.WriteClassApprovalKey) {
				t.Fatal("the steps left no /auto-edits grant")
			}
			out := strictTurn(t, sess, "N\n")
			if exists(t, filepath.Join(sess.root, "out.txt")) {
				t.Fatalf("the held grant wrote out.txt in an exposed conversation:\n%s", out)
			}
			if strings.Count(out, writeQuestion) != 1 || !strings.Contains(out, tc.notice+"\n") || strings.Contains(out, grantedApproval) {
				t.Fatalf("want one fresh prompt with %q:\n%s", tc.notice, out)
			}
		})
		t.Run(tc.name+" control", func(t *testing.T) {
			sess := newStrictSession(t, &scriptCaller{responses: []agent.ModelResult{writeOut("out.txt"), answerStep("done")}})
			tc.steps(t, sess, clean)
			out := strictTurn(t, sess, "")
			if readFile(t, filepath.Join(sess.root, "out.txt")) != "x\n" || !strings.Contains(out, grantedApproval) || strings.Contains(out, "? [y/N") {
				t.Fatalf("a clean conversation did not write on the grant:\n%s", out)
			}
		})
	}
}

// strictMCPTools serves one MCP tool, mcp__fs__act, from a loopback server and
// adapts it with the real mcpclient, as the M1 containment row does.
func strictMCPTools(t *testing.T) ([]agent.Tool, *atomic.Int32) {
	t.Helper()
	pins, err := mcpclient.NewPinStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := gomcp.NewServer(&gomcp.Implementation{Name: "containment", Version: "1"}, nil)
	var invokes atomic.Int32
	srv.AddTool(&gomcp.Tool{Name: "act", Description: "Act", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		invokes.Add(1)
		return &gomcp.CallToolResult{}, nil
	})
	httpServer := httptest.NewServer(gomcp.NewStreamableHTTPHandler(func(*http.Request) *gomcp.Server { return srv }, nil))
	t.Cleanup(httpServer.Close)
	mgr, warns, err := mcpclient.Connect(t.Context(), mcpclient.Implementation{Name: "containment", Version: "1"}, []mcpclient.Server{mcpclient.HTTPServer("fs", httpServer.URL)}, mcpclient.ConnectOptions{Pins: pins})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	if len(mgr.Tools()) != 1 || mgr.Tools()[0].Spec().Name != "mcp__fs__act" {
		t.Fatalf("want the single adapted tool mcp__fs__act; got %d tools, warnings %v", len(mgr.Tools()), warns)
	}
	return mgr.Tools(), &invokes
}

func act() agent.ModelResult { return call("mcp__fs__act", map[string]any{}) }

// TestStrictMCPOnePromptPerCall: a real MCP tool exposes the conversation, and
// the next MCP call asks once, fresh, [y/N] only.
func TestStrictMCPOnePromptPerCall(t *testing.T) {
	tools, invokes := strictMCPTools(t)
	sess := newStrictSession(t, &scriptCaller{responses: []agent.ModelResult{act(), act(), answerStep("done")}}, tools...)
	sess.mcpAttached = true
	out := strictTurn(t, sess, "y\ny\n")
	const notice = "reusable approvals suspended: outside content entered this conversation (mcp__fs__act); /new or /clear starts fresh\n"
	if strings.Count(out, mcpQuestion) != 2 || strings.Count(out, "? [y/N") != 2 || strings.Count(out, notice) != 1 {
		t.Fatalf("want two MCP prompts, the second fresh:\n%s", out)
	}
	if first, second, _ := strings.Cut(out, mcpQuestion); strings.Contains(first, notice) || !strings.Contains(second, notice) {
		t.Fatalf("the notice belongs to the second call only:\n%s", out)
	}
	if strings.Contains(out, "a=") {
		t.Fatalf("an MCP prompt offered a grant:\n%s", out)
	}
	if n := invokes.Load(); n != 2 {
		t.Fatalf("server invocations = %d, want 2", n)
	}
	wantExposure(t, sess, sess.session.id, agent.Exposure{Recorded: true, Foreign: true, First: "tool:mcp__fs__act"})
	if got := slashOutput(t, sess, "/grants"); !strings.Contains(got, "exposure: foreign (mcp__fs__act)\n") {
		t.Fatalf("/grants = %q", got)
	}
}

// TestStrictNoSessionNextTurnClean is Review Focus 4: under --no-session a
// foreign read suspends grants for the rest of its own turn only.
func TestStrictNoSessionNextTurnClean(t *testing.T) {
	tools, _ := strictMCPTools(t)
	sess := newStrictSession(t, &scriptCaller{responses: []agent.ModelResult{
		act(), writeOut("one.txt"), answerStep("done"),
		writeOut("two.txt"), answerStep("done"),
	}}, tools...)
	sess.session = nil
	sess.mcpAttached = true
	slashOutput(t, sess, "/auto-edits on")

	out := strictTurn(t, sess, "y\nN\n")
	if exists(t, filepath.Join(sess.root, "one.txt")) || strings.Count(out, writeQuestion) != 1 || !strings.Contains(out, "(mcp__fs__act); /new or /clear starts fresh\n") {
		t.Fatalf("turn 1: the write after the foreign read must ask fresh:\n%s", out)
	}
	out = strictTurn(t, sess, "")
	if readFile(t, filepath.Join(sess.root, "two.txt")) != "x\n" || !strings.Contains(out, grantedApproval) || strings.Contains(out, "? [y/N") {
		t.Fatalf("turn 2: the write must ride the grant:\n%s", out)
	}
	if got := slashOutput(t, sess, "/grants"); !strings.Contains(got, "exposure: clean (--no-session: each turn starts fresh)\n") {
		t.Fatalf("/grants = %q", got)
	}
}
