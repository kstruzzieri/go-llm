//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	golemruntime "github.com/kstruzzieri/go-llm/golem"
	"github.com/kstruzzieri/go-llm/provider"
)

// strict_evaluation_harness_test.go is the harness the strict-mode workflow
// evaluation (#581) runs on: strictRuntime's real REPL turns over golem.Runtime
// and SQLite, with strict approval on or off, one root for the write tools,
// the exec tools and the verifier, and a line source that answers approvals
// from a script and counts them. It is unix-only because the /consult fake is
// a shell script.

// The approval questions' stems, as replApprover.ask renders them
// (approver.go:155-196). A grantable prompt appends its legend after the stem.
const (
	askApply  = "Apply this change?"
	askRun    = "Run this command?"
	askVerify = "Run this verification command?"
	askStart  = "Start this background command?"
	askMCP    = "Run this MCP tool?"
)

// suspendedPrefix opens every fresh prompt's notice (suspendedNotice).
const suspendedPrefix = "reusable approvals suspended: "

// evalAdvice is the synthetic advice the fake consultant returns.
const evalAdvice = "SYNTHETIC ADVICE edit the file then run the tests"

// evalOpts chooses what newEvalRuntime builds. The zero value is a strict-off
// interactive session with the write and exec tools and no verifier.
type evalOpts struct {
	strict bool // -strict-approval, in the CLI and the runtime
	// verify is the post-write verifier's argv, run in the root; nil mounts
	// no verifier.
	verify []string
	// mcp mounts strictMCPTools' mcp__fs__act and marks the session as having
	// an MCP server attached.
	mcp bool
	// extra tools are mounted last (foreignStub for the headless row).
	extra []agent.Tool
	// allowTools installs the headless -allow-tool approver over these names,
	// which runOnce prefers to the line source. Headless mounts no verifier
	// (main.go:1641-1645), so leave verify nil with it.
	allowTools []string
}

// evalRuntime is a sessioned REPL for the workflow evaluation: strictRuntime
// generalized to a strict choice, a shared root and a verifier. act.sh sits in
// the root and appends one line to sink per run.
type evalRuntime struct {
	sess     *replSession
	model    *scriptCaller // serves the script across every turn
	sink     string
	argv     map[string]any // bash act.sh <sink>
	mcpCalls *atomic.Int32  // the MCP server's invocations; nil unless opts.mcp
}

func newEvalRuntime(t *testing.T, o evalOpts) *evalRuntime {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "act.sh"), []byte(appendScript("x")), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &evalRuntime{model: &scriptCaller{}, sink: filepath.Join(t.TempDir(), "sink.log")}
	r.argv = map[string]any{"argv": []string{"bash", "act.sh", r.sink}}

	sess := newSessionedTestSession(t, r.model, root, "user:active")
	sess.root, sess.grants = root, newApprovalGrants()
	store := openTestStore(t, root)
	writeTools, journal, _, err := buildWriteTools(root, store, testStoreGetenv(store))
	if err != nil {
		t.Fatalf("buildWriteTools: %v", err)
	}
	mgr, execTools, err := buildExecMount(root, agenttools.SandboxConfig{}, agenttools.ExecToolsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Shutdown)
	mounted := slices.Concat(writeTools, execTools)
	if o.mcp {
		tools, calls := strictMCPTools(t)
		mounted, r.mcpCalls, sess.mcpAttached = append(mounted, tools...), calls, true
	}
	mounted = append(mounted, o.extra...)

	if o.verify != nil {
		ws, err := agenttools.NewWorkspace(root)
		if err != nil {
			t.Fatal(err)
		}
		cmd, err := agenttools.NewVerifyCommand(ws, o.verify, "", 30*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		slot := newTestVerifierSlot(t, root)
		if err := slot.Set(cmd); err != nil {
			t.Fatal(err)
		}
		sess.orch = agent.New(r.model, agent.ContextManager{}, agent.WithVerifier(slot))
	}
	if o.allowTools != nil {
		set, err := newAllowToolSet(o.allowTools)
		if err != nil {
			t.Fatal(err)
		}
		sess.headlessApprover = headlessApproverFor(set)
	}

	sess.tools = append(sess.tools, mounted...)
	sess.journal, sess.allowWrite, sess.allowExec, sess.strictApproval = journal, true, true, o.strict
	installCompactRuntime(t, sess, golemruntime.Options{Profile: golemruntime.ProfileTrusted, Tools: mounted, StrictApproval: o.strict})
	r.sess = sess
	return r
}

