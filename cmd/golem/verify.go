package main

import (
	"errors"

	agenttools "github.com/kstruzzieri/go-llm/agent/tools"
)

// verifyToolName is the synthetic identity the post-write verification
// command is approved under (#347): the library slot's VerifyToolName. It is
// NOT a registered tool; the approver renders an action-appropriate prompt
// for it and grantScope gives verification its own grant namespace.
const verifyToolName = agenttools.VerifyToolName

// buildVerifier resolves the workspace's post-write verification command
// (#347) under the session's sandbox policy (#576). It returns:
//   - (nil, "", nil) when the workspace declares none — the byte-for-byte
//     unchanged path;
//   - (nil, "verification disabled: ...", nil) for a repository mistake (a
//     malformed config, a missing command, an invalid declaration, a command
//     that cannot be prepared), so it costs a notice rather than the session;
//   - (nil, "", err) with err wrapping agenttools.ErrSandboxPolicy when the
//     policy cannot be honored. That is fatal: never a notice, and never a
//     fallback to another runtime.
//
// It has exactly two call sites, both REPL-only: startup under f.allowWrite
// (AFTER mode normalization: applyOneShotMode clears the flag, and task,
// planning and Agentflow modes reject it outright) and the REPL's
// /allow-write (#372). Together those keep every non-interactive mode from
// reading the file at all; do not add a third site outside the REPL.
func buildVerifier(root string, policy agenttools.SandboxConfig) (*agenttools.VerifyCommand, string, error) {
	disabled := func(err error) (*agenttools.VerifyCommand, string, error) {
		return nil, "verification disabled: " + err.Error(), nil
	}
	spec, err := loadVerifyConfig(root)
	if err != nil {
		return disabled(err)
	}
	if spec == nil {
		return nil, "", nil
	}
	ws, err := agenttools.NewWorkspace(root)
	if err != nil {
		return disabled(err)
	}
	cmd, err := agenttools.NewSandboxedVerifyCommand(ws, policy, spec.Argv, spec.Dir, spec.Timeout())
	if errors.Is(err, agenttools.ErrSandboxPolicy) {
		return nil, "", err
	}
	if err != nil {
		return disabled(err)
	}
	return cmd, "", nil
}
