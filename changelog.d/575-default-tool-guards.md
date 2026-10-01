### Changed — Golem installs deterministic tool guards by default (#575)

Golem now installs argument invariants, exec-class egress labels and
scoped-child refusal reporting on every run, including `-p` and dispatch
children, with or without `-interceptors`. There is no opt-out. Every
session prints a new stderr startup line:
`guards: invariants, egress, child_scope_denials (always on; -interceptors adds detectors, secrets, canary)`.

#### Upgrade note

The invariants are lexical checks on specific tool arguments:
`write_file`, `edit_file` and `promote_artifact` paths under a `.git`, `.ssh`,
`.gnupg`, `.aws` or `.kube` component; `read_file` paths under `.ssh`,
`.gnupg`, `.aws` or `.kube`, or named `.env`; `run_command`/`start_command`
inline `sh`/`bash`/`dash`/`ksh`/`zsh` `-c` (or `-lc`, `-ec`, `-euc`) scripts
that pipe a `curl`/`wget` stdout fetch into a bare shell (optionally `-s`,
optionally under `sudo`); and the same guarded argument spelled twice.
Matching calls are refused before approval; grants and `-allow-tool` cannot
override this. The model sees the refusal as a tool error, and three
consecutive errors stop the run. A `-p` run in `json` or `stream-json` format
stopped this way reports `status: error` with `empty_answer` and exits 1.
Blocked calls emit no `tool.started`/`tool.finished` events. Other routes to
the same files or effects are not covered: shell commands, search, retrieval,
MCP tools, verifier commands, and shell forms the recognizer does not model
(for example `bash -e -c` or `sh -xc`). This is a tripwire, not confinement.

Egress labels classify the `argv` of any exec-class call whose arguments
carry one, MCP tools included, unless the command is on the quiet set. They
appear on interactive approval prompts. Invariant refusals (30 each), egress
labels and scoped-child refusals (10 each) add to the `interceptor risk`
score shown on prompts and stderr footers, now also without
`-interceptors`; a dispatch child's envelope gains its own `risk_score` the
same way. Labels and scores are informational and do not confine network
access or revoke grants.

`golem.result.v1`, protocol-v1 events and exit codes are unchanged. Content
detectors, Secrets, canaries and `/consult` still require `-interceptors`.
Library consumers (`agent`, `agent/interceptor`, and `golem.New`'s
config-driven bootstrap) are unaffected and remain opt-in; embedders supply
an `Options.Orchestrator` built with `agent.WithInterceptors`.