// script sets the model's responses for every turn, in order.
func (r *evalRuntime) script(responses ...agent.ModelResult) { r.model.responses = responses }

// command scripts act.sh as a foreground command; start scripts it as a
// background start.
func (r *evalRuntime) command() agent.ModelResult { return call("run_command", r.argv) }
func (r *evalRuntime) start() agent.ModelResult   { return call("start_command", r.argv) }

// lines counts the sink's lines.
func (r *evalRuntime) lines(t *testing.T) int { return len(sinkLines(readFile(t, r.sink))) }

// consult stages accepted advice for the next turn through /consult, with
// the fake consultant answering evalAdvice. The orchestrator carries no
// interceptor chain, as in consult_test.go, so the advice is admitted as is;
// the turn that consumes it admits it as foreign content (#582).
func (r *evalRuntime) consult(t *testing.T) {
	t.Helper()
	r.sess.consultants = fakeConsultants(t, "claude", consultTranscript(evalAdvice))
	r.sess.interceptorsOn = true
	if out := slashOutput(t, r.sess, "/consult claude review the plan"); r.sess.advisory == nil {
		t.Fatalf("/consult staged nothing:\n%s", out)
	}
}

// resumedID is the conversation resume stores and switches to.
const resumedID = "user:resumed"

// resume stores a conversation with two messages and exposure, then switches
// to it with /resume. agent.Exposure{} is stored as NULL, the legacy record.
// /resume clears the session's grants (repl.go:802), so a row re-grants after
// it.
func (r *evalRuntime) resume(t *testing.T, exposure agent.Exposure) {
	t.Helper()
	seedExposed(t, r.sess, resumedID, exposure)
	if out := slashOutput(t, r.sess, "/resume "+resumedID); r.sess.session.id != resumedID {
		t.Fatalf("/resume %s: %s", resumedID, out)
	}
}

// answerScript is the evaluation's line source: it answers each approval
// question from a script, in order, and records the question. One ReadAnswer
// is one prompt. A question past the end of the script aborts the run with
// that question, so nothing is ever answered by default. runOnce reads only
// answers, so the embedded source stays nil.
type answerScript struct {
	lineSource
	out       io.Writer // the question is printed here, as scannerSource does
	answers   []string
	questions []string
}

func (s *answerScript) ReadAnswer(_ context.Context, question string) (string, bool, error) {
	_, _ = fmt.Fprint(s.out, question)
	i := len(s.questions)
	s.questions = append(s.questions, question)
	if i >= len(s.answers) {
		return "", false, fmt.Errorf("answer script exhausted at %q", question)
	}
	return s.answers[i], true, nil
}

// evalTurn is one turn's result, everything it printed and every question it
// asked.
type evalTurn struct {
	res       agent.Result
	out       string
	questions []string
}

// asked counts the questions of one kind (askApply ... askMCP).
func (e evalTurn) asked(kind string) int {
	n := 0
	for _, q := range e.questions {
		if strings.HasPrefix(q, kind) {
			n++
		}
	}
	return n
}

// notices counts suspension notices: each opens a fresh prompt.
func (e evalTurn) notices() int { return strings.Count(e.out, suspendedPrefix) }

// networkLines counts "network is not confined" lines.
func (e evalTurn) networkLines() int { return strings.Count(e.out, networkLine+"\n") }

// granted counts calls a session grant approved. They are not prompts.
func (e evalTurn) granted() int { return strings.Count(e.out, grantedApproval) }

