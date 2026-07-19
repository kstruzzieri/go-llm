package main

import (
	"context"
	"fmt"
	"io"

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
		return err
	}
	disposition := resumeDisposition(state.State)
	if jsonOutput {
		if _, err := out.Write(state.RawJSON); err != nil {
			return fmt.Errorf("write Agentflow status: %w", err)
		}
		return statusExit(disposition.exitCode)
	}

	var proof *agentflow.ProofSummary
	if disposition.action == reportComplete {
		summary, err := client.ProofSummary()
		if err != nil {
			return err
		}
		proof = &summary
	}
	renderAgentflowStatus(out, state, proof)
	return statusExit(disposition.exitCode)
}

func statusExit(code int) error {
	if code == 0 {
		return nil
	}
	return &agentflowStatusExit{code: code}
}

func renderAgentflowStatus(out io.Writer, state agentflow.NextActionState, proof *agentflow.ProofSummary) {
	fmt.Fprintf(out, "state: %s\nreason: %s\nblocking: %t\n", state.State, state.Reason, state.Blocking)
	if state.StepID != nil {
		fmt.Fprintf(out, "step: %s\n", *state.StepID)
	}
	if state.Gate != nil {
		fmt.Fprintf(out, "current gate: %s\n", *state.Gate)
	}
	if projection := state.Resumability; projection != nil {
		if state.StepID == nil && projection.Step != nil {
			fmt.Fprintf(out, "step: %s\n", projection.Step.ID)
		}
		if projection.Attempt != nil {
			fmt.Fprintf(out, "attempt: %s owner=%s open=%t\n", projection.Attempt.ID, projection.Attempt.Owner, projection.Attempt.Open)
		}
		if projection.Lease != nil {
			policy := "unknown"
			if projection.Lease.Policy != nil {
				policy = *projection.Lease.Policy
			}
			fmt.Fprintf(out, "lease: policy=%s state=%s", policy, projection.Lease.State)
			if projection.Lease.ExpiresAt != nil {
				fmt.Fprintf(out, " expires=%s", *projection.Lease.ExpiresAt)
			}
			fmt.Fprintln(out)
		}
		for _, gate := range projection.Gates {
			fmt.Fprintf(out, "gate: %s %s (%s)\n", gate.Kind, gate.Label, gate.Status)
		}
		for _, diagnostic := range projection.Diagnostics {
			fmt.Fprintf(out, "diagnostic: [%s] %s", diagnostic.Code, diagnostic.Message)
			if diagnostic.Artifact != "" {
				fmt.Fprintf(out, " (%s)", diagnostic.Artifact)
			}
			fmt.Fprintln(out)
		}
	}
	for _, diagnostic := range state.Diagnostics {
		fmt.Fprintf(out, "diagnostic: %s\n", diagnostic)
	}
	if state.Command != "" || len(state.Args) > 0 {
		fmt.Fprintf(out, "advisory (display only): command=%q args=%q\n", state.Command, state.Args)
	}
	fmt.Fprintf(out, "resume: %s\n", resumeDisposition(state.State).description)
	if proof != nil {
		fmt.Fprintf(out, "proof: verified\nartifact: %s\nchecks: passed=%d warning=%d failed=%d total=%d\n",
			proof.Path, proof.Passed, proof.Warning, proof.Failed, proof.Total)
	}
}
