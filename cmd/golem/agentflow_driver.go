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
	ProbeParallel(context.Context) error
	ProbeWorkflow(context.Context) error
	RecommendWorkflow(context.Context, agentflow.TaskBrief, string, string) (agentflow.WorkflowRecommendation, error)
	ProbeReview(context.Context) error
	Init(context.Context) error
	LockPlan(context.Context, string) error
	MaterializeWorkflowContract(context.Context, agentflow.WorkflowRecommendation) error
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
	af              afClient
	plan            *agentflow.Plan
	planPath        string
	reviewManifest  string
	taskBrief       agentflow.TaskBrief
	workflowProfile string
	workflowReason  string
	recommendation  agentflow.WorkflowRecommendation
	// approvedRecommendation is the exact route approved and materialized by
	// planning mode. When present, task mode verifies it before constructing the
	// client and must neither recommend nor materialize a replacement.
	approvedRecommendation *agentflow.WorkflowRecommendation
	evidence               []agentflow.EvidenceEntry
	runStep                runStepFunc
	parallelCohort         func(context.Context) error
	out                    io.Writer
}

// validateFreshWorkerProjection rejects copied worktrees that have begun or
// cannot authoritatively prove execution is still fresh.
func validateFreshWorkerProjection(state agentflow.NextActionState, agentID string) error {
	projection := state.Resumability
	if projection == nil {
		return errors.New("agentflow resumability projection is missing")
	}
	if projection.Contract == nil {
		return errors.New("agentflow resumability contract is missing")
	}
	if !projection.Contract.Locked {
		return errors.New("agentflow resumability contract is not locked")
	}
	if strings.TrimSpace(projection.Contract.PlanSHA256) == "" ||
		strings.TrimSpace(projection.Contract.ExecutionContractSHA256) == "" {
		return errors.New("agentflow resumability contract is missing digests")
	}
	if projection.AgentID != agentID {
		return fmt.Errorf("agentflow resumability agent is %q, want %q", projection.AgentID, agentID)
	}
	if !projection.HasAttemptField() {
		return errors.New("agentflow resumability attempt is missing")
	}
	if projection.Attempt != nil {
		return fmt.Errorf("agentflow resumability reports present attempt %q", projection.Attempt.ID)
	}
	if !projection.HasDiagnosticsField() {
		return errors.New("agentflow resumability diagnostics are missing")
	}
	if len(projection.Diagnostics) != 0 {
		return errors.New("agentflow resumability reports diagnostics")
	}
	return nil
}