// turnErr runs one REPL turn through runOnce, answering its questions from
// answers in order, and returns the run error.
func (r *evalRuntime) turnErr(t *testing.T, answers ...string) (evalTurn, error) {
	t.Helper()
	var out strings.Builder
	src := &answerScript{out: &out, answers: answers}
	res, err := runOnce(t.Context(), &out, nil, r.sess, "go", src)
	return evalTurn{res: res, out: out.String(), questions: src.questions}, err
}

// turn is turnErr where any run error, an exhausted script included, fails
// the test.
func (r *evalRuntime) turn(t *testing.T, answers ...string) evalTurn {
	t.Helper()
	tr, err := r.turnErr(t, answers...)
	if err != nil {
		t.Fatalf("turn: %v\n%s", err, tr.out)
	}
	return tr
}

// TestStrictEvalHarnessCleanLoop is the harness's self-check: a strict-off
// edit/test loop asks what S2 and S4 establish for the same calls. S4: a
// write answered y and its verifier answered a are two prompts and store a
// verify grant; the second write asks again, since y stores nothing. S2: a
// command answered a is one prompt and stores an exec grant. S2's repeat and
// S4's second verifier are fresh prompts there only because a foreign read
// exposed the run; in a clean loop they ride the grants. So 4 prompts and 2
// grant-covered calls. The first write edits act.sh, and both the verifier
// and the command run the edited script, so all three share one root.
func TestStrictEvalHarnessCleanLoop(t *testing.T) {
	vsink := filepath.Join(t.TempDir(), "verify.log")
	r := newEvalRuntime(t, evalOpts{verify: []string{"bash", "act.sh", vsink}})
	edit := call("write_file", map[string]string{"path": "act.sh", "content": appendScript("edited")})
	r.script(edit, r.command(), fileCall("write_file", "two.txt"), r.command(), answerStep("done"))
	tr := r.turn(t, "y", "a", "a", "y")

	wantQuestions := []string{
		"Apply this change? [y/N/a=all edits this session] ",
		"Run this verification command? [y/N/a=always this verifier] ",
		"Run this command? [y/N/a=always this command] ",
		"Apply this change? [y/N/a=all edits this session] ",
	}
	if !slices.Equal(tr.questions, wantQuestions) {
		t.Fatalf("questions = %q, want %q\n%s", tr.questions, wantQuestions, tr.out)
	}
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"apply", tr.asked(askApply), 2},
		{"verify", tr.asked(askVerify), 1},
		{"run", tr.asked(askRun), 1},
		{"start", tr.asked(askStart), 0},
		{"mcp", tr.asked(askMCP), 0},
		{"grant-covered", tr.granted(), 2},
		{"notices", tr.notices(), 0},
		{"network lines", tr.networkLines(), 0},
		{"grants held", r.sess.grants.count(), 2},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	rec := tr.res.ToolCalls
	if len(rec) != 4 || !rec[0].Invoked || rec[0].AutoApproved || !rec[1].Invoked || rec[1].AutoApproved ||
		!rec[2].Invoked || rec[2].AutoApproved || !rec[3].Invoked || !rec[3].AutoApproved {
		t.Errorf("want both writes and the first command run at their prompts and the repeat on its grant; records %+v", rec)
	}
	if got := sinkLines(readFile(t, r.sink)); !slices.Equal(got, []string{"edited", "edited"}) {
		t.Errorf("command sink = %q, want the edited act.sh run twice", got)
	}
	if got := sinkLines(readFile(t, vsink)); !slices.Equal(got, []string{"edited", "edited"}) {
		t.Errorf("verify sink = %q, want the edited act.sh verified twice", got)
	}
}

// TestStrictEvalHarnessMCPRead: the real loopback MCP tool runs once at its
// own prompt and its synthetic text reaches the conversation; under strict
// the command after it asks fresh, saying the host network is not confined.
func TestStrictEvalHarnessMCPRead(t *testing.T) {
	r := newEvalRuntime(t, evalOpts{strict: true, mcp: true})
	r.script(act(), r.command(), answerStep("done"))
	tr := r.turn(t, "y", "y")
	if n := r.mcpCalls.Load(); n != 1 {
		t.Fatalf("server invocations = %d, want 1", n)
	}
	if !slices.ContainsFunc(tr.res.Messages, func(m provider.ChatMessage) bool { return strings.Contains(m.Content, mcpReadText) }) {
		t.Fatalf("the synthetic MCP text never reached the conversation: %+v", tr.res.Messages)
	}
	if tr.asked(askMCP) != 1 || tr.asked(askRun) != 1 || len(tr.questions) != 2 || tr.notices() != 1 || tr.networkLines() != 1 || r.lines(t) != 1 {
		t.Fatalf("want the MCP prompt, then one fresh command prompt with the network line, and one run; questions %q:\n%s", tr.questions, tr.out)
	}
}

