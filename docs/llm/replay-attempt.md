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

The Bearer credential comes from `-candidate-api-key` or `LLM_BENCH_CANDIDATE_API_KEY`.

## Guarantees

- **One request sequence.** One replay, no retries, no fallback, no followed redirects. A
  multi-turn trace sends one request per turn. Go's transport may resend a request it never
  wrote (or retry an HTTP/2 refused stream); such a resend carries the same correlation value.
- **Correlation.** Every request carries `X-Client-Correlation: <attempt-id>/<ordinal>`.
  `-attempt-id` must match `[A-Za-z0-9._:-]{1,128}`.
- **Raw bytes are wire bytes.** Compression is disabled; bodies are read to EOF before parsing.
- **Unknown usage stays unknown.** Usage comes from the raw response, and absent fields are
  `null`.
- **Immutable output.** `-attempt-dir` must not exist. Files are written exclusively and
  fsynced; `attempt.json` is published last by rename, so its presence means every file it
  references is complete.
- **Platforms.** darwin and linux. On windows the mode refuses and exits 1.

## Exit code

`0` once `attempt.json` is published and synced, whatever the attempt status. `1` when there is
no durable terminal: the directory could not be claimed, or publication failed. If the rename
succeeded and the final directory sync failed, `attempt.json` is visible and the exit is 1; a
present `attempt.json` that parses with the matching `attempt_id` is authoritative.

## `attempt.json` (schema `llm-bench.replay-attempt/v1`)

| Field | Meaning |
|---|---|
| `schema`, `attempt_id`, `arm` | Identity; `arm` is recorded verbatim |
| `trace.id`, `trace.sha256` | Trace id and the sha256 of the trace file bytes |
| `replay_kind` | `scripted` or `prefilled` |
| `build` | Go version, module version, VCS revision and modified flag (`null` without VCS stamping) |
| `settings` | transport, sanitized endpoint, model selector, temperature (`0` for prefilled, else `null`), timeout |
| `started_at`, `ended_at`, `elapsed_ms` | Attempt timing |
| `status` | `completed` or `failed` |
| `error`, `artifact_error` | Execution failure and evidence-write failure, kept separate; each `null` or `{code, stage, http_status?, cancel_source?, request_ordinal?, message}` |
| `transcript`, `notes` | Ordered partial transcript and replay notes |
| `divergences`, `sentinel_substituted` | Scored divergences (`tool_route_bypassed`, `plain_chat_tool_call`, `prefilled_tool_call`) and whether llm-bench replaced the candidate's content with its divergence sentinel. A substituted sentinel is never a candidate answer. |
| `requests[]` | One row per logical request: ordinal, correlation, timing, `http_status`, raw `request`/`response` refs `{path, sha256, bytes, redacted}` (response adds `complete`), `usage`, `error_code` |

**Null until known.** An `input` failure can end the run before it has learned some identity
fields, so these are JSON `null` until the run learns them, never an empty string:
`trace.sha256` is set once the trace file is read, `trace.id` once the trace parses with a
non-empty id, `replay_kind` once the trace passes validation, and `settings.endpoint` once the
base URL passes the http/https check. `settings.transport` is always `openai-compat`, and
`settings.model` is the `-models` selector as given, with surrounding whitespace trimmed.

`message` is a category stub; the full error, with the key redacted, goes to stderr.

`error.request_ordinal` names the request that failed or was about to be sent. A cancellation or
refusal before that request reached the wire can therefore report an ordinal greater than
`len(requests)`: a cancellation before the first request reports `1` with `requests` empty. The
field is absent when no request is involved, as for an `input` failure before the first request.

Redaction: when the candidate key is at least 16 bytes, its exact occurrences become
`[REDACTED]` in raw request and response files (a key echoed in one reply re-enters the next
request) and in `attempt.json`'s transcript and notes. Raw refs carry `redacted`, and their
`sha256` hashes the stored bytes. A shorter key cannot be told apart from ordinary data, so it is
not redacted; stderr says so.

## Error codes

| Code | Meaning |
|---|---|
| `input` | Invalid flag, config (including a non-http(s) base URL), or trace fixture (nothing was sent, or the fixture broke mid-replay) |
| `connection` | Connection, TLS, or response-body read failure |
| `http_status` | Non-2xx response, 3xx included |
| `canceled` | `cancel_source` `attempt_timeout` (`-timeout`) or `signal` (SIGINT/SIGTERM) |
| `response_decode` | A 2xx body that is not one JSON object, carries a non-null `error` member, has no `choices` array, has a wrong-typed field, or exceeds 64 MiB |
| `argument_decode` | A tool call whose arguments do not decode to a JSON object |
| `empty_response` | Zero choices, or an assistant reply with no content and no tool calls |
| `replay_divergence` | The candidate's tool calls do not match the scripted route |
| `internal` | Anything else |
| `artifact_write` | An evidence file could not be written (in `artifact_error`) |

Stages: `input`, `request`, `response`, `replay`, `publish`.
