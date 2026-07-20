package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kstruzzieri/go-llm/agentflow"
)

type recoveryAction uint8

const (
	failClosed recoveryAction = iota
	resumeSerial
	reportComplete
)

type recoveryDisposition struct {
	action      recoveryAction
	exitCode    int
	description string
}

func resumeDisposition(state string) recoveryDisposition {
	switch state {
	case "uninitialized":
		return recoveryDisposition{failClosed, 3, "setup required; initialize Agentflow explicitly"}
	case "plan_unlocked":
		return recoveryDisposition{failClosed, 3, "setup required; lock the plan explicitly"}
	case "execution_uninitialized":
		return recoveryDisposition{failClosed, 3, "setup required; initialize execution explicitly"}
	case "state_invalid":
		return recoveryDisposition{failClosed, 3, "blocked by invalid Agentflow state"}
	case "step_unclaimed":
		return recoveryDisposition{resumeSerial, 2, "enter the serial step loop"}
	case "file_receipts_missing":
		return recoveryDisposition{failClosed, 3, "blocked by missing file receipts"}
	case "validation_missing":
		return recoveryDisposition{resumeSerial, 2, "settle missing command gates, then continue serially"}
	case "step_unverified":
		return recoveryDisposition{resumeSerial, 2, "verify the owned attempt, then continue serially"}
	case "step_uncompleted":
		return recoveryDisposition{resumeSerial, 2, "complete the verified owned attempt, then continue serially"}
	case "drift_failing":
		return recoveryDisposition{failClosed, 3, "blocked by drift findings"}
	case "run_unverified":
		return recoveryDisposition{resumeSerial, 2, "run terminal gates and generate proof"}
	case "proof_missing":
		return recoveryDisposition{resumeSerial, 2, "generate and verify proof"}
	case "proof_stale":
		return recoveryDisposition{failClosed, 3, "blocked by stale proof"}
	case "proof_failing":
		return recoveryDisposition{failClosed, 3, "blocked by failing proof"}
	case "complete":
		return recoveryDisposition{reportComplete, 0, "report verified proof without mutation"}
	default:
		return recoveryDisposition{failClosed, 3, "unknown Agentflow state; update Golem before recovery"}
	}
}

type agentflowStatusExit struct{ code int }

func (e *agentflowStatusExit) Error() string { return fmt.Sprintf("agentflow status exit %d", e.code) }
func (e *agentflowStatusExit) ExitCode() int { return e.code }

func runAgentflowStatus(ctx context.Context, out io.Writer, root, source string, jsonOutput bool) error {
	source, err := resolveTaskAgentflowSource(root, source)
	if err != nil {
		return err
	}
	var runner agentflow.Runner = agentflow.NewExecRunner(root)
	if source != "" {
		runner = agentflow.NewSrcExecRunner(root, source)
	}
	return runAgentflowStatusWithRunner(ctx, out, root, jsonOutput, runner)
}

func runAgentflowStatusWithRunner(ctx context.Context, out io.Writer, root string, jsonOutput bool, runner agentflow.Runner) error {
	client := agentflow.NewOwnedClient(runner, root, "golem")
	state, err := client.NextAction(ctx)
	if err != nil {
		if state.RawJSON != nil {
			if jsonOutput {
				if _, writeErr := out.Write(state.RawJSON); writeErr != nil {
					return fmt.Errorf("write Agentflow status: %w", writeErr)
				}
				return statusExit(3)
			}
			state.Diagnostics = append(state.Diagnostics, "next-action projection is malformed: "+err.Error())
			disposition := recoveryDisposition{failClosed, 3, "blocked by malformed Agentflow projection"}
			renderAgentflowStatus(out, state, nil, disposition)
			return statusExit(3)
		}
		return fmt.Errorf("agentflow status unavailable: %s", recoveryDisplayText(err.Error()))
	}
	disposition := resumeDisposition(state.State)
	if disposition.action != failClosed {
		if err := validateStatusRecovery(root, state); err != nil {
			disposition = recoveryDisposition{failClosed, 3, "blocked by unsafe or malformed projection: " + err.Error()}
		}
	}
	var proof *agentflow.ProofSummary
	if disposition.action == reportComplete {
		summary, err := verifiedAgentflowProofSummary(ctx, client)
		if err != nil {
			state.Diagnostics = append(state.Diagnostics, "proof summary is inconsistent with complete state: "+err.Error())
			disposition = recoveryDisposition{failClosed, 3, "blocked by inconsistent proof summary"}
		} else {
			proof = &summary
		}
	}
	if jsonOutput {
		if _, err := out.Write(state.RawJSON); err != nil {
			return fmt.Errorf("write Agentflow status: %w", err)
		}
		return statusExit(disposition.exitCode)
	}
	renderAgentflowStatus(out, state, proof, disposition)
	return statusExit(disposition.exitCode)
}

