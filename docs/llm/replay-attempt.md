# llm-bench single-attempt capture (`-replay-attempt`)

`-replay-attempt` replays one trace against one openai-compat candidate exactly once and
publishes an immutable attempt directory. It exists for external harnesses (the Golem
model-qualification lab's `replay-diagnostic` driver) that score attempts themselves and need
evidence that survives failure. Legacy `-calibrate-capture` and replay runs are unchanged.

```
llm-bench -replay-attempt \
  -attempt-trace <trace.json> -models openai-compat/<model> \
  -candidate-base-url <server root> \
  -attempt-id <id> -attempt-arm <opaque> -attempt-dir <new dir> [-timeout 5m]
```

The Bearer credential is optional: without one no `Authorization` header is sent. It comes from
`-candidate-api-key`, else from `LLM_BENCH_CANDIDATE_API_KEY`. The flag wins, and both are
whitespace-trimmed.

## Guarantees

- **One request sequence.** One replay, no retries, no fallback, no followed redirects. A
  scripted trace sends one request per candidate response: one per user turn, plus one for each
  tool round the candidate makes (`tool-canary-01` has 3 turns and sends 2). A prefilled trace
  sends exactly one request, carrying the whole conversation. Go's transport may resend a request
  it never wrote (or retry an HTTP/2 refused stream); such a resend carries the same correlation
  value.
- **Correlation.** Every request carries `X-Client-Correlation: <attempt-id>/<ordinal>`.
  `-attempt-id` must match `[A-Za-z0-9._:-]{1,128}`.
- **Raw files are bodies.** `raw/NNNN.request.json` and `raw/NNNN.response.body` hold the request
  and response bodies only, with no headers or status line. Compression is disabled and a
  response is read to EOF before it is parsed, so a stored body is the bytes that crossed the
  wire, except where its ref says `redacted`, and except that a response over 64 MiB is stored
  truncated to 64 MiB.
- **Unknown usage stays unknown.** Usage comes from the raw response, and absent fields are
  `null`.
- **Immutable output.** `-attempt-dir` must not exist, and its parent must already exist (only
  the directory itself is created). Files are written exclusively and fsynced; `attempt.json` is
  published last by rename, so its presence means every file it references is fully written and
  synced.
- **Platforms.** darwin and linux. On windows the mode refuses and exits 1.

## Exit code

`0` once `attempt.json` is published and synced, whatever the attempt status. That includes
problems found after the directory is claimed, such as an invalid `-attempt-id`, `-timeout 0s` or
an unreadable trace: they are recorded as an `input` failure. Exit 0 means "terminal published"
only for an actual `-replay-attempt` run: `llm-bench -h` also exits 0 and publishes nothing.

`1` when there is no durable terminal. Usage errors exit 1 with nothing written: a stray
`-attempt-*` flag, `-replay-attempt` combined with another mode, a missing `-attempt-dir`, an
`-attempt-dir` that already exists or whose parent does not exist. So does a publication failure:
if the rename succeeded and the final directory sync failed, `attempt.json` is visible and the
exit is 1; a present `attempt.json` that parses with the matching `attempt_id` is authoritative.

`2` for a command-line parse error from the flag package (for example `-timeout abc`), before any
attempt exists.

## `attempt.json` (schema `llm-bench.replay-attempt/v1`)

| Field | Meaning |
|---|---|
| `schema`, `attempt_id`, `arm` | Identity; `arm` is recorded verbatim |
| `trace.id`, `trace.sha256` | Trace id and the sha256 of the trace file bytes |
| `replay_kind` | `scripted` or `prefilled` |
| `build` | `{go_version, module_version, vcs_revision, vcs_modified}`; the `vcs_*` fields are `null` without VCS stamping |
| `settings` | `{transport, endpoint, model, temperature, timeout_ms}`: transport, sanitized endpoint, model selector, temperature (`0` for prefilled, else `null`), timeout in milliseconds |
| `started_at`, `ended_at`, `elapsed_ms` | Attempt timing; `elapsed_ms` is whole milliseconds |
| `status` | `failed` if and only if `error` or `artifact_error` is non-null, else `completed` |
| `error`, `artifact_error` | Execution failure and evidence-write failure, kept separate; each `null` or `{code, stage, http_status?, cancel_source?, request_ordinal?, message}` |
| `transcript`, `notes` | Ordered partial transcript and replay notes |
| `divergences`, `sentinel_substituted` | Scored divergences (`tool_route_bypassed`, `plain_chat_tool_call`, `prefilled_tool_call`) and whether llm-bench replaced the candidate's content with its divergence sentinel, `__LLM_BENCH_TOOL_DIVERGENCE_8F3E0B2C__`. A substituted sentinel is never a candidate answer. |
| `requests[]` | One row per logical request; see Request rows |

A scored divergence does not by itself fail the attempt, and `status` `completed` does not mean
every turn of the trace was replayed. What the replay does next depends on the kind:

- `plain_chat_tool_call`: the replay ends at that turn. A 2-user-turn plain-chat trace sends 1
  request and the transcript has one turn, with `sentinel_substituted` true.
- `tool_route_bypassed`: the scripted tool group is skipped and the replay continues with the
  next user turn. A later failure still fails the attempt, and `divergences` keeps the kind.
- `prefilled_tool_call`: a prefilled replay sends one request, so nothing follows.

`replay_divergence` is different: a strict mismatch between the candidate's tool calls and the
scripted route, which fails the attempt.

**Null until known.** An `input` failure can end the run before it has learned some identity
fields, so these are JSON `null` until the run learns them, never an empty string:
`trace.sha256` is set once the trace file is read, `trace.id` once the trace parses with a
non-empty id, `replay_kind` once the trace passes validation, and `settings.endpoint` once the
base URL passes the http/https check. `settings.transport` is always `openai-compat`, and
`settings.model` is the `-models` selector as given, with surrounding whitespace trimmed.

`error.message` is one of four fixed stubs, `<error: timeout>`, `<error: network>`,
`<error: parse>` or `<error: other>`, chosen by substring match on the underlying error text, not
derived from `code` (an `input` error that mentions `-timeout` reads `<error: timeout>`).
`artifact_error.message` is `unwritten evidence: ` followed by the raw paths that were not
written. The full error, with the key redacted, goes to stderr.

Redaction: when the candidate key is at least 16 bytes, its exact occurrences become
`[REDACTED]` in raw request and response files (a key echoed in one reply re-enters the next
request) and in `attempt.json`'s transcript and notes. Raw refs carry `redacted`, and their
`sha256` hashes the stored bytes. A shorter key cannot be told apart from ordinary data, so it is
not redacted; stderr says so.

## Request rows

`requests[]` holds one row per logical request, in order: `ordinal`, `correlation`, `started_at`,
`headers_at`, `ended_at`, `elapsed_ms` (whole milliseconds from `started_at` to `ended_at`),
`http_status`, `request`, `response`, `usage`, `error_code`.

- `headers_at` and `http_status` are null when no response arrived.
- `request` and `response` are refs `{path, sha256, bytes, redacted}`; `response` adds `complete`.
  `request` is null when its file could not be written. `response` is null when no response
  arrived or its file could not be written.
- `response.complete` false means the stored body is a truncated prefix of what the server sent:
  the body read failed or the body exceeded the 64 MiB cap (a body of exactly 64 MiB is
  complete). It is about the server's body, not the file; every referenced file is fully
  written.
- `usage` is `{prompt_tokens, completion_tokens, reasoning_tokens}`, each an integer or null.
  `reasoning_tokens` is read from `usage.completion_tokens_details.reasoning_tokens`. The whole
  object is null when the response has no `usage` object, the body is unparseable, or any of
  those three counts is not an integer (for example `7.0` or `"7"`). The provider's own decode
  rejects such a response too, so that attempt fails as `response_decode`.
- `error_code` is set only for failures the HTTP layer sees: `connection`, `canceled`,
  `http_status`, and the envelope-level `response_decode` and `empty_response`. It stays null for
  failures found after a 2xx body was accepted (`argument_decode`, a wrong-typed field
  `response_decode`, an empty assistant message, `replay_divergence`, an `input` failure
  mid-replay), where only `error.code` carries the class. When it is set, it equals `error.code`.

`error.request_ordinal` is the ordinal of the failing row. A request cancelled before it reached
the wire (for example by `-timeout`) still gets a row: `headers_at`, `http_status` and `response`
are null, `error_code` is `canceled`, and `raw/NNNN.request.json` holds the body that would have
been sent. A null `headers_at` says only that no response arrived: the server may or may not have
received that request. The field is absent when no request is involved (an `input` failure before
the first request), and a consumer must not assume a row exists for it.

## Error codes

| Code | Meaning |
|---|---|
| `input` | Invalid flag, config (including a non-http(s) base URL), or trace fixture (nothing was sent, or the fixture broke mid-replay) |
| `connection` | Connection, TLS, or response-body read failure that is not a cancellation |
| `http_status` | Non-2xx response, 3xx included |
| `canceled` | A timeout or signal during a request or its response-body read: `cancel_source` `attempt_timeout` (`-timeout`, which bounds the whole attempt across all requests) or `signal` (SIGINT/SIGTERM) |
| `response_decode` | A 2xx body that is not one JSON object, carries a non-null `error` member, has no `choices` array, has a wrong-typed field, or exceeds 64 MiB |
| `argument_decode` | A tool call whose arguments do not decode to a JSON object |
| `empty_response` | Zero choices, or an assistant reply with no content and no tool calls. Whitespace-only content counts as empty for prefilled replays only; a scripted replay treats it as content. |
| `replay_divergence` | The candidate's tool calls do not match the scripted route |
| `internal` | Anything else |
| `artifact_write` | An evidence file could not be written (in `artifact_error`) |

Stages: `input`, `request`, `response`, `replay`, `publish`. One code can carry different
stages: `empty_response` is stage `response` for zero choices and stage `replay` for an empty
assistant message.
