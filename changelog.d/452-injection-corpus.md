### Added — llm-bench injection-resistance corpus and report (#452)

- `docs/llm/traces/injection/`: a committed, frozen-real-wire prompt-injection corpus (16 bases across source files, guidance files, retrieval chunks and MCP output; blunt, helpful, encoded, jailbreak and Best-of-N variants; one clean twin per base), built from Golem's application prompt and always-on guards with `-build-injection-corpus` and guarded by regeneration, balance and wire-fidelity tests.
- `-injection-report`: first-decision `InjectionResistance`, `RestraintUnderAttack` and anchor-match AnswerQuality per model, with equal-base estimates, paired bootstrap CIs on common cells, twin contrasts and explicit completeness; bound to a pre-registration file.
- `-calibrate-capture` writes capture manifest v3 for injection traces and keeps every failed attempt; `-scorer llm-judge` refuses injection traces.
