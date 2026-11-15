//go:build unix

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

// strict_evaluation_test.go is the strict-mode workflow evaluation (#581,
// spec section 10). Each workflow runs on the evalRuntime harness with
// -strict-approval off and on, and asserts hard-coded counts for both arms and
// the relation the spec requires of strict on. The measured counts render
// docs/security/strict-mode-evaluation.md, which a regeneration gate keeps
// fresh; the table reports and never decides.

var updateStrictEvaluation = flag.Bool("update-strict-evaluation", false, "rewrite docs/security/strict-mode-evaluation.md from the workflow evaluation rows")

var strictEvaluationPath = filepath.Join("..", "..", "docs", "security", "strict-mode-evaluation.md")

// The prompt kinds, in the order the MCP row runs its calls (E4).
const (
	kindMCP = iota
	kindApply
	kindVerify
	kindRun
	kindStart
	numKinds
)

var (
	kindStems = [numKinds]string{askMCP, askApply, askVerify, askRun, askStart}
	kindNames = [numKinds]string{"MCP", "apply", "verify", "run", "start"}
)

// evalCounts is what one arm's measured turn did. It is comparable, so a row
// states each arm's expected counts as one value.
type evalCounts struct {
	prompts [numKinds]int // questions asked, by kind
	// calls are the consequential calls by kind: Result.ToolCalls by name,
	// and for verify the verifications run. Nothing is keyed by call ID.
	calls   [numKinds]int
	ran     int    // consequential calls that ran
	denied  int    // consequential tool calls denied
	reads   int    // foreign stub reads that ran
	offered int    // questions offering "a"
	granted int    // calls a session grant approved; not prompts
	notices int    // fresh-approval notices
	why     string // the notices' reasons, "" with none
	network int    // "network is not confined" lines
	grants  string // /grants right after /resume; resume rows only
}

func (c evalCounts) total() int { return sum(c.prompts) }

func (c evalCounts) consequential() int { return sum(c.calls) }

// duplicates counts prompts beyond one per consequential call, per kind.
func (c evalCounts) duplicates() int {
	n := 0
	for k := range c.prompts {
		n += max(0, c.prompts[k]-c.calls[k])
	}
	return n
}

func sum(a [numKinds]int) int {
	n := 0
	for _, v := range a {
		n += v
	}
	return n
}

// measure counts a measured turn that ran verified verifications.
func measure(t *testing.T, tr evalTurn, verified int) evalCounts {
	t.Helper()
	c := evalCounts{granted: tr.granted(), notices: tr.notices(), why: noticeReasons(tr.out), network: tr.networkLines()}
	for k, stem := range kindStems {
		c.prompts[k] = tr.asked(stem)
	}
	for _, q := range tr.questions {
		if strings.Contains(q, "/a=") {
			c.offered++
		}
	}
	c.calls[kindVerify], c.ran = verified, verified
	for _, rec := range tr.res.ToolCalls {
		if rec.Name == foreignStubName {
			if rec.Invoked {
				c.reads++
			}
			continue
		}
		c.calls[callKind(t, rec.Name)]++
		if rec.Invoked {
			c.ran++
		}
		if rec.Denied {
			c.denied++
		}
	}
	return c
}

func callKind(t *testing.T, name string) int {
	t.Helper()
	switch {
	case name == "write_file" || name == "edit_file":
		return kindApply
	case name == "run_command":
		return kindRun
	case name == "start_command":
		return kindStart
	case strings.HasPrefix(name, "mcp__"):
		return kindMCP
	}
	t.Fatalf("unexpected tool call %q", name)
	return 0
}

// noticeReasons returns the distinct reasons the fresh-approval notices give.
func noticeReasons(out string) string {
	var why []string
	for line := range strings.SplitSeq(out, "\n") {
		if r, ok := strings.CutPrefix(line, suspendedPrefix); ok {
			if r = strings.TrimSuffix(r, "; /new or /clear starts fresh"); !slices.Contains(why, r) {
				why = append(why, r)
			}
		}
	}
	return strings.Join(why, " | ")
}

// seedGrants runs a clean turn that answers a to each of its n prompts, so
// the session holds n grants.
func seedGrants(t *testing.T, r *evalRuntime, n int) {
	t.Helper()
	tr := r.turn(t, slices.Repeat([]string{"a"}, n)...)
	if len(tr.questions) != n || tr.notices() != 0 || r.sess.grants.count() != n {
		t.Fatalf("seed: want %d prompts answered a and %d grants; grants %d, questions %q\n%s", n, n, r.sess.grants.count(), tr.questions, tr.out)
	}
}

