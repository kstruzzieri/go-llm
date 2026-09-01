package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/kstruzzieri/go-llm/agent"
)

// PromoteArtifact applies ONE captured scratch create to the canonical
// workspace (#443). It is deliberately singular — one path per prompt is the
// smallest honest transaction boundary (D7) — structurally ungrantable
// (empty ApprovalKey: every promotion prompts, the stop_command precedent),
// and it exists only when the factory received a PreparingJournal, so every
// canonical write it makes carries a write-ahead intent and a tracked
// after-mode for /undo.
type PromoteArtifact struct {
	ws *Workspace
	rt *scratchRuntime

	mu         sync.Mutex
	argsHash   string
	pending    promotePending
	hasPending bool
}

// promotePending is the immutable deep copy Plan caches and Invoke consumes:
// the selected change plus a digest of the whole outcome, so any store
// change between approval and execution refuses.
type promotePending struct {
	id     string
	path   string
	change scratchChange
	digest string
}

// NewPromoteArtifact builds the promotion tool over the shared runtime.
func NewPromoteArtifact(ws *Workspace, rt *scratchRuntime) *PromoteArtifact {
	return &PromoteArtifact{ws: ws, rt: rt}
}

type promoteArtifactArgs struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// Spec implements agent.Tool.
func (t *PromoteArtifact) Spec() agent.ToolSpec {
	return agent.ToolSpec{
		Name: "promote_artifact",
		Description: "Apply one newly created file from a captured scratch outcome to the real workspace. " +
			"Create-only: updates, deletes, and binary content are never promotable. The complete file content " +
			"is shown to the user and applies only after they approve this specific promotion.",
		Parameters: json.RawMessage(`{
  "type":"object",
  "properties":{
    "id":{"type":"string","description":"scratch id from a run_command/start_command result"},
    "path":{"type":"string","description":"workspace-relative path of one promotable create from scratch_changes"}
  },
  "required":["id","path"]
}`),
	}
}

// Effect implements agent.Tool: a canonical write that always prompts.
func (t *PromoteArtifact) Effect() agent.Effect {
	return agent.Effect{Class: agent.Write, Approval: agent.ApprovalAlways}
}

// promoteOutcomeDigest fingerprints an outcome's approval-relevant shape so
// Invoke can detect any store change after Plan.
func promoteOutcomeDigest(out scratchOutcome) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	write(out.id)
	write(strconv.FormatBool(out.truncated))
	for _, c := range out.changes {
		write(c.path)
		write(string(c.kind))
		write(c.hash)
		write(strconv.FormatInt(c.size, 10))
		write(c.mode.String())
		write(strconv.FormatBool(c.promotable))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Plan validates the selection and caches an immutable pending promotion.
// Any refusal happens here, before an approval prompt ever renders.
func (t *PromoteArtifact) Plan(_ context.Context, raw json.RawMessage) (agent.ToolPlan, error) {
	eff := t.Effect()
	var args promoteArtifactArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("invalid arguments: %w", err)
	}
	if args.ID == "" || args.Path == "" {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("id and path are required")
	}
	out, status := t.rt.store.get(args.ID)
	switch status {
	case scratchStatusUnknown:
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("unknown scratch id %q (evicted or never issued)", args.ID)
	case scratchStatusPending:
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("scratch %s is still pending; wait for the command to finish", args.ID)
	}
	if out.truncated {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("scratch %s is truncated; nothing in it is promotable", args.ID)
	}
	var selected *scratchChange
	for i := range out.changes {
		if out.changes[i].path == args.Path {
			selected = &out.changes[i]
			break
		}
	}
	if selected == nil {
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("scratch %s has no change for %q", args.ID, args.Path)
	}
	if !selected.promotable {
		reason := selected.reason
		if reason == "" {
			reason = "not promotable"
		}
		return agent.ToolPlan{Effect: eff}, fmt.Errorf("%q is not promotable: %s", args.Path, reason)
	}
	p := promotePending{id: args.ID, path: args.Path, change: *selected, digest: promoteOutcomeDigest(out)}
	t.mu.Lock()
	t.argsHash, t.pending, t.hasPending = ContentHash(raw), p, true
	t.mu.Unlock()
	preview := fmt.Sprintf(
		"promote scratch artifact:\n  id:     %s\n  path:   %q\n  size=%d mode=%04o hash=%s\n  parent: %q (identity pinned)\n%s",
		p.id, p.path, p.change.size, p.change.mode.Perm(), shortHash(p.change.hash), p.change.parent.path, p.change.preview)
	return agent.ToolPlan{Effect: eff, Preview: preview, ApprovalKey: ""}, nil
}

