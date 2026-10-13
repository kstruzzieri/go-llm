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

The Bearer credential is optional. It comes from `-candidate-api-key`, else from
`LLM_BENCH_CANDIDATE_API_KEY`; the flag wins, and both are whitespace-trimmed. Without one no
`Authorization` header is sent. llm-bench refuses a key containing any control character (a byte
below 0x20, tab included, or DEL) as `input`. That is llm-bench's own policy: such a key is a paste
error. Redaction, its 16-byte threshold and the stderr redaction all use the trimmed key, the one
that is sent.

`-candidate-base-url` must be an absolute http or https URL with no userinfo
(`http://user:pass@host`), no query (a bare `?` included) and no fragment (a bare `#` included);
any of those is refused as `input`. The provider appends its request path to the URL string, so a
query or fragment would send the requests to a different path, and userinfo would be sent as Basic
auth that the record did not show. `settings.endpoint` is the base URL the requests used. The
mode always dials the endpoint directly and ignores `HTTP_PROXY`, `HTTPS_PROXY` and `NO_PROXY`.

## Guarantees

- **One request sequence.** One replay, no retries, no fallback, no followed redirects. A
  scripted trace sends one request per candidate response: one per user turn, plus one for each
  tool round the candidate makes (`tool-canary-01` has 3 turns and sends 2). A prefilled trace
  sends exactly one request, carrying the whole conversation. Go's transport may resend a request
  it never wrote (or retry an HTTP/2 refused stream); such a resend carries the same correlation
  value.
- **Correlation.** Every request carries `X-Client-Correlation: <attempt-id>/<ordinal>`.
  `-attempt-id` must match `[A-Za-z0-9._:-]{1,128}`. `-attempt-arm` is opaque and never sent to
  the server; it must be 1-256 bytes of valid UTF-8 with no control characters.
- **Raw files are bodies.** `raw/NNNN.request.json` and `raw/NNNN.response.body` hold the request
  and response bodies only, with no headers or status line. Compression is disabled and a
  response is read to EOF before it is parsed, so a stored body is the bytes that crossed the
  wire, except where its ref says `redacted`, and except that a response over 64 MiB is stored
  truncated to 64 MiB. That cap bounds one response body, not the attempt: all request and
  response bodies are held in memory until publication.
- **No protocol upgrades.** An HTTP `101 Switching Protocols` response is closed immediately
  and fails as `http_status`. The upgraded connection is never read: its response evidence is
  an empty file with `complete: false`, so a server cannot hold publication open after switching
  away from HTTP.
- **Unknown usage stays unknown.** Usage comes from the raw response, and absent fields are
  `null`.
- **Immutable output.** `-attempt-dir` must not exist, and its parent must already exist (only
  the directory itself is created). Files are written exclusively and fsynced, and so are the
  directories that hold them: the attempt directory's parent once the directory is created, the
  attempt directory once `raw/` is created in it, and `raw/` once its files are written.
  `attempt.json` is published last by rename, so its presence means every file it references is
  fully written and synced.
- **Platforms.** darwin and linux. On windows the mode refuses and exits 1.

## Exit code

`0` once `attempt.json` is published and synced, whatever the attempt status. That includes
problems found after the directory is claimed, such as an invalid `-attempt-id`, a `-timeout`
under 1ms or an unreadable trace: they are recorded as an `input` failure. Exit 0 means
"terminal published" only for an actual `-replay-attempt` run: `llm-bench -h` also exits 0 and
publishes nothing.

