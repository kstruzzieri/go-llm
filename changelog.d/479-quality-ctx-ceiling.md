### Fixed — Quality-sensitive routes budget against the trained context (#479)

`ModelProfile.QualityCtxCeiling` was declared and read by the router's
admission budget and Golem's per-turn input ceiling, but nothing populated it,
so `chat`, `reasoning`, and `code-review` always assembled against the full
advertised window. It is now populated from a per-family
`quality_ctx_ceiling` in the embedded catalog (Qwen3.5 and Qwen3.6: 262,144)
and from a new per-model `quality_ctx_ceiling` in `models.json`, which wins
over the catalog.

#### Behavior change

- `planning` and `analysis` are now quality-sensitive; `agent`, `fim`,
  `embedding`, and side tasks are unchanged.
- `chat`, `reasoning`, `code-review`, `planning`, and `analysis` are the
  quality-sensitive use cases; `agent`, `fim`, `embedding`, and side tasks are
  unchanged. The MCP `generate` tool routes as `chat`, so it is
  quality-sensitive too.
- A cut happens only when a ceiling applies, it is below the known served
  window (or the window is unknown), and the use case is quality-sensitive.
  The budget is then the ceiling, clamped to a known window; with an unknown
  window the configured ceiling is the budget. A ceiling at or above a known
  window changes nothing. No catalog family changes at its default catalog
  window; the catalog ceiling engages for Qwen3.5 and Qwen3.6 served above
  262,144.
- A smaller budget also lowers the router's headroom score, which can change
  model selection, sticky routing, and fallback order before admission fails.
- Golem's per-turn input ceiling follows the active route's use case: in
  `-goal` mode that is `planning`, so the chain minimum uses the same reduced
  budget the router applies, including a configured ceiling on a model whose
  window is unknown. The ordinary agent loop and an explicit input-ceiling
  override are unaffected.
- Catalog defaults match by parsed family name; Hugging Face repository or
  GGUF identifiers may not match, so set `quality_ctx_ceiling` explicitly for
  them.
- The `qwen2.5` and `qwen3` families are deliberately unseeded: `qwen2.5` also
  matches the Qwen2.5-1M models (trained to 256K), and `qwen3` also matches
  262,144-native Qwen3-Coder and 2507 models. Users serving an ordinary
  Qwen2.5 or original-release Qwen3 checkpoint with a YaRN window should set
  `quality_ctx_ceiling: 32768`.
- Opt out per model by setting `quality_ctx_ceiling` equal to `context_window`.
- `quality_ctx_ceiling` of 0 or omitted means unset; positive values override
  the catalog; Load rejects negative values.
- Two roles on the same provider/model with different nonzero
  `quality_ctx_ceiling` values are now a selector conflict at Load.
- `configview` (and `golem models -json`, the MCP configview resource) gains an
  optional `quality_ctx_ceiling` field, omitted at 0, on inventory models and
  model summaries. It is the raw merged ceiling, not a per-use-case window,
  and may exceed `context_window`; a config-only summary (as in the MCP
  resource) shows only a configured value, not catalog defaults. The package
  doc now states the v1 extension policy: v1 may gain optional `omitempty`
  fields, consumers must ignore unknown members, and renaming, removing, or
  retyping a field still requires v2.
- `config.SetRoleModelResult` gains `DroppedQualityCtxCeiling`. `SetRoleModel`
  drops a role's ceiling, and reports it, only when the role's provider/model
  changes; re-selecting the same model keeps it. `ForkRoleModel` likewise
  keeps the ceiling only on a fork to the source's own provider/model.
