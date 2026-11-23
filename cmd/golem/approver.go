package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kstruzzieri/go-llm/agent"
	"github.com/kstruzzieri/go-llm/agent/interceptor"
	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/conversation"
	"github.com/kstruzzieri/go-llm/internal/promptfence"
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
	// hostCommands reports that the session's commands run on the host, so a
	// fresh prompt for a command, background start or verifier says the
	// network is not confined (#584).
	hostCommands bool
}

// Compile-time assertions: replApprover must satisfy all four approver contracts.
var _ agent.Approver = (*replApprover)(nil)
var _ agent.KeyedApprover = (*replApprover)(nil)
var _ agent.RiskApprover = (*replApprover)(nil)
var _ agent.FreshApprover = (*replApprover)(nil)

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
// by agenttools.VerifierSlot's approveVerify and by tests).
func (a *replApprover) ApproveKeyed(ctx context.Context, call provider.ToolCall, preview, key string) (agent.ApprovalDecision, error) {
	return a.ApproveWithRisk(ctx, call, preview, key, agent.RiskReport{})
}

// ApproveWithRisk is the ordinary entry to ask, the one prompt body (#514
// D3). It shows the preview and reads one line. "y"/"yes" (case-insensitive)
// approves. "n", empty, and EOF deny with a nil error. A canceled context
// (Ctrl-C) denies and returns the context error so the run aborts.
// The prompt and rendering are action-neutral: run_command and MCP calls get a
// plain preview and run prompt; the agent-memory writes show their arguments;
// all other calls get the diff rendering and "Apply this change?" prompt, with
// the tool name and its arguments in place of an empty preview.
//
// Grant path (#341): grantability requires ALL of a non-empty structural key, a
// grant store, and a tool name on the grantScope allowlist — scope comes from
// the name, never from the key, so a colliding key cannot transfer
// authorization across tools. A grantable call whose (scope, key) already
// holds a session grant auto-approves (ViaGrant) after rendering the same
// preview the prompt would have shown. A grantable prompt offers "a" (always
// this session): approve now and store the grant. MCP tools and submit_plan
// are never on the allowlist. The key is opaque: never parsed, never derived
// from the preview. A write_file or edit_file call with the empty key is a
// demand for a human (#725): a harness target or a spelling the tool could
// not verify. It prompts [y/N] with a notice line after the diff.
func (a *replApprover) ApproveWithRisk(ctx context.Context, call provider.ToolCall, preview, key string, risk agent.RiskReport) (agent.ApprovalDecision, error) {
	return a.ask(ctx, call, preview, key, risk, harnessFileCall(call.Function.Name, key), nil)
}

// ApproveFresh is the agent.FreshApprover entry (#584): a strict run whose
// conversation is exposed asks here about this one call. It prints the
// suspension notice, and on the host runtime "network is not confined" for a
// command, background start or verifier, then runs the one prompt body with
// the empty key, so no grant is looked up or stored, the question is [y/N]
// and "a" denies. keyed decides only the #725 harness notice: a file-tool
// call that planned without a key keeps it, by the same conservative rule as
// harnessFileCall.
func (a *replApprover) ApproveFresh(ctx context.Context, call provider.ToolCall, preview string, keyed bool, risk agent.RiskReport, exposure agent.Exposure) (bool, error) {
	notice := []string{suspendedNotice(exposure)}
	scope := grantScope(call.Function.Name)
	if a.hostCommands && (scope == grantScopeExec || scope == grantScopeVerify) {
		notice = append(notice, "network is not confined")
	}
	harness := scope == grantScopeFiles && !keyed
	d, err := a.ask(ctx, call, preview, "", risk, harness, notice)
	return d.Approved, err
}

// suspendedNotice is the line a fresh prompt opens with (#584).
func suspendedNotice(e agent.Exposure) string {
	foreign, label := exposureSource(e)
	why := "this conversation's provenance is unknown"
	if foreign {
		why = "outside content entered this conversation"
	}
	if label != "" {
		why += " (" + label + ")"
	}
	return "reusable approvals suspended: " + why + "; /new or /clear starts fresh"
}

// exposureSource reads an exposed record for the strict notice and /grants
// (#584): whether outside content entered it, and a label naming the first
// exposing source. The label is First, bounded, without its "tool:" prefix
// and made one inert line as riskLine makes its badge, because an MCP tool
// name comes from the server. A missing record reads as unknown "history",
// as the agent reads it, and so does an unknown one with no First; a foreign
// one with no First has no label.
func exposureSource(e agent.Exposure) (foreign bool, label string) {
	if !e.Recorded {
		return false, "history"
	}
	label = strings.TrimPrefix(conversation.BoundExposureFirst(e.First), "tool:")
	label = sanitizeApprovalPreview(promptfence.FlattenLine(label))
	if label == "" && !e.Foreign {
		label = "history"
	}
	return e.Foreign, label
}

