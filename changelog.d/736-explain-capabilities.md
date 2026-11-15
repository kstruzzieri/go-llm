### Changed — offline capability explanations, and a checked no now beats a catalog or type yes on the evidence-log path (#736)

Hosts can now explain a model's capabilities, and a provider's model listing, without contacting any server: `configview.ExplainCapabilities` and `configview.ExplainListing` each make at most one read of the shared capability store (none with a nil store) and no other I/O. Golem's default behavior does not change in this release step: it does not attach the evidence log yet, so the precedence and probe changes below apply only to a registry given `provider.WithEvidenceLog`. Without a log, routing, listings and explanations work as before.

#### Offline explanations

- Pass `configview.ExplainCapabilities` the effective config the registry was built from and the catalog from `provider.LoadCatalog`; with a nil catalog the catalog tier is skipped. `config.(*Config).CapabilityPolicy` returns what the configuration says about one model's capabilities (its explicit list and the capabilities its type implies), read the way the registry reads it.

#### One precedence on the evidence-log path

- Routing, the listing projection (`ProjectListedModels`), `ExplainToolCall` and `configview.ExplainCapabilities` rank capability evidence the same way: an explicit `capabilities` list first, then an answer from a check or from use, then what the provider reports, then the catalog, then the model type.
- A checked no beats a catalog or type yes: a model the catalog lists with tool calls, but whose check answered no, is not routed for tool calls.
- What the provider reports as missing also beats a catalog yes: an Ollama model whose capabilities array lacks `tools` is not routed for tool calls, even when the catalog says it supports them.
- Evidence stored under either Ollama spelling of a model (`m` or `m:latest`) counts for both, so a stored answer under `qwen3:latest` settles `qwen3` without a check.
- Capabilities in stored fingerprint profiles do not add to a model's capabilities. Those profiles still supply memory and context-window figures.
- A listing projection's `KnownMask` also marks the capabilities the evidence says no to.
- FIM routing and completion follow the decided `insert` capability: an explicit list without `insert` refuses FIM (error "the model's decided capabilities exclude insert") even when the Ollama template uses `.Suffix`, and an explicit list with `insert` admits it even when the template does not. Without the evidence log the template still decides FIM, as before.

#### Known differences between the offline explanations and routing

- The offline explanations see what Ollama reports about a model only after a refresh publishes it (`configio.RefreshInventory` with `WithPublish`); routing reads it live.
- They ignore the model family Ollama reports, so a model the catalog knows only by that family gets no catalog answer offline.
- When the store cannot be read they return `provider.ErrEvidenceUnreadable`, while routing keeps serving an answer it has already cached.

#### Tool-call explanations and probe results

- `provider.ToolCallExplanation` has a new `EvidenceUnreadable` field, true when that explanation could not read the evidence log and so may hide a stored answer.
- `configio.ProbeToolCall` reports a result whose save failed as `Persisted: false` even when the catalog also says yes, and does the same for any result explained while the log could not be read. After a restart a stored no, or the provider's own report, could overturn either, so the report errs toward asking for a new check. An explicit `capabilities` list still counts as persisted.
- With the evidence log attached, `ExplainToolCall` no longer runs the profile merge, so it no longer calls a hook installed with `SetOverrideRejectionHook`; `Lookup` and the listing projection still do.

#### New exported names

- `provider`: `ExplainEvidence`, `EvidenceInputs`, `CapabilityFact`, `CheckedAnswer`, `CapabilityFlag`, `OtherAdapterAnswer`, `HistoryAnswer`, `CapabilityMasks`, `Catalog`, `LoadCatalog`, `(*Catalog).ProfileFor`, `EvidenceModelNames`, and the `EvidenceUnreadable` field of `ToolCallExplanation`.
- `fingerprint`: `DetailRunRefused`.
- `config`: `ModelCapabilityPolicy` and `(*Config).CapabilityPolicy`.
- `configview`: `ExplainCapabilities`, `ExplainListing`, `AccessFact`, `CapabilityExplanation` (with its `Masks` method), `ListedModel` and `ListingExplanation`.
