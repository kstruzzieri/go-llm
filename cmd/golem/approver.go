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

// Compile-time assertions: replApprover must satisfy both approver contracts.
var _ agent.Approver = (*replApprover)(nil)
var _ agent.KeyedApprover = (*replApprover)(nil)

func newReplApprover(src lineSource, out io.Writer, color bool) *replApprover {
	return &replApprover{src: src, out: out, color: color}
}

// Approve keeps the plain agent.Approver contract: no key, so never grantable.
func (a *replApprover) Approve(ctx context.Context, call provider.ToolCall, preview string) (bool, error) {
	d, err := a.ApproveKeyed(ctx, call, preview, "")
	return d.Approved, err
}

// ApproveKeyed shows the preview and reads one line. "y"/"yes" (case-insensitive)
// approves. "n", empty, and EOF deny with a nil error. A canceled context (Ctrl-C)
// denies and returns the context error so the run aborts.
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
func (a *replApprover) ApproveKeyed(ctx context.Context, call provider.ToolCall, preview, key string) (agent.ApprovalDecision, error) {
	if a.beforeWrite != nil {
		if err := a.beforeWrite(); err != nil {
			return agent.ApprovalDecision{}, err
		}
	}
	isExec := call.Function.Name == "run_command"
	isMCP := strings.HasPrefix(call.Function.Name, "mcp__")
	isPlan := call.Function.Name == submitPlanToolName
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
	case isExec:
		a.renderPlain(preview)
		question = grantQuestion("Run this command?", grantable, "a=always this command")
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
