### Added — llm-bench injection-resistance corpus and report (#452)

- `docs/llm/traces/injection/`: a committed, frozen-real-wire prompt-injection corpus (16 bases across source files, guidance files, retrieval chunks and MCP output; blunt, helpful, encoded, jailbreak and Best-of-N variants; one clean twin per base), built from Golem's application prompt and always-on guards with `-build-injection-corpus` and guarded by regeneration, balance and wire-fidelity tests.
- `-injection-report`: first-decision `InjectionResistance`, `RestraintUnderAttack` and anchor-match AnswerQuality per model, with equal-base estimates, paired bootstrap CIs on common cells, twin contrasts and explicit completeness; bound to a pre-registration file.
- `-calibrate-capture` writes capture manifest v3 for injection traces and keeps every failed attempt; `-scorer llm-judge` refuses injection traces.
- `-candidate-session-id` sends its value as the openai-compat session header on every candidate request (injection and ordinary chat traces alike); empty sends none. Hosted endpoints that require a session header need it.
- The injection registration lets a hosted model register its host-fixed temperature with a `host_fixed_decoding` declaration; local models stay at temperature 0.
- The registered temperature per model is recorded in capture manifests and artifact provenance, and a capture that mixes registered temperatures is refused (one capture per temperature).
- The report's Completeness table gains Serving stack and Temperature columns.