// verifier returns a verifier argv that runs act.sh appending to a fresh
// sink, and that sink.
func verifier(t *testing.T) ([]string, string) {
	sink := filepath.Join(t.TempDir(), "verify.log")
	return []string{"bash", "act.sh", sink}, sink
}

func ys(n int) []string { return slices.Repeat([]string{"y"}, n) }

// evalCleanLoop is row 1: a clean turn grants an edit, its verifier, a
// command and a background start; the measured turn repeats all four.
func evalCleanLoop(t *testing.T, strict bool) evalCounts {
	argv, vsink := verifier(t)
	r := newEvalRuntime(t, evalOpts{strict: strict, verify: argv})
	r.script(writeOut("src.txt"), r.command(), r.start(), answerStep("seeded"),
		fileCall("write_file", "src.txt"), r.command(), r.start(), answerStep("done"))
	seedGrants(t, r, 4)
	tr := r.turn(t)
	if readFile(t, filepath.Join(r.sess.root, "src.txt")) != "new\n" || r.lines(t) != 2 || len(r.started(t)) != 2 {
		t.Errorf("want the edit, command and start to land twice in all:\n%s", tr.out)
	}
	return measure(t, tr, len(sinkLines(readFile(t, vsink)))-1)
}

// evalMCPRead is row 2: row 1's grants, then a real loopback MCP read ahead
// of the edit, verifier, command and start.
func evalMCPRead(t *testing.T, strict bool) evalCounts {
	argv, vsink := verifier(t)
	r := newEvalRuntime(t, evalOpts{strict: strict, verify: argv, mcp: true})
	r.script(writeOut("src.txt"), r.command(), r.start(), answerStep("seeded"),
		act(), fileCall("write_file", "src.txt"), r.command(), r.start(), answerStep("done"))
	seedGrants(t, r, 4)
	answers := []string{"y"} // the MCP call is never grantable
	if strict {
		answers = ys(5)
	}
	tr := r.turn(t, answers...)
	if r.mcpCalls.Load() != 1 || readFile(t, filepath.Join(r.sess.root, "src.txt")) != "new\n" || r.lines(t) != 2 || len(r.started(t)) != 2 {
		t.Errorf("want the MCP read once and the edit, command and start to land:\n%s", tr.out)
	}
	return measure(t, tr, len(sinkLines(readFile(t, vsink)))-1)
}

// evalConsult is row 3: a clean turn grants an edit, its verifier and a
// command; /consult advice is accepted; the measured turn repeats all three.
func evalConsult(t *testing.T, strict bool) evalCounts {
	argv, vsink := verifier(t)
	r := newEvalRuntime(t, evalOpts{strict: strict, verify: argv})
	r.script(writeOut("src.txt"), r.command(), answerStep("seeded"),
		fileCall("write_file", "src.txt"), r.command(), answerStep("done"))
	seedGrants(t, r, 3)
	r.consult(t)
	var answers []string
	if strict {
		answers = ys(3)
	}
	tr := r.turn(t, answers...)
	if r.sess.advisory != nil || readFile(t, filepath.Join(r.sess.root, "src.txt")) != "new\n" || r.lines(t) != 2 {
		t.Errorf("want the advice consumed and the edit and command to land:\n%s", tr.out)
	}
	return measure(t, tr, len(sinkLines(readFile(t, vsink)))-1)
}

// evalHeadless is row 5: a -p run with -allow-tool write_file, run_command
// and start_command, each after a foreign read, so a denial is never one of
// three consecutive tool errors.
func evalHeadless(t *testing.T, strict bool) evalCounts {
	r := newEvalRuntime(t, evalOpts{strict: strict, extra: []agent.Tool{foreignStub{}}, allowTools: []string{"write_file", "run_command", "start_command"}})
	r.script(fetch(), writeOut("src.txt"), fetch(), r.command(), fetch(), r.start(), answerStep("done"))
	tr := r.turn(t)
	ran := 1
	if strict {
		ran = 0
	}
	written := exists(t, filepath.Join(r.sess.root, "src.txt"))
	if written != (ran == 1) || r.lines(t) != ran || len(r.started(t)) != ran || len(r.mgr.List()) != ran {
		t.Errorf("want each allowed call to land %d times; written %v\n%s", ran, written, tr.out)
	}
	return measure(t, tr, 0)
}

