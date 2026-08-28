package agent

import (
	"context"

	"github.com/kstruzzieri/go-llm/provider"
)

// WriteFileToolName and EditFileToolName name the built-in tools whose
// successful invocation changes files in the workspace tree (#347).
//
// They are declared here rather than in agent/tools because agent cannot
// import its own tool package (that package imports this one). agent/tools
// pins them against the real ToolSpec names so the pair cannot drift.
const (
	WriteFileToolName = "write_file"
	EditFileToolName  = "edit_file"
)

// Verifier is the consumer-supplied post-write check the Orchestrator runs
// once after a tool-call batch that successfully mutated workspace files
// (#347). It is not a model-visible tool: the model can neither call it nor
// see it in the tool schema.
//
// Verify returns the model-visible observation, which the Orchestrator appends
// to that batch's last successful workspace-write observation; "" appends
// nothing. Every outcome of the CHECK ITSELF is data on the string return —
// approval denial, spawn failure, non-zero exit, timeout, truncation — and
// none of them may fail the run.
//
// A non-nil error is reserved for what the check did not decide: an
// interrupted approval prompt, an approver or control-plane failure, or parent
// cancellation. The Orchestrator hard-aborts the run on it, matching dispatch's
// handling of approval errors. This channel cannot be replaced by a later
// ctx.Err() check: a consumer normalizes an interrupted prompt to
// context.Canceled at its own approval boundary WITHOUT cancelling the parent
// context, so ctx.Err() would report nil and the interrupt would be swallowed.
//
// approver is the run's approver so an implementation can gate execution. It
// may be nil, which an implementation must treat as fail-safe.
type Verifier interface {
	Verify(ctx context.Context, approver Approver) (string, error)
}

// WithVerifier installs the post-write verification hook (#347). A nil
// verifier leaves every batch byte-for-byte unchanged.
func WithVerifier(v Verifier) Option {
	return func(o *Orchestrator) { o.verifier = v }
}

// mutatesWorkspaceFiles reports whether a successful call to this tool changed
// files in the workspace tree.
//
// Deliberately NOT derived from EffectClass: Write also covers agent-memory
// writes, which touch no workspace file, and IsMutating additionally covers
// the exec tools — run_command already reports its own exit status and output,
// and start_command returns before its job has done anything to verify. Exact
// names are the smallest correct rule; a MutatesWorkspaceFiles capability is
// the upgrade path when a third such tool appears.
func mutatesWorkspaceFiles(name string) bool {
	return name == WriteFileToolName || name == EditFileToolName
}

// batch carries the post-batch policy inputs that the shared recordResult tail
// accumulates across BOTH dispatch paths.
type batch struct {
	// verifyAnchor is the index in State.Messages of the last observation in
	// this batch produced by a successfully invoked workspace-file mutator;
	// -1 when the batch mutated no workspace file.
	//
	// An index, never a pointer or slice element: State.Messages keeps being
	// appended to for the rest of the batch and may reallocate.
	verifyAnchor int
}

func newBatch() batch { return batch{verifyAnchor: -1} }

// note records one completed call's contribution to the post-batch policy. It
// is called from recordResult AFTER the observation has been appended, so
// len(state.Messages)-1 is that call's own message.
func (b *batch) note(state *State, call provider.ToolCall, rec ToolCallRecord, out ToolResult) {
	if rec.Invoked && !out.IsError && mutatesWorkspaceFiles(call.Function.Name) {
		b.verifyAnchor = len(state.Messages) - 1
	}
}

// verifyBatch applies the post-batch verification policy. See Verifier for why
// the error return cannot be folded into a ctx.Err() check, and why the
// ctx.Err() check is nonetheless kept: the error covers what the context does
// not know, the context check covers a cancellation that landed while the
// check was running, before State is mutated.
func (o *Orchestrator) verifyBatch(ctx context.Context, state *State, approver Approver, b *batch) error {
	if o.verifier == nil || b.verifyAnchor < 0 {
		return nil
	}
	out, err := o.verifier.Verify(ctx, approver)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if out == "" {
		return nil
	}
	state.Messages[b.verifyAnchor].Content += out
	return nil
}