// TestStrictEvalHarnessConsult: staged advice is admitted by the turn that
// consumes it, so under strict its first write asks fresh, naming consult.
func TestStrictEvalHarnessConsult(t *testing.T) {
	r := newEvalRuntime(t, evalOpts{strict: true})
	r.consult(t)
	r.script(writeOut("out.txt"), answerStep("done"))
	tr := r.turn(t, "y")
	if r.sess.advisory != nil {
		t.Fatal("the turn did not consume the staged advice")
	}
	if tr.asked(askApply) != 1 || len(tr.questions) != 1 || tr.notices() != 1 || !strings.Contains(tr.out, "outside content entered this conversation (consult)") {
		t.Fatalf("want one fresh write prompt naming consult; questions %q:\n%s", tr.questions, tr.out)
	}
	if readFile(t, filepath.Join(r.sess.root, "out.txt")) != "x\n" {
		t.Fatal("the approved write did not land")
	}
	wantExposure(t, r.sess, r.sess.session.id, agent.Exposure{Recorded: true, Foreign: true, First: "consult"})
}

// TestStrictEvalHarnessHeadless: the real headless approver authorizes an
// -allow-tool write between two foreign reads with strict off, and is denied
// without being asked once a strict run is exposed. It asks nothing in either.
func TestStrictEvalHarnessHeadless(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			r := newEvalRuntime(t, evalOpts{strict: strict, extra: []agent.Tool{foreignStub{}}, allowTools: []string{"write_file"}})
			r.script(fetch(), writeOut("out.txt"), fetch(), answerStep("done"))
			tr := r.turn(t)
			rec := tr.res.ToolCalls
			if len(rec) != 3 || !rec[0].Invoked || !rec[2].Invoked || len(tr.questions) != 0 {
				t.Fatalf("want both reads invoked and no question; records %+v questions %q", rec, tr.questions)
			}
			written := exists(t, filepath.Join(r.sess.root, "out.txt"))
			if strict && (!deniedAtPrompt(rec[1]) || written) {
				t.Fatalf("strict: want the write denied, not run; record %+v written %v", rec[1], written)
			}
			if !strict && (!rec[1].Invoked || rec[1].AutoApproved || !written) {
				t.Fatalf("strict off: want the -allow-tool write run; record %+v written %v", rec[1], written)
			}
		})
	}
}

// TestStrictEvalHarnessResume: each of the three stored states is the active
// conversation's record after resume.
func TestStrictEvalHarnessResume(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exposure agent.Exposure
	}{
		{"legacy", agent.Exposure{}},
		{"clean", agent.Exposure{Recorded: true}},
		{"exposed", foreignExposure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newEvalRuntime(t, evalOpts{strict: true})
			r.resume(t, tc.exposure)
			wantExposure(t, r.sess, r.sess.session.id, tc.exposure)
		})
	}
}

// TestStrictEvalHarnessExhaustedScript: a question past the end of the script
// aborts the run with that question, and the call does not run.
func TestStrictEvalHarnessExhaustedScript(t *testing.T) {
	r := newEvalRuntime(t, evalOpts{})
	r.script(r.command(), answerStep("done"))
	_, err := r.turnErr(t)
	if err == nil || !strings.Contains(err.Error(), `"Run this command? [y/N/a=always this command] "`) {
		t.Fatalf("turn error = %v, want the exhausted script naming the question", err)
	}
	if r.lines(t) != 0 {
		t.Fatal("the command ran without an answer")
	}
}
