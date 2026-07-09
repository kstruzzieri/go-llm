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
	Init(context.Context) error
	LockPlan(context.Context, string) error
	InitExecution(context.Context) error
	Doctor(context.Context) error
	NextStep(context.Context) (string, error)
	ClaimStep(context.Context, string) (string, error)
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
type runStepFunc func(ctx context.Context, step agentflow.Step, attempt string) error

type driver struct {
	af       afClient
	plan     *agentflow.Plan
	planPath string
	evidence []agentflow.EvidenceEntry
	runStep  runStepFunc
	out      io.Writer
}

// run drives the P0 sequence and returns the proof-pack path on success.
func (d *driver) run(ctx context.Context) (string, error) {
	if err := d.af.Probe(ctx); err != nil {
		return "", fmt.Errorf("agentflow unavailable: %w", err)
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
	if err := d.runStep(ctx, step, attempt); err != nil {
		return err // includes a fatal record-file-change failure surfaced via ctx cancel
	}
	gates, err := agentflow.ExtractCommandGates(step)
	if err != nil {
		return err
	}
	for _, g := range gates {
		if err := d.af.RunGate(ctx, id, attempt, g.Label, g.Argv); err != nil {
			return fmt.Errorf("gate %q failed: %w", g.Label, err)
		}
	}
	return d.af.FinishStep(ctx, id, attempt)
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
	evidence, err := readEvidenceSidecar(f.evidencePath)
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
	runStep := func(sctx context.Context, step agentflow.Step, attempt string) error {
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
			Goal:     stepGoal(step),
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
	d := &driver{af: client, plan: &plan, planPath: planPath, evidence: evidence, runStep: runStep, out: stdout}
	proof, err := d.run(runCtx)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agentflow task failed: %v\n", err)
		reportAgentflowRecovery(ctx, stderr, client)
		return errAgentflowTaskFailed
	}
	_, _ = fmt.Fprintf(stdout, "proof pack: %s\n", proof)
	return nil
}

func resolveTaskPlanPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Abs(path)
}

func stepGoal(s agentflow.Step) string {
	return fmt.Sprintf("Make exactly the change described. Action: %s\nTarget files: %s\nExpected diff:\n%s",
		s.Action, strings.Join(s.Files, ", "), strings.Join(s.ExpectedDiff, "\n"))
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
