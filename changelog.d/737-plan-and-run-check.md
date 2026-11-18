### Changed — golem routes on the capability evidence log, and explicit capability checks show what they send and delete nothing (#737)

Golem now reads and writes capability answers in the shared store's evidence log, the same log `configview.ExplainCapabilities` reads, and `golem models -probe-all` and `-reprobe` run through a new library pair, `configio.PlanCheck` and `configio.RunCheck`, that sends exactly the checks it showed.

#### Golem routing and automatic checks

- Golem routes on the evidence log. A checked no beats a catalog or type yes. Answers in the old `capability_probes` table stop counting; an older golem keeps working on the same file and does not see new answers.
- Automatic checks share an allowance of two requests per golem invocation, startup included, which buys one model's tools check. An exhausted allowance leaves the model unknown, never a no. The progress line says so once per model; the startup preflight message and each `/model set` reply that meets it repeat the reason: the next run has a fresh allowance, and `golem models -probe-all` checks the configured agent chain. A chain whose first entry comes back inconclusive can fail startup on that run and move on at the next, since a matching inconclusive attempt suppresses automatic checks for 24 hours.
- Each automatic check prints its progress on stderr: before its first request (model, endpoint host, method, requests reserved, output tokens requested per request), then when it finishes or is cancelled. None of it reaches stdout, a `-p` record or the headless stream.
- `-no-cap-probe` turns automatic checks off and, as before, makes routing ignore stored checks; the store still opens for a derived input ceiling.
- Startup preflight reports a refused check (allowance used up, the capability store unreadable, or a provider with no evidence binding) as an ordinary error with its reason, never as a configured capability gap.
- A model whose provider reports no tool support no longer lowers golem's input ceiling.

#### `golem models`

- `-probe-all` and `-reprobe` delete nothing. For every provider of the agent chain they print, on stderr, what they will check (each quoted model, its method, usual and maximum requests, output tokens requested per request, request timeout, response byte limit, and whether it may load the model), then check exactly that and print each result there; `-reprobe` re-checks fresh answers too. stdout keeps the listing's one line per entry. A provider with more than 25 distinct models is refused (`batch_too_large`) rather than split into unshown checks.
- An Ollama model is checked with one `/api/show` and filed under the content digest golem's listing reports for it, under either spelling (`m` or `m:latest`); once checked it is not re-read until `-reprobe` or a new model version.
- Ctrl-C (or SIGTERM) stops a check in flight or the listing, sends nothing more and reports the cancellation.
- `golem models -json` reports a checked no, and Ollama's live no (a capabilities array without tools), as a known no, so the candidate is ineligible rather than unknown. A listed `m:latest` still does not answer for a configured `m`.
- `golem models` prints `tool_call=MISSING (runtime)`, not `(unknown)`, when Ollama's live capabilities array leaves out tools.
- A tools check requests up to 2,048 output tokens per request and sends at most two requests. A server may ignore the token request; local time and response bytes are bounded, billing is the provider's. It is not an end-to-end tool round-trip certificate. A reply that completes but carries an unusable tool call reads as chat and stream yes, tool_call `tool_call_invalid`. A local openai-compat check may load a model under llama-swap.

#### Library

- `configio.PlanCheck` plans an explicit check of one provider's models offline, from the config and one store read, and seals the plan to its key and endpoint; `configio.RunCheck` sends exactly that plan through its own guarded client, never through a model registry. A run refuses a plan whose key or endpoint changed (`plan_stale`) and a remote endpoint its policy does not grant (`destination_not_granted`), before sending anything. A plan holds 25 models at most. `CheckOptions.RuntimeDigests` files results under the identity the host routes on. An executable plan stays in the process that made it.
- New check codes on `configio.ErrorCode`: `auth_failed`, `model_refused`, `refused`, `rate_limited`, `timeout`, `unreachable`, `server_error`, `rejected`, `malformed_response`, `destination_not_granted`, `plan_stale`, `listing_required`, `batch_too_large`, `response_too_large`. A 403 or 404 is `model_refused` and remembered for 7 days only when another model gave a completed reply in the same check run; otherwise it is `refused` and not saved.
- `fingerprint.CapabilityProbeResult` gains `Digest`, the digest the reply named, and the registry's automatic Ollama writer files under it first. Golem passes its explicit checks the content digests from its own startup listing, so their answers are filed under the identity its routing reads.
- A name-only yes goes stale after 30 days and still counts; daily use does not refresh it (#741).

#### Known issues

- Ctrl-C during golem's startup ends golem without a `cancelled` line; no further check request is sent and nothing is half-saved.
- Startup preflight can accept a model that has a tool answer but lacks chat or stream, which routing then rejects.
- While the capability store cannot be read after opening, every model lookup re-queries the provider, a kept no is dropped, and no golem output says the store could not be read. An unsaved check's yes beside a catalog yes is labelled `catalog`.
- A library host that checks an Ollama model without a runtime digest or a current stored listing, against a server that names no digest, files a name-only result that golem's routing does not read. A re-pulled Ollama model reads under its old digest until the next listing.
- FIM admission differs between the evidence-log and legacy paths in both directions, and its refusal says "decided capabilities exclude insert" when insert is only unknown.
- `ExplainToolCall` still lists the provider's models unless an explicit list answers. `mcp.WithFingerprintStore` hosts stay on the old table and the non-streaming probe. Per-capability assertions and `insert`/`vision` checks are not yet available.
