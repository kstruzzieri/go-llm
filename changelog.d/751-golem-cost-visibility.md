### Added — Golem cost and side-task visibility (#751)

Golem's `/context` now prints this process's model usage and an estimated cost,
including the side tasks that used to be invisible. A model entry in
`models.json` takes an optional `price`, in USD per 1,000,000 tokens:
`{"input": 3, "output": 15, "cached_input": 0.3, "cache_write": 3.75}`. `input`
and `output` are the plain rates and both are required (a free model is
`{"input": 0, "output": 0}`); `cached_input` and `cache_write` price tokens
served from, and written to, a provider's prompt cache, and a missing one bills
those tokens at `input`. Any other member, such as a typo'd `input_per_mtok`, is
refused. A refused price, or a value that is not finite and at least 0, makes
the config fail to load, naming the entry, and two entries that name the same
`provider/model` and both set a price must carry the same one. There is no
built-in price table: a model without a `price` is never guessed. Prices are
read at startup, so a `models.json` edit takes effect on restart.

`/context` then shows, after the `configured input ceiling` line: `usage (this process)` (agent
input, cached share, output and requests across every model used, `/model`
switches included), `last run`, `est. cost`, and one `side task <name>` line for
each of `summarize`, `probe`, `embed`, `grounding`, `source-summary` and
`delegate` that has made a call. `est. cost` prices each call by the
`provider/model` that served it, so a fallback is priced at the model that
answered. Totals are process-local and reset on restart.

Startup prints `notice: summarize uses the same model as chat (<provider/model>);
set defaults.summarize to a cheaper model to lower compression cost` when the
summarize route and the active chat route start with the same model and that
model has a nonzero `input` or `output` price.

The figures never claim more than the provider reported. A model with no price
is named under `excludes unpriced` and left out of the dollar total. A request
with no input count, or with a count Golem had to clamp (a negative count,
cached tokens above input tokens, or cache-write tokens above the input left
after cached), is flagged `unreliable usage`. A priced bucket with such a
request is named under `excludes unreliable usage` and also left out of the
total; an unpriced one is already under `excludes unpriced`. Both clauses name
an agent-run bucket by its model and a side-task bucket by model and task, as
in `a/priced (probe)`. Dispatch children report one total without a
model, so they appear under `dispatch children: N input tok, cache coverage
unknown` and under `excludes unpriced`. Successful children with missing or
invalid counters remain under `excludes unpriced: dispatch children` even
when no child tokens can be reported. Measured 2026-11-01 on Abacus RouteLLM
with gpt-5-mini: a streaming response's prompt count was smaller than its
cached count, and a non-streaming response had no prompt count, so both read as
unreliable. An Ollama embedding reports the server's `prompt_eval_count` as its
input count; it used to report the number of input texts, which also changes
`usage.prompt_tokens` on the compat `/v1/embeddings` endpoint for Ollama-backed
models. A server that omits the count reports no input count, so the call reads
as unreliable. A side-task call that fails is not counted, and a rare concurrent
identical embed request can be counted twice.

The new docs section "Calls, caching and cost" in `docs/golem.md` lists every
model call Golem makes with the flag that gates it, the provider cache
lifetimes as of 2026-10-30, and how to read each `/context` line.

#### Library additions

- `config.ModelPrice` (with a strict `UnmarshalJSON`) and
  `config.ModelConfig.Price`; `providerbootstrap.Bundle.Prices` maps each priced
  `provider.ModelKey` to its price. `config.Document.SetRoleModel` and
  `config.Document.ForkRoleModel` clear the price when the role, or the fork,
  moves to a different provider/model; `SetRoleModel` reports it as
  `SetRoleModelResult.DroppedPrice`.
- `ollama.Client.EmbedWithPromptCount` is `Embed` plus the server's
  `prompt_eval_count`, which `ollama.EmbedResponse.PromptEvalCount` now
  carries. `Embed` is unchanged. `provider.OllamaProvider.Embed` reports the
  summed count as `Usage.PromptTokens`, or 0 when the server omits any.
- `agent.NewRouterSummarizerWithUsage` is `NewRouterSummarizer` plus a callback
  that receives the model and usage of each successful summarizer call.
  `NewRouterSummarizer` is unchanged.
- `probers.WithOpenAICompatUsageObserver`, `ProberFactoryInput.UsageObserver` and
  `providerbootstrap.Options.ProbeUsageObserver` report the usage of each
  successful openai-compat probe request, both tool-capability probes and
  fingerprint profiling probes (Golem runs only the former; a library host that
  enables profiling gets the latter too). Ollama probes are not reported. The
  observer must be safe for concurrent use. Probe results are unchanged.