A closed stderr does not prevent publication: the mode ignores SIGPIPE, and a failed write to
stderr is discarded, so a supervisor whose stderr reader has gone away still gets its
`attempt.json`.

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
| `schema`, `attempt_id`, `arm` | Identity; an accepted `arm` is recorded verbatim (a rejected one fails `input`, and invalid UTF-8 in it is recorded as U+FFFD, since JSON cannot carry it) |
| `trace.id`, `trace.sha256` | Trace id and the sha256 of the trace file bytes |
| `replay_kind` | `scripted` or `prefilled` |
| `build` | `{go_version, module_version, vcs_revision, vcs_modified}`; `module_version` is the main module version Go embeds (`(devel)` is kept as Go reports it) and is `null` when build info is unavailable or carries no version; the `vcs_*` fields are `null` without VCS stamping |
| `settings` | `{transport, endpoint, model, temperature, timeout_ms}`: transport, endpoint, model selector, temperature (`0` for prefilled, else `null`), timeout in whole milliseconds, truncated toward zero (`-timeout 1900us` is recorded as `1`; a `-timeout` under 1ms is refused as `input`) |
| `started_at`, `ended_at`, `elapsed_ms` | Attempt timing; `elapsed_ms` is whole milliseconds |
| `status` | `failed` if and only if `error` or `artifact_error` is non-null, else `completed` |
| `error`, `artifact_error` | Execution failure and evidence-write failure, kept separate; each `null` or `{code, stage, http_status?, cancel_source?, request_ordinal?, message}` |
| `transcript`, `notes` | Ordered partial transcript and replay notes |
| `divergences`, `sentinel_substituted` | Scored divergences (`tool_route_bypassed`, `plain_chat_tool_call`, `prefilled_tool_call`) and whether llm-bench replaced the candidate's content with its divergence sentinel, `__LLM_BENCH_TOOL_DIVERGENCE_8F3E0B2C__`. A substituted sentinel is never a candidate answer. |
| `requests[]` | One row per logical request; see Request rows |

A scored divergence does not by itself fail the attempt, and `status` `completed` does not mean
every turn of the trace was replayed. What the replay does next depends on the kind:

- `plain_chat_tool_call`: the replay ends after the diverging turn, and `sentinel_substituted` is
  true. A 2-user-turn plain-chat trace sends 1 request, with one transcript turn, when the first
  user turn diverges, and 2 requests when only the second does.
- `tool_route_bypassed`: the scripted tool group is skipped and the replay continues with the
  next user turn. A later failure still fails the attempt, and `divergences` keeps the kind.
- `prefilled_tool_call`: a prefilled replay sends one request, so nothing follows.

`replay_divergence` is different: a strict mismatch between the candidate's tool calls and the
scripted route, which fails the attempt. A frozen tool result whose name disagrees with the
scripted tool call is a fixture defect and fails as `input`, even when discovered mid-replay.

**Formats.** Every `sha256` (`trace.sha256` and each ref's) is lowercase hex with no prefix.
Every timestamp (`started_at`, `ended_at`, `headers_at`) is RFC 3339 in UTC with a `Z` suffix and
fractional seconds of up to nanosecond precision, trailing zeros trimmed.

**Null until known.** An `input` failure can end the run before it has learned some identity
fields, so these are JSON `null` until the run learns them, never an empty string:
`trace.sha256` is set once the trace file is read, `trace.id` once the trace parses with a
non-empty id, `replay_kind` once the trace passes validation, and `settings.endpoint` once the
base URL passes validation (an absolute http or https URL with no userinfo, query or fragment).
`settings.transport` is always `openai-compat`, and `settings.model` is the `-models` selector as
given, with surrounding whitespace trimmed.

`error.message` is one fixed sentence per `code`, listed in the error codes table below. It is
derived from the code alone, never from the underlying error text, so it carries no path, key or
server wording and a server's error body cannot steer it.
`artifact_error.message` is `unreferenced evidence: ` followed by the raw paths that no
`requests[]` ref points at, for example `raw/0001.response.body`. The name `raw/` means the
directory could not be created, or a sync of `raw/` or of the attempt directory failed, and then
no row has a ref. A listed path is not evidence even if a file exists there (it may be partial,
or fully written but not confirmed durable): only a `request` or `response` ref in `requests[]`
makes a file evidence. The full error, with the key redacted, goes to stderr. Redaction there
recognizes both the literal key and its Go-quoted form in diagnostics, including escaped quotes
and backslashes, before applying the one-line escaping. Each diagnostic there is exactly one
line, because control characters in it are escaped: a newline is written
as the two characters backslash and `n`, a carriage return and a tab likewise, other C0
controls, DEL and bytes that are not valid UTF-8 as `\xNN`, and C1 controls, U+2028 and U+2029
as `\uNNNN`. A backslash is written as two, so the escaping is reversible: a body that holds the
two characters backslash and `n` (`\\n` on stderr) is distinct from one that holds a newline
(`\n`). A server's error body therefore cannot forge a second evidence line or drive a
terminal. For `artifact_write` there is one line per cause, with the underlying OS error for
each file that could not be written. An execution error is reported there too.

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
  complete). An HTTP 101 has an empty, incomplete response because the upgraded connection is
  closed without reading. It is about the server's body, not the file; every referenced file
  is fully written.