// ask is the one prompt body behind ApproveWithRisk and ApproveFresh. notice
// lines print after beforeWrite and before the preview; harness adds the #725
// notice after a diff.
func (a *replApprover) ask(ctx context.Context, call provider.ToolCall, preview, key string, risk agent.RiskReport, harness bool, notice []string) (agent.ApprovalDecision, error) {
	if a.beforeWrite != nil {
		if err := a.beforeWrite(); err != nil {
			// The question will not be asked, so it ends the REPL rather
			// than leave the line typed for it to be read as a goal.
			return agent.ApprovalDecision{}, questionNotShown(err)
		}
	}
	for _, line := range notice {
		_, _ = fmt.Fprintln(a.out, line)
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
	// #584: the agent-memory writes ask only in an exposed strict
	// conversation. They do not plan, so the preview is empty and the
	// arguments are the display. Neither has a grant scope.
	case call.Function.Name == agenttools.AgentMemoryCreateToolName:
		_, _ = fmt.Fprintln(a.out, "agent memory note:")
		a.renderPlain(memoryNoteText(call.Function.Arguments))
		question = "Save this agent memory note? [y/N] "
	case call.Function.Name == agenttools.AgentMemoryPromoteToolName:
		a.renderPlain(memoryPromoteLine(call.Function.Arguments))
		question = "Promote this agent memory record? [y/N] "
	default:
		if preview == "" {
			// A tool that does not plan has no preview; show the call itself
			// rather than an empty diff.
			a.renderPlain(rawArgsLine(call.Function.Name + " " + string(call.Function.Arguments)))
		} else {
			a.renderDiff(preview)
		}
		// #725: an empty file-tool key marks a harness file. grantable is
		// already false, so the prompt is [y/N]; the notice says why.
		if harness {
			_, _ = fmt.Fprintln(a.out, "harness file: "+agenttools.HarnessApprovalNotice)
		}
		// The legend spells out the asymmetry: an exec grant covers one exact
		// command, but a write/edit grant covers the whole class — "a" here is
		// /auto-edits on for the session, not "always this file".
		question = grantQuestion("Apply this change?", grantable, "a=all edits this session")
	}
	// #514 D3: the run's cumulative report, between the preview and the
	// decision, on the prompted and the grant-covered path alike. #439 D6:
	// the same line carries this call's egress badge when the report's
	// current-call findings hold one.
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

// sanitizeApprovalPreview keeps its name for the callers that render untrusted
// text (this approver, render.go, ops_table.go); the shared implementation
// lives in agent.SanitizePreview so embedded approvers get the same rules.
func sanitizeApprovalPreview(preview string) string {
	return agent.SanitizePreview(preview)
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
// D3) and, when the report carries the approved call's own egress
// classification, the #439 badge on the same line. Findings, not the score,
// gate the line: a zero-risk finding is still something the human should
// see. The badge label is the one place a finding detail reaches a prompt,
// so the suffix is flattened to one line first (the preview sanitizer keeps
// newlines on purpose) and then neutralized like a preview.
func riskLine(risk agent.RiskReport) string {
	if len(risk.Findings) == 0 {
		return ""
	}
	line := fmt.Sprintf("interceptor risk %d", risk.Score)
	if f, ok := currentEgress(risk); ok {
		line += sanitizeApprovalPreview(promptfence.FlattenLine(" · egress: " + f.Rule + " (" + f.Detail + ")"))
	}
	return line
}

// egressName and egressClasses come from the classifier itself, so the badge
// accepts exactly the classes it emits.
var (
	egressName    = interceptor.Egress{}.Name()
	egressClasses = map[string]bool{
		interceptor.EgressPrivileged:     true,
		interceptor.EgressNetwork:        true,
		interceptor.EgressPackageManager: true,
		interceptor.EgressInterpreter:    true,
		interceptor.EgressUnknown:        true,
	}
)

// currentEgress returns the first egress classification among the approved
// call's own findings (#439 D6). Only CurrentToolCallFindings is consulted:
// cumulative history may hold an earlier call's badge, and provider IDs may
// be empty or reused, so neither identifies this call. A finding is accepted
// only when it came from the egress interceptor on the tool-call hook with a
// tool-call target and names a known class.
func currentEgress(risk agent.RiskReport) (agent.Finding, bool) {
	for _, f := range risk.CurrentToolCallFindings {
		if f.Interceptor == egressName && f.Hook == agent.HookToolCall && f.Target == agent.TargetToolCall && egressClasses[f.Rule] {
			return f, true
		}
	}
	return agent.Finding{}, false
}

// memoryNoteText renders an agent_memory_create call's note as the tool will
// store it: decoded as the tool decodes it and trimmed as the tool trims it.
// Each sanitized line is marked "| " and a size line follows, so a note
// cannot fake its own header or push the real one off screen and still end
// right above the question. A note over the tool's limit shows only its size,
// since it will not be saved. Arguments that do not decode, or a note that is
// empty after trimming, show as the one-line raw arguments.
func memoryNoteText(args json.RawMessage) string {
	var a struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(args, &a) != nil {
		return rawArgsLine(string(args))
	}
	note := strings.TrimSpace(a.Content)
	if note == "" {
		return rawArgsLine(string(args))
	}
	n := strings.Count(note, "\n") + 1
	lines := "lines"
	if n == 1 {
		lines = "line"
	}
	size := fmt.Sprintf("(note: %d %s, %d bytes", n, lines, len(note))
	if len(note) > agenttools.MaxAgentMemoryContentBytes {
		return fmt.Sprintf("%s: over the %d-byte limit, so it will not be saved)", size, agenttools.MaxAgentMemoryContentBytes)
	}
	var b strings.Builder
	for _, line := range strings.Split(sanitizeApprovalPreview(note), "\n") {
		b.WriteString(strings.TrimRight("| "+line, " "))
		b.WriteByte('\n')
	}
	return b.String() + size + ")"
}

// memoryPromoteLine is an agent_memory_promote call as one sanitized line,
// or the raw arguments when they do not decode to an id and a kind.
func memoryPromoteLine(args json.RawMessage) string {
	var a struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	if json.Unmarshal(args, &a) != nil || a.ID == "" || a.Kind == "" {
		return rawArgsLine(string(args))
	}
	return rawArgsLine("promote agent memory record " + a.ID + " to " + a.Kind)
}

// rawArgsLine makes model-written call text one inert display line.
func rawArgsLine(s string) string {
	return sanitizeApprovalPreview(promptfence.FlattenLine(s))
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
