package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/agentflow"
	"github.com/kstruzzieri/go-llm/provider"
)

// errAgentflowTaskFailed signals a task-mode run that already reported its
// failure on stderr; main exits non-zero without printing a second message.
var errAgentflowTaskFailed = errors.New("agentflow task failed")

// afClient is the driver's view of agentflow (satisfied by *agentflow.Client).
type afClient interface {
	Probe(context.Context) error
	ProbeReview(context.Context) error
	Init(context.Context) error
	LockPlan(context.Context, string) error
	InitExecution(context.Context) error
	Doctor(context.Context) error
	NextStep(context.Context) (string, error)
	ClaimStep(context.Context, string) (string, error)
	RecordReview(context.Context, string) (agentflow.ReviewRun, error)
	AmendStep(context.Context, string, []string) (string, error)
	RunGate(context.Context, string, string, string, []string) error
	FinishStep(context.Context, string, string) error
	FinishRun(context.Context) (string, error)
	RecordFileChange(context.Context, string, string, string) error
	RecordEvidence(context.Context, agentflow.EvidenceEntry) error
	NextAction(context.Context) (agentflow.NextActionState, error)
}

// runStepFunc runs the agent loop for one claimed step. Injected so the state
// machine is testable without a model; production wiring (runAgentflowTask)
// builds the step-scoped Request and calls sess.orch.Run.
type runStepFunc func(ctx context.Context, step agentflow.Step, attempt, goal string) error

type driver struct {
	af             afClient
	plan           *agentflow.Plan
	planPath       string
	reviewManifest string
	evidence       []agentflow.EvidenceEntry
	runStep        runStepFunc
	out            io.Writer
}

// run drives the P0 sequence and returns the proof-pack path on success.
func (d *driver) run(ctx context.Context) (string, error) {
	if err := d.af.Probe(ctx); err != nil {
		return "", fmt.Errorf("agentflow unavailable: %w", err)
	}
	if d.reviewManifest != "" {
		if err := d.af.ProbeReview(ctx); err != nil {
			return "", fmt.Errorf("agentflow review unavailable: %w", err)
		}
	}
	if err := d.af.Init(ctx); err != nil {
		return "", err
	}
	for _, evidence := range d.evidence {
		if err := d.af.RecordEvidence(ctx, evidence); err != nil {
			return "", fmt.Errorf("record evidence %s: %w", evidence.ID, err)
		}
	}
	if err := d.af.LockPlan(ctx, d.planPath); err != nil {
		return "", err // *CommandError carries the structured validation errors
	}
	if err := d.af.InitExecution(ctx); err != nil {
		return "", err
	}
	if err := d.af.Doctor(ctx); err != nil {
		return "", err
	}
	for {
		id, err := d.af.NextStep(ctx)
		if err != nil {
			return "", err
		}
		if id == "" {
			break // no eligible step remains
		}
		if err := d.runOneStep(ctx, id); err != nil {
			return "", err
		}
	}
	if d.reviewManifest != "" {
		if err := d.runReviewAmendments(ctx); err != nil {
			return "", err
		}
	}
	return d.af.FinishRun(ctx)
}

func (d *driver) runOneStep(ctx context.Context, id string) error {
	step, ok := findStep(d.plan, id)
	if !ok {
		return fmt.Errorf("agentflow returned unknown step %q", id)
	}
	attempt, err := d.af.ClaimStep(ctx, id)
	if err != nil {
		return err
	}
	goal, err := stepGoal(d.plan, step)
	if err != nil {
		return err
	}
	return d.runAttempt(ctx, step, attempt, goal)
}

func (d *driver) runAttempt(ctx context.Context, step agentflow.Step, attempt, goal string) error {
	if err := d.runStep(ctx, step, attempt, goal); err != nil {
		return err // includes a fatal record-file-change failure surfaced via ctx cancel
	}
	gates, err := agentflow.ExtractCommandGates(step)
	if err != nil {
		return err
	}
	for _, g := range gates {
		if err := d.af.RunGate(ctx, step.ID, attempt, g.Label, g.Argv); err != nil {
			return fmt.Errorf("gate %q failed: %w", g.Label, err)
		}
	}
	return d.af.FinishStep(ctx, step.ID, attempt)
}

type reviewAmendment struct {
	step     agentflow.Step
	findings []agentflow.ReviewFinding
}

