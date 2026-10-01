### Changed — Golem installs deterministic tool guards by default (#575)

Golem now installs argument invariants, exec-class egress labels and
scoped-child refusal reporting on every run, including `-p` and dispatch
children, with or without `-interceptors`. There is no opt-out.

#### Upgrade note

The invariants are lexical checks on specific tool arguments:
`write_file`, `edit_file` and `promote_artifact` paths under a `.git`, `.ssh`,
`.gnupg`, `.aws` or `.kube` component; `read_file` paths under `.ssh`,
`.gnupg`, `.aws` or `.kube`, or named `.env`; `run_command`/`start_command`
inline `sh`/`bash`/`dash`/`ksh`/`zsh` `-c` scripts that pipe a recognized
`curl`/`wget` fetch into a shell; and the same guarded argument spelled twice.
Matching calls are refused before approval; grants and `-allow-tool` cannot
override this. The model sees the refusal as a tool error, and three
consecutive errors stop the run. Other routes to the same files or effects
are not covered: shell commands, search, retrieval, MCP tools, verifier
commands, and shell forms the recognizer does not model. This is a tripwire,
not confinement.

Egress labels classify the `argv` of any exec-class call whose arguments
carry one, MCP tools included. They appear on interactive approval prompts
and add to the `interceptor risk` score shown on prompts and stderr footers.
Labels are informational and do not confine network access.

`golem.result.v1`, protocol-v1 events and exit codes are unchanged. Content
detectors, Secrets, canaries and `/consult` still require `-interceptors`.
Library consumers (`agent`, `agent/interceptor`) are unaffected and remain
opt-in.
