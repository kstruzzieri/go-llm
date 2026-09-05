### Added — tool argument invariants and egress classification (#439)

Two guard interceptors join `interceptor.Defaults()` (ZT-104, epic #429
Phase 2), so they run wherever `-interceptors` is on, dispatch children
included. Guards inspect tool calls only and add no model-visible trailer.

- `interceptor.Invariants`: a sealed, typed table of per-tool argument
  bounds (`interceptor.Invariant` with the `PathDeny` and `RemoteScript`
  checks; `DefaultInvariants()`, `NewInvariants()`). A violation blocks the
  call before `Plan` and approval regardless of origin, with the
  invariant's name as the finding rule and in the model-visible observation
  (`tool call blocked by interceptor invariants (protected_path)`). The
  guard reads the argument the tool's own decoder would use: field names
  match case-insensitively, and two equivalent spellings block as
  `ambiguous_argument`. Paths are normalized natively (`filepath.Clean`,
  `ToSlash`, the Win32 trailing period and space trim) and component
  case-folded. Defaults: `protected_path` (`.git`, `.ssh`, `.gnupg`,
  `.aws`, `.kube` components) on `write_file`, `edit_file`,
  `promote_artifact`; `credential_path` (`.ssh`, `.gnupg`, `.aws`, `.kube`
  components and the exact basename `.env`) on `read_file`;
  `remote_script_execution` on `run_command` and `start_command`: an inline
  `sh`/`bash`/`dash`/`ksh`/`zsh -c` script that pipes a recognized `curl`
  or `wget` stdout fetch into a bare shell, optionally under `sudo`.
  Substitution, `eval` and `source` forms are deliberately outside the block
  set and stay badges.
- `interceptor.Egress`: classifies an exec-class argv as `privileged`,
  `network`, `package-manager`, `interpreter` or `unknown` after peeling
  the supported `env`/`nohup`/`nice`/`time`/`timeout`/`stdbuf` forms, with
  explicit `git` and `go` subcommand tables and a literal-word scan of a
  recognized inline shell script. Anything it cannot parse, and anything
  outside an explicit quiet set, stays visible as `unknown`. One tag
  finding per call: `Rule` is the class, `Detail` a bounded label. Weights:
  network 20, privileged 20, package-manager 10, unknown 10, interpreter 0.
- `agent.RiskReport.CurrentToolCallFindings`: set only on the report handed
  to an approver, the approved call's own findings, independent of
  provider tool-call IDs; excluded from JSON, `Result.Risk` and observer
  events, which stay cumulative.
- Golem: the approval prompt's risk line carries the current call's egress
  badge, `interceptor risk 20 · egress: network (git push)`, on prompted
  and grant-covered approvals alike; prompts without a current egress
  finding are byte-identical to before. The startup notice lists five
  interceptors. Windows-native path cases are compiled by the existing CI
  Windows step; a native run needs a Windows host.

The hard line-count limit the issue mentioned is deferred: the existing
256 KiB write/edit bounds remain, and an approval budget through the
grant-key seam is the recorded follow-up.