var reviewRunIDPattern = regexp.MustCompile(`^RR-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)

func (d *driver) runReviewAmendments(ctx context.Context) error {
	run, err := d.af.RecordReview(ctx, d.reviewManifest)
	if err != nil {
		return fmt.Errorf("record review: %w", err)
	}
	amendments, err := reviewAmendments(d.plan, run)
	if err != nil {
		return fmt.Errorf("invalid Agentflow review projection: %w", err)
	}
	if d.out != nil {
		blocking := "none"
		if len(run.ActiveBlocking) > 0 {
			blocking = strings.Join(run.ActiveBlocking, ", ")
		}
		_, _ = fmt.Fprintf(d.out, "review %s gate=%s amendment_ready=%t active_blocking=%s\n",
			run.ReviewRunID, run.GateStatus, run.AmendmentReady, blocking)
		for _, finding := range run.Findings.Index {
			mode := "display-only"
			if run.AmendmentReady && activeReviewFinding(finding.Status) {
				mode = "queued"
			}
			_, _ = fmt.Fprintf(d.out, "review finding %s#%s status=%s amendment=%s\n",
				run.ReviewRunID, finding.FindingID, finding.Status, mode)
		}
	}
	for _, amendment := range amendments {
		refs := make([]string, len(amendment.findings))
		for i, finding := range amendment.findings {
			refs[i] = run.ReviewRunID + "#" + finding.FindingID
		}
		goal, err := amendmentGoal(d.plan, amendment.step, run.ReviewRunID, amendment.findings)
		if err != nil {
			return err
		}
		attempt, err := d.af.AmendStep(ctx, amendment.step.ID, refs)
		if err != nil {
			return err
		}
		if err := d.runAttempt(ctx, amendment.step, attempt, goal); err != nil {
			return err
		}
	}
	return nil
}

func reviewAmendments(plan *agentflow.Plan, run agentflow.ReviewRun) ([]reviewAmendment, error) {
	if !reviewRunIDPattern.MatchString(run.ReviewRunID) {
		return nil, fmt.Errorf("invalid review_run_id %q", run.ReviewRunID)
	}
	byStep := make(map[string][]agentflow.ReviewFinding)
	seen := make(map[string]bool)
	for _, finding := range run.Findings.Index {
		if strings.TrimSpace(finding.FindingID) == "" {
			return nil, errors.New("finding_id must be non-empty")
		}
		if run.AmendmentReady {
			if seen[finding.FindingID] {
				return nil, fmt.Errorf("duplicate finding_id %q", finding.FindingID)
			}
			seen[finding.FindingID] = true
		}
		// Display-only findings (legacy amendment_ready=false runs, or inactive
		// statuses) are surfaced but never amended, and their severity and
		// location are never consumed. Tolerate unrecognized vocabulary there so
		// one forward-compat finding cannot abort a fully completed run; only
		// findings we are about to turn into work are strictly validated.
		if !run.AmendmentReady || !activeReviewFinding(finding.Status) {
			continue
		}
		if !validReviewSeverity(finding.Severity) {
			return nil, fmt.Errorf("finding %s has invalid severity %q", finding.FindingID, finding.Severity)
		}
		if finding.Location != nil {
			loc := finding.Location
			if strings.TrimSpace(loc.Path) == "" || loc.Line < 0 || loc.LineEnd < 0 ||
				(loc.LineEnd > 0 && loc.Line == 0) || (loc.LineEnd > 0 && loc.LineEnd < loc.Line) {
				return nil, fmt.Errorf("finding %s has invalid location", finding.FindingID)
			}
		}
		if strings.TrimSpace(finding.OwningStep) == "" || strings.TrimSpace(finding.Claim) == "" || strings.TrimSpace(finding.SuggestedFix) == "" {
			return nil, fmt.Errorf("finding %s requires owning_step, claim, and suggested_fix", finding.FindingID)
		}
		if _, ok := findStep(plan, finding.OwningStep); !ok {
			return nil, fmt.Errorf("finding %s has unknown owning_step %q", finding.FindingID, finding.OwningStep)
		}
		byStep[finding.OwningStep] = append(byStep[finding.OwningStep], finding)
	}

	var amendments []reviewAmendment
	for _, step := range plan.Steps {
		if findings := byStep[step.ID]; len(findings) > 0 {
			amendments = append(amendments, reviewAmendment{step: step, findings: findings})
		}
	}
	return amendments, nil
}

// activeReviewFinding reports whether a finding is amendment work. Agentflow's
// other statuses (fixed, rejected, superseded) are inactive and stay
// display-only; any status outside "open"/"accepted" degrades to display-only
// rather than being rejected. The severity/status vocabulary this adapter is
// coupled to is pinned by agentflow/review_contract_test.go (AGENTFLOW_SRC).
func activeReviewFinding(status string) bool { return status == "open" || status == "accepted" }

func validReviewSeverity(severity string) bool {
	return severity == "critical" || severity == "high" || severity == "medium" || severity == "low"
}

func amendmentGoal(plan *agentflow.Plan, step agentflow.Step, reviewRunID string, findings []agentflow.ReviewFinding) (string, error) {
	base, err := stepGoal(plan, step)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\nReview amendment context (authoritative Agentflow projection):")
	for _, finding := range findings {
		fmt.Fprintf(&b, "\n- Reference: %s#%s\n  Claim: %s", reviewRunID, finding.FindingID, finding.Claim)
		if finding.Location != nil {
			fmt.Fprintf(&b, "\n  Location: %s", formatReviewLocation(*finding.Location))
		}
		fmt.Fprintf(&b, "\n  Suggested fix: %s", finding.SuggestedFix)
	}
	return b.String(), nil
}

func formatReviewLocation(location agentflow.ReviewLocation) string {
	if location.Line == 0 {
		return location.Path
	}
	if location.LineEnd > 0 {
		return fmt.Sprintf("%s:%d-%d", location.Path, location.Line, location.LineEnd)
	}
	return fmt.Sprintf("%s:%d", location.Path, location.Line)
}

func findStep(p *agentflow.Plan, id string) (agentflow.Step, bool) {
	for _, s := range p.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return agentflow.Step{}, false
}

// runAgentflowTask assembles the real pieces (plan, evidence, agentflow client,
// composite journal, step-scoped tools) and drives the P0 sequence. Task mode is
// always headless: both approval classes must be opted in up front.
func runAgentflowTask(ctx context.Context, stdout, stderr io.Writer, interrupts <-chan struct{}, sess *replSession, f flags, root string) error {
	// 1. Read + parse the plan document, preflight P0 gates.
	planPath, err := resolveTaskPlanPath(f.planPath)
	if err != nil {
		return fmt.Errorf("resolve plan: %w", err)
	}
	planBytes, err := os.ReadFile(planPath)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	var plan agentflow.Plan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		return fmt.Errorf("parse plan: %w", err)
	}
	if err := agentflow.PreflightP0(&plan); err != nil {
		return err
	}
	if err := validateTraceability(plan); err != nil {
		return err
	}
	evidence, err := readEvidenceSidecar(f.evidencePath)
	if err != nil {
		return err
	}

	// Resolve the optional review manifest against the process cwd (like the
	// plan) and preflight it, so a relative path never resolves against
	// agentflow's --root and a bad path fails before the step loop runs.
	reviewManifest, err := resolveReviewManifest(f.reviewManifest)
	if err != nil {
		return err
	}

	// 2. Approval-class preflight: headless task mode needs both opt-ins before
	// any AgentFlow call. ponytail: task mode has no TTY approver, so it is always
	// headless; both classes are hard-required.
	if !f.approveEdits {
		return errors.New("task mode needs -approve-plan-edits to run step edits headlessly")
	}
	if !f.approveGates {
		return errors.New("task mode needs -approve-plan-gates to run validation gates headlessly")
	}

	// 3. Build the agentflow client (binary or python-src).
	var runner agentflow.Runner
	if f.agentflowSrc != "" {
		runner = agentflow.NewSrcExecRunner(root, f.agentflowSrc)
	} else {
		runner = agentflow.NewExecRunner(root)
	}
	client := agentflow.NewClient(runner, root)

	// 4. Run context we can cancel on a fatal record failure.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 5. Workspace + composite journal (undo + agentflow, fatal-on-failure).
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return err
	}
	undo := newMutationJournal(ws)
	afJournal := newAgentflowJournal(client.RecordFileChange, cancel)
	journal := compositeJournal{sinks: []agenttools.Journal{undo, afJournal}}

	// 6. runStep: set scope guard + step, build a step-scoped Request, run the loop.
	runStep := func(sctx context.Context, step agentflow.Step, attempt, goal string) error {
		stepCtx, stepCancel := context.WithCancel(sctx)
		defer stepCancel()
		done := make(chan struct{})
		defer close(done)
		if interrupts != nil {
			// Drain a stale interrupt buffered between steps so it does not
			// spuriously cancel this one (mirrors runOnce in repl.go).
			select {
			case <-interrupts:
			default:
			}
			go func() {
				select {
				case <-interrupts:
					stepCancel()
				case <-done:
				}
			}()
		}

		ws.SetScopeGuard(stepScopeGuard(&plan, step.ID))
		defer ws.SetScopeGuard(nil)
		afJournal.setStep(step.ID, attempt)

		readTools := agenttools.NewFileToolsForWorkspace(ws)
		writeTools := agenttools.NewMutatingTools(ws, journal) // no run_command in proof mode
		tools := append(readTools, writeTools...)

		approver := taskApprover(f.approveEdits) // auto-approve scoped edits only
		req := agent.Request{
			Goal:     goal,
			System:   sess.baseSystem,
			Tools:    tools,
			MaxSteps: sess.maxSteps,
			Budget:   sess.budget,
			Approver: approver,
			Options:  sess.modelOptions,
		}
		_, runErr := sess.orch.Run(stepCtx, req, agent.Observer(newRenderer(stderr, false, sess.maxSteps, sess.clock)))
		if fe := afJournal.fatalErr(); fe != nil {
			return fmt.Errorf("unreceipted edit aborted the run: %w", fe)
		}
		return runErr
	}

	// 7. Drive.
	d := &driver{
		af: client, plan: &plan, planPath: planPath, reviewManifest: reviewManifest,
		evidence: evidence, runStep: runStep, out: stdout,
	}
	proof, err := d.run(runCtx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentflow task failed: %v\n", err)
		reportAgentflowRecovery(ctx, stderr, client)
		return errAgentflowTaskFailed
	}
	_, _ = fmt.Fprintf(stdout, "proof pack: %s\n", proof)
	return nil
}

func validateTraceability(plan agentflow.Plan) error {
	ds := agentflow.TraceabilityDiagnostics(plan)
	if len(ds) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("invalid locked plan traceability")
	for _, diag := range ds {
		fmt.Fprintf(&b, "; [%s] %s", diag.Code, diag.Message)
	}
	return errors.New(b.String())
}

func resolveTaskPlanPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Abs(path)
}

// resolveReviewManifest absolutizes an optional review manifest against the
// process cwd and confirms it exists. Returning early on "" keeps task mode
// review-free by default. Absolutizing here is load-bearing: the agentflow
// runner's Cmd.Dir is the workspace root, so a relative --manifest would
// otherwise resolve against -root rather than the caller's cwd.
func resolveReviewManifest(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve review manifest: %w", err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("review manifest: %w", err)
	}
	return abs, nil
}

func stepGoal(p *agentflow.Plan, s agentflow.Step) (string, error) {
	if len(p.Requirements) == 0 {
		return fmt.Sprintf("Make exactly the change described. Action: %s\nTarget files: %s\nExpected diff:\n%s",
			s.Action, strings.Join(s.Files, ", "), strings.Join(s.ExpectedDiff, "\n")), nil
	}

	var b strings.Builder
	b.WriteString("Locked specification slice\n\nObjective:\n")
	b.WriteString(p.Objective)
	b.WriteString("\n\nInvariants:\n")
	writeStepGoalList(&b, p.Invariants)
	b.WriteString("\n\nNon-goals:\n")
	writeStepGoalList(&b, p.NonGoals)
	fmt.Fprintf(&b, "\n\nStep %s\nPreconditions:\n", s.ID)
	writeStepGoalList(&b, s.Preconditions)
	b.WriteString("\nAction:\n")
	b.WriteString(s.Action)
	b.WriteString("\nTarget files:\n")
	writeStepGoalList(&b, s.Files)
	b.WriteString("\nExpected diff:\n")
	writeStepGoalList(&b, s.ExpectedDiff)
	gates, err := agentflow.ExtractCommandGates(s)
	if err != nil {
		return "", err
	}
	labels := make([]string, len(gates))
	for i, gate := range gates {
		labels[i] = gate.Label
	}
	b.WriteString("\nValidation intent:\n")
	writeStepGoalList(&b, labels)
	b.WriteString("\nStructured gates:\n")
	if len(gates) == 0 {
		b.WriteString("(none)")
	}
	for i, gate := range gates {
		if i > 0 {
			b.WriteByte('\n')
		}
		argv := make([]string, len(gate.Argv))
		for j, arg := range gate.Argv {
			argv[j] = strconv.Quote(arg)
		}
		fmt.Fprintf(&b, "- %s: [%s]", gate.Label, strings.Join(argv, ", "))
		if len(gate.CriterionIDs) > 0 {
			fmt.Fprintf(&b, " (criteria: %s)", strings.Join(gate.CriterionIDs, ", "))
		}
	}

	b.WriteString("\nRequirements and acceptance criteria:\n")
	selected := make(map[string]bool, len(s.CriterionIDs))
	for _, id := range s.CriterionIDs {
		selected[id] = true
	}
	emitted := false
	for _, requirement := range p.Requirements {
		var criteria []agentflow.Criterion
		for _, criterion := range requirement.AcceptanceCriteria {
			if selected[criterion.ID] {
				criteria = append(criteria, criterion)
			}
		}
		if len(criteria) == 0 {
			continue
		}
		if emitted {
			b.WriteByte('\n')
		}
		emitted = true
		fmt.Fprintf(&b, "- %s: %s", requirement.ID, requirement.Text)
		for _, criterion := range criteria {
			fmt.Fprintf(&b, "\n  - %s: %s", criterion.ID, criterion.Text)
			if criterion.Review != nil {
				fmt.Fprintf(&b, " (review minimum depth: %s)", criterion.Review.MinimumDepth)
			}
		}
	}
	if !emitted {
		b.WriteString("(none)")
	}
	return b.String(), nil
}

func writeStepGoalList(b *strings.Builder, values []string) {
	if len(values) == 0 {
		b.WriteString("(none)")
		return
	}
	for i, value := range values {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("- ")
		b.WriteString(value)
	}
}

// reportAgentflowRecovery prints the authoritative AgentFlow recovery state after
// a failed run. next-action is advisory only: its command is printed, never
// executed, so proof state stays adapter-driven.
func reportAgentflowRecovery(ctx context.Context, out io.Writer, client *agentflow.Client) {
	if st, err := client.NextAction(ctx); err == nil {
		_, _ = fmt.Fprintf(out, "agentflow next-action: %s", st.State)
		if st.Reason != "" {
			_, _ = fmt.Fprintf(out, " (%s)", st.Reason)
		}
		_, _ = fmt.Fprintln(out)
		if st.Command != "" {
			cmd := st.Command
			if len(st.Args) > 0 {
				cmd = strings.Join(append([]string{st.Command}, st.Args...), " ")
			}
			_, _ = fmt.Fprintf(out, "agentflow suggested command: %s\n", cmd)
		}
		for _, d := range st.Diagnostics {
			_, _ = fmt.Fprintf(out, "  %s\n", d)
		}
	}
	if b, err := client.Status(ctx); err == nil && len(bytes.TrimSpace(b)) > 0 {
		_, _ = fmt.Fprintln(out, strings.TrimSpace(string(b)))
	}
}

// readEvidenceSidecar parses an optional evidence sidecar: either one object or
// an array of objects. id, claim, and source are required before any AgentFlow
// call (they are the CLI's own record-evidence requirements).
func readEvidenceSidecar(path string) ([]agentflow.EvidenceEntry, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read evidence sidecar: %w", err)
	}
	trimmed := strings.TrimSpace(string(b))
	var entries []agentflow.EvidenceEntry
	if strings.HasPrefix(trimmed, "[") {
		err = json.Unmarshal(b, &entries)
	} else {
		var one agentflow.EvidenceEntry
		err = json.Unmarshal(b, &one)
		entries = []agentflow.EvidenceEntry{one}
	}
	if err != nil {
		return nil, fmt.Errorf("parse evidence sidecar: %w", err)
	}
	for i, e := range entries {
		if e.ID == "" || e.Claim == "" || e.Source == "" {
			return nil, fmt.Errorf("evidence sidecar entry %d requires id, claim, and source", i)
		}
	}
	return entries, nil
}

// taskApprover auto-approves tool calls when edits are opted in. Scope is enforced
// structurally by the step-scope guard, not the human; gates never flow through
// here (they run via RunGate, gated by -approve-plan-gates in the preflight).
type taskApprover bool

func (a taskApprover) Approve(context.Context, provider.ToolCall, string) (bool, error) {
	return bool(a), nil
}
