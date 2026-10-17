### Fixed — Quality-sensitive routes budget against the trained context (#479)

`ModelProfile.QualityCtxCeiling` was declared and read by the router's
admission budget and Golem's per-turn input ceiling, but nothing populated it,
so `chat`, `reasoning`, and `code-review` always assembled against the full
advertised window. It is now populated from a per-family
`quality_ctx_ceiling` in the embedded catalog (Qwen2.5: 32,768; Qwen3.5 and
Qwen3.6: 262,144) and from a new per-model `quality_ctx_ceiling` in
`models.json`, which wins over the catalog.

#### Behavior change

- `chat`, `reasoning`, `code-review`, `planning`, and `analysis` requests are
  budgeted against `min(trained context, served window)`: the router admits
  less input and Golem compacts earlier by the same amount. No catalog family
  changes at its default catalog window. The ceiling engages for Qwen2.5
  served with a YaRN window, Qwen3.5 and Qwen3.6 served above 262,144, and any
  model given a `quality_ctx_ceiling`.
- The `qwen3` family is deliberately unseeded: its name also matches
  262,144-native Qwen3-Coder and 2507 models. Original-release Qwen3 users
  running a YaRN window should set `quality_ctx_ceiling: 32768`.
- `planning` and `analysis` are now quality-sensitive; `agent`, `fim`,
  `embedding`, and side tasks are unchanged.
- Opt out per model by setting `quality_ctx_ceiling` equal to `context_window`.
- `quality_ctx_ceiling` must be >= 1; Load rejects negative values.
- Two roles on the same provider/model with different `quality_ctx_ceiling`
  values are now a selector conflict at Load.
- `configview` (and `golem models -json`, the MCP configview resource) gains an
  additive `quality_ctx_ceiling` field on models.
- `config.SetRoleModelResult` gains `DroppedQualityCtxCeiling`: re-pointing a
  role at a different model drops its ceiling, and forking a role does not
  copy it.
