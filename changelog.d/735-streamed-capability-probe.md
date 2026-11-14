### Added — Streamed tool-call check and capability resolution from the evidence log (#735)

A host that attaches the shared capability store's evidence log to its model registry now resolves tool calls from that log. On an OpenAI-compatible server (llama.cpp, vLLM, LM Studio) such a registry checks a model with one streamed chat request offering a single no-argument tool, so a check that passes also shows that the model chats and streams. Golem's own behavior is unchanged in this release step: it does not attach the log yet, so it keeps the existing `capability_probes` table and its `-reprobe` flag works as before.

#### The streamed check

- The check streams `POST /v1/chat/completions` with `max_tokens` 2048 and sends none of the model card's sampling settings. Its responses are size-limited and its redirects are refused; ordinary chat requests are not affected.
- A yes needs a well-formed call of the offered tool. A reply that does not settle the question is inconclusive and carries one of five codes: `tool_ignored`, `tool_not_called`, `request_rejected`, `cut_off` or `tool_call_invalid`. An inconclusive result holds off a new check of that model for 24 hours.
- On Ollama the check stays a metadata read of `/api/show`; it sends no generation request.

#### Responses

- `provider.ChatResponse` has a new `FinishReason` field (`finish_reason` in JSON), set by the OpenAI-compatible provider. Golem trace files carry it for each step.
- The OpenAI-compatible shim (`compat`) now passes an upstream finish reason through when the provider reports one, so a reply cut off at `length` reports `length` even when it carries a tool call. The one exception: a reply carrying tool calls whose upstream reason is `stop` still reports `tool_calls`, as before, because some servers send `stop` with tool calls and clients run their tool loop on `tool_calls`.

#### Behavior changes

- Capability names from Ollama's `/api/show` are matched case-insensitively, and `tool_call` is accepted beside `tools`. This also changes the existing native tool-call probe: a model listing `Tools` or `tool_call` now reads yes.
- Recommendations break an exact tie on provider name, then model name. Exactly tied models used to come out in a different order from run to run; they now list in a fixed order, and with tool-call checks that order decides which model is checked first.

#### Registries with an evidence log attached

These apply to a registry given `provider.WithEvidenceLog` (or `Options.EvidenceLog` in the internal bootstrap package). Without a log, resolution works as before.

- Tool-call resolution reads and writes only the evidence log. It never reads or writes the `capability_probes` table, and a lookup never starts fingerprint profiling on such a registry; stored profiles are still read. An answer already in the log is used with no request, including one another process wrote.
- Automatic checks share an allowance of two generation requests per registry, whoever asks for them. The allowance counts the requests the check sends; redirects are refused rather than followed, and a resend made below the HTTP client is not counted. A request that was never sent, for example because the connection was refused, does not count. A check that panics spends everything it reserved, since a request may already have gone out. Time does not refill it. A check the allowance cannot fund sends nothing, saves nothing and returns `provider.ErrAutomaticProbeBudgetExhausted`; it is never read as a no. Ollama metadata checks do not spend it.
- A provider with no evidence binding returns `provider.ErrEvidenceUnbound`, and a log that cannot be read returns `provider.ErrEvidenceUnreadable`. Neither starts a capability check, though the registry may still ask the server for the model's runtime details first. `configio.ProbeToolCall` reports the first as `probe_unavailable` and the second, like an exhausted allowance, as `probe_failed`.
- `provider.WithCapabilityProbeProgress` reports a generation check as it starts and ends, or when the allowance refuses it, with request counts and the endpoint's host only.
- When a completed check's result cannot be saved, the registry keeps it in memory for its whole life and uses it while the result is still valid and the log can be read, so a route right after the check is still served. It is not saved later, and other processes do not see it. `provider.ToolCallExplanation.Unsaved` marks such a result, and `configio.ProbeToolCall` reports it as `Persisted: false` unless an explicit declaration or the catalog supplies the answer.

#### New exported names

- `fingerprint`: `CapabilityProber`, `CapabilityProbeRequest`, `CapabilityProbeResult`, `ProbeAnswer`, `RecipeForRequest`, and the `Detail*` constants (`DetailToolCalled` and the five inconclusive codes). The shipped probers implement `CapabilityProber` through the new methods `(*fingerprint.OllamaProber).ProbeCapabilities` and `(*probers.OpenAICompatProber).ProbeCapabilities` (package `fingerprint/probers`).
- `provider`: `WithCapabilityProbeProgress`, `CapabilityProbeProgress`, `ErrAutomaticProbeBudgetExhausted`, `ErrEvidenceUnbound`, `ErrEvidenceUnreadable`, and the `Unsaved` field of `ToolCallExplanation`.
- `providerbootstrap.Options` gains `EvidenceLog` and `CapabilityProbeProgress`. That package is internal, so only code inside this module can use them.