// evalResume is rows 6-8: a clean turn holds two grants, /resume switches to
// a stored conversation with the given record and clears them, and the
// measured turn edits and tests twice, answering on as the arm's script.
func evalResume(exposure agent.Exposure, on []string) func(*testing.T, bool) evalCounts {
	return func(t *testing.T, strict bool) evalCounts {
		r := newEvalRuntime(t, evalOpts{strict: strict})
		r.script(writeOut("seed.txt"), r.command(), answerStep("seeded"),
			writeOut("src.txt"), r.command(), fileCall("write_file", "src.txt"), r.command(), answerStep("done"))
		seedGrants(t, r, 2)
		r.resume(t, exposure)
		if n := len(r.sess.session.msgs); n != 2 {
			t.Fatalf("/resume loaded %d messages, want the stored 2", n)
		}
		grants := slashOutput(t, r.sess, "/grants")
		answers := []string{"a", "a"} // re-grant the edit and the command
		if strict {
			answers = on
		}
		tr := r.turn(t, answers...)
		if readFile(t, filepath.Join(r.sess.root, "src.txt")) != "new\n" || r.lines(t) != 3 {
			t.Errorf("want both edits and commands to land:\n%s", tr.out)
		}
		c := measure(t, tr, 0)
		c.grants = grants
		return c
	}
}

// freshPerCall is the exposed rows' strict-on relation: one prompt per
// consequential call of every kind (0 duplicates), none covered by a grant or
// offering a, a notice on each of the fresh ones, and a "not confined" line
// on each command, verifier and start.
func freshPerCall(on evalCounts, fresh int) bool {
	return on.prompts == on.calls && on.granted == 0 && on.offered == 0 && on.notices == fresh &&
		on.network == on.prompts[kindVerify]+on.prompts[kindRun]+on.prompts[kindStart]
}

// sameAsOff is the clean rows' relation: strict on asks and runs exactly
// what strict off does, with no notice.
func sameAsOff(off, on evalCounts) bool {
	return on.prompts == off.prompts && on.calls == off.calls && on.offered == off.offered &&
		on.granted == off.granted && on.ran == off.ran && on.notices == 0
}

// evalRow is one spec section 10 workflow. A nil run is the native row,
// measured outside this file.
type evalRow struct {
	workflow  string
	threshold string // the spec's "strict on must equal", verbatim
	run       func(t *testing.T, strict bool) evalCounts
	off, on   evalCounts
	relation  func(off, on evalCounts) bool
}

