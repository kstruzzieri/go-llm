package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
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
	case "step_amendment_needed":
		return recoveryDisposition{resumeSerial, 2, "amend the completed step, then continue serially"}
	case "attempt_timed_out":
		return recoveryDisposition{resumeSerial, 2, "block the timed-out attempt, then re-run the step serially"}
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

// agentflowStatusExit carries a status exit code. diagnostic, when set, is the
// one stderr line main prints before exiting; stdout stays the JSON contract.
type agentflowStatusExit struct {
	code       int
	diagnostic string
}

func (e *agentflowStatusExit) Error() string { return fmt.Sprintf("agentflow status exit %d", e.code) }
func (e *agentflowStatusExit) ExitCode() int { return e.code }

func runAgentflowStatus(ctx context.Context, out io.Writer, root, source string, envNames []string, jsonOutput bool) error {
	source, err := resolveTaskAgentflowSource(root, source)
	if err != nil {
		return err
	}
	return runAgentflowStatusWithRunner(ctx, out, root, jsonOutput, mustAgentflowRunner(root, source, envNames))
}

func runAgentflowStatusWithRunner(ctx context.Context, out io.Writer, root string, jsonOutput bool, runner agentflow.Runner) error {
	client := agentflow.NewOwnedClient(runner, root, "golem")
	// #612 R6: refuse an unsupported AgentFlow before relaying anything it says.
	var state agentflow.NextActionState
	err := client.CheckVersion(ctx)
	if err == nil {
		state, err = client.NextAction(ctx)
	}
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
			renderAgentflowStatus(out, state, nil, disposition, true)
			return statusExit(3)
		}
		// CheckVersion wraps launch failures; an unset approved name keeps its
		// own one-line text in both modes.
		var unset *agentflow.EnvNotSetError
		if errors.As(err, &unset) {
			err = unset
		}
		if !jsonOutput {
			_, _ = fmt.Fprintf(out, "agentflow status unavailable: %s\n", recoveryDisplayText(err.Error()))
			return statusExit(3)
		}
		// JSON mode: only Golem-owned text reaches stderr (printed by main): an
		// unset approved name or a rejected version. Other runner errors may
		// carry arbitrary text and stay silent.
		var versionErr *agentflow.VersionError
		switch {
		case unset != nil:
			return &agentflowStatusExit{code: 3, diagnostic: unset.Error()}
		case errors.As(err, &versionErr):
			return &agentflowStatusExit{code: 3, diagnostic: versionErr.Error()}
		}
		return statusExit(3)
	}
	disposition := resumeDisposition(state.State)
	if disposition.action != failClosed {
		if err := validateStatusRecovery(root, state); err != nil {
			disposition = recoveryDisposition{failClosed, 3, "blocked by unsafe or malformed projection: " + err.Error()}
		} else if hasFailedCommandGate(state) {
			disposition.description = "block the attempt whose gate failed, then re-run the step serially" // #652 A6
		}
	}
	// #642 A9: after validateStatusRecovery, so earlier projection refusals keep precedence (D9).
	if disposition.action == resumeSerial {
		if err := refuseReviewAmendmentReplacement(root, state); err != nil {
			disposition = recoveryDisposition{failClosed, 3, "blocked: " + err.Error()}
		} else if err := refuseUnrecoverableTimeout(root); err != nil {
			disposition = recoveryDisposition{failClosed, 3, "blocked: " + err.Error()}
		}
	}
	if disposition.action == resumeSerial {
		if _, err := newResumeProofGuard(ctx, client, root); err != nil {
			disposition = recoveryDisposition{failClosed, 3, "blocked: " + err.Error()}
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
	renderAgentflowStatus(out, state, proof, disposition, true)
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
		case "file_receipts_missing", "validation_missing", "step_unverified", "step_uncompleted", "attempt_timed_out":
			return fmt.Errorf("agentflow state %q requires an open attempt", state.State)
		case "step_unclaimed":
			if projection.Step == nil || *projection.Step.Completed || !isEligibleUnclaimedStepState(projection.Step.State) {
				return fmt.Errorf("agentflow step_unclaimed projection has ineligible step state")
			}
			if err := validateAutomaticRecoveryAction(projection, "claim"); err != nil {
				return err
			}
		case "step_amendment_needed":
			// AgentFlow 1.1 (agentflow#70): a completed step needs a
			// replacement attempt, which only amend-step opens.
			if projection.Step == nil || !*projection.Step.Completed {
				return fmt.Errorf("agentflow step_amendment_needed projection has no completed step")
			}
			if err := validateAutomaticRecoveryAction(projection, "amend"); err != nil {
				return err
			}
		case "run_unverified", "drift_failing", "proof_missing", "proof_stale", "proof_failing", "complete":
			if projection.Step != nil {
				return fmt.Errorf("agentflow state %q unexpectedly projects a step", state.State)
			}
			// No recovery-action requirement here: Agentflow's action
			// vocabulary (claim/continue/renew/reclaim/fail/block/amend) covers
			// attempt lifecycle only and projects every action as not allowed
			// in these terminal phases. FinishRun re-enters Agentflow's own
			// terminal gates, which enforce their own policy.
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
	case "validation_missing", "step_unverified", "step_uncompleted", "attempt_timed_out":
		// These are the only resumable states backed by an existing attempt.
	default:
		return fmt.Errorf("agentflow state %q unexpectedly projects an open attempt", state.State)
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
	case "attempt_timed_out":
		// A verified attempt is still open (execution.py OPEN_EVENTS); a later
		// command in it can time out.
		if attempt.State != "claimed" && attempt.State != "amendment_started" && attempt.State != "in_progress" && attempt.State != "verified" {
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
	case "expired":
		// AgentFlow lets a timed-out advisory attempt be blocked whatever its
		// lease (porcelain.py _timeout_close); an expired enforced one is
		// reclaim-only, which Golem never does.
		if state.State != "attempt_timed_out" || *lease.Policy != "advisory" || lease.ExpiresAt == nil || strings.TrimSpace(*lease.ExpiresAt) == "" {
			return fmt.Errorf("agentflow resumability lease state %q is not recoverable", lease.State)
		}
	default:
		return fmt.Errorf("agentflow resumability lease state %q is not recoverable", lease.State)
	}
	if state.State == "attempt_timed_out" {
		// The attempt can never complete; block-step is the close AgentFlow
		// allows this caller (agentflow#70). Golem never reclaims: AgentFlow
		// offers that only for an expired enforced lease, refused above.
		return validateAutomaticRecoveryAction(projection, "block")
	}
	return validateAutomaticRecoveryAction(projection, "continue")
}

// refuseCompletedStepAttempt refuses an open attempt on a step AgentFlow
// already counts completed: an amendment, such as a review amendment, that
// settlement would finish on its gates alone, without the work it was opened
// for (#652 D1). Status and resume run it on the projection they start from,
// not on an amendment resume opens itself. A timed-out one is only blocked, so
// it passes.
func refuseCompletedStepAttempt(state agentflow.NextActionState) error {
	projection := state.Resumability
	if projection == nil || projection.Attempt == nil || projection.Step == nil || projection.Step.Completed == nil ||
		!*projection.Step.Completed || state.State == "attempt_timed_out" {
		return nil
	}
	return fmt.Errorf("agentflow resumability open attempt belongs to a completed step")
}

// refuseReviewAmendmentReplacement refuses to replace an attempt of a step
// whose review-linked amendment never completed. Resume replaces an attempt
// with the step's own goal, so the review findings would go unaddressed while
// the replacement supersedes the amendment (agentflow#70; golem-integration.md
// review cycle). Read-only; it runs before resume blocks or amends anything.
func refuseReviewAmendmentReplacement(root string, state agentflow.NextActionState) error {
	if state.State != "attempt_timed_out" && state.State != "step_amendment_needed" {
		return nil
	}
	if state.Resumability == nil || state.Resumability.Step == nil {
		return fmt.Errorf("agentflow %s projection has no step", state.State)
	}
	step := state.Resumability.Step.ID
	review, err := unfinishedReviewAmendment(root, step)
	if err != nil || !review {
		return err
	}
	return fmt.Errorf("step %s has an unfinished review amendment; resume would replace it with the step's own goal and drop the review findings, so it changes nothing; block the attempt if it is still open, then address the findings in a new -review-manifest round over the same plan", step)
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
	if err := refuseCompletedStepAttempt(state); err != nil {
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
		return fmt.Errorf("agentflow execution contract digest does not match resumability projection")
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
	// Positional pairing is proven by the joined-argv label check below, which
	// cannot distinguish two gates with identical argv. If Agentflow ever
	// reordered such twins with mixed satisfied/missing statuses, recovery
	// could re-run a satisfied receipt, so ambiguous duplicates fail closed.
	joined := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		key := strings.Join(command.Argv, " ")
		if _, dup := joined[key]; dup {
			return nil, nil, fmt.Errorf("agentflow step %q has duplicate command-gate argv %q; positional recovery pairing would be ambiguous", step.ID, key)
		}
		joined[key] = struct{}{}
	}
	projected := make([]agentflow.ResumabilityGate, 0, len(state.Resumability.Gates))
	for _, gate := range state.Resumability.Gates {
		switch gate.Status {
		case "missing", "satisfied", "failed": // AgentFlow 1.0 verify_step's vocabulary (#652)
		default:
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

// resumeFailedGateBlockReason is the ledger reason when resume blocks an open
// attempt whose command gate AgentFlow reports failed (#652 A6). As with
// gateBlockReason, the gate's label stays out of the ledger.
const resumeFailedGateBlockReason = "golem: resume found failed command gate"

// resumeTimedOutBlockReason is the ledger reason when resume blocks an open
// attempt AgentFlow reports attempt_timed_out (agentflow#70).
const resumeTimedOutBlockReason = "golem: resume found timed-out gate"

// resumeAmendReason is the ledger reason when resume opens the replacement
// attempt AgentFlow reports step_amendment_needed for (agentflow#70).
const resumeAmendReason = "golem: resume replaces the step's attempt"

// hasFailedCommandGate reports whether a validation_missing projection holds a
// command gate AgentFlow reports failed: a nonzero exit, a run that
// risk_policy: block refused (exit_code null, decision "blocked"), or an
// attested receipt under strict policy (execution_coverage.py verify_step). An
// attempt with a timed-out gate reports attempt_timed_out instead (agentflow#70).
// AgentFlow credits an attempt's first matching receipt, so neither re-running
// the gate nor finish-step can settle that attempt (#652 A6).
func hasFailedCommandGate(state agentflow.NextActionState) bool {
	if state.State != "validation_missing" || state.Resumability == nil {
		return false
	}
	for _, gate := range state.Resumability.Gates {
		if gate.Kind == "command" && gate.Status == "failed" {
			return true
		}
	}
	return false
}

// validateRecoveryMutationSafety rejects finite enforced recovery when the
// fixed Agentflow operation is not atomic with its final lease check. A time
// estimate cannot prove safety: run may append lease_renewed after a pause,
// finish-step records verification before rechecking expiry, and complete-step
// checks expiry before taking the separate close lock. Advisory and no-deadline
// attempts do not have these races. It also rejects, under any enforce policy, a
// failed-gate attempt whose block would leave step_unclaimed (#652 A6).
func validateRecoveryMutationSafety(state agentflow.NextActionState) error {
	projection := state.Resumability
	if projection == nil || projection.Lease == nil || projection.Lease.Policy == nil || *projection.Lease.Policy != "enforce" {
		return nil
	}
	switch state.State {
	case "step_unclaimed":
		return fmt.Errorf("agentflow resume blocks step_unclaimed under an enforced lease because claiming would enter non-atomic gate recovery")
	case "step_amendment_needed":
		return fmt.Errorf("agentflow resume blocks step_amendment_needed under an enforced lease because amending would enter non-atomic gate recovery")
	case "attempt_timed_out":
		// Blocking leaves step_unclaimed or step_amendment_needed, which an
		// enforced lease refuses above, so refuse before the block.
		return fmt.Errorf("agentflow resume blocks attempt_timed_out under an enforced lease because blocking the attempt would leave a step to claim or amend, which an enforced lease refuses")
	}
	if projection.Lease.State == "live" && projection.Lease.ExpiresAt != nil {
		switch state.State {
		case "validation_missing", "step_unverified", "step_uncompleted":
			return fmt.Errorf("agentflow resume blocks %s under a finite enforced lease because Agentflow cannot guarantee duplicate-free, no-renew recovery", state.State)
		}
	}
	// #652 A6: blocking a failed-gate attempt leaves step_unclaimed, which the
	// first check refuses under any enforce policy, so refuse before the block.
	if hasFailedCommandGate(state) {
		return fmt.Errorf("agentflow resume blocks validation_missing with a failed command gate under an enforced lease because blocking the attempt would leave step_unclaimed, which an enforced lease refuses")
	}
	return nil
}

func settleAgentflowAttempt(ctx context.Context, client afClient, plan *agentflow.Plan, state agentflow.NextActionState) (bool, error) {
	switch state.State {
	case "step_unclaimed", "step_amendment_needed", "run_unverified", "proof_missing", "complete":
		return false, nil
	case "validation_missing", "step_unverified", "step_uncompleted", "attempt_timed_out":
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
	case "attempt_timed_out":
		// AgentFlow 1.1 refuses to complete the attempt; blocking it lets a
		// replacement supersede its timed-out receipt (agentflow#60, #70).
		if err := recordBlocked(ctx, client, stepID, attemptID, resumeTimedOutBlockReason); err != nil {
			return true, fmt.Errorf("step %s attempt %s: gate timed out; %w", stepID, attemptID, err)
		}
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
		// #652 A6: a failed gate's receipt stays this attempt's (AgentFlow
		// credits the first matching one), so no gate run or finish-step can
		// settle it. Block it and run no gate, not even a missing one; the
		// serial loop then claims a new attempt and runs the model again.
		for i, command := range commands {
			if projected[i].Status != "failed" {
				continue
			}
			if err := recordBlocked(ctx, client, stepID, attemptID, resumeFailedGateBlockReason); err != nil {
				return true, fmt.Errorf("step %s attempt %s: gate %q already failed; %w", stepID, attemptID, command.Label, err)
			}
			return true, nil
		}
		for i, command := range commands {
			if projected[i].Status != "missing" {
				continue
			}
			if err := client.RunGate(ctx, stepID, attemptID, command.Label, command.Argv); err != nil {
				return true, blockAttempt(ctx, client, stepID, attemptID, gateBlockReason, fmt.Errorf("gate %q failed: %w", command.Label, err))
			}
		}
		if err := client.FinishStep(ctx, stepID, attemptID); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (d *driver) resume(ctx context.Context, root string, planJSON []byte, approved *agentflow.WorkflowRecommendation) (agentflow.NextActionState, error) {
	// #612 R6: resume mutates, so it runs the base probe (version gate and the
	// base capability probes, including #611's block-step) before its first
	// next-action. It does not probe every command resume later uses.
	if err := d.af.Probe(ctx); err != nil {
		return agentflow.NextActionState{}, &agentflowUnavailableError{fmt.Errorf("agentflow unavailable: %w", err)}
	}
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
	if err := refuseCompletedStepAttempt(state); err != nil {
		return agentflow.NextActionState{}, err
	}
	if err := validateRecoveryMutationSafety(state); err != nil {
		return agentflow.NextActionState{}, err
	}
	if disposition.action == reportComplete {
		return state, nil
	}
	if err := refuseReviewAmendmentReplacement(root, state); err != nil {
		return agentflow.NextActionState{}, err
	}
	// #642 A9: no recovery of a root that retains a timed-out receipt no
	// attempt can supersede passes run verification, so refuse before any
	// mutation.
	if err := refuseUnrecoverableTimeout(root); err != nil {
		return agentflow.NextActionState{}, err
	}
	proof, err := newResumeProofGuard(ctx, d.af, root)
	if err != nil {
		return agentflow.NextActionState{}, err
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
		switch state.State {
		case "complete":
			return state, nil
		case "step_unclaimed", "step_amendment_needed", "run_unverified", "proof_missing":
		case "proof_stale":
			// #689: the retained proof verified before settlement. The tail
			// also checks its bytes have not changed before replacing it;
			// proof_stale alone cannot establish either fact.
		default:
			disposition = resumeDisposition(state.State)
			if disposition.action == failClosed {
				return agentflow.NextActionState{}, fmt.Errorf("agentflow resume blocked after settlement in state %q: %s", state.State, disposition.description)
			}
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
				return fmt.Errorf("agentflow resumed step %q already has %s command gate %q; refusing duplicate execution", step.ID, gate.Status, gate.Label)
			}
		}
		return validateRecoveryMutationSafety(projected)
	}
	defer func() { d.beforeGates = previousBeforeGates }()
	if state.State == "step_amendment_needed" {
		// The serial loop never selects a completed step, so resume opens its
		// replacement attempt and runs the model for it here (agentflow#70).
		if err := d.amendCompletedStep(ctx, root, state); err != nil {
			return agentflow.NextActionState{}, err
		}
	}
	if err := d.runSerialSteps(ctx); err != nil {
		return agentflow.NextActionState{}, err
	}
	if _, err := d.finishRun(ctx, proof); err != nil {
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

// amendCompletedStep opens a replacement attempt for the completed step a
// step_amendment_needed state projects and runs it as the serial loop runs a
// claimed one: the step's own goal, then its gates and finish-step. It refuses
// a step with an unfinished review amendment first, also when settlement led
// here. validateRecoveryProjection has checked the step and that AgentFlow
// allows the amend. One amendment per resume: a second step that needs one
// stops resume at the step-work check before finish-run, and the next resume
// amends it.
func (d *driver) amendCompletedStep(ctx context.Context, root string, state agentflow.NextActionState) error {
	if err := refuseReviewAmendmentReplacement(root, state); err != nil {
		return err
	}
	stepID := state.Resumability.Step.ID
	step, ok := findStep(d.plan, stepID)
	if !ok {
		return fmt.Errorf("agentflow returned unknown step %q", stepID)
	}
	goal, err := stepGoal(d.plan, step)
	if err != nil {
		return err
	}
	attempt, err := d.af.AmendCompletedStep(ctx, stepID, resumeAmendReason)
	if err != nil {
		return err
	}
	return d.runAttempt(ctx, step, attempt, goal)
}

// renderAgentflowStatus prints the human status layout. showBlockers adds the
// current recorded blockers; only -agentflow-status passes it (#731 B5).
func renderAgentflowStatus(out io.Writer, state agentflow.NextActionState, proof *agentflow.ProofSummary, disposition recoveryDisposition, showBlockers bool) {
	blocking := state.Blocking || disposition.action == failClosed
	_, _ = fmt.Fprintf(out, "state: %s\nreason: %s\nblocking: %t\n", recoveryDisplayText(state.State), recoveryDisplayText(state.Reason), blocking)
	if state.StepID != nil {
		_, _ = fmt.Fprintf(out, "step: %s\n", recoveryDisplayText(*state.StepID))
	}
	if state.Gate != nil {
		_, _ = fmt.Fprintf(out, "current gate: %s\n", recoveryDisplayText(*state.Gate))
	}
	if projection := state.Resumability; projection != nil {
		if state.StepID == nil && projection.Step != nil {
			_, _ = fmt.Fprintf(out, "step: %s\n", recoveryDisplayText(projection.Step.ID))
		}
		if projection.Attempt != nil {
			_, _ = fmt.Fprintf(out, "attempt: %s owner=%s open=%t\n", recoveryDisplayText(projection.Attempt.ID), recoveryDisplayText(projection.Attempt.Owner), projection.Attempt.Open)
		}
		if projection.Lease != nil {
			policy := "unknown"
			if projection.Lease.Policy != nil {
				policy = *projection.Lease.Policy
			}
			_, _ = fmt.Fprintf(out, "lease: policy=%s state=%s", recoveryDisplayText(policy), recoveryDisplayText(projection.Lease.State))
			if projection.Lease.ExpiresAt != nil {
				_, _ = fmt.Fprintf(out, " expires=%s", recoveryDisplayText(*projection.Lease.ExpiresAt))
			}
			_, _ = fmt.Fprintln(out)
		}
		for _, gate := range projection.Gates {
			_, _ = fmt.Fprintf(out, "gate: %s %s (%s)\n", recoveryDisplayText(gate.Kind), recoveryDisplayText(gate.Label), recoveryDisplayText(gate.Status))
		}
		for _, diagnostic := range projection.Diagnostics {
			_, _ = fmt.Fprintf(out, "diagnostic: [%s] %s", recoveryDisplayText(diagnostic.Code), recoveryDisplayText(diagnostic.Message))
			if diagnostic.Artifact != "" {
				_, _ = fmt.Fprintf(out, " (%s)", recoveryDisplayText(diagnostic.Artifact))
			}
			_, _ = fmt.Fprintln(out)
		}
	}
	for _, diagnostic := range state.Diagnostics {
		_, _ = fmt.Fprintf(out, "diagnostic: %s\n", recoveryDisplayText(diagnostic))
	}
	if showBlockers {
		renderRecordedBlockers(out, state.RecordedBlockers)
	}
	if state.Command != "" || len(state.Args) > 0 {
		_, _ = fmt.Fprintf(out, "advisory (display only): command=%q args=%q\n", state.Command, state.Args)
	}
	_, _ = fmt.Fprintf(out, "resume: %s\n", recoveryDisplayText(disposition.description))
	if proof != nil {
		_, _ = fmt.Fprintf(out, "proof: verified\nartifact: %s\nchecks: passed=%d warning=%d failed=%d not_run=%d skipped=%d not_applicable=%d total=%d\n",
			recoveryDisplayText(proof.Path), proof.Passed, proof.Warning, proof.Failed, proof.NotRun, proof.Skipped, proof.NotApplicable, proof.Total)
	}
}

// renderResumeResult prints a successful resume's final state. It shares the
// status layout but not the recorded blockers, which belong to
// -agentflow-status (#731 B5).
func renderResumeResult(out io.Writer, final agentflow.NextActionState, summary agentflow.ProofSummary) {
	renderAgentflowStatus(out, final, &summary, resumeDisposition(final.State), false)
}

// recordedBlockerReasonRunes caps a blocker reason before escaping, so an
// escape sequence is never split (#731 B3).
const recordedBlockerReasonRunes = 200

// renderRecordedBlockers prints next-action's current recorded blockers, one
// line each, then one line for any entries it could not read (#731 B2-B3).
// Display only: nothing branches on them.
func renderRecordedBlockers(out io.Writer, raw json.RawMessage) {
	blockers := agentflow.ParseRecordedBlockers(raw)
	if blockers.NotAnArray {
		_, _ = fmt.Fprintln(out, "recorded blockers: not shown (recorded_blockers is not an array); see -agentflow-status -json")
		return
	}
	for _, b := range blockers.Current {
		at := ""
		if b.RecordedAt != nil {
			at = " at " + recoveryDisplayText(*b.RecordedAt)
		}
		owner := "no owner"
		if b.AttemptOwner != nil {
			owner = "owner " + recoveryDisplayText(*b.AttemptOwner)
		}
		reason := "(no reason)"
		if b.Reason != nil {
			reason = *b.Reason
			if runes := []rune(reason); len(runes) > recordedBlockerReasonRunes {
				reason = string(runes[:recordedBlockerReasonRunes]) + "..."
			}
			reason = recoveryDisplayText(reason)
		}
		_, _ = fmt.Fprintf(out, "recorded blocker: %s/%s %s%s (%s): %s\n",
			recoveryDisplayText(b.StepID), recoveryDisplayText(b.AttemptID), recoveryDisplayText(b.Event), at, owner, reason)
	}
	if blockers.Malformed > 0 {
		_, _ = fmt.Fprintf(out, "recorded blockers: %d malformed entries not shown; see -agentflow-status -json\n", blockers.Malformed)
	}
}

func recoveryDisplayText(value string) string {
	quoted := previewText(value)
	return quoted[1 : len(quoted)-1]
}
