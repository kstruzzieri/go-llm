### Security — AgentFlow subprocesses no longer inherit Golem's environment (#577)

AgentFlow, every validation gate it runs and its own `git` calls now receive an
environment built from scratch: `PATH`, `HOME`, `USER`, `TMPDIR` and `LANG`
(plus `SYSTEMROOT`, `TEMP`, `TMP`, `PATHEXT`, `USERPROFILE` and `COMSPEC` on
Windows), operator-approved names (`-agentflow-env`), `AGENTFLOW_STRICT=1` when
set to exactly `1`, and the runner's own `PYTHONPATH` and
`PYTHONDONTWRITEBYTECODE`. Provider API keys and other parent variables no
longer reach gates unless explicitly approved.

#### Upgrade note

Gates that depended on inherited variables (for example `GOFLAGS`, `GOPRIVATE`,
`HTTPS_PROXY` or `SSL_CERT_FILE`) now run without them. Approve each one with
the new repeatable `-agentflow-env NAME` flag on `golem` and `golem audit`. An
approved name that is unset fails the launch, and an approved value reaches
every gate.

#### Library and CLI changes

- `agentflow.ValidateEnvNames` and `(*agentflow.ExecRunner).AllowEnv` approve
  parent variables by name.
- `agentflow.EnvNotSetError` reports an approved name that is unset at launch;
  it carries the name only. `golem audit` names the variable in its
  `agentflow_unavailable` diagnostic, and `golem -agentflow-status -json` prints
  it on stderr while keeping exit 3 and empty stdout.
- `agentflow.NewSrcExecRunner` now requires an absolute checkout containing
  `src/agentflow/__init__.py` and resolves symlinks; an invalid checkout fails
  at `Run`.
- `golem -goal` now resolves a relative `-agentflow-src` against `-root`, like
  task and status modes, and its printed "execute separately" command carries
  every `-agentflow-env` name.
- `golem` and `golem audit` report unexpected positional arguments by count
  instead of echoing them.