func strictEvalRows() []evalRow {
	const (
		mcp     = "outside content entered this conversation (mcp__fs__act)"
		consult = "outside content entered this conversation (consult)"
		unknown = "this conversation's provenance is unknown (history)"
		foreign = "outside content entered this conversation (mcp__docs__fetch)"
		off     = "session grants: 0\nauto-edits: off\n"
	)
	exposed := func(off, on evalCounts) bool { return on.calls == off.calls && freshPerCall(on, on.total()) }
	loop := evalCounts{calls: [numKinds]int{0, 1, 1, 1, 1}, ran: 4, granted: 4}
	resumed := evalCounts{prompts: [numKinds]int{0, 1, 0, 1, 0}, calls: [numKinds]int{0, 2, 0, 2, 0}, ran: 4, offered: 2, granted: 2, grants: off}
	resumedOn := func(why, exposure string) evalCounts {
		return evalCounts{prompts: resumed.calls, calls: resumed.calls, ran: 4, notices: 4, why: why, network: 2,
			grants: "session grants: 0\n" + exposure + "\nauto-edits: off\n"}
	}
	cleanOn := resumed
	cleanOn.grants = "session grants: 0\nexposure: clean\nauto-edits: off\n"
	return []evalRow{
		{
			workflow:  "Clean edit/test loop, grants held: edit, verifier, foreground test, background start",
			threshold: "same prompts as strict off: **0 added**",
			run:       evalCleanLoop, off: loop, on: loop, relation: sameAsOff,
		},
		{
			workflow:  "MCP read on host (real loopback MCP), then edit, verifier, foreground test, background start, grants held for the four",
			threshold: "exactly 1 prompt per consequential call, 0 duplicates; command prompts state that network is not confined",
			run:       evalMCPRead,
			off:       evalCounts{prompts: [numKinds]int{1, 0, 0, 0, 0}, calls: [numKinds]int{1, 1, 1, 1, 1}, ran: 5, granted: 4},
			on:        evalCounts{prompts: [numKinds]int{1, 1, 1, 1, 1}, calls: [numKinds]int{1, 1, 1, 1, 1}, ran: 5, notices: 4, why: mcp, network: 3},
			relation: func(off, on evalCounts) bool {
				return on.calls == off.calls && freshPerCall(on, on.total()-on.prompts[kindMCP])
			},
		},
		{
			workflow:  "`/consult` advice accepted, then edit (and its verifier) and foreground test, grants held",
			threshold: "exactly 1 prompt per consequential call, 0 duplicates",
			run:       evalConsult,
			off:       evalCounts{calls: [numKinds]int{0, 1, 1, 1, 0}, ran: 3, granted: 3},
			on:        evalCounts{prompts: [numKinds]int{0, 1, 1, 1, 0}, calls: [numKinds]int{0, 1, 1, 1, 0}, ran: 3, notices: 3, why: consult, network: 2},
			relation:  exposed,
		},
		{
			workflow:  "`/consult` advice or exposed resume, under a native sandbox with `-sandbox-network allowed`",
			threshold: "same prompts, and a command needing IP network fails; record which",
		},
		{
			workflow:  "Stub foreign tool, then `-allow-tool` write, run and start through the real headless approver, reads interleaved",
			threshold: "every consequential call denied, reads complete",
			run:       evalHeadless,
			off:       evalCounts{calls: [numKinds]int{0, 1, 0, 1, 1}, ran: 3, reads: 3},
			on:        evalCounts{calls: [numKinds]int{0, 1, 0, 1, 1}, denied: 3, reads: 3},
			relation: func(off, on evalCounts) bool {
				return on.total() == 0 && on.ran == 0 && on.denied == on.consequential() && on.reads == off.reads && off.ran == off.consequential()
			},
		},
		{
			workflow:  "Legacy resume (no record), then edit and foreground test, re-granting after the resume",
			threshold: "Unknown, behaves as exposed",
			run:       evalResume(agent.Exposure{}, ys(4)),
			off:       resumed, on: resumedOn(unknown, "exposure: unknown (history)"), relation: exposed,
		},
		{
			workflow:  "Clean resume with a record, same script as row 6",
			threshold: "same as a clean loop",
			run:       evalResume(agent.Exposure{Recorded: true}, []string{"a", "a"}),
			off:       resumed, on: cleanOn, relation: sameAsOff,
		},
		{
			workflow:  "Exposed resume with a record, same script as row 6",
			threshold: "behaves as exposed",
			run:       evalResume(foreignExposure, ys(4)),
			off:       resumed, on: resumedOn(foreign, "exposure: foreign (mcp__docs__fetch)"), relation: exposed,
		},
	}
}

// evalArms is one row's measured arms.
type evalArms struct{ off, on evalCounts }

// runStrictEvaluation runs every measured row in both arms. Each arm must
// equal its hard-coded counts, and the two must keep the row's relation.
func runStrictEvaluation(t *testing.T, rows []evalRow) []evalArms {
	got := make([]evalArms, len(rows))
	for i, row := range rows {
		if row.run == nil {
			continue
		}
		arm := func(name string, strict bool, want evalCounts, dst *evalCounts) (done bool) {
			t.Run(fmt.Sprintf("row%d/strict=%s", i+1, name), func(t *testing.T) {
				*dst, done = row.run(t, strict), true
				if *dst != want {
					t.Errorf("counts\n got %+v\nwant %+v", *dst, want)
				}
			})
			return done
		}
		offDone := arm("off", false, row.off, &got[i].off)
		onDone := arm("on", true, row.on, &got[i].on)
		if offDone && onDone && !row.relation(got[i].off, got[i].on) {
			t.Errorf("row %d: strict on breaks %q\noff %+v\n on %+v", i+1, row.threshold, got[i].off, got[i].on)
		}
	}
	return got
}

