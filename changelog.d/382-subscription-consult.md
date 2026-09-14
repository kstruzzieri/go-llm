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
  written to `State`, session history, `Result.Messages` or durable summaries.
  Custom compactors receive only ordinary state and a budget reduced by the
  advisory cost; changing or losing its pinned goal fails before the model call.
- `run.failed` gains one code, `policy_blocked`, emitted only when the
  interceptor chain refuses a staged advisory at step 0
  (`agent.ErrAdvisoryBlocked`). The arm matches that sentinel alone: every
  other interceptor refusal (canary, secrets) still reports `internal`, and
  reclassifying those is deferred. A step-0 refusal also **drops** the staged
  advisory rather than retaining it, with a notice naming the consultant: the
  refusal is deterministic in the staged bytes, so keeping the slot would fail
  every later goal identically. Context exhaustion also drops the slot with a
  notice so an oversized answer cannot block later goals. Other failures and
  clean turns that produced no answer still retain it. `/consult drop`
  discards advice without clearing conversation history or grants.
- Consultant traffic is the vendor process's own and bypasses
  `-allow-destination`; the config field `trusted_process_egress: true` is an
  explicit acknowledgement, not a filter.
- Only `model: "opus"` and CLI version `2.1.240` are accepted. A bounded
  `--version` probe with empty stdin rejects an unpinned version before the
  prompt is sent. Both launches share one deadline and must use the same
  executable digest, even without a configured pin. Admission also rejects
  non-Opus or missing assistant models, non-object usage blocks, non-Opus usage
  models, overflowing token totals and reported usage entries without
  `provider: "firstParty"`.
- The consultant `command` must be an absolute path to a regular file whose
  path traverses no symlink (Homebrew shims and `/usr/local/bin` links must be
  given as their resolved target) and, on Unix, is not group- or world-writable —
  a digest pin over a file the group can replace pins nothing. An optional
  `sha256` is re-verified immediately before exec. Path and permission checks
  are repeated before both launches, including for hand-built consultants.
  On Unix, the executable and every ancestor must be owned by root or the
  effective user; writable ancestors require the sticky bit.
- Temp parents, including inherited `TMPDIR`, are resolved to a canonical
  path and checked for trusted ownership and safe ancestor permissions before
  any envelope is created. Cancellation prints `consult canceled`.
- Linux cleanup treats an unreaped zombie-only process group as exited;
  `getpgid` filters unrelated processes before reading their state, and
  polling backs off to 100 ms within the one-second cleanup window. Live
  members, unknown membership or unreadable member data still fail cleanup.
- CRLF answers are normalized to LF before control sanitization and the
  64 KiB answer cap. Standalone carriage returns still become U+FFFD.
- Unix only: the runner depends on `Setpgid` and negative-PID process-group
  signalling, so a consult on Windows fails with `unsupported-platform`.
