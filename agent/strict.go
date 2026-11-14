package agent

import (
	"context"
	"slices"

	"github.com/kstruzzieri/go-llm/provider"
)

// FreshApprover is optionally implemented by an Approver that can ask a
// person about a single call (#584). While approvals are suspended
// (ApprovalsSuspended), every approval a strict run needs, the post-write
// verifier's included, goes to ApproveFresh instead of Approve, ApproveKeyed
// or ApproveWithRisk. An approver that does not implement it is denied
// without being asked.
//
// An implementation must ask a person about this exact call, and must not
// consult, reuse or create any reusable grant. The approval key is withheld,
// but withholding it is not by itself what guarantees that; this contract
// is. A yes approves this call only, and the run never records it as
// approved through a grant.
//
// preview is sanitized display text and is empty for a tool that does not
// plan, so call must be rendered as well. keyed reports whether the original
// request carried an approval key; it is display information only, so a host
// can keep its notice for a target that always prompts. risk is the run's
// cumulative report, empty for the verifier. exposure is the conversation's
// record at the time of the call, naming what exposed it.
type FreshApprover interface {
	ApproveFresh(ctx context.Context, call provider.ToolCall, preview string, keyed bool, risk RiskReport, exposure Exposure) (bool, error)
}

// ApprovalsSuspended reports whether reusable approvals are suspended for the
// run ctx belongs to: the run is strict (Request.StrictApproval, its own or
// an enclosing run's) and the conversation's exposure record, shared with
// nested runs, reads as Exposed. It is false outside a run. A tool's Invoke
// receives a context derived from its run's, so a tool can ask as well.
func ApprovalsSuspended(ctx context.Context) bool {
	r := exposureFrom(ctx)
	// A nil node's snapshot is the zero value, which reads as exposed.
	return r != nil && r.strict && r.snapshot().Exposed()
}

// strictExempt reports whether name is in this run's own StrictExempt.
func strictExempt(ctx context.Context, name string) bool {
	r := exposureFrom(ctx)
	return r != nil && slices.Contains(r.exempt, name)
}

// strictApprover is a strict run's approver. RunWithVerifier installs it in
// place of a non-nil Request.Approver, so dispatch and the verifier both reach
// it. It reads the shared exposure record at each decision: while clean, each
// method reaches inner exactly as the runtime would without the wrapper; once
// exposed, every method asks inner's FreshApprover, an inner approver without
// one is denied, and no decision carries ViaGrant.
type strictApprover struct {
	inner    Approver
	exposure *exposureRun
}

var (
	_ RiskApprover  = (*strictApprover)(nil)
	_ KeyedApprover = (*strictApprover)(nil)
)

// ApproveWithRisk is dispatch's path: approve prefers it.
func (g *strictApprover) ApproveWithRisk(ctx context.Context, call provider.ToolCall, preview, approvalKey string, risk RiskReport) (ApprovalDecision, error) {
	if exp := g.exposure.snapshot(); exp.Exposed() {
		return g.fresh(ctx, call, preview, approvalKey != "", risk, exp)
	}
	return approve(ctx, g.inner, call, preview, approvalKey, risk)
}

// ApproveKeyed is the verifier's path; while clean it picks keyed or plain
// as the verifier itself would.
func (g *strictApprover) ApproveKeyed(ctx context.Context, call provider.ToolCall, preview, approvalKey string) (ApprovalDecision, error) {
	if exp := g.exposure.snapshot(); exp.Exposed() {
		return g.fresh(ctx, call, preview, approvalKey != "", RiskReport{}, exp)
	}
	if ka, ok := g.inner.(KeyedApprover); ok {
		return ka.ApproveKeyed(ctx, call, preview, approvalKey)
	}
	ok, err := g.inner.Approve(ctx, call, preview)
	return ApprovalDecision{Approved: ok}, err
}

// Approve serves any caller that holds only the Approver interface.
func (g *strictApprover) Approve(ctx context.Context, call provider.ToolCall, preview string) (bool, error) {
	if exp := g.exposure.snapshot(); exp.Exposed() {
		d, err := g.fresh(ctx, call, preview, false, RiskReport{}, exp)
		return d.Approved, err
	}
	return g.inner.Approve(ctx, call, preview)
}

// fresh asks inner's FreshApprover about this one call, or denies it with a
// nil error when inner cannot ask a person.
func (g *strictApprover) fresh(ctx context.Context, call provider.ToolCall, preview string, keyed bool, risk RiskReport, exp Exposure) (ApprovalDecision, error) {
	fa, ok := g.inner.(FreshApprover)
	if !ok {
		return ApprovalDecision{}, nil
	}
	yes, err := fa.ApproveFresh(ctx, call, SanitizePreview(preview), keyed, risk, exp)
	return ApprovalDecision{Approved: yes}, err
}