// TestStrictModeEvaluation is the spec section 10 workflow evaluation and
// its regeneration gate: the rows run twice, and both renders must equal
// each other and the committed file. A failing row renders nothing.
func TestStrictModeEvaluation(t *testing.T) {
	rows := strictEvalRows()
	var renders [2]string
	for i := range renders {
		t.Run(fmt.Sprintf("run%d", i+1), func(t *testing.T) {
			renders[i] = renderStrictEvaluation(rows, runStrictEvaluation(t, rows))
		})
	}
	if t.Failed() {
		return
	}
	if renders[0] != renders[1] {
		t.Fatal("two runs of the strict-mode evaluation render differently: the report is not deterministic")
	}
	if *updateStrictEvaluation {
		if err := os.WriteFile(strictEvaluationPath, []byte(renders[0]), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(strictEvaluationPath)
	if err != nil {
		t.Fatalf("%v; generate it with: %s", err, strictEvaluationRegen)
	}
	if renders[0] != string(want) {
		t.Errorf("%s is stale; regenerate it with: %s", strictEvaluationPath, strictEvaluationRegen)
	}
}

const strictEvaluationRegen = "go test ./cmd/golem -run '^TestStrictModeEvaluation$' -update-strict-evaluation"

// renderStrictEvaluation renders the measured rows. Nothing host-dependent
// reaches it: counts, notice reasons and /grants lines only.
func renderStrictEvaluation(rows []evalRow, got []evalArms) string {
	var b strings.Builder
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(&b, format, args...) }
	p("# Strict approval workflow evaluation (#581)\n\n")
	p("Generated by `TestStrictModeEvaluation`; do not edit by hand. Regenerate it with `%s`. A regeneration gate runs the rows twice and compares both renders with this file.\n\n", strictEvaluationRegen)
	p("A scripted model drives Golem's real REPL turns over synthetic content, once with `-strict-approval` off and once with it on. " +
		"A counting approver answers each question from a script, `a` where it is offered and `y` otherwise, and counts it; a call a session grant covers is not a prompt. " +
		"Counts are for the measured turn, after any clean turn that seeds grants. " +
		"Fresh notices, \"not confined\" lines and duplicates are strict-on counts; strict off has none in any row. " +
		"The test asserts every count; this table only reports them.\n\n")
	p("| Workflow | Strict off prompts | Strict on prompts | Fresh notices | \"Not confined\" lines | Duplicates | Outcome | Threshold |\n")
	p("|---|---|---|---|---|---|---|---|\n")
	for i, row := range rows {
		if row.run == nil {
			p("| %d. %s | measured outside this file: see [strict-mode-decision.md](strict-mode-decision.md) | - | - | - | - | - | %s |\n", i+1, row.workflow, row.threshold)
			continue
		}
		off, on := got[i].off, got[i].on
		notices := strconv.Itoa(on.notices)
		if on.why != "" {
			notices += ": " + on.why
		}
		p("| %d. %s | %s | %s | %s | %d | %d | off: %s; on: %s | %s |\n", i+1, row.workflow, promptCell(off), promptCell(on),
			notices, on.network, on.duplicates(), outcome(off), outcome(on), row.threshold)
	}
	return b.String()
}

// promptCell renders an arm's prompts: the total, by kind, how many offered
// a, and the calls a grant covered instead.
func promptCell(c evalCounts) string {
	s := strconv.Itoa(c.total())
	var kinds []string
	for k, n := range c.prompts {
		if n > 0 {
			kinds = append(kinds, fmt.Sprintf("%s %d", kindNames[k], n))
		}
	}
	if len(kinds) > 0 {
		s += fmt.Sprintf(" (%s); %d offering `a`", strings.Join(kinds, ", "), c.offered)
	}
	if c.granted > 0 {
		s += fmt.Sprintf("; %d grant-covered", c.granted)
	}
	return s
}

// outcome renders what an arm's consequential calls did, and the exposure
// /grants showed after a resume.
func outcome(c evalCounts) string {
	s := fmt.Sprintf("%d of %d ran", c.ran, c.consequential())
	if c.denied > 0 {
		s += fmt.Sprintf(", %d denied", c.denied)
	}
	if c.reads > 0 {
		s += fmt.Sprintf(", %d reads completed", c.reads)
	}
	for line := range strings.SplitSeq(c.grants, "\n") {
		if strings.HasPrefix(line, "exposure: ") {
			s += ", `" + line + "`"
		}
	}
	return s
}
