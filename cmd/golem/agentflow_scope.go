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
func stepScopeGuard(plan *agentflow.Plan, stepID string) tools.ScopeGuard {
	allowed, blocked := agentflow.EffectiveScope(plan, stepID)
	return func(rel string, write bool) error {
		first := rel
		if i := strings.IndexByte(rel, '/'); i >= 0 {
			first = rel[:i]
		}
		if strings.EqualFold(first, ".agent") {
			return errProofState
		}
		if write {
			if !(agentflow.MatchesPath(rel, allowed) && !agentflow.MatchesPath(rel, blocked)) {
				return fmt.Errorf("%q is outside step %s effective scope", rel, stepID)
			}
		}
		return nil
	}
}
