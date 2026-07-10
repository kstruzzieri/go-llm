package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kstruzzieri/go-llm/agent/tools"
	"github.com/kstruzzieri/go-llm/agentflow"
)

var errProofState = errors.New("path is under .agent/ proof state (opaque to the model)")

// stepScopeGuard builds the proof-mode scope guard for one claimed step:
//   - anything under .agent/ is denied for read AND write (proof state is opaque);
//   - writes must fall in the step's effective scope (step.files ∩ allowed_files,
//     minus blocked_files) — mirroring agentflow's own record-file-change check
//     so Golem's pre-write guard and agentflow's scope cannot drift.
// denyProofState returns errProofState when rel's first path segment is .agent
// (case-insensitive), else nil. Shared by the step guard (#209) and the planner
// read guard (#210) so both stay identical. Note ".agentflow" and "agent" are
// NOT proof state — only the exact ".agent" first segment is. Case-insensitivity
// is deliberate: this guards a real filesystem target where APFS case-folding
// matters (the CRITICAL bypass class fixed in #209).
func denyProofState(rel string) error {
	first := rel
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		first = rel[:i]
	}
	if strings.EqualFold(first, ".agent") {
		return errProofState
	}
	return nil
}

func stepScopeGuard(plan *agentflow.Plan, stepID string) tools.ScopeGuard {
	allowed, blocked := agentflow.EffectiveScope(plan, stepID)
	return func(rel string, write bool) error {
		if err := denyProofState(rel); err != nil {
			return err
		}
		if write {
			if !agentflow.MatchesPath(rel, allowed) || agentflow.MatchesPath(rel, blocked) {
				return fmt.Errorf("%q is outside step %s effective scope", rel, stepID)
			}
		}
		return nil
	}
}
