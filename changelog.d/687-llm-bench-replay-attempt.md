### Added — llm-bench single-attempt capture mode (#687)

`llm-bench -replay-attempt` replays one trace against one openai-compat candidate exactly once
and publishes an immutable attempt directory: raw request and response bytes under `raw/`, and
an `attempt.json` (schema `llm-bench.replay-attempt/v1`) with identity, build and settings
provenance, the ordered partial transcript, per-request timing and usage, and one of ten stable
error codes. Failed attempts keep their timing and any usage the server reported; unknown usage
stays `null`. Every request carries `X-Client-Correlation: <attempt-id>/<ordinal>`. There are no
retries and no followed redirects. The process exits 0 once `attempt.json` is published, whatever
the attempt's status. The mode runs on darwin and linux and refuses on windows. Legacy
`-calibrate-capture` and replay output is unchanged. See `docs/llm/replay-attempt.md`.