func verifiedAgentflowProofSummary(ctx context.Context, client *agentflow.Client) (agentflow.ProofSummary, error) {
	summary, err := client.ProofSummary(ctx)
	if err != nil {
		return agentflow.ProofSummary{}, err
	}
	if summary.Failed != 0 {
		return agentflow.ProofSummary{}, fmt.Errorf("proof summary contains %d failed checks", summary.Failed)
	}
	return summary, nil
}

func statusExit(code int) error {
	if code == 0 {
		return nil
	}
	return &agentflowStatusExit{code: code}
}

// validateRecoveryProjection checks only authoritative next-action fields. It
// is read-only and shared by status and resume; resume adds local digest and
// materialized-workflow checks before any mutation.
func validateRecoveryProjection(state agentflow.NextActionState) error {
	projection := state.Resumability
	if projection == nil {
		return fmt.Errorf("agentflow resumability projection is missing")
	}
	if projection.Contract == nil {
		return fmt.Errorf("agentflow resumability contract is missing")
	}
	if !projection.Contract.Locked {
		return fmt.Errorf("agentflow resumability contract is not locked")
	}
	if projection.Contract.PlanSHA256 == "" || projection.Contract.ExecutionContractSHA256 == "" {
		return fmt.Errorf("agentflow resumability contract digests are incomplete")
	}
	if projection.AgentID != "golem" {
		return fmt.Errorf("agentflow resumability agent is %q, want %q", projection.AgentID, "golem")
	}
	if !projection.HasAttemptField() {
		return fmt.Errorf("agentflow resumability attempt is missing")
	}
	if !projection.HasLeaseField() || projection.Lease == nil {
		return fmt.Errorf("agentflow resumability lease is missing")
	}
	if !projection.HasGatesField() {
		return fmt.Errorf("agentflow resumability gates are missing")
	}
	if !projection.HasRecoveryActionsField() {
		return fmt.Errorf("agentflow resumability recovery actions are missing")
	}
	if !projection.HasDiagnosticsField() {
		return fmt.Errorf("agentflow resumability diagnostics are missing")
	}
	if len(projection.Diagnostics) != 0 {
		return fmt.Errorf("agentflow resumability reports diagnostics")
	}
	if projection.Step != nil {
		if strings.TrimSpace(projection.Step.ID) == "" || strings.TrimSpace(projection.Step.State) == "" || projection.Step.Completed == nil {
			return fmt.Errorf("agentflow resumability step is incomplete")
		}
		if state.StepID != nil && *state.StepID != projection.Step.ID {
			return fmt.Errorf("agentflow next-action step %q does not match resumability step %q", *state.StepID, projection.Step.ID)
		}
	}

	lease := projection.Lease
	if lease.Policy == nil || (*lease.Policy != "advisory" && *lease.Policy != "enforce") {
		return fmt.Errorf("agentflow resumability lease policy is missing or unknown")
	}
	if lease.TTLMinutes == nil || *lease.TTLMinutes <= 0 || lease.GraceSeconds == nil || *lease.GraceSeconds < 0 {
		return fmt.Errorf("agentflow resumability lease settings are missing or invalid")
	}
	if lease.Exclusive != (*lease.Policy == "enforce") {
		return fmt.Errorf("agentflow resumability lease exclusivity is inconsistent")
	}

	attempt := projection.Attempt
	if attempt == nil {
		switch state.State {
		case "file_receipts_missing", "validation_missing", "step_unverified", "step_uncompleted":
			return fmt.Errorf("agentflow state %q requires an open attempt", state.State)
		case "step_unclaimed":
			if projection.Step == nil || *projection.Step.Completed || !isEligibleUnclaimedStepState(projection.Step.State) {
				return fmt.Errorf("agentflow step_unclaimed projection has ineligible step state")
			}
			if err := validateAutomaticRecoveryAction(projection, "claim"); err != nil {
				return err
			}
		case "run_unverified", "proof_missing", "complete":
			if projection.Step != nil {
				return fmt.Errorf("agentflow state %q unexpectedly projects a step", state.State)
			}
		}
		if lease.State != "not_applicable" || lease.ExpiresAt != nil {
			return fmt.Errorf("agentflow resumability lease is inconsistent without an attempt")
		}
		return nil
	}
	if projection.Step == nil {
		return fmt.Errorf("agentflow resumability attempt has no step")
	}
	switch state.State {
	case "validation_missing", "step_unverified", "step_uncompleted":
		// These are the only resumable states backed by an existing attempt.
	default:
		return fmt.Errorf("agentflow state %q unexpectedly projects an open attempt", state.State)
	}
	if *projection.Step.Completed {
		return fmt.Errorf("agentflow resumability open attempt belongs to a completed step")
	}
	if strings.TrimSpace(attempt.ID) == "" || strings.TrimSpace(attempt.State) == "" {
		return fmt.Errorf("agentflow resumability attempt identity is incomplete")
	}
	if projection.Step.State != attempt.State {
		return fmt.Errorf("agentflow resumability step state %q does not match attempt state %q", projection.Step.State, attempt.State)
	}
	switch state.State {
	case "step_uncompleted":
		if attempt.State != "verified" {
			return fmt.Errorf("agentflow step_uncompleted attempt has state %q, want verified", attempt.State)
		}
	case "validation_missing", "step_unverified":
		if attempt.State != "claimed" && attempt.State != "amendment_started" && attempt.State != "in_progress" {
			return fmt.Errorf("agentflow %s attempt has state %q", state.State, attempt.State)
		}
	}
	if !attempt.Open {
		return fmt.Errorf("agentflow resumability attempt %q is not open", attempt.ID)
	}
	if attempt.Owner != "golem" {
		return fmt.Errorf("agentflow resumability attempt %q is owned by %q, want %q", attempt.ID, attempt.Owner, "golem")
	}
	switch lease.State {
	case "live":
		if lease.ExpiresAt == nil || strings.TrimSpace(*lease.ExpiresAt) == "" {
			return fmt.Errorf("agentflow live lease has no deadline")
		}
	case "no_deadline":
		if lease.ExpiresAt != nil {
			return fmt.Errorf("agentflow no-deadline lease has a deadline")
		}
	default:
		return fmt.Errorf("agentflow resumability lease state %q is not recoverable", lease.State)
	}
	return validateAutomaticRecoveryAction(projection, "continue")
}