- `usage` is `{prompt_tokens, completion_tokens, reasoning_tokens}`, each an integer or null.
  `reasoning_tokens` is read from `usage.completion_tokens_details.reasoning_tokens`. The whole
  object is null when the response has no `usage` object, the body is unparseable, or any of
  those three counts is not an integer (for example `7.0` or `"7"`). A 2xx response that passes
  the envelope check and carries such a count fails as `response_decode`, because the provider's
  own decode rejects it too. A non-2xx response keeps its `http_status` failure and zero choices
  keeps `empty_response`; `usage` is null on those rows as well.
- `error_code` is set only for failures the HTTP layer sees: `connection`, `canceled`,
  `http_status`, and the envelope-level `response_decode` and `empty_response`. It stays null for
  failures found after a 2xx body was accepted (`argument_decode`, a wrong-typed field
  `response_decode`, an empty assistant message, `replay_divergence`, an `input` failure
  mid-replay), where only `error.code` carries the class. When it is set, it equals `error.code`.

`error.http_status` is set when the failing request's response headers arrived and the failure is
one the HTTP layer sees, or a wrong-typed-field `response_decode`. Its presence does not imply
`code` `http_status`: it also appears on every `response_decode` (the 64 MiB cap included), on
zero-choices `empty_response`, and on `connection` or `canceled` failures during the body read. It
is absent when no response arrived, and for the other failures found after a 2xx body was accepted
(`argument_decode`, `replay_divergence`, an empty assistant message, a mid-replay `input` failure,
`internal`); there only the row's `http_status` carries the status.

`error.request_ordinal` is the ordinal of the failing row. A request cancelled before it reached
the wire (for example by `-timeout`) still gets a row: `headers_at`, `http_status` and `response`
are null, `error_code` is `canceled`, and `raw/NNNN.request.json` holds the body that would have
been sent. A null `headers_at` says only that no response arrived: the server may or may not have
received that request. The field is absent when no request is involved (an `input` failure before
the first request), and a consumer must not assume a row exists for it.

## Error codes

| Code | Meaning | `error.message` |
|---|---|---|
| `input` | Invalid flag, config (including a non-http(s) base URL, a base URL with userinfo, a query or a fragment, or a key with a control character), or trace fixture (nothing was sent, or the fixture broke mid-replay) | `an input to the attempt was invalid` |
| `connection` | Connection, TLS, or response-body read failure that is not a cancellation | `the connection to the endpoint failed` |
| `http_status` | Non-2xx response, including 3xx and a refused 101 protocol upgrade | `the server returned a non-2xx status` |
| `canceled` | A timeout or signal during a request or its response-body read: `cancel_source` `attempt_timeout` (`-timeout`, which bounds the whole attempt across all requests) or `signal` (SIGINT/SIGTERM) | `the attempt was canceled` |
| `response_decode` | A 2xx body that is not one JSON object, carries a non-null `error` member, has no `choices` array or more than one `choices` member, has a wrong-typed field, or exceeds 64 MiB. Member names match case-insensitively, as the provider's decode does: `CHOICES` is the `choices` array and `Error` is an `error` member. Duplicate `choices` members are rejected even with identical spelling or JSON-escaped names, because the provider can merge earlier values into later ones. | `the server's response could not be decoded` |
| `argument_decode` | A tool call whose arguments do not decode to a JSON object | `a tool call's arguments could not be decoded` |
| `empty_response` | Zero choices, or an assistant reply with no content and no tool calls. Whitespace-only content counts as empty for prefilled replays only; a scripted replay treats it as content. | `the candidate returned no reply` |
| `replay_divergence` | The candidate's tool calls do not match the scripted route | `the candidate's tool calls did not match the scripted route` |
| `internal` | Anything else | `an internal error occurred` |
| `artifact_write` | An evidence file could not be written (in `artifact_error`) | `unreferenced evidence: ` and the paths, as above |

Stages: `input`, `request`, `response`, `replay`, `publish`. One code can carry different
stages: `empty_response` is stage `response` for zero choices and stage `replay` for an empty
assistant message.
