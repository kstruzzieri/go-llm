package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/provider"
)

// replApprover renders a mutation's diff preview and prompts the user for a single
// [y/N] decision over the shared lineSource. It is wired into agent.Request.Approver
// only when a configured tool needs approval; otherwise the runtime's nil-approver
// fail-safe denies every mutating call.
type replApprover struct {
	src         lineSource
	out         io.Writer
	color       bool
	beforeWrite func() error
	// grants is the session grant store (#341). nil disables the grant path
	// entirely (the Agentflow author's plan-lock approver stays per-invocation).
	grants *approvalGrants
}

// Compile-time assertions: replApprover must satisfy all three approver contracts.
var _ agent.Approver = (*replApprover)(nil)
var _ agent.KeyedApprover = (*replApprover)(nil)
var _ agent.RiskApprover = (*replApprover)(nil)

func newReplApprover(src lineSource, out io.Writer, color bool) *replApprover {
	return &replApprover{src: src, out: out, color: color}
}

// Approve keeps the plain agent.Approver contract: no key, so never grantable.
func (a *replApprover) Approve(ctx context.Context, call provider.ToolCall, preview string) (bool, error) {
	d, err := a.ApproveKeyed(ctx, call, preview, "")
	return d.Approved, err
}

// ApproveKeyed is the KeyedApprover entry point: the same prompt with no
// interceptor report (the library prefers ApproveWithRisk, so this is reached
// by the verifier's approveVerify and by tests).
func (a *replApprover) ApproveKeyed(ctx context.Context, call provider.ToolCall, preview, key string) (agent.ApprovalDecision, error) {
	return a.ApproveWithRisk(ctx, call, preview, key, agent.RiskReport{})
}

// ApproveWithRisk is the one prompt body (#514 D3). It shows the preview and
// reads one line. "y"/"yes" (case-insensitive) approves. "n", empty, and EOF
// deny with a nil error. A canceled context (Ctrl-C) denies and returns the
// context error so the run aborts.
// The prompt and rendering are action-neutral: run_command and MCP calls get a
// plain preview and run prompt; all other calls get the diff rendering and "Apply
// this change?" prompt.
//
// Grant path (#341): grantability requires ALL of a non-empty structural key, a
// grant store, and a tool name on the grantScope allowlist — scope comes from
// the name, never from the key, so a colliding key cannot transfer
// authorization across tools. A grantable call whose (scope, key) already
// holds a session grant auto-approves (ViaGrant) after rendering the same
// preview the prompt would have shown. A grantable prompt offers "a" (always
// this session): approve now and store the grant. MCP tools and submit_plan
// are never on the allowlist. The key is opaque: never parsed, never derived
// from the preview.
func (a *replApprover) ApproveWithRisk(ctx context.Context, call provider.ToolCall, preview, key string, risk agent.RiskReport) (agent.ApprovalDecision, error) {
	if a.beforeWrite != nil {
		if err := a.beforeWrite(); err != nil {
			return agent.ApprovalDecision{}, err
		}
	}
	isExec := call.Function.Name == "run_command"
	isStart := call.Function.Name == "start_command"
	isStop := call.Function.Name == "stop_command"
	isMCP := strings.HasPrefix(call.Function.Name, "mcp__")
	isPlan := call.Function.Name == submitPlanToolName
	isVerify := call.Function.Name == verifyToolName
	scope := grantScope(call.Function.Name)
	grantable := key != "" && a.grants != nil && scope != ""
	preview = sanitizeApprovalPreview(preview)
	// The preview still renders here; only the question moves, because the
	// source owns prompt printing and the editor repaints its prompt on every
	// asynchronous write.
	var question string
	switch {
	case isPlan:
		a.renderPlain(preview)
		question = "Lock this plan? [y/N] "
	case isVerify:
		// #347: an argv and a cwd, not a diff — rendered plain like exec. The
		// grant covers this exact verifier for the session.
		a.renderPlain(preview)
		question = grantQuestion("Run this verification command?", grantable, "a=always this verifier")
	case isExec:
		a.renderPlain(preview)
		question = grantQuestion("Run this command?", grantable, "a=always this command")
	case isStart:
		// #346: the grant covers this exact background command (the exec-bg
		// key), one-time approval otherwise.
		a.renderPlain(preview)
		question = grantQuestion("Start this background command?", grantable, "a=always this command")
	case isStop:
		// stop_command's ApprovalKey is "" by frozen contract, so grantable is
		// structurally false; the prompt stays yes/no and never shows a legend.
		a.renderPlain(preview)
		question = "Stop this background command? [y/N] "
	case isMCP:
		a.renderPlain(preview)
		question = "Run this MCP tool? [y/N] "
	default:
		a.renderDiff(preview)
		// The legend spells out the asymmetry: an exec grant covers one exact
		// command, but a write/edit grant covers the whole class — "a" here is
		// /auto-edits on for the session, not "always this file".
		question = grantQuestion("Apply this change?", grantable, "a=all edits this session")
	}
	// #514 D3: the run's cumulative report, between the preview and the
	// decision, on the prompted and the grant-covered path alike. The line
	// contains only fixed text plus the integer score.
	if line := riskLine(risk); line != "" {
		_, _ = fmt.Fprintln(a.out, line)
	}
	if grantable && a.grants.granted(scope, key) {
		_, _ = fmt.Fprintln(a.out, "auto-approved (session grant)")
		return agent.ApprovalDecision{Approved: true, ViaGrant: true}, nil
	}
	line, ok, err := a.src.ReadAnswer(ctx, question)
	if errors.Is(err, errInterrupted) {
		_, _ = fmt.Fprintln(a.out)
		// Normalized here, at the approval boundary, so runOnce and the
		// Agentflow author classify one shared error: an interrupted approval
		// IS a cancellation, and the editor-local sentinel must not leak into
		// either caller's error taxonomy.
		return agent.ApprovalDecision{}, context.Canceled
	}
	if err != nil {
		_, _ = fmt.Fprintln(a.out)
		return agent.ApprovalDecision{}, err // ctx canceled: abort the run
	}
	if !ok {
		_, _ = fmt.Fprintln(a.out)
		return agent.ApprovalDecision{}, nil // EOF: deny
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return agent.ApprovalDecision{Approved: true}, nil
	case "a", "always":
		if grantable {
			a.grants.grant(scope, key)
			return agent.ApprovalDecision{Approved: true}, nil
		}
		_, _ = fmt.Fprintln(a.out)
		return agent.ApprovalDecision{}, nil
	default:
		_, _ = fmt.Fprintln(a.out)
		return agent.ApprovalDecision{}, nil
	}
}