func validateAutomaticRecoveryAction(projection *agentflow.ResumabilityProjection, action string) error {
	var matched *agentflow.ResumabilityRecoveryAction
	for i := range projection.RecoveryActions {
		if projection.RecoveryActions[i].Action != action {
			continue
		}
		if matched != nil {
			return fmt.Errorf("agentflow resumability has duplicate %s actions", action)
		}
		matched = &projection.RecoveryActions[i]
	}
	if matched == nil || !matched.Allowed || !matched.Automatic || matched.BreakGlass == nil || *matched.BreakGlass {
		return fmt.Errorf("agentflow resumability does not allow automatic non-break-glass %s", action)
	}
	return nil
}

func isEligibleUnclaimedStepState(state string) bool {
	switch state {
	case "pending", "blocked", "failed", "abandoned":
		return true
	default:
		return false
	}
}

func validateStatusRecovery(root string, state agentflow.NextActionState) error {
	if err := validateRecoveryProjection(state); err != nil {
		return err
	}
	if resumeDisposition(state.State).action == resumeSerial {
		planBytes, err := os.ReadFile(filepath.Join(root, ".agent", "plan.lock.json"))
		if err != nil {
			return fmt.Errorf("read Agentflow locked plan for recovery status: %w", err)
		}
		var plan agentflow.Plan
		if err := decodeAgentflowPlanJSON(planBytes, &plan); err != nil {
			return fmt.Errorf("parse Agentflow locked plan for recovery status: %w", err)
		}
		if err := agentflow.PreflightP0(&plan); err != nil {
			return fmt.Errorf("preflight Agentflow locked plan for recovery status: %w", err)
		}
		if err := validateTraceability(plan); err != nil {
			return err
		}
		if state.State == "validation_missing" {
			if _, _, err := projectedCommandGates(&plan, state); err != nil {
				return err
			}
		}
	}
	return validateRecoveryMutationSafety(state)
}

