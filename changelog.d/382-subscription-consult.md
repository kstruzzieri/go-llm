### Added — /consult seam and Claude subscription adapter (#382)

`golem` can ask one configured external subscription CLI for a single-shot
judgment: `/consult <name> <prompt>` runs the consultant with the prompt on
stdin inside a bounded, private process envelope, admits the reply through a
frozen stream-json admission catalog, shows the answer, and stages it as
fenced advisory text on the next goal only. Consultants come only from
`-consultants-config <absolute path>` or `<os.UserConfigDir>/go-llm/consultants.json`;
a missing default file disables the command, and an unreadable or invalid file
fails startup. The first adapter is Claude Code `2.1.240` (`claude -p`, with
adapter-owned argv; the subscription login is owned by the vendor CLI, so
go-llm handles no token). `/consult` requires `-interceptors`.

The new `consult` package owns the runner, the adapter and the unsigned
receipt. It is not a provider, not a `provider.Router` member and not an
`agent.Tool`. `agent.Advisory` plus `agent.Orchestrator.InspectAdvisory` are
the runtime seam; `golem.Turn.Advisory` carries one staged receipt.

#### Upgrade notes

- The staged advice is projected onto the wire copy of the next goal inside a
  `CONSULT_ADVICE` fence and charged to the pinned segment; it is never
  written to session history, `Result.Messages` or durable summaries.
- Consultant traffic is the vendor process's own and bypasses
  `-allow-destination`; the config field `trusted_process_egress: true` is an
  explicit acknowledgement, not a filter.
- Only `model: "opus"` and CLI version `2.1.240` are accepted; an unpinned
  version fails admission after the process runs, and an unsupported model
  fails before any prompt is sent.
- The consultant `command` must be an absolute path to a regular file whose
  path traverses no symlink (Homebrew shims and `/usr/local/bin` links must be
  given as their resolved target). An optional `sha256` is re-verified
  immediately before exec.
- Unix only: the runner depends on `Setpgid` and negative-PID process-group
  signalling, so a consult on Windows fails with `unsupported-platform`.
