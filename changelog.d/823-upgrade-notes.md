### Changed — v0.8.0 consumer upgrade notes (#823)

Read this before upgrading from v0.7.0. Consumers on older pins must also apply
the earlier upgrade notes. The
[v0.8.0 upgrade guide](docs/releases/v0.8.0.md) has the details, the known
issues and the consumer compile check.

- `apidiff` reports three incompatible changes since v0.7.0:
  `agentflow.(*Client).ProbeRecovery` is removed (#766),
  `configio.RefreshInventory` takes variadic options (#734), and
  `fingerprint.SQLiteStore` is no longer comparable (#734).
- Golem requires AgentFlow 1.3 (#766).
- A working-directory `models.json` with `${NAME}` keys needs `-config` or
  `GO_LLM_CONFIG` (#776). Consumers that mirror `config`'s error codes add
  `key_reference_refused`.
- Commands that name credential files are refused, and their values are
  withheld from command output (#777). Writes to harness files always ask
  (#725, #784). Some `git` commands fail inside a native sandbox (#721).
- Answers in the old `capability_probes` table stop counting, so Golem checks
  each model's tool support again on first use (#737).
- A provider's 4xx refusal of a run, other than 401, 403 and 429, fails with
  `provider_rejected` instead of `internal` (#740). A turn that ends without an
  answer starts its footer with `no answer ·` instead of `done ·` (#767).
- Library hosts that opt in to strict approval implement
  `agent.FreshApprover` (#584).