func validateResumeProjection(root string, planJSON []byte, state agentflow.NextActionState, approved *agentflow.WorkflowRecommendation) error {
	if err := validateRecoveryProjection(state); err != nil {
		return err
	}
	projection := state.Resumability
	planDigest, err := canonicalPlanJSONSHA256(planJSON)
	if err != nil {
		return fmt.Errorf("digest task plan for Agentflow resume: %w", err)
	}
	if projection.Contract.PlanSHA256 == "" || projection.Contract.PlanSHA256 != planDigest {
		return fmt.Errorf("task plan does not match Agentflow resumability contract")
	}
	execution, err := os.ReadFile(filepath.Join(root, ".agent", "execution.contract.json"))
	if err != nil {
		return fmt.Errorf("read Agentflow execution contract: %w", err)
	}
	executionDigest := fmt.Sprintf("%x", sha256.Sum256(execution))
	if projection.Contract.ExecutionContractSHA256 == "" || projection.Contract.ExecutionContractSHA256 != executionDigest {
		return fmt.Errorf("Agentflow execution contract digest does not match resumability projection")
	}
	workflow, err := os.ReadFile(filepath.Join(root, ".agent", "workflow.contract.json"))
	if err != nil {
		return fmt.Errorf("read Agentflow workflow contract: %w", err)
	}
	if err := agentflow.ValidateMaterializedWorkflowContract(workflow); err != nil {
		return fmt.Errorf("validate Agentflow workflow contract: %w", err)
	}
	if approved != nil {
		if err := approved.VerifyMaterializedWorkflowContract(workflow); err != nil {
			return fmt.Errorf("verify approved workflow handoff for resume: %w", err)
		}
	}
	return nil
}

func projectedCommandGates(plan *agentflow.Plan, state agentflow.NextActionState) ([]agentflow.CommandGate, []agentflow.ResumabilityGate, error) {
	if state.Resumability == nil || state.Resumability.Step == nil {
		return nil, nil, fmt.Errorf("agentflow command-gate recovery requires a projected step")
	}
	step, ok := findStep(plan, state.Resumability.Step.ID)
	if !ok {
		return nil, nil, fmt.Errorf("agentflow returned unknown step %q", state.Resumability.Step.ID)
	}
	commands, err := agentflow.ExtractCommandGates(step)
	if err != nil {
		return nil, nil, err
	}
	projected := make([]agentflow.ResumabilityGate, 0, len(state.Resumability.Gates))
	for _, gate := range state.Resumability.Gates {
		if gate.Status != "missing" && gate.Status != "satisfied" {
			return nil, nil, fmt.Errorf("agentflow gate %q has unknown status %q", gate.Label, gate.Status)
		}
		switch gate.Kind {
		case "command":
			projected = append(projected, gate)
		case "inspection", "legacy":
			// Known non-command projections do not participate in argv pairing.
		default:
			return nil, nil, fmt.Errorf("agentflow gate %q has unknown kind %q", gate.Label, gate.Kind)
		}
	}
	if len(projected) != len(commands) {
		return nil, nil, fmt.Errorf("agentflow projected %d command gates for %d plan gates", len(projected), len(commands))
	}
	for i, command := range commands {
		if projected[i].Label != strings.Join(command.Argv, " ") {
			return nil, nil, fmt.Errorf("agentflow command gate %d label %q does not match plan argv", i, projected[i].Label)
		}
	}
	return commands, projected, nil
}

// validateRecoveryMutationSafety rejects finite enforced recovery when the
// fixed Agentflow operation is not atomic with its final lease check. A time
// estimate cannot prove safety: run may append lease_renewed after a pause,
// finish-step records verification before rechecking expiry, and complete-step
// checks expiry before taking the separate close lock. Advisory and no-deadline
// attempts do not have these races.
func validateRecoveryMutationSafety(state agentflow.NextActionState) error {
	projection := state.Resumability
	if projection == nil || projection.Lease == nil || projection.Lease.Policy == nil || *projection.Lease.Policy != "enforce" {
		return nil
	}
	if state.State == "step_unclaimed" {
		return fmt.Errorf("agentflow resume blocks step_unclaimed under a finite enforced lease because claiming would enter non-atomic gate recovery")
	}
	if projection.Lease.State == "live" && projection.Lease.ExpiresAt != nil {
		switch state.State {
		case "validation_missing", "step_unverified", "step_uncompleted":
			return fmt.Errorf("agentflow resume blocks %s under a finite enforced lease because Agentflow cannot guarantee duplicate-free, no-renew recovery", state.State)
		}
	}
	return nil
}