// run drives the P0 sequence and returns the proof-pack path on success.
func (d *driver) run(ctx context.Context) (string, error) {
	if err := d.af.Probe(ctx); err != nil {
		return "", fmt.Errorf("agentflow unavailable: %w", err)
	}
	if d.parallelCohort != nil {
		if err := d.af.ProbeParallel(ctx); err != nil {
			return "", fmt.Errorf("agentflow parallel runtime unavailable: %w", err)
		}
	}
	if err := d.af.ProbeWorkflow(ctx); err != nil {
		return "", fmt.Errorf("agentflow workflow routing unavailable: %w", err)
	}
	var recommendation agentflow.WorkflowRecommendation
	if d.approvedRecommendation != nil {
		recommendation = *d.approvedRecommendation
	} else {
		var err error
		recommendation, err = d.af.RecommendWorkflow(ctx, d.taskBrief, d.workflowProfile, d.workflowReason)
		if err != nil {
			return "", fmt.Errorf("recommend Agentflow workflow: %w", err)
		}
	}
	d.recommendation = recommendation
	if d.out != nil {
		_, _ = fmt.Fprintln(d.out, "Agentflow workflow route at task startup")
		_, _ = fmt.Fprint(d.out, renderWorkflowPreview(recommendation))
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
	if d.approvedRecommendation == nil {
		if err := d.af.MaterializeWorkflowContract(ctx, recommendation); err != nil {
			return "", fmt.Errorf("materialize Agentflow workflow contract: %w", err)
		}
	}
	if err := d.af.InitExecution(ctx); err != nil {
		return "", err
	}
	if err := d.af.Doctor(ctx); err != nil {
		return "", err
	}
	if d.parallelCohort != nil {
		if err := d.parallelCohort(ctx); err != nil {
			return "", fmt.Errorf("run parallel cohort: %w", err)
		}
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
	if d.out != nil {
		_, _ = fmt.Fprintln(d.out, "Workflow route before review ingestion")
		_, _ = fmt.Fprint(d.out, renderWorkflowPreview(d.recommendation))
	}
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

// newTaskStepRunner builds the model/tool runtime bound to one workspace root.
func newTaskStepRunner(root string, plan *agentflow.Plan, af afClient, orch *agent.Orchestrator, sess *replSession, approveEdits bool, out io.Writer, interrupts <-chan struct{}, cancel context.CancelFunc) (runStepFunc, error) {
	if orch == nil {
		return nil, errors.New("task orchestrator is nil")
	}
	if sess == nil {
		return nil, errors.New("task session is nil")
	}
	if cancel == nil {
		return nil, errors.New("task cancel function is nil")
	}
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return nil, err
	}
	undo := newMutationJournal(ws)
	afJournal := newAgentflowJournal(af.RecordFileChange, cancel)
	journal := compositeJournal{sinks: []agenttools.Journal{undo, afJournal}}

	return func(sctx context.Context, step agentflow.Step, attempt, goal string) error {
		stepCtx, stepCancel := context.WithCancel(sctx)
		defer stepCancel()
		done := make(chan struct{})
		defer close(done)
		if interrupts != nil {
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

		ws.SetScopeGuard(stepScopeGuard(plan, step.ID))
		defer ws.SetScopeGuard(nil)
		afJournal.setStep(step.ID, attempt)

		readTools := agenttools.NewFileToolsForWorkspace(ws)
		writeTools := agenttools.NewMutatingTools(ws, journal)
		tools := append(readTools, writeTools...)
		req := agent.Request{
			Goal:     goal,
			System:   sess.baseSystem,
			Tools:    tools,
			MaxSteps: sess.maxSteps,
			Budget:   sess.budget,
			Approver: taskApprover(approveEdits),
			Options:  sess.modelOptions,
		}
		_, runErr := orch.Run(stepCtx, req, agent.Observer(newRenderer(out, false, sess.maxSteps, sess.clock)))
		if fatal := afJournal.fatalErr(); fatal != nil {
			return fmt.Errorf("unreceipted edit aborted the run: %w", fatal)
		}
		return runErr
	}, nil
}

func runTaskDriver(ctx context.Context, d *driver, coordinator *parallelCoordinator, stderr io.Writer) (string, error) {
	proof, err := d.run(ctx)
	if err != nil {
		if coordinator != nil && stderr != nil {
			for _, root := range coordinator.preservedRoots() {
				_, _ = fmt.Fprintf(stderr, "parallel worktree preserved: %s\n", root)
			}
		}
		return "", err
	}
	if coordinator != nil {
		if err := coordinator.cleanup(ctx); err != nil && stderr != nil {
			_, _ = fmt.Fprintf(stderr, "warning: clean up parallel worktrees: %v\n", err)
		}
	}
	return proof, nil
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
	taskBrief, err := readExternalTaskBrief(f.taskBriefPath, plan)
	if err != nil {
		return err
	}
	approvedRecommendation, err := readApprovedWorkflowHandoff(f.workflowHandoffPath, root, planBytes, taskBrief)
	if err != nil {
		return err
	}
	if f.planWorkers > 1 && (sess == nil || sess.newOrchestrator == nil) {
		return errors.New("parallel orchestrator factory is nil")
	}
	agentflowSrc, err := resolveTaskAgentflowSource(root, f.agentflowSrc)
	if err != nil {
		return fmt.Errorf("resolve Agentflow source: %w", err)
	}

	// 3. Build root-specific Agentflow runners for the canonical and optional
	// worker roots.
	runnerForRoot := func(root string) agentflow.Runner {
		if agentflowSrc != "" {
			return agentflow.NewSrcExecRunner(root, agentflowSrc)
		}
		return agentflow.NewExecRunner(root)
	}
	client := agentflow.NewClient(runnerForRoot(root), root)

	// 4. Run context we can cancel on a fatal record failure.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 5. Canonical root step runtime. Serial rendering and interrupt behavior
	// remain unchanged.
	runStep, err := newTaskStepRunner(root, &plan, client, sess.orch, sess, f.approveEdits, stderr, interrupts, cancel)
	if err != nil {
		return err
	}

	// 6. Drive.
	d := &driver{
		af: client, plan: &plan, planPath: planPath, reviewManifest: reviewManifest,
		taskBrief: taskBrief, workflowProfile: f.workflowProfile, workflowReason: f.workflowReason,
		approvedRecommendation: approvedRecommendation,
		evidence:               evidence, runStep: runStep, out: stdout,
	}
	var coordinator *parallelCoordinator
	if f.planWorkers > 1 {
		workerOut := &synchronizedWriter{out: stderr}
		coordinator = newParallelCoordinator(root, &plan, f.planWorkers,
			newAssignedParallelWorker(&plan, sess, f.approveEdits, workerOut, runnerForRoot))
		coordinator.interrupts = interrupts
		coordinator.aggregate = newParallelAggregate(runnerForRoot)
		d.parallelCohort = func(ctx context.Context) error {
			ran, err := coordinator.runCohort(ctx)
			if err == nil && !ran {
				_, _ = fmt.Fprintln(stdout, "no safe parallel cohort; continuing serially")
			}
			return err
		}
	}
	proof, err := runTaskDriver(runCtx, d, coordinator, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentflow task failed: %v\n", err)
		reportAgentflowRecovery(ctx, stderr, client)
		return errAgentflowTaskFailed
	}
	_, _ = fmt.Fprintf(stdout, "proof pack: %s\n", proof)
	return nil
}

// readApprovedWorkflowHandoff verifies that the durable route report still
// matches Agentflow's already-materialized contract before task mode makes any
// Agentflow call. A changed handoff, contract, or planning/execution pairing
// therefore fails closed instead of silently re-routing the approved plan.
func readApprovedWorkflowHandoff(path, root string, planJSON []byte, brief agentflow.TaskBrief) (*agentflow.WorkflowRecommendation, error) {
	if path == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve approved workflow handoff: %w", err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("read approved workflow handoff: %w", err)
	}
	var handoff approvedWorkflowHandoff
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&handoff); err != nil {
		return nil, fmt.Errorf("parse approved workflow handoff: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("parse approved workflow handoff: trailing JSON")
	}
	if handoff.SchemaVersion != approvedWorkflowHandoffSchemaVersion {
		return nil, fmt.Errorf("approved workflow handoff schema_version %q is incompatible; want %s", handoff.SchemaVersion, approvedWorkflowHandoffSchemaVersion)
	}
	planSHA256, err := canonicalPlanJSONSHA256(planJSON)
	if err != nil {
		return nil, fmt.Errorf("digest task plan for approved workflow handoff: %w", err)
	}
	if handoff.PlanSHA256 != planSHA256 {
		return nil, fmt.Errorf("task plan does not match approved workflow handoff")
	}
	taskBriefSHA256, err := canonicalJSONSHA256(brief)
	if err != nil {
		return nil, fmt.Errorf("digest task brief for approved workflow handoff: %w", err)
	}
	if handoff.TaskBriefSHA256 != taskBriefSHA256 {
		return nil, fmt.Errorf("task brief does not match approved workflow handoff")
	}
	materialized, err := os.ReadFile(filepath.Join(root, ".agent", "workflow.contract.json"))
	if err != nil {
		return nil, fmt.Errorf("read Agentflow contract for approved workflow handoff: %w", err)
	}
	if err := handoff.Recommendation.VerifyMaterializedWorkflowContract(materialized); err != nil {
		return nil, fmt.Errorf("verify approved workflow handoff: %w", err)
	}
	return &handoff.Recommendation, nil
}

// readExternalTaskBrief resolves and strictly decodes an optional caller brief.
// Exact file/gate facts from the locked plan are always unioned into an explicit
// brief, so caller-supplied routing hints can add context but cannot conceal
// scope. All other missing signals remain unknown. Without a brief,
// task_type=feature is the conservative compatibility floor—Golem does not
// infer a lighter task class from prose.
func readExternalTaskBrief(path string, plan agentflow.Plan) (agentflow.TaskBrief, error) {
	if path == "" {
		brief := agentflow.TaskBriefFromPlan(plan, "feature")
		if err := validateExternalTaskBrief(brief, plan.RiskLevel); err != nil {
			return agentflow.TaskBrief{}, err
		}
		return brief, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return agentflow.TaskBrief{}, fmt.Errorf("resolve task brief: %w", err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return agentflow.TaskBrief{}, fmt.Errorf("read task brief: %w", err)
	}
	var brief agentflow.TaskBrief
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&brief); err != nil {
		return agentflow.TaskBrief{}, fmt.Errorf("parse task brief: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return agentflow.TaskBrief{}, fmt.Errorf("parse task brief: trailing JSON")
	}
	if err := validateExternalTaskBrief(brief, plan.RiskLevel); err != nil {
		return agentflow.TaskBrief{}, err
	}
	facts := agentflow.TaskBriefFromPlan(plan, brief.TaskType)
	brief.CandidateFiles = unionTaskBriefFacts(facts.CandidateFiles, brief.CandidateFiles)
	brief.ValidationNeeds = unionTaskBriefFacts(facts.ValidationNeeds, brief.ValidationNeeds)
	return brief, nil
}

func unionTaskBriefFacts(exact, supplied *[]string) *[]string {
	if exact == nil && supplied == nil {
		return nil
	}
	length := 0
	if exact != nil {
		length += len(*exact)
	}
	if supplied != nil {
		length += len(*supplied)
	}
	out := make([]string, 0, length)
	seen := make(map[string]struct{}, length)
	if exact != nil {
		for _, value := range *exact {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	if supplied != nil {
		for _, value := range *supplied {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	return &out
}

func validateExternalTaskBrief(brief agentflow.TaskBrief, planRisk string) error {
	if brief.SchemaVersion != agentflow.TaskBriefSchemaVersion {
		return fmt.Errorf("task brief schema_version %q is incompatible; want %s", brief.SchemaVersion, agentflow.TaskBriefSchemaVersion)
	}
	switch brief.TaskType {
	case "docs", "bugfix", "feature", "refactor":
	default:
		return fmt.Errorf("task brief task_type %q must be docs, bugfix, feature, or refactor", brief.TaskType)
	}
	if brief.DeclaredRisk != "low" && brief.DeclaredRisk != "medium" && brief.DeclaredRisk != "high" {
		return fmt.Errorf("task brief declared_risk %q must be low, medium, or high", brief.DeclaredRisk)
	}
	if brief.DeclaredRisk != planRisk {
		return fmt.Errorf("task brief declared_risk %q does not match plan risk %q", brief.DeclaredRisk, planRisk)
	}
	if brief.BlastRadius != nil && *brief.BlastRadius != "isolated" && *brief.BlastRadius != "local" && *brief.BlastRadius != "cross_cutting" {
		return fmt.Errorf("task brief blast_radius %q is invalid", *brief.BlastRadius)
	}
	if brief.DeclaredSize != nil && *brief.DeclaredSize != "xs" && *brief.DeclaredSize != "s" && *brief.DeclaredSize != "m" && *brief.DeclaredSize != "l" && *brief.DeclaredSize != "xl" {
		return fmt.Errorf("task brief declared_size %q is invalid", *brief.DeclaredSize)
	}
	for field, values := range map[string]*[]string{"candidate_files": brief.CandidateFiles, "validation_needs": brief.ValidationNeeds} {
		if values == nil {
			continue
		}
		for _, value := range *values {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("task brief %s must contain non-empty strings", field)
			}
		}
	}
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

func resolveTaskAgentflowSource(root, source string) (string, error) {
	if source == "" {
		return "", nil
	}
	if !filepath.IsAbs(source) {
		source = filepath.Join(root, source)
	}
	return filepath.Abs(source)
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
