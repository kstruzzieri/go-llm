### Fixed — Quality-sensitive routes budget against the trained context (#479)

`ModelProfile.QualityCtxCeiling` was declared and read by the router's
admission budget and Golem's per-turn input ceiling, but nothing populated it,
so `chat`, `reasoning`, and `code-review` always assembled against the full
advertised window. It is now populated from a per-family
`quality_ctx_ceiling` in the embedded catalog (Qwen3 and Qwen2.5: 32,768;
Qwen3.5 and Qwen3.6: 262,144) and from a new per-model `quality_ctx_ceiling`
in `models.json`, which wins over the catalog.

#### Behavior change

- For models in those families, `chat`, `reasoning`, `code-review`,
  `planning`, and `analysis` requests are budgeted against
  `min(trained context, served window)`: the router admits less input and
  Golem compacts earlier by the same amount. At default windows this changes
  Qwen3 (40,960 to 32,768); the others engage only when the served window is
  configured above the trained length.
- `planning` and `analysis` are now quality-sensitive; `agent`, `fim`,
  `embedding`, and side tasks are unchanged.
- Opt out per model by setting `quality_ctx_ceiling` equal to `context_window`.
- `configview` (and `golem models -json`, the MCP configview resource) gains an
  additive `quality_ctx_ceiling` field on models.
- `config.SetRoleModelResult` gains `DroppedQualityCtxCeiling`: re-pointing a
  role at a different model drops its ceiling, and forking a role does not
  copy it.