func shortHash(h string) string {
	if len(h) > fingerprintLen {
		return h[:fingerprintLen]
	}
	return h
}

// consumePending returns and clears the cached plan only on an exact
// raw-args hash match, so Invoke fails closed on any drift.
func (t *PromoteArtifact) consumePending(argsHash string) (promotePending, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.hasPending || t.argsHash != argsHash {
		return promotePending{}, false
	}
	p := t.pending
	t.hasPending, t.pending, t.argsHash = false, promotePending{}, ""
	return p, true
}

// Invoke consumes the approved pending promotion, atomically claims the
// (id, path), re-verifies the store digest, and applies the single create
// through the write-ahead journal with a descriptor-anchored, durable,
// no-replace install. A pre-write failure releases the claim for retry; a
// journal-commit or post-verify failure after the filesystem commit marks
// the path consumed/indeterminate — journal recovery or /undo is the only
// repair path.
func (t *PromoteArtifact) Invoke(ctx context.Context, raw json.RawMessage) (agent.ToolResult, error) {
	p, ok := t.consumePending(ContentHash(raw))
	if !ok {
		return errResult("promote preview missing; retry"), nil
	}
	if err := t.rt.store.claim(p.id, p.path); err != nil {
		return errResult(err.Error()), nil
	}
	out, status := t.rt.store.get(p.id)
	if status != scratchStatusCaptured || promoteOutcomeDigest(out) != p.digest {
		t.rt.store.release(p.id, p.path)
		return errResult(fmt.Sprintf("scratch %s changed since approval; re-check scratch_changes and retry", p.id)), nil
	}
	rec := MutationRecord{
		Path:        p.path,
		Existed:     false,
		AfterHash:   p.change.hash,
		Summary:     "promote scratch artifact " + p.id,
		At:          time.Now(),
		TrackedMode: true,
		AfterMode:   p.change.mode.Perm(),
	}
	toolErr, internalErr := runJournaledWrite(ctx, t.rt.journal, rec, func() error {
		return installPromotedCreate(t.ws.root, p.change)
	})
	if internalErr != nil {
		t.rt.store.consume(p.id, p.path)
		return errResult(fmt.Sprintf(
			"promotion of %q is indeterminate: %v; the file may be written — journal recovery or /undo is the only repair path",
			p.path, internalErr)), nil
	}
	if toolErr != nil {
		t.rt.store.release(p.id, p.path)
		return errResult(fmt.Sprintf("promotion of %q refused: %v", p.path, toolErr)), nil
	}
	// Post-commit verification through the canonical workspace primitives.
	data, mode, err := t.ws.ReadFileWithModeForUndo(p.path)
	if err != nil || ContentHash(data) != p.change.hash || mode.Perm() != p.change.mode.Perm() || mode&^fsModePermMask != 0 {
		t.rt.store.consume(p.id, p.path)
		return errResult(fmt.Sprintf(
			"promotion of %q is indeterminate: post-commit verification failed (%v); journal recovery or /undo is the only repair path",
			p.path, err)), nil
	}
	t.rt.store.consume(p.id, p.path)
	return agent.ToolResult{Content: fmt.Sprintf("promoted %q (%d bytes, mode %04o) from %s",
		p.path, p.change.size, p.change.mode.Perm(), p.id)}, nil
}
