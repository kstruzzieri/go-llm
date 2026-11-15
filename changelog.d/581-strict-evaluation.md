### Added — Strict approval workflow evaluation (#581)

- A scripted evaluation of opt-in `-strict-approval` over eight workflows, run with strict approval off and on and counting prompts, with its generated record in `docs/security/strict-mode-evaluation.md`.
- A native-sandbox leg that checks an exposed conversation's commands lose IP network under `-sandbox-network allowed`; it skips on a host with no native backend.
- A decision record, `docs/security/strict-mode-decision.md`, with the limits and the #581 acceptance map, and `docs/least-privilege.md` rewritten to describe the shipped behavior, including that strict approval does not limit what reaches the model.