func sanitizeApprovalPreview(preview string) string {
	var safe strings.Builder
	for _, r := range preview {
		if r == '\n' || unicode.IsGraphic(r) {
			safe.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteToGraphic(string(r))
		safe.WriteString(quoted[1 : len(quoted)-1])
	}
	return safe.String()
}

// grantQuestion appends the grant answer only when this call can be granted.
// The legend names the grant's real scope in the prompt itself, because the
// two grantable classes are asymmetric (exact command vs whole write class)
// and a bare "a" would let the user assume the narrower one.
func grantQuestion(q string, grantable bool, legend string) string {
	if grantable {
		return q + " [y/N/" + legend + "] "
	}
	return q + " [y/N] "
}

// riskLine renders the run's cumulative interceptor score for a prompt (#514
// D3). Findings, not the score, gate the line: a zero-risk finding is still
// something the human should see.
func riskLine(risk agent.RiskReport) string {
	if len(risk.Findings) == 0 {
		return ""
	}
	return fmt.Sprintf("interceptor risk %d", risk.Score)
}

// renderPlain prints a non-diff preview verbatim (no +/- coloring).
func (a *replApprover) renderPlain(preview string) {
	_, _ = fmt.Fprintln(a.out, strings.TrimRight(preview, "\n"))
}

func (a *replApprover) renderDiff(preview string) {
	if !a.color {
		_, _ = fmt.Fprintln(a.out, preview)
		return
	}
	for _, ln := range strings.Split(strings.TrimRight(preview, "\n"), "\n") {
		switch {
		case strings.HasPrefix(ln, "+"):
			_, _ = fmt.Fprintf(a.out, "\x1b[32m%s\x1b[0m\n", ln) // green add
		case strings.HasPrefix(ln, "-"):
			_, _ = fmt.Fprintf(a.out, "\x1b[31m%s\x1b[0m\n", ln) // red remove
		default:
			_, _ = fmt.Fprintln(a.out, ln)
		}
	}
}