func settleAgentflowAttempt(ctx context.Context, client afClient, plan *agentflow.Plan, state agentflow.NextActionState) (bool, error) {
	switch state.State {
	case "step_unclaimed", "run_unverified", "proof_missing", "complete":
		return false, nil
	case "validation_missing", "step_unverified", "step_uncompleted":
		// Settled below with fixed adapter methods.
	default:
		return false, fmt.Errorf("agentflow state %q has no automatic settlement", state.State)
	}
	if state.Resumability == nil || state.Resumability.Step == nil || state.Resumability.Attempt == nil {
		return false, fmt.Errorf("agentflow settlement requires a projected step and attempt")
	}
	stepID := state.Resumability.Step.ID
	attemptID := state.Resumability.Attempt.ID
	if state.StepID != nil && *state.StepID != stepID {
		return false, fmt.Errorf("agentflow settlement step %q does not match projection %q", *state.StepID, stepID)
	}

	switch state.State {
	case "step_unverified":
		if err := client.FinishStep(ctx, stepID, attemptID); err != nil {
			return true, err
		}
	case "step_uncompleted":
		if err := client.CompleteStep(ctx, stepID, attemptID); err != nil {
			return true, err
		}
	case "validation_missing":
		commands, projected, err := projectedCommandGates(plan, state)
		if err != nil {
			return false, err
		}
		for i, command := range commands {
			if projected[i].Status != "missing" {
				continue
			}
			if err := client.RunGate(ctx, stepID, attemptID, command.Label, command.Argv); err != nil {
				return true, fmt.Errorf("gate %q failed: %w", command.Label, err)
			}
		}
		if err := client.FinishStep(ctx, stepID, attemptID); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (d *driver) resume(ctx context.Context, root string, planJSON []byte, approved *agentflow.WorkflowRecommendation) (agentflow.NextActionState, error) {
	state, err := d.af.NextAction(ctx)
	if err != nil {
		return agentflow.NextActionState{}, err
	}
	disposition := resumeDisposition(state.State)
	if disposition.action == failClosed {
		return agentflow.NextActionState{}, fmt.Errorf("agentflow resume blocked in state %q: %s", state.State, disposition.description)
	}
	if err := validateResumeProjection(root, planJSON, state, approved); err != nil {
		return agentflow.NextActionState{}, err
	}
	if err := validateRecoveryMutationSafety(state); err != nil {
		return agentflow.NextActionState{}, err
	}
	if disposition.action == reportComplete {
		return state, nil
	}

	settled, err := settleAgentflowAttempt(ctx, d.af, d.plan, state)
	if err != nil {
		return agentflow.NextActionState{}, err
	}
	if settled {
		progressed, err := d.af.NextAction(ctx)
		if err != nil {
			return agentflow.NextActionState{}, err
		}
		if progressed.State == state.State {
			return agentflow.NextActionState{}, fmt.Errorf("agentflow state %q did not progress after settlement", state.State)
		}
		if err := validateResumeProjection(root, planJSON, progressed, approved); err != nil {
			return agentflow.NextActionState{}, err
		}
		if err := validateRecoveryMutationSafety(progressed); err != nil {
			return agentflow.NextActionState{}, err
		}
		state = progressed
		disposition = resumeDisposition(state.State)
		if disposition.action == failClosed {
			return agentflow.NextActionState{}, fmt.Errorf("agentflow resume blocked after settlement in state %q: %s", state.State, disposition.description)
		}
		if disposition.action == reportComplete {
			return state, nil
		}
		switch state.State {
		case "step_unclaimed", "run_unverified", "proof_missing":
		default:
			return agentflow.NextActionState{}, fmt.Errorf("agentflow settlement progressed to unsupported in-flight state %q", state.State)
		}
	}

	previousBeforeGates := d.beforeGates
	d.beforeGates = func(ctx context.Context, step agentflow.Step, attempt string) error {
		projected, err := d.af.NextAction(ctx)
		if err != nil {
			return err
		}
		if projected.State != "validation_missing" {
			return fmt.Errorf("agentflow resumed step %q reached state %q before gates, want validation_missing", step.ID, projected.State)
		}
		if err := validateResumeProjection(root, planJSON, projected, approved); err != nil {
			return err
		}
		if projected.Resumability.Step.ID != step.ID || projected.Resumability.Attempt.ID != attempt {
			return fmt.Errorf("agentflow resumed gate projection identifies step %q attempt %q, want step %q attempt %q",
				projected.Resumability.Step.ID, projected.Resumability.Attempt.ID, step.ID, attempt)
		}
		_, gates, err := projectedCommandGates(d.plan, projected)
		if err != nil {
			return err
		}
		for _, gate := range gates {
			if gate.Status != "missing" {
				return fmt.Errorf("agentflow resumed step %q already has satisfied command gate %q; refusing duplicate execution", step.ID, gate.Label)
			}
		}
		return validateRecoveryMutationSafety(projected)
	}
	defer func() { d.beforeGates = previousBeforeGates }()
	if err := d.runSerialSteps(ctx); err != nil {
		return agentflow.NextActionState{}, err
	}
	if _, err := d.af.FinishRun(ctx); err != nil {
		return agentflow.NextActionState{}, err
	}
	final, err := d.af.NextAction(ctx)
	if err != nil {
		return agentflow.NextActionState{}, err
	}
	if err := validateResumeProjection(root, planJSON, final, approved); err != nil {
		return agentflow.NextActionState{}, err
	}
	if final.State != "complete" {
		return agentflow.NextActionState{}, fmt.Errorf("agentflow resume finished in state %q, want complete", final.State)
	}
	return final, nil
}

func renderAgentflowStatus(out io.Writer, state agentflow.NextActionState, proof *agentflow.ProofSummary, disposition recoveryDisposition) {
	blocking := state.Blocking || disposition.action == failClosed
	fmt.Fprintf(out, "state: %s\nreason: %s\nblocking: %t\n", recoveryDisplayText(state.State), recoveryDisplayText(state.Reason), blocking)
	if state.StepID != nil {
		fmt.Fprintf(out, "step: %s\n", recoveryDisplayText(*state.StepID))
	}
	if state.Gate != nil {
		fmt.Fprintf(out, "current gate: %s\n", recoveryDisplayText(*state.Gate))
	}
	if projection := state.Resumability; projection != nil {
		if state.StepID == nil && projection.Step != nil {
			fmt.Fprintf(out, "step: %s\n", recoveryDisplayText(projection.Step.ID))
		}
		if projection.Attempt != nil {
			fmt.Fprintf(out, "attempt: %s owner=%s open=%t\n", recoveryDisplayText(projection.Attempt.ID), recoveryDisplayText(projection.Attempt.Owner), projection.Attempt.Open)
		}
		if projection.Lease != nil {
			policy := "unknown"
			if projection.Lease.Policy != nil {
				policy = *projection.Lease.Policy
			}
			fmt.Fprintf(out, "lease: policy=%s state=%s", recoveryDisplayText(policy), recoveryDisplayText(projection.Lease.State))
			if projection.Lease.ExpiresAt != nil {
				fmt.Fprintf(out, " expires=%s", recoveryDisplayText(*projection.Lease.ExpiresAt))
			}
			fmt.Fprintln(out)
		}
		for _, gate := range projection.Gates {
			fmt.Fprintf(out, "gate: %s %s (%s)\n", recoveryDisplayText(gate.Kind), recoveryDisplayText(gate.Label), recoveryDisplayText(gate.Status))
		}
		for _, diagnostic := range projection.Diagnostics {
			fmt.Fprintf(out, "diagnostic: [%s] %s", recoveryDisplayText(diagnostic.Code), recoveryDisplayText(diagnostic.Message))
			if diagnostic.Artifact != "" {
				fmt.Fprintf(out, " (%s)", recoveryDisplayText(diagnostic.Artifact))
			}
			fmt.Fprintln(out)
		}
	}
	for _, diagnostic := range state.Diagnostics {
		fmt.Fprintf(out, "diagnostic: %s\n", recoveryDisplayText(diagnostic))
	}
	if state.Command != "" || len(state.Args) > 0 {
		fmt.Fprintf(out, "advisory (display only): command=%q args=%q\n", state.Command, state.Args)
	}
	fmt.Fprintf(out, "resume: %s\n", recoveryDisplayText(disposition.description))
	if proof != nil {
		fmt.Fprintf(out, "proof: verified\nartifact: %s\nchecks: passed=%d warning=%d failed=%d not_run=%d skipped=%d not_applicable=%d total=%d\n",
			recoveryDisplayText(proof.Path), proof.Passed, proof.Warning, proof.Failed, proof.NotRun, proof.Skipped, proof.NotApplicable, proof.Total)
	}
}

func recoveryDisplayText(value string) string {
	quoted := previewText(value)
	return quoted[1 : len(quoted)-1]
}
